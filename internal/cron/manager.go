// Package cron implements scheduled job management.
// Ported from astrbot/core/cron/
package cron

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/log"
)

var logger = log.GetDefault().WithComponent("Cron")

// JobFunc is a scheduled task callback.
type JobFunc func(ctx context.Context) error

// Job represents a scheduled task.
type Job struct {
	ID       string
	Name     string
	Schedule string // cron expression (placeholder)
	Handler  JobFunc
	NextRun  time.Time
	Interval time.Duration
}

// CronJobManager manages scheduled tasks.
type CronJobManager struct {
	mu   sync.Mutex
	jobs map[string]*Job
	stop chan struct{}
}

// NewCronJobManager creates a manager.
func NewCronJobManager() *CronJobManager {
	return &CronJobManager{
		jobs: make(map[string]*Job),
		stop: make(chan struct{}),
	}
}

// Add schedules a job at a fixed interval.
func (m *CronJobManager) Add(id, name string, interval time.Duration, handler JobFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.jobs[id] = &Job{
		ID:       id,
		Name:     name,
		Handler:  handler,
		Interval: interval,
		NextRun:  time.Now().Add(interval),
	}
	logger.Info("Scheduled job %s (%s) every %v", id, name, interval)
}

// Remove cancels a job.
func (m *CronJobManager) Remove(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.jobs, id)
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
	close(m.stop)
}

func (m *CronJobManager) tick(ctx context.Context, now time.Time) {
	m.mu.Lock()
	due := []*Job{}
	for _, job := range m.jobs {
		if !job.NextRun.After(now) {
			due = append(due, job)
			job.NextRun = now.Add(job.Interval)
		}
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

// ListInfo returns job info as serializable maps.
func (m *CronJobManager) ListInfo() []map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]map[string]interface{}, 0, len(m.jobs))
	for _, j := range m.jobs {
		result = append(result, map[string]interface{}{
			"id":       j.ID,
			"name":     j.Name,
			"schedule": j.Schedule,
			"next_run": j.NextRun.Unix(),
		})
	}
	return result
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
