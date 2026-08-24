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

// Add schedules a job (persisted). The job is snapshotted under the lock so
// persist (and any later caller) never reads fields being written by tick.
func (m *CronJobManager) Add(job *Job) {
	m.mu.Lock()
	job.Enabled = true
	m.jobs[job.ID] = job
	m.armJobLocked(job)
	m.computeNextRunLocked(job, time.Now())
	snapshot := job.clone()
	m.mu.Unlock()
	m.persist(snapshot)
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

// UpdateJob atomically mutates an existing job under the manager lock, then
// re-arms and persists it (recomputing NextRun). The mutate callback runs while
// holding the lock, so the job's fields — including the Payload map — are safe
// to modify without racing the cron tick goroutine or concurrent List/Get.
// Returns false when the job does not exist.
func (m *CronJobManager) UpdateJob(id string, mutate func(*Job)) bool {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return false
	}
	mutate(job)
	m.armJobLocked(job)
	m.computeNextRunLocked(job, time.Now())
	snapshot := job.clone()
	m.mu.Unlock()
	m.persist(snapshot)
	return true
}

// AddActiveJob creates an active_agent job (cron or one-time run_at).
func (m *CronJobManager) AddActiveJob(name, cronExpr string, payload map[string]interface{}, description, timezone string, runOnce bool, runAt time.Time) (*Job, error) {
	if runOnce && runAt.IsZero() {
		return nil, fmt.Errorf("run_at is required when run_once=true")
	}
	if !runOnce && cronExpr == "" {
		return nil, fmt.Errorf("cron_expression is required when run_once=false")
	}
	if !runOnce {
		if _, err := ParseCron(cronExpr); err != nil {
			return nil, fmt.Errorf("invalid cron_expression %q: %w", cronExpr, err)
		}
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
	// 返回副本（Get 在锁内 clone），调用方读 NextRun 等字段不会与 tick 的
	// 持锁写竞争。
	return m.Get(job.ID), nil
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

// clone returns a copy of the job with a deep-copied Payload (nested maps and
// slices included), so callers outside the manager lock cannot mutate (or race)
// the live job state.
func (j *Job) clone() *Job {
	c := *j
	c.Payload = deepCopyPayload(j.Payload)
	return &c
}

func deepCopyPayload(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = deepCopyPayloadValue(v)
	}
	return out
}

func deepCopyPayloadValue(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		return deepCopyPayload(val)
	case []interface{}:
		out := make([]interface{}, len(val))
		for i, e := range val {
			out[i] = deepCopyPayloadValue(e)
		}
		return out
	default:
		return v
	}
}

// snapshotForFireLocked clones a job for fire-and-forget execution and rebinds
// its handler to the snapshot, so the handler only reads the deep-copied
// payload and never races UpdateJob's lock-protected writes (the original
// Handler closure captures the live job pointer). Caller must hold m.mu.
func (m *CronJobManager) snapshotForFireLocked(job *Job) *Job {
	snap := job.clone()
	if h, ok := m.handlers[job.JobType]; ok {
		snap.Handler = func(ctx context.Context) error { return h(ctx, snap) }
	} else {
		snap.Handler = nil
	}
	return snap
}

// Get returns a job by id. The returned job is a copy: callers may read it
// freely without racing the manager's lock-protected live state.
func (m *CronJobManager) Get(id string) *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return nil
	}
	return job.clone()
}

// RunNow immediately executes a job's handler in a background goroutine.
// It shares the tick's anti-overlap semantics: a job that is already running
// is rejected, and the handler runs against a locked-in snapshot so it never
// races UpdateJob's writes.
func (m *CronJobManager) RunNow(id string) error {
	m.mu.Lock()
	job := m.jobs[id]
	if job == nil {
		m.mu.Unlock()
		return fmt.Errorf("job not found: %s", id)
	}
	if job.Handler == nil {
		m.mu.Unlock()
		return fmt.Errorf("job %s has no handler", id)
	}
	if job.running {
		m.mu.Unlock()
		return fmt.Errorf("job %s is already running", id)
	}
	job.running = true
	snap := m.snapshotForFireLocked(job)
	runCtx := m.ctx
	m.mu.Unlock()
	// 任务挂到 manager 的运行上下文：Start 传入的 ctx 被取消（或 Stop）时，
	// 在途的 run-now 任务也能随之停止，而不是用无法取消的 Background。
	if runCtx == nil {
		runCtx = context.Background()
	}
	go func(live, j *Job) {
		defer func() {
			m.mu.Lock()
			live.running = false
			m.mu.Unlock()
		}()
		if err := j.Handler(runCtx); err != nil {
			logger.Error("Cron job %s run-now failed: %v", j.ID, err)
		}
	}(job, snap)
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

// dueJob pairs the live job (for clearing the running flag) with the locked-in
// snapshot the handler executes against.
type dueJob struct {
	live *Job
	snap *Job
}

func (m *CronJobManager) tick(ctx context.Context, now time.Time) {
	// tick 自身计入 WaitGroup，使内部 per-job 的 wg.Add 全部发生在 Stop 的
	// wg.Wait 观察到零计数之前（WaitGroup 约定），避免 Add 与 Wait 竞争。
	m.wg.Add(1)
	defer m.wg.Done()
	m.mu.Lock()
	due := []dueJob{}
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
		// 持锁阶段克隆快照：handler 只读触发时刻的任务快照（深拷贝 Payload），
		// 不与 UpdateJob 的持锁写并发。
		due = append(due, dueJob{live: job, snap: m.snapshotForFireLocked(job)})
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

	for _, dj := range due {
		m.wg.Add(1)
		go func(live, j *Job) {
			defer m.wg.Done()
			// 无论成败都清掉 running 标志，让下一次到点能再次触发。
			defer func() {
				m.mu.Lock()
				live.running = false
				m.mu.Unlock()
			}()
			if err := j.Handler(ctx); err != nil {
				logger.Error("Cron job %s failed: %v", j.ID, err)
			}
		}(dj.live, dj.snap)
	}
}

// computeNextRunLocked computes the next run time for a job based on its
// cron expression (recurring) or RunAt (one-time). If the schedule cannot be
// computed (missing/invalid cron expression or run_at), the job is pushed far
// into the future and disabled so a zero NextRun can never make IsDue return
// true forever (which would fire the job every tick).
func (m *CronJobManager) computeNextRunLocked(job *Job, now time.Time) {
	next, err := m.computeNextRun(job, now)
	if err != nil {
		job.Enabled = false
		job.NextRun = now.Add(cronUnparseableBackoff)
		logger.Error(
			"Cron job %s (%s) 无法计算下次执行时间，任务已禁用（cron 表达式请使用 5 或 6 字段格式）: %v",
			job.ID, job.Name, err,
		)
		return
	}
	job.NextRun = next
}

// computeNextRun derives the next fire time without touching manager state.
func (m *CronJobManager) computeNextRun(job *Job, now time.Time) (time.Time, error) {
	if m.nextRunFn != nil {
		t, err := m.nextRunFn(job)
		if err != nil {
			return time.Time{}, err
		}
		if t.IsZero() {
			return time.Time{}, fmt.Errorf("schedule never matches")
		}
		return t, nil
	}
	if job.RunOnce {
		if job.RunAt.IsZero() {
			return time.Time{}, fmt.Errorf("run_once job has no run_at")
		}
		return job.RunAt, nil
	}
	if job.CronExpression == "" {
		return time.Time{}, fmt.Errorf("job %s has no cron expression", job.ID)
	}
	sched, err := ParseCron(job.CronExpression)
	if err != nil {
		return time.Time{}, err
	}
	next := sched.Next(now)
	if next.IsZero() {
		return time.Time{}, fmt.Errorf("cron expression %q never matches", job.CronExpression)
	}
	return next, nil
}

// cronUnparseableBackoff is how far into the future a job with an
// unparseable schedule is pushed so it cannot fire repeatedly.
const cronUnparseableBackoff = 365 * 24 * time.Hour

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
	now := time.Now()
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
		if job.Payload == nil {
			job.Payload = map[string]interface{}{}
		}
		// The table has no run_at column; run_once jobs persist their schedule
		// in the payload (payload["run_at"], ISO datetime). Restore it so a
		// restart never rebuilds a RunOnce job with a zero RunAt — which would
		// make NextRun zero and fire (then delete) the job immediately.
		if job.RunOnce {
			if ra, _ := job.Payload["run_at"].(string); ra != "" {
				if t, err := time.Parse(time.RFC3339, ra); err == nil {
					job.RunAt = t
				} else if t, err := ParseRunAt(ra); err == nil {
					job.RunAt = t
				}
			}
			if job.RunAt.IsZero() {
				logger.I18nWarn("run_once 任务 %s（%s）缺少 run_at，已禁用以避免立即误触发", job.ID, job.Name)
				job.Enabled = false
			}
		}
		m.jobs[job.ID] = job
		m.armJobLocked(job)
		m.computeNextRunLocked(job, now)
		// A run_once task whose scheduled time already passed while the bot was
		// down can never fire again on reload (tick would run it once and then
		// delete it with no chance to honor the original schedule). Drop it
		// explicitly and warn instead of letting a stale job fire late.
		if job.RunOnce && !job.RunAt.IsZero() && job.Enabled && !job.NextRun.After(now) {
			logger.I18nWarn("run_once 任务 %s（%s）原定于 %v 执行，已过期，本次重启后丢弃", job.ID, job.Name, job.RunAt.Format(time.RFC3339))
			delete(m.jobs, job.ID)
			if err := m.db.DeleteCronJob(job.ID); err != nil {
				logger.Error("Failed to delete expired run_once cron job %s: %v", job.ID, err)
			}
			continue
		}
	}
	logger.Debug("Loaded %d cron job(s) from database", len(m.jobs))
}

func (m *CronJobManager) persist(job *Job) {
	if m.db == nil {
		return
	}
	// run_once jobs persist their schedule in the payload (there is no run_at
	// column in cron_jobs); Load() restores RunAt from it after a restart.
	if job.RunOnce && !job.RunAt.IsZero() {
		if job.Payload == nil {
			job.Payload = map[string]interface{}{}
		}
		if ra, _ := job.Payload["run_at"].(string); ra == "" {
			job.Payload["run_at"] = job.RunAt.Format(time.RFC3339)
		}
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

// List returns all scheduled jobs. Each job is a copy (see clone), so the
// caller can iterate without racing concurrent UpdateJob mutation.
func (m *CronJobManager) List() []*Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		result = append(result, j.clone())
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
