package log

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level represents log severity.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelCritical
)

var levelNames = map[Level]string{
	LevelDebug:    "DEBUG",
	LevelInfo:     "INFO",
	LevelWarn:     "WARN",
	LevelError:    "ERRO",
	LevelCritical: "CRIT",
}

var levelColors = map[Level]string{
	LevelDebug:    "\033[37m",
	LevelInfo:     "\033[36m",
	LevelWarn:     "\033[33m",
	LevelError:    "\033[31m",
	LevelCritical: "\033[35m",
}

const colorReset = "\033[0m"

// Logger is a structured logger that supports multiple subscribers (LogBroker pattern).
type Logger struct {
	mu          sync.RWMutex
	level       Level
	out         io.Writer
	subscribers []chan LogEntry
	useColor    bool
}

// LogEntry represents a single log record.
type LogEntry struct {
	Timestamp time.Time
	Level     Level
	Component string
	Message   string
}

var defaultLogger = &Logger{
	level:    LevelInfo,
	out:      os.Stdout,
	useColor: true,
}

// GetDefault returns the default logger instance.
func GetDefault() *Logger { return defaultLogger }

// SetLevel sets the minimum log level.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
}

// SetOutput sets the output writer.
func (l *Logger) SetOutput(w io.Writer) {
	l.mu.Lock()
	l.out = w
	l.mu.Unlock()
}

// Subscribe creates a new log subscriber channel (for WebUI log streaming).
func (l *Logger) Subscribe(buf int) <-chan LogEntry {
	ch := make(chan LogEntry, buf)
	l.mu.Lock()
	l.subscribers = append(l.subscribers, ch)
	l.mu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel.
func (l *Logger) Unsubscribe(ch <-chan LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, sub := range l.subscribers {
		if sub == ch {
			l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
			close(sub)
			return
		}
	}
}

func (l *Logger) log(level Level, component, format string, args ...interface{}) {
	if level < l.level {
		return
	}
	entry := LogEntry{
		Timestamp: time.Now(),
		Level:     level,
		Component: component,
		Message:   fmt.Sprintf(format, args...),
	}

	l.mu.RLock()
	subs := make([]chan LogEntry, len(l.subscribers))
	copy(subs, l.subscribers)
	l.mu.RUnlock()

	for _, ch := range subs {
		select {
		case ch <- entry:
		default: // drop if subscriber is slow
		}
	}

	l.mu.RLock()
	out := l.out
	useColor := l.useColor
	l.mu.RUnlock()

	ts := entry.Timestamp.Format("2006-01-02 15:04:05.000")
	color := ""
	reset := ""
	if useColor {
		color = levelColors[level]
		reset = colorReset
	}
	fmt.Fprintf(out, "%s[%s] [%s] [%s] %s%s\n",
		color, ts, levelNames[level], entry.Component, entry.Message, reset)
}

func (l *Logger) Debug(format string, args ...interface{}) {
	l.log(LevelDebug, "Core", format, args...)
}
func (l *Logger) Info(format string, args ...interface{}) {
	l.log(LevelInfo, "Core", format, args...)
}
func (l *Logger) Warn(format string, args ...interface{}) {
	l.log(LevelWarn, "Core", format, args...)
}
func (l *Logger) Error(format string, args ...interface{}) {
	l.log(LevelError, "Core", format, args...)
}
func (l *Logger) Critical(format string, args ...interface{}) {
	l.log(LevelCritical, "Core", format, args...)
}

// WithComponent creates a component-scoped logger.
func (l *Logger) WithComponent(component string) *ComponentLogger {
	return &ComponentLogger{parent: l, component: component}
}

// ComponentLogger logs with a fixed component tag.
type ComponentLogger struct {
	parent    *Logger
	component string
}

func (c *ComponentLogger) Debug(format string, args ...interface{}) {
	c.parent.log(LevelDebug, c.component, format, args...)
}
func (c *ComponentLogger) Info(format string, args ...interface{}) {
	c.parent.log(LevelInfo, c.component, format, args...)
}
func (c *ComponentLogger) Warn(format string, args ...interface{}) {
	c.parent.log(LevelWarn, c.component, format, args...)
}
func (c *ComponentLogger) Error(format string, args ...interface{}) {
	c.parent.log(LevelError, c.component, format, args...)
}

// ParseLevel converts a string to a Level.
func ParseLevel(s string) Level {
	switch strings.ToUpper(s) {
	case "DEBUG":
		return LevelDebug
	case "INFO":
		return LevelInfo
	case "WARN", "WARNING":
		return LevelWarn
	case "ERROR":
		return LevelError
	case "CRITICAL":
		return LevelCritical
	default:
		return LevelInfo
	}
}
