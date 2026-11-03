package dashboard

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/procsandbox"
	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
)

// Ported from astrbot/dashboard/services/diagnostic_service.py (4.29.2):
// a redactor that masks credentials before serialization. IP-literal aliasing
// and free-form assignment scanning from the Python version are intentionally
// not ported (RE2 lacks lookbehind); configured secrets, auth headers, tokens
// and private keys are still scrubbed.

const diagnosticRedacted = "[REDACTED]"

var (
	reSecretKey = regexp.MustCompile(`(?i)(password|passwd|secret|token$|api[_-]?key|access[_-]?key|private[_-]?key|aes[_-]?key|authorization|cookie|credential|(?:client|app|signing|encrypt(?:ion)?)[_-]?key|recovery[_-]?code|(?:^|\.)key$|^headers$|^env$)`)
	reANSI      = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	reAuth      = regexp.MustCompile(`(?i)\b(Bearer|Basic|ApiKey)\s+[^\s"'<>]+`)
	reCookie    = regexp.MustCompile(`(?i)(\b(?:set-cookie|cookie)\s*:\s*)[^\r\n]+`)
	reURLAuth   = regexp.MustCompile(`(?i)(https?://)[^/\s@]+@`)
	reToken     = regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,})`)
	rePrivate   = regexp.MustCompile(`(?s)-----BEGIN [\w ]*PRIVATE KEY-----.*?(?:-----END [\w ]*PRIVATE KEY-----|\z)`)
)

type diagnosticRedactor struct {
	enabled bool
	secrets []string
}

// collect records configured credential values (without exporting them).
func (r *diagnosticRedactor) collect(value interface{}, sensitive bool) {
	if !r.enabled {
		return
	}
	switch v := value.(type) {
	case map[string]interface{}:
		for k, item := range v {
			r.collect(item, sensitive || reSecretKey.MatchString(k))
		}
	case []interface{}:
		for _, item := range v {
			r.collect(item, sensitive)
		}
	default:
		if sensitive {
			if s, ok := value.(string); ok && s != "" {
				r.secrets = append(r.secrets, s)
			}
		}
	}
}

// redact returns a sanitized copy while preserving containers and booleans.
func (r *diagnosticRedactor) redact(value interface{}, sensitive bool) interface{} {
	if !r.enabled {
		return value
	}
	switch v := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for k, item := range v {
			out[k] = r.redact(item, sensitive || reSecretKey.MatchString(k))
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, item := range v {
			out[i] = r.redact(item, sensitive)
		}
		return out
	case bool, nil:
		return value
	default:
		if sensitive {
			return diagnosticRedacted
		}
		if s, ok := value.(string); ok {
			return r.text(s)
		}
		return value
	}
}

// text scrubs credentials from free text (logs).
func (r *diagnosticRedactor) text(value string) string {
	if !r.enabled {
		return value
	}
	if strings.Contains(value, "\x1b[") {
		value = reANSI.ReplaceAllString(value, "")
	}
	if strings.Contains(value, "PRIVATE KEY-----") {
		value = rePrivate.ReplaceAllString(value, diagnosticRedacted)
	}
	if len(r.secrets) > 0 {
		secrets := append([]string(nil), r.secrets...)
		sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
		for _, s := range secrets {
			if s != "" {
				value = strings.ReplaceAll(value, s, diagnosticRedacted)
			}
		}
	}
	value = reAuth.ReplaceAllStringFunc(value, func(m string) string {
		return reAuth.FindStringSubmatch(m)[1] + " " + diagnosticRedacted
	})
	value = reCookie.ReplaceAllStringFunc(value, func(m string) string {
		return reCookie.FindStringSubmatch(m)[1] + diagnosticRedacted
	})
	value = reURLAuth.ReplaceAllStringFunc(value, func(m string) string {
		return reURLAuth.FindStringSubmatch(m)[1] + diagnosticRedacted + "@"
	})
	value = reToken.ReplaceAllString(value, diagnosticRedacted)
	return value
}

// handleLogExport serves GET /api/v1/logs/export (application/zip): a redacted
// bundle with the global config, environment metadata and recent logs.
func (s *Server) handleLogExport(w http.ResponseWriter, r *http.Request) {
	redact := r.URL.Query().Get("redact") != "false"
	red := &diagnosticRedactor{enabled: redact}

	cfg := s.getConfigSnapshot()
	red.collect(cfg, false)
	var configSection interface{} = cfg
	if redact {
		configSection = red.redact(cfg, false)
	}

	env := map[string]interface{}{
		"astrbot_version": version.Version,
		"python_version":  version.PythonVersion,
		"go_version":      runtime.Version(),
		"os":              runtime.GOOS,
		"architecture":    runtime.GOARCH,
		"deployment":      "docker",
		"generated_at":    time.Now().Format(time.RFC3339),
		"process_sandbox": procsandbox.GetProcessSandboxStatus(),
	}

	var sb strings.Builder
	for _, entry := range log.GetDefault().History() {
		if entry.IsTrace() {
			continue
		}
		sb.WriteString(sseLogLine(entry))
		sb.WriteByte('\n')
	}
	logsText := sb.String()
	if redact {
		logsText = red.text(logsText)
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	suffix := ""
	if !redact {
		suffix = "raw-"
	}
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=\"astrbot-diagnostics-%s%s.zip\"", suffix, time.Now().Format("20060102-150405")))

	zw := zip.NewWriter(w)
	writeZipJSON(zw, "config.json", configSection)
	writeZipJSON(zw, "environment.json", env)
	if f, err := zw.Create("logs.txt"); err == nil {
		_, _ = f.Write([]byte(logsText))
	}
	_ = zw.Close()
}

func writeZipJSON(zw *zip.Writer, name string, value interface{}) {
	f, err := zw.Create(name)
	if err != nil {
		return
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	_ = enc.Encode(value)
}
