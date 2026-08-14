package cron

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
)

func TestComputeNextRunLockedDisablesUnparseable(t *testing.T) {
	m := NewCronJobManager(nil)
	job := &Job{
		ID:             "j1",
		Name:           "bad",
		JobType:        "active_agent",
		CronExpression: "not a cron",
		Enabled:        true,
		Payload:        map[string]interface{}{},
	}
	m.mu.Lock()
	m.computeNextRunLocked(job, time.Now())
	m.mu.Unlock()
	if job.Enabled {
		t.Fatal("unparseable job should have been disabled")
	}
	if job.NextRun.IsZero() {
		t.Fatal("unparseable job must not keep a zero NextRun")
	}
	if !job.NextRun.After(time.Now().Add(300 * 24 * time.Hour)) {
		t.Fatalf("NextRun should be pushed far into the future, got %v", job.NextRun)
	}
}

func TestAddActiveJobValidatesCron(t *testing.T) {
	m := NewCronJobManager(nil)
	if _, err := m.AddActiveJob("ok", "0 8 * * *", map[string]interface{}{"note": "n"}, "d", "", false, time.Time{}); err != nil {
		t.Fatalf("valid 5-field cron rejected: %v", err)
	}
	if _, err := m.AddActiveJob("ok6", "0 30 8 * * *", map[string]interface{}{"note": "n"}, "d", "", false, time.Time{}); err != nil {
		t.Fatalf("valid 6-field cron rejected: %v", err)
	}
	if _, err := m.AddActiveJob("bad", "0 61 * * *", map[string]interface{}{"note": "n"}, "d", "", false, time.Time{}); err == nil {
		t.Fatal("invalid cron expression should be rejected at creation")
	}
}

func TestComputeNextRunLockedSixField(t *testing.T) {
	m := NewCronJobManager(nil)
	job := &Job{
		ID:             "j2",
		Name:           "every-minute-seconds",
		JobType:        "active_agent",
		CronExpression: "15 * * * * *",
		Enabled:        true,
		Payload:        map[string]interface{}{},
	}
	now := time.Now()
	m.mu.Lock()
	m.computeNextRunLocked(job, now)
	m.mu.Unlock()
	if !job.Enabled {
		t.Fatal("valid job should remain enabled")
	}
	if job.NextRun.IsZero() || !job.NextRun.After(now) {
		t.Fatalf("expected a future NextRun, got %v (now=%v)", job.NextRun, now)
	}
	if job.NextRun.Second() != 15 {
		t.Fatalf("expected next run at second 15, got %v", job.NextRun)
	}
}

func TestUpdateJobMutatesUnderLock(t *testing.T) {
	m := NewCronJobManager(nil)
	m.RegisterHandler("active_agent", func(ctx context.Context, j *Job) error { return nil })
	job, err := m.AddActiveJob("t", "0 8 * * *", map[string]interface{}{"note": "old", "session": "s"}, "old", "", false, time.Time{})
	if err != nil {
		t.Fatalf("AddActiveJob failed: %v", err)
	}
	oldNext := job.NextRun

	if !m.UpdateJob(job.ID, func(j *Job) {
		j.Name = "renamed"
		j.CronExpression = "0 9 * * *"
		j.RunOnce = false
		j.Payload["note"] = "new"
	}) {
		t.Fatal("UpdateJob on an existing job should return true")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.jobs[job.ID]
	if stored == nil {
		t.Fatal("job should still be present after update")
	}
	if stored.Name != "renamed" {
		t.Fatalf("expected Name=renamed, got %q", stored.Name)
	}
	if stored.CronExpression != "0 9 * * *" {
		t.Fatalf("expected cron updated, got %q", stored.CronExpression)
	}
	if got := stored.Payload["note"]; got != "new" {
		t.Fatalf("expected Payload note=new, got %v", got)
	}
	if !stored.NextRun.After(oldNext) && !stored.NextRun.Before(oldNext) {
		t.Fatalf("NextRun should have been recomputed after changing the schedule, got %v (old %v)", stored.NextRun, oldNext)
	}
	if stored.Handler == nil {
		t.Fatal("job should be re-armed after update")
	}
}

func TestUpdateJobMissingID(t *testing.T) {
	m := NewCronJobManager(nil)
	if m.UpdateJob("nope", func(j *Job) {}) {
		t.Fatal("UpdateJob on a missing id should return false")
	}
}

func TestUpdateJobSwitchToRunOnce(t *testing.T) {
	m := NewCronJobManager(nil)
	job, err := m.AddActiveJob("t", "0 8 * * *", map[string]interface{}{"note": "n"}, "n", "", false, time.Time{})
	if err != nil {
		t.Fatalf("AddActiveJob failed: %v", err)
	}
	at := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if !m.UpdateJob(job.ID, func(j *Job) {
		j.CronExpression = ""
		j.RunAt = at
		j.RunOnce = true
	}) {
		t.Fatal("UpdateJob should succeed")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	stored := m.jobs[job.ID]
	if !stored.RunOnce {
		t.Fatal("expected RunOnce=true after update")
	}
	if stored.RunAt.IsZero() || !stored.RunAt.Equal(at) {
		t.Fatalf("expected RunAt=%v, got %v", at, stored.RunAt)
	}
	if !stored.NextRun.Equal(at) {
		t.Fatalf("expected NextRun=%v, got %v", at, stored.NextRun)
	}
}

// TestLoadRestoresRunAtForRunOnceJob is a regression test for M-34: a
// run_once job persisted with run_at in its payload must have RunAt restored
// after Load() so a restart does not rebuild a zero RunAt (which would fire
// and delete the job immediately).
func TestLoadRestoresRunAtForRunOnceJob(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "cron.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	runAt := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	m1 := NewCronJobManager(database)
	job, err := m1.AddActiveJob("once", "", map[string]interface{}{
		"session": "qq:group:1",
		"note":    "do it",
		"run_at":  runAt.Format(time.RFC3339),
	}, "do it", "", true, runAt)
	if err != nil {
		t.Fatalf("AddActiveJob: %v", err)
	}

	// Fresh manager over the same DB: simulates a process restart.
	m2 := NewCronJobManager(database)
	m2.Load()

	loaded := m2.Get(job.ID)
	if loaded == nil {
		t.Fatal("job not loaded after restart")
	}
	if !loaded.RunOnce {
		t.Fatalf("expected RunOnce=true, got %v", loaded.RunOnce)
	}
	if loaded.RunAt.IsZero() {
		t.Fatal("RunAt must be restored from payload after Load")
	}
	if !loaded.RunAt.Equal(runAt) {
		t.Fatalf("expected RunAt=%v, got %v", runAt, loaded.RunAt)
	}
	if !loaded.NextRun.Equal(runAt) {
		t.Fatalf("expected NextRun=%v, got %v", runAt, loaded.NextRun)
	}
	if !loaded.Enabled {
		t.Fatal("restored run_once job with a valid future run_at must stay enabled")
	}
}

// TestLoadPersistRoundTripRunAt verifies persist records run_at into the
// payload even when the caller only passed RunAt (the in-memory field).
func TestLoadPersistRoundTripRunAt(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "cron2.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	runAt := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	m1 := NewCronJobManager(database)
	job, err := m1.AddActiveJob("once2", "", map[string]interface{}{"session": "qq:group:2"}, "d", "", true, runAt)
	if err != nil {
		t.Fatalf("AddActiveJob: %v", err)
	}

	row, found, err := database.GetCronJob(job.ID)
	if err != nil || !found {
		t.Fatalf("GetCronJob: found=%v err=%v", found, err)
	}
	if row.Payload == "" {
		t.Fatal("payload must not be empty after persist")
	}

	m2 := NewCronJobManager(database)
	m2.Load()
	loaded := m2.Get(job.ID)
	if loaded == nil || loaded.RunAt.IsZero() || !loaded.RunAt.Equal(runAt) {
		t.Fatalf("RunAt not restored via payload: %+v", loaded)
	}
}

// TestLoadDropsExpiredRunOnceJob verifies the restart policy for a run_once
// job whose scheduled time already passed while the bot was down: it is
// dropped (and removed from the DB) rather than firing late.
func TestLoadDropsExpiredRunOnceJob(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "cron3.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()

	past := time.Now().Add(-1 * time.Hour).Truncate(time.Second)
	m1 := NewCronJobManager(database)
	job, err := m1.AddActiveJob("expired", "", map[string]interface{}{
		"session": "qq:group:3",
		"run_at":  past.Format(time.RFC3339),
	}, "d", "", true, past)
	if err != nil {
		t.Fatalf("AddActiveJob: %v", err)
	}

	m2 := NewCronJobManager(database)
	m2.Load()
	if got := m2.Get(job.ID); got != nil {
		t.Fatalf("expired run_once job should have been dropped on reload, got %+v", got)
	}
	if _, found, err := database.GetCronJob(job.ID); err != nil || found {
		t.Fatalf("expired run_once job should be removed from DB (found=%v err=%v)", found, err)
	}
}
