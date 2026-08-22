package cron

import (
	"strings"
	"testing"
	"time"
)

func TestParseCronFieldCounts(t *testing.T) {
	if _, err := ParseCron("0 8 * * *"); err != nil {
		t.Fatalf("5-field parse failed: %v", err)
	}
	if _, err := ParseCron("*/30 0 8 * * *"); err != nil {
		t.Fatalf("6-field parse failed: %v", err)
	}
	if _, err := ParseCron("0 8 * * * extra extra"); err == nil {
		t.Fatal("expected error for 7 fields")
	}
	if _, err := ParseCron("bad 8 * * *"); err == nil {
		t.Fatal("expected error for invalid minute field")
	}
}

func TestSixFieldSeconds(t *testing.T) {
	sched, err := ParseCron("30 * * * * *")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	next := sched.Next(base)
	want := time.Date(2026, 1, 1, 0, 0, 30, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
}

func TestFiveFieldStillMinuteGranularity(t *testing.T) {
	sched, err := ParseCron("*/15 * * * *")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	next := sched.Next(base)
	want := time.Date(2026, 1, 1, 0, 15, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
}

func TestSingleValueWithStepExpandsToMax(t *testing.T) {
	// croniter 语义：`5/15` 等价于 5-59/15 → 5,20,35,50。
	sched, err := ParseCron("5/15 * * * *")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	base := time.Date(2026, 1, 1, 0, 4, 0, 0, time.UTC)
	next := sched.Next(base)
	want := time.Date(2026, 1, 1, 0, 5, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
	next = sched.Next(want)
	want = time.Date(2026, 1, 1, 0, 20, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
	next = sched.Next(want)
	want = time.Date(2026, 1, 1, 0, 35, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
	next = sched.Next(want)
	want = time.Date(2026, 1, 1, 0, 50, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
	// 50 之后的下一跳回到下一小时的 5 分。
	next = sched.Next(want)
	want = time.Date(2026, 1, 1, 1, 5, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("expected %v, got %v", want, next)
	}
}

func TestParseCronNeverMatches(t *testing.T) {
	// Feb 30 never occurs: Next must return zero, not loop forever.
	sched, err := ParseCron("0 0 30 2 *")
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !sched.Next(time.Now()).IsZero() {
		t.Fatal("expected zero time for never-matching schedule")
	}
}

func TestAddActiveJobRejectsInvalidCron(t *testing.T) {
	m := NewCronJobManager(nil)
	_, err := m.AddActiveJob("bad", "61 * * * *", map[string]interface{}{"note": "n"}, "d", "", false, time.Time{})
	if err == nil {
		t.Fatal("expected error for out-of-range minute field")
	}
	if !strings.Contains(err.Error(), "cron_expression") {
		t.Fatalf("error should mention cron_expression: %v", err)
	}
}
