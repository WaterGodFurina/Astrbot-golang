package log

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/i18n"
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
	LevelDebug:    "\033[1;36m", // bright cyan bold (loguru DEBUG)
	LevelInfo:     "\033[1;34m", // bright blue bold (loguru INFO)
	LevelWarn:     "\033[1;33m", // bright yellow bold (loguru WARNING)
	LevelError:    "\033[31m",   // red (loguru ERROR)
	LevelCritical: "\033[1;31m", // bright red bold (loguru CRITICAL)
}

const colorReset = "\033[0m"

// Logger is a structured logger that supports multiple subscribers (LogBroker pattern).
type Logger struct {
	mu           sync.RWMutex
	level        Level
	out          io.Writer
	fileOut      io.Writer
	subscribers  []chan LogEntry
	useColor     bool
	history      []LogEntry
	historyBytes int
}

// maxHistory caps the number of buffered log entries returned by History().
const maxHistory = 200

// maxHistoryBytes bounds the in-memory log history by total text size (~1MB,
// "流动日志消除法"): the console page only ever holds a bounded buffer so a
// long-running process cannot bloat memory.
const maxHistoryBytes = 1 << 20 // 1 MiB

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

// EnableFileLog starts writing every log line to a segmented file (path may be
// relative to the working dir). When the current segment exceeds maxBytes the
// file rotates: astrbot.log -> astrbot.log.1 -> astrbot.log.2 -> ... and the
// oldest segments beyond keepSegs are pruned. Logging to stdout continues.
func (l *Logger) EnableFileLog(path string, maxBytes int64, keepSegs int) error {
	w, err := newSegmentedWriter(path, maxBytes, keepSegs)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.fileOut = w
	l.mu.Unlock()
	return nil
}

// DisableFileLog stops segmented file logging and closes the file.
func (l *Logger) DisableFileLog() {
	l.mu.Lock()
	if w, ok := l.fileOut.(*segmentedWriter); ok {
		_ = w.Close()
	}
	l.fileOut = nil
	l.mu.Unlock()
}

// segmentedWriter rotates a log file into numbered segments once it exceeds
// maxBytes, keeping the most recent keepSegs segments.
type segmentedWriter struct {
	mu       sync.Mutex
	path     string // e.g. logs/astrbot.log
	maxBytes int64
	keepSegs int
	file     *os.File
	size     int64
}

func newSegmentedWriter(path string, maxBytes int64, keepSegs int) (*segmentedWriter, error) {
	if maxBytes <= 0 {
		maxBytes = 5 << 20
	}
	if keepSegs <= 0 {
		keepSegs = 3
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	w := &segmentedWriter{path: path, maxBytes: maxBytes, keepSegs: keepSegs}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *segmentedWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.size = info.Size()
	return nil
}

func (w *segmentedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return len(p), nil
	}
	if w.size+int64(len(p)) > w.maxBytes {
		_ = w.rotateLocked()
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *segmentedWriter) rotateLocked() error {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
	// shift segments: .n -> .n+1, current -> .1
	for i := w.keepSegs - 1; i >= 1; i-- {
		old := fmt.Sprintf("%s.%d", w.path, i)
		next := fmt.Sprintf("%s.%d", w.path, i+1)
		if _, err := os.Stat(old); err == nil {
			_ = os.Remove(next)
			_ = os.Rename(old, next)
		}
	}
	seg1 := fmt.Sprintf("%s.1", w.path)
	_ = os.Remove(seg1)
	_ = os.Rename(w.path, seg1)
	return w.open()
}

func (w *segmentedWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		err := w.file.Close()
		w.file = nil
		return err
	}
	return nil
}

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
//
// It deliberately does NOT close the channel: log() snapshots the subscriber
// list without holding the lock and may still send to an unsubscribed channel
// in flight. Closing it there would race with those sends and panic with
// "send on closed channel". Subscribers are expected to exit via their own
// lifetime signal (the SSE handler uses r.Context().Done()); once removed from
// the list the channel is no longer written to and is garbage collected when
// the subscriber drops its reference.
func (l *Logger) Unsubscribe(ch <-chan LogEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, sub := range l.subscribers {
		if sub == ch {
			l.subscribers = append(l.subscribers[:i], l.subscribers[i+1:]...)
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

	l.mu.Lock()
	// Buffer recent entries for the WebUI log history. Bound by BOTH entry count
	// and total text size (~1MB) so a long session cannot bloat memory.
	l.history = append(l.history, entry)
	for len(l.history) > maxHistory || l.historyBytes > maxHistoryBytes {
		l.historyBytes -= len(l.history[0].Message)
		l.history = l.history[1:]
		if len(l.history) == 0 {
			break
		}
	}
	l.historyBytes += len(entry.Message)
	subs := make([]chan LogEntry, len(l.subscribers))
	copy(subs, l.subscribers)
	fileOut := l.fileOut
	l.mu.Unlock()

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
	line := fmt.Sprintf("%s[%s] [%s] [%s] %s%s\n",
		color, ts, levelNames[level], entry.Component, entry.Message, reset)
	fmt.Fprint(out, line)

	if fileOut != nil {
		// Plain (no color) line for the segmented log file.
		plain := fmt.Sprintf("[%s] [%s] [%s] %s\n", ts, levelNames[level], entry.Component, entry.Message)
		_, _ = io.WriteString(fileOut, plain)
	}
}

// History returns the buffered log entries (newest first is NOT applied;
// entries are in chronological order).
func (l *Logger) History() []LogEntry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]LogEntry, len(l.history))
	copy(out, l.history)
	return out
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

// I18nInfo 记录一条按当前语言翻译的 INFO 日志（key 经 i18n.Get 格式化）。
func (c *ComponentLogger) I18nInfo(key string, args ...interface{}) {
	c.parent.log(LevelInfo, c.component, "%s", i18n.Get(key, args...))
}

// I18nWarn 记录一条按当前语言翻译的 WARN 日志。
func (c *ComponentLogger) I18nWarn(key string, args ...interface{}) {
	c.parent.log(LevelWarn, c.component, "%s", i18n.Get(key, args...))
}

// I18nError 记录一条按当前语言翻译的 ERROR 日志。
func (c *ComponentLogger) I18nError(key string, args ...interface{}) {
	c.parent.log(LevelError, c.component, "%s", i18n.Get(key, args...))
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
