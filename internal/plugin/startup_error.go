package plugin

import (
	"bytes"
	"strings"
	"sync"
	"time"
)

// astrbotStartupError is the parsed content of a
// `[ASTRBOT] STARTUP_ERROR phase=<name> type=<ExceptionType> plugin=<plugin>
// error=<message>` stderr line emitted by the Python bridge
// (internal/pysdk/astrbot/_bridge/progress.py). error is a single folded line
// (newlines → spaces) carrying the full exception chain.
type astrbotStartupError struct {
	// Phase is the startup phase that failed (fallback: the last phase= line
	// seen before the error).
	Phase string
	// Type is the Python exception type (e.g. ModuleNotFoundError).
	Type string
	// Plugin is the plugin name (dir name) the bridge attributed the failure
	// to (may be empty).
	Plugin string
	// Error is the single-line exception text.
	Error string
}

// astrbotStartupParser intercepts the Python plugin subprocess stderr and
// extracts the [ASTRBOT] phase/STARTUP_ERROR protocol lines while forwarding
// every other line to the host log unchanged.
//
// go-plugin reads the plugin's stderr itself and writes each line to
// ClientConfig.Stderr; wiring this parser there keeps the original forwarding
// semantics (non-protocol lines still reach the host log) and gives the host a
// structured view of where startup failed, so a handshake failure can be
// reported as a phase-specific error instead of go-plugin's generic
// "failed to read any lines from plugin's stdout".
type astrbotStartupParser struct {
	mu         sync.Mutex
	buf        []byte // partial-line buffer (Write may deliver half lines)
	phases     []string
	lastPhase  string
	startupErr *astrbotStartupError
	errCh      chan struct{} // closed on the first STARTUP_ERROR line
	tail       []string      // 最近若干行原始 stderr（含非协议行），供无协议错误时回吐诊断
	lang       string        // 插件语言（Go/Python），仅用于错误前缀措辞
}

// maxTailLines caps the raw stderr ring so startup failures (especially Go 插件在 Windows 上握手失败——无 [ASTRBOT] 协议行) can surface the child's actual output.
const maxTailLines = 40

func newAstrbotStartupParser() *astrbotStartupParser {
	return &astrbotStartupParser{errCh: make(chan struct{})}
}

// Write implements io.Writer: it buffers incoming bytes, splits them into
// lines and classifies each line. The caller (go-plugin's stderr forwarder)
// delivers the plugin's stderr line by line, but chunks may split a line
// arbitrarily, so a partial-line buffer is kept.
func (p *astrbotStartupParser) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		idx := bytes.IndexByte(p.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(p.buf[:idx])
		p.buf = p.buf[idx+1:]
		p.handleLine(strings.TrimSuffix(line, "\r"))
	}
	return len(b), nil
}

// handleLine classifies a single complete line. Must be called with p.mu held.
func (p *astrbotStartupParser) handleLine(line string) {
	const prefix = "[ASTRBOT]"
	if !strings.HasPrefix(line, prefix) {
		// 非协议行：原样转发（保持 go-plugin 默认的 stderr 转发语义），并入尾部环形缓存。
		logger.Debug("插件 stderr: %s", line)
		if strings.TrimSpace(line) != "" {
			p.tail = append(p.tail, line)
			if len(p.tail) > maxTailLines {
				p.tail = p.tail[len(p.tail)-maxTailLines:]
			}
		}
		return
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	switch {
	case strings.HasPrefix(rest, "phase="):
		name := strings.TrimSpace(strings.TrimPrefix(rest, "phase="))
		p.lastPhase = name
		p.phases = append(p.phases, name)
		logger.Info("Python 插件启动阶段: %s", name)
	case strings.HasPrefix(rest, "STARTUP_ERROR"):
		p.handleStartupError(strings.TrimSpace(strings.TrimPrefix(rest, "STARTUP_ERROR")), line)
	default:
		logger.Debug("插件 stderr: %s", line)
	}
}

// handleStartupError parses the key=value fields of a STARTUP_ERROR line.
// Values may contain spaces; per the protocol error= is the last field and
// takes the rest of the line. When error= is missing, the whole line is used
// (truncated) so the message is never lost. Must be called with p.mu held.
func (p *astrbotStartupParser) handleStartupError(fields, rawLine string) {
	se := &astrbotStartupError{Phase: p.lastPhase}
	rest := fields
	foundError := false
	for rest != "" {
		rest = strings.TrimLeft(rest, " \t")
		if rest == "" {
			break
		}
		if strings.HasPrefix(rest, "error=") {
			se.Error = strings.TrimSpace(rest[len("error="):])
			foundError = true
			break
		}
		eq := strings.IndexByte(rest, '=')
		if eq < 0 {
			break
		}
		end := eq + 1
		for end < len(rest) && !isSpaceByte(rest[end]) {
			end++
		}
		key, val := rest[:eq], rest[eq+1:end]
		rest = rest[end:]
		switch key {
		case "phase":
			se.Phase = val
		case "type":
			se.Type = val
		case "plugin":
			se.Plugin = val
		}
	}
	if !foundError {
		// 找不到 error=：整行截断作为错误消息，保证信息不丢失。
		se.Error = truncateLine(strings.TrimSpace(rawLine))
	} else {
		se.Error = truncateLine(se.Error)
	}
	if se.Phase == "" {
		se.Phase = p.lastPhase
	}
	if p.startupErr == nil {
		p.startupErr = se
		close(p.errCh)
	}
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\t'
}

// maxStartupErrorLen caps a single STARTUP_ERROR message so an oversized
// line cannot balloon the error returned to the caller.
const maxStartupErrorLen = 2000

func truncateLine(s string) string {
	if len(s) > maxStartupErrorLen {
		return s[:maxStartupErrorLen] + "...(截断)"
	}
	return s
}

// Tail returns the last raw stderr lines (non-protocol), joined for error
// messages ("" when nothing captured).
func (p *astrbotStartupParser) Tail() string {
	if p == nil {
		return ""
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.tail) == 0 {
		return ""
	}
	out := strings.Join(p.tail, "\n")
	if len(out) > maxStartupErrorLen {
		out = out[len(out)-maxStartupErrorLen:]
	}
	return out
}

// Phases returns the startup phases observed so far, in order.
func (p *astrbotStartupParser) Phases() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.phases))
	copy(out, p.phases)
	return out
}

// StartupError returns the first STARTUP_ERROR seen so far (nil if none).
func (p *astrbotStartupParser) StartupError() *astrbotStartupError {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startupErr
}

// WaitError blocks until a STARTUP_ERROR line arrives or timeout elapses, and
// returns the first one (nil when none arrived). go-plugin drains the plugin's
// stderr asynchronously, so an error path that returns before the drain can
// call this to give the protocol line a chance to land.
func (p *astrbotStartupParser) WaitError(timeout time.Duration) *astrbotStartupError {
	if p == nil {
		return nil
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.errCh:
	case <-timer.C:
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startupErr
}
