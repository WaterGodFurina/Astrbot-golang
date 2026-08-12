package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

func TestSSELogLineHasAnsiPrefix(t *testing.T) {
	entry := log.LogEntry{
		Timestamp: time.Date(2026, 8, 12, 12, 0, 0, 0, time.Local),
		Level:     log.LevelInfo,
		Component: "Test",
		Message:   "hello",
	}
	line := sseLogLine(entry)
	if !strings.HasPrefix(line, "\x1b[1;34m") {
		t.Errorf("INFO line should start with bright-blue ANSI (loguru INFO), got %q", line)
	}
	if !strings.Contains(line, "[INFO]") {
		t.Errorf("INFO line should contain [INFO], got %q", line)
	}

	err := log.LogEntry{Timestamp: time.Now(), Level: log.LevelError, Message: "boom"}
	if got := sseLogLine(err); !strings.HasPrefix(got, "\x1b[31m") {
		t.Errorf("ERROR line should start with red ANSI, got %q", got)
	}

	crit := log.LogEntry{Timestamp: time.Now(), Level: log.LevelCritical, Message: "fatal"}
	if got := sseLogLine(crit); !strings.HasPrefix(got, "\x1b[1;31m") {
		t.Errorf("CRITICAL line should start with bright-red bold ANSI, got %q", got)
	}
}
