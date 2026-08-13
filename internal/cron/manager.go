// Package cron implements scheduled job management.
// Ported from astrbot/core/cron/ and astrbot/core/tools/cron_tools.py
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Cron")

// JobFunc is a scheduled task callback.
type JobFunc func(ctx context.Context) error

// Job represents a scheduled task.
type Job struct {
	ID             string
	Name           string
	Description    string
	JobType        string
	CronExpression string
	Timezone       string
	RunOnce        bool
	RunAt          time.Time
	Payload        map[string]interface{}
	Handler        JobFunc
	NextRun        time.Time
	Enabled        bool
	running        bool // 执行中标志（在 m.mu 保护下读写），防止同 job 重叠执行
}

// IsDue reports whether the job should fire at `now`.
func (j *Job) IsDue(now time.Time) bool {
	return !j.NextRun.After(now)
}

// JobHandler runs a job that fires. It receives the concrete job so it can
// read the payload (session, note, etc.).
type JobHandler func(ctx context.Context, job *Job) error

// CronJobManager manages scheduled tasks with persistence and a per-job-type
// handler registry. Handlers are not serializable, so on reload jobs are
// re-armed with the registered factory for their job type.
type CronJobManager struct {
	mu        sync.Mutex
	db        *db.Database
	jobs      map[string]*Job
	stop      chan struct{}
	handlers  map[string]JobHandler
	nextRunFn func(j *Job) (time.Time, error)
	// ctx 记录 Start 传入的运行上下文，RunNow 派生的任务随其取消，不再用
	// context.Background()（Start 内加锁写入/读取，避免数据竞争）。
	ctx context.Context
	// wg 跟踪正在执行的 job goroutine，Stop 时等待它们退出（关闭数据库前
	// 确保没有 job 仍在访问存储）。
	wg sync.WaitGroup
}

// NewCronJobManager creates a manager.
func NewCronJobManager(database *db.Database) *CronJobManager {
	return &CronJobManager{
		db:       database,
		jobs:     make(map[string]*Job),
		stop:     make(chan struct{}),
		handlers: make(map[string]JobHandler),
	}
}

// RegisterHandler registers the handler used when a job of `jobType` fires.
// Used to re-arm persisted jobs after a restart.
func (m *CronJobManager) RegisterHandler(jobType string, fn JobHandler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[jobType] = fn
	for _, j := range m.jobs {
		if j.JobType == jobType {
			m.armJobLocked(j)
		}
	}
}

// armJobLocked binds a job to its registered type handler.
func (m *CronJobManager) armJobLocked(job *Job) {
	h, ok := m.handlers[job.JobType]
	if !ok {
		job.Handler = nil
		return
	}
	j := job
	job.Handler = func(ctx context.Context) error { return h(ctx, j) }
}

// SetNextRunFn overrides how a job's next run is computed (used to support
// run_once vs cron expressions).
func (m *CronJobManager) SetNextRunFn(fn func(j *Job) (time.Time, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextRunFn = fn
	for _, j := range m.jobs {
		m.computeNextRunLocked(j, time.Now())
	}
}

// Add schedules a job (persisted).
func (m *CronJobManager) Add(job *Job) {
	m.mu.Lock()
	job.Enabled = true
	m.jobs[job.ID] = job
	m.armJobLocked(job)
	m.computeNextRunLocked(job, time.Now())
	m.mu.Unlock()
	m.persist(job)
	logger.Debug("Scheduled job %s (%s) type=%s next=%v", job.ID, job.Name, job.JobType, job.NextRun)
}

// SetEnabled enables or disables a job (persisted). Disabled jobs remain in
// the list but are not scheduled/fired.
func (m *CronJobManager) SetEnabled(id string, enabled bool) bool {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return false
	}
	job.Enabled = enabled
	if enabled {
		m.computeNextRunLocked(job, time.Now())
	}
	m.mu.Unlock()
	if m.db != nil {
		if err := m.db.UpdateCronJob(id, map[string]interface{}{"enabled": enabled}); err != nil {
			logger.Error("Failed to persist cron job %s enabled=%v: %v", id, enabled, err)
		}
	}
	logger.Debug("Cron job %s enabled=%v", id, enabled)
	return true
}

// UpdateJob re-arms and persists an already-existing job after in-place field
// edits. It preserves the job's Enabled state.
func (m *CronJobManager) UpdateJob(job *Job) {
	m.mu.Lock()
	m.jobs[job.ID] = job
	m.armJobLocked(job)
	m.computeNextRunLocked(job, time.Now())
	m.mu.Unlock()
	m.persist(job)
}

// AddActiveJob creates an active_agent job (cron or one-time run_at).
func (m *CronJobManager) AddActiveJob(name, cronExpr string, payload map[string]interface{}, description, timezone string, runOnce bool, runAt time.Time) (*Job, error) {
	if runOnce && runAt.IsZero() {
		return nil, fmt.Errorf("run_at is required when run_once=true")
	}
	if !runOnce && cronExpr == "" {
		return nil, fmt.Errorf("cron_expression is required when run_once=false")
	}
	job := &Job{
		ID:             fmt.Sprintf("job_%d", time.Now().UnixNano()),
		Name:           name,
		Description:    description,
		JobType:        "active_agent",
		CronExpression: cronExpr,
		Timezone:       timezone,
		RunOnce:        runOnce,
		RunAt:          runAt,
		Payload:        payload,
	}
	m.Add(job)
	return job, nil
}

// Remove cancels a job (persisted).
func (m *CronJobManager) Remove(id string) {
	m.mu.Lock()
	delete(m.jobs, id)
	m.mu.Unlock()
	if m.db != nil {
		if err := m.db.DeleteCronJob(id); err != nil {
			logger.Error("Failed to delete cron job %s: %v", id, err)
		}
	}
}

// Get returns a job by id.
func (m *CronJobManager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.jobs[id]
}

// RunNow immediately executes a job's handler in a background goroutine.
func (m *CronJobManager) RunNow(id string) error {
	m.mu.Lock()
	job := m.jobs[id]
	runCtx := m.ctx
	m.mu.Unlock()
	if job == nil {
		return fmt.Errorf("job not found: %s", id)
	}
	if job.Handler == nil {
		return fmt.Errorf("job %s has no handler", id)
	}
	// 任务挂到 manager 的运行上下文：Start 传入的 ctx 被取消（或 Stop）时，
	// 在途的 run-now 任务也能随之停止，而不是用无法取消的 Background。
	if runCtx == nil {
		runCtx = context.Background()
	}
	go func(j *Job) {
		if err := j.Handler(runCtx); err != nil {
			logger.Error("Cron job %s run-now failed: %v", j.ID, err)
		}
	}(job)
	return nil
}

// Start begins the cron loop.
func (m *CronJobManager) Start(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case now := <-ticker.C:
			m.tick(ctx, now)
		}
	}
}

// Stop halts the cron loop and waits for in-flight jobs to finish (bounded by
// a timeout). Job contexts derive from the manager context (cancelled by the
// lifecycle before Stop is called), so handlers should exit promptly; the
// timeout guards against a stuck handler.
func (m *CronJobManager) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(cronStopTimeout):
		logger.I18nWarn("关闭时定时任务在 %v 内未完成", cronStopTimeout)
	}
}

// cronStopTimeout bounds how long Stop waits for in-flight cron jobs.
const cronStopTimeout = 10 * time.Second

func (m *CronJobManager) tick(ctx context.Context, now time.Time) {
	m.mu.Lock()
	due := []*Job{}
	for _, job := range m.jobs {
		if job.Handler == nil || !job.Enabled {
			continue
		}
		if !job.IsDue(now) {
			continue
		}
		// 上次触发尚未执行完（长任务/超时），跳过本次，避免同 job 重叠执行。
		if job.running {
			logger.Debug("Cron job %s still running, skipping this tick", job.ID)
			continue
		}
		due = append(due, job)
		job.running = true
		// Advance next run.
		if job.RunOnce {
			delete(m.jobs, job.ID)
			if m.db != nil {
				if err := m.db.DeleteCronJob(job.ID); err != nil {
					logger.Error("Failed to delete one-shot cron job %s: %v", job.ID, err)
				}
			}
			continue
		}
		m.computeNextRunLocked(job, now)
	}
	m.mu.Unlock()

	for _, job := range due {
		m.wg.Add(1)
		go func(j *Job) {
			defer m.wg.Done()
			// 无论成败都清掉 running 标志，让下一次到点能再次触发。
			defer func() {
				m.mu.Lock()
				j.running = false
				m.mu.Unlock()
			}()
			if err := j.Handler(ctx); err != nil {
				logger.Error("Cron job %s failed: %v", j.ID, err)
			}
		}(job)
	}
}

// computeNextRunLocked computes the next run time for a job based on its
// cron expression (recurring) or RunAt (one-time).
func (m *CronJobManager) computeNextRunLocked(job *Job, now time.Time) {
	if m.nextRunFn != nil {
		if t, err := m.nextRunFn(job); err == nil {
			job.NextRun = t
		}
		return
	}
	if job.RunOnce {
		job.NextRun = job.RunAt
		return
	}
	if job.CronExpression != "" {
		if sched, err := ParseCron(job.CronExpression); err == nil {
			job.NextRun = sched.Next(now)
		}
	}
}

// Load reloads persisted jobs from the DB and re-arms them with registered
// handlers.
func (m *CronJobManager) Load() {
	if m.db == nil {
		return
	}
	rows, err := m.db.ListCronJobs()
	if err != nil {
		logger.I18nWarn("加载定时任务失败: %v", err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, row := range rows {
		job := &Job{
			ID:             row.JobID,
			Name:           row.Name,
			Description:    row.Description,
			JobType:        row.JobType,
			CronExpression: row.CronExpression,
			Timezone:       row.Timezone,
			RunOnce:        row.RunOnce,
			Enabled:        row.Enabled,
		}
		if row.Payload != "" {
			var payload map[string]interface{}
			if json.Unmarshal([]byte(row.Payload), &payload) == nil {
				job.Payload = payload
			}
		}
		m.jobs[job.ID] = job
		m.armJobLocked(job)
		m.computeNextRunLocked(job, time.Now())
	}
	logger.Debug("Loaded %d cron job(s) from database", len(m.jobs))
}

func (m *CronJobManager) persist(job *Job) {
	if m.db == nil {
		return
	}
	payloadJSON, err := json.Marshal(job.Payload)
	if err != nil {
		logger.Error("Failed to marshal cron job %s payload: %v", job.ID, err)
		return
	}
	_, found, err := m.db.GetCronJob(job.ID)
	if err != nil {
		// Distinguish "not found" (normal first persist) from a real DB error
		// (e.g. SQLITE_BUSY): the old code silently fell through to Create and
		// could duplicate the job.
		logger.Error("Failed to read cron job %s before persist: %v", job.ID, err)
		return
	}
	if found {
		if err := m.db.UpdateCronJob(job.ID, map[string]interface{}{
			"name":            job.Name,
			"description":     job.Description,
			"job_type":        job.JobType,
			"cron_expression": job.CronExpression,
			"timezone":        job.Timezone,
			"payload":         string(payloadJSON),
			"run_once":        job.RunOnce,
			"enabled":         job.Enabled,
		}); err != nil {
			logger.Error("Failed to persist cron job %s: %v", job.ID, err)
		}
	} else {
		if err := m.db.CreateCronJob(job.ID, job.Name, job.Description, job.JobType, job.CronExpression, job.Timezone, string(payloadJSON), job.RunOnce); err != nil {
			logger.Error("Failed to create cron job %s: %v", job.ID, err)
		} else if !job.Enabled {
			if err := m.db.UpdateCronJob(job.ID, map[string]interface{}{"enabled": false}); err != nil {
				logger.Error("Failed to persist cron job %s enabled=false: %v", job.ID, err)
			}
		}
	}
	if !job.NextRun.IsZero() {
		if err := m.db.SetCronJobNextRun(job.ID, job.NextRun.Format(time.RFC3339)); err != nil {
			logger.Error("Failed to persist cron job %s next_run: %v", job.ID, err)
		}
	}
}

// List returns all scheduled jobs.
func (m *CronJobManager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		result = append(result, j)
	}
	return result
}

// ListInfo returns job info as serializable maps (Python's serialize_job shape).
func (m *CronJobManager) ListInfo() []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]map[string]interface{}, 0, len(m.jobs))
	for _, j := range m.jobs {
		result = append(result, SerializeJob(j))
	}
	return result
}

// SerializeJob renders a job in the dashboard API shape.
func SerializeJob(j *Job) map[string]interface{} {
	note, _ := j.Payload["note"].(string)
	if note == "" {
		note = j.Description
	}
	runAt, _ := j.Payload["run_at"].(string)
	nextRun := ""
	if !j.NextRun.IsZero() {
		nextRun = j.NextRun.Format(time.RFC3339)
	}
	payloadCopy := map[string]interface{}{}
	for k, v := range j.Payload {
		payloadCopy[k] = v
	}
	return map[string]interface{}{
		"id":              j.ID,
		"job_id":          j.ID,
		"name":            j.Name,
		"description":     j.Description,
		"job_type":        j.JobType,
		"cron_expression": j.CronExpression,
		"timezone":        j.Timezone,
		"payload":         payloadCopy,
		"enabled":         j.Enabled,
		"persistent":      true,
		"run_once":        j.RunOnce,
		"note":            note,
		"run_at":          runAt,
		"next_run_time":   nextRun,
	}
}

// FormatDuration returns a human-readable duration string.
func FormatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}
