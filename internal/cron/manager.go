// Package cron implements scheduled job management.
// Ported from astrbot/core/cron/ and astrbot/core/tools/cron_tools.py
package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/db"
	"github.com/AstrBotDevs/AstrBot/internal/log"
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
	logger.Info("Scheduled job %s (%s) type=%s next=%v", job.ID, job.Name, job.JobType, job.NextRun)
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
		_ = m.db.UpdateCronJob(id, map[string]interface{}{"enabled": enabled})
	}
	logger.Info("Cron job %s enabled=%v", id, enabled)
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
		_ = m.db.DeleteCronJob(id)
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
	m.mu.Unlock()
	if job == nil {
		return fmt.Errorf("job not found: %s", id)
	}
	if job.Handler == nil {
		return fmt.Errorf("job %s has no handler", id)
	}
	go func(j *Job) {
		if err := j.Handler(context.Background()); err != nil {
			logger.Error("Cron job %s run-now failed: %v", j.ID, err)
		}
	}(job)
	return nil
}

// Start begins the cron loop.
func (m *CronJobManager) Start(ctx context.Context) {
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

// Stop halts the cron loop.
func (m *CronJobManager) Stop() {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
}

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
		due = append(due, job)
		// Advance next run.
		if job.RunOnce {
			delete(m.jobs, job.ID)
			if m.db != nil {
				_ = m.db.DeleteCronJob(job.ID)
			}
			continue
		}
		m.computeNextRunLocked(job, now)
	}
	m.mu.Unlock()

	for _, job := range due {
		go func(j *Job) {
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
		logger.Warn("Failed to load cron jobs: %v", err)
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
	logger.Info("Loaded %d cron job(s) from database", len(m.jobs))
}

func (m *CronJobManager) persist(job *Job) {
	if m.db == nil {
		return
	}
	payloadJSON, _ := json.Marshal(job.Payload)
	if _, found, err := m.db.GetCronJob(job.ID); err == nil && found {
		_ = m.db.UpdateCronJob(job.ID, map[string]interface{}{
			"name":            job.Name,
			"description":     job.Description,
			"job_type":        job.JobType,
			"cron_expression": job.CronExpression,
			"timezone":        job.Timezone,
			"payload":         string(payloadJSON),
			"run_once":        job.RunOnce,
			"enabled":         job.Enabled,
		})
	} else {
		_ = m.db.CreateCronJob(job.ID, job.Name, job.Description, job.JobType, job.CronExpression, job.Timezone, string(payloadJSON), job.RunOnce)
		if !job.Enabled {
			_ = m.db.UpdateCronJob(job.ID, map[string]interface{}{"enabled": false})
		}
	}
	if !job.NextRun.IsZero() {
		_ = m.db.SetCronJobNextRun(job.ID, job.NextRun.Format(time.RFC3339))
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
