// Shipyard Neo (Bay) URL-based sandbox booter.
//
// Ported from astrbot/core/computer/booters/shipyard_neo.py and the
// shipyard_neo SDK. The sandbox connects over HTTP to a Bay endpoint:
//
//	POST /v1/sandboxes                      create sandbox {profile, ttl}
//	GET  /v1/sandboxes/{id}                 poll status until "ready"
//	POST /v1/sandboxes/{id}/shell/exec      {command, cwd, timeout}
//	POST /v1/sandboxes/{id}/python/exec     {code, timeout}
//	GET  /v1/sandboxes/{id}/filesystem/files?path=...
//	PUT  /v1/sandboxes/{id}/filesystem/files {path, content}
//	GET  /v1/sandboxes/{id}/filesystem/directories?path=...
//	GET  /v1/profiles                        profile auto-selection
//
// Auth is a Bearer token; all file paths are relative to /workspace.
package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
)

var neoLogger = log.GetDefault().WithComponent("SandboxNeo")

// neoReadinessTimeout and neoPollInterval mirror Python's _wait_until_ready.
const (
	neoReadinessTimeout = 180 * time.Second
	neoPollInterval     = 2 * time.Second
)

// ShipyardNeoBooter connects to a Bay (Shipyard Neo) sandbox over HTTP.
type ShipyardNeoBooter struct {
	mu          sync.Mutex
	running     bool
	endpointURL string
	accessToken string
	profile     string
	ttl         int
	// client 用于创建/就绪轮询/文件等短请求（60s 总超时）。
	client *http.Client
	// execClient 用于 shell/python exec：请求体携带 timeout:300 秒，不设总
	// 超时，超时语义完全交给 ctx（与 Manager.Exec 的调用方上下文一致），否则
	// Client.Timeout=60s 会提前掐断运行超过 1 分钟的命令。
	execClient *http.Client
	sandboxID  string
	caps       []string
}

// NewShipyardNeoBooter creates a Bay-backed booter.
// Mirrors ShipyardNeoBooter.__init__(endpoint_url, access_token, profile, ttl).
func NewShipyardNeoBooter(endpointURL, accessToken, profile string, ttl int) *ShipyardNeoBooter {
	if ttl <= 0 {
		ttl = 3600
	}
	if profile == "" {
		profile = ""
	}
	return &ShipyardNeoBooter{
		endpointURL: strings.TrimSpace(endpointURL),
		accessToken: strings.TrimSpace(accessToken),
		profile:     strings.TrimSpace(profile),
		ttl:         ttl,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		// execClient 无总超时：Exec 的命令超时由请求体 timeout 字段（300s）与
		// ctx 控制，Client.Timeout 会覆盖整个请求周期，60s 会把长命令掐断。
		execClient: &http.Client{},
	}
}

func (b *ShipyardNeoBooter) Type() BooterType { return BooterRemote }

func (b *ShipyardNeoBooter) IsRunning() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.running
}

// Start boots a sandbox on Bay: resolves the profile, creates the sandbox and
// waits until it is ready. Mirrors ShipyardNeoBooter.boot + _wait_until_ready.
func (b *ShipyardNeoBooter) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running {
		return nil
	}

	ep := strings.TrimSpace(b.endpointURL)
	if ep == "" || ep == "__auto__" {
		// Python auto-starts a Bay Docker container here; we require the user
		// to configure the endpoint explicitly.
		return fmt.Errorf("shipyard_neo_endpoint is empty; set the Bay endpoint URL (or configure access token/endpoint) to use the URL-based sandbox")
	}
	ep = strings.TrimSuffix(ep, "/")

	if b.accessToken == "" {
		b.accessToken = discoverBayCredentials()
	}
	if b.accessToken == "" {
		return fmt.Errorf("shipyard_neo_access_token is empty and no Bay credentials.json was found")
	}

	// Resolve the profile: explicit > best from /v1/profiles > python-default.
	profile, err := b.resolveProfile(ctx, ep)
	if err != nil {
		return err
	}

	body := map[string]interface{}{
		"profile": profile,
		"ttl":     b.ttl,
	}
	resp, err := b.post(ctx, ep, "/v1/sandboxes", body)
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	id, _ := resp["id"].(string)
	if id == "" {
		return fmt.Errorf("create sandbox: response missing id: %v", resp)
	}
	b.sandboxID = id

	// Readiness gate (mirrors _wait_until_ready): poll until "ready".
	deadline := time.Now().Add(neoReadinessTimeout)
	for {
		info, err := b.get(ctx, ep, "/v1/sandboxes/"+id, nil)
		if err != nil {
			b.delete(ep, id)
			return fmt.Errorf("sandbox %s readiness check failed: %w", id, err)
		}
		status, _ := info["status"].(string)
		if status == "ready" {
			if caps, ok := info["capabilities"].([]interface{}); ok {
				for _, c := range caps {
					if s, ok := c.(string); ok {
						b.caps = append(b.caps, s)
					}
				}
			}
			b.running = true
			neoLogger.Debug("Shipyard Neo sandbox ready: id=%s profile=%s capabilities=%v", id, profile, b.caps)
			return nil
		}
		if status == "failed" || status == "expired" {
			b.delete(ep, id)
			return fmt.Errorf("sandbox %s reached terminal state: %s", id, status)
		}
		if time.Now().After(deadline) {
			b.delete(ep, id)
			return fmt.Errorf("sandbox %s did not become ready within %s (last status: %s)", id, neoReadinessTimeout, status)
		}
		select {
		case <-ctx.Done():
			b.delete(ep, id)
			return ctx.Err()
		case <-time.After(neoPollInterval):
		}
	}
}

// resolveProfile mirrors Python's _resolve_profile: user profile > best from
// /v1/profiles (browser preferred, then most capabilities) > python-default.
func (b *ShipyardNeoBooter) resolveProfile(ctx context.Context, ep string) (string, error) {
	if b.profile != "" {
		return b.profile, nil
	}
	resp, err := b.get(ctx, ep, "/v1/profiles", map[string]string{"detail": "true"})
	if err != nil {
		neoLogger.I18nWarn("查询 Bay 配置档失败，回退到 python-default: %v", err)
		return "python-default", nil
	}
	items, _ := resp["items"].([]interface{})
	best := ""
	bestCaps := -1
	bestHasBrowser := false
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		caps, _ := m["capabilities"].([]interface{})
		hasBrowser := false
		for _, c := range caps {
			if cs, ok := c.(string); ok && cs == "browser" {
				hasBrowser = true
			}
		}
		score := len(caps)
		if best == "" || (hasBrowser && !bestHasBrowser) || (hasBrowser == bestHasBrowser && score > bestCaps) {
			best = id
			bestCaps = score
			bestHasBrowser = hasBrowser
		}
	}
	if best == "" {
		return "python-default", nil
	}
	return best, nil
}

func (b *ShipyardNeoBooter) Stop() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running {
		return nil
	}
	// Python does not delete the sandbox on normal shutdown; it stays until the
	// Bay TTL cleans it up. Just drop the local reference.
	b.running = false
	neoLogger.Debug("Shipyard Neo sandbox client shut down: id=%s", b.sandboxID)
	return nil
}

// Exec runs a command in the sandbox, mapping the (cmd, args) pair onto the
// Bay shell/python endpoints. Mirrors NeoShellComponent/NeoPythonComponent.
func (b *ShipyardNeoBooter) Exec(ctx context.Context, cmd string, args []string, workdir string) (string, string, int, error) {
	b.mu.Lock()
	running := b.running
	id := b.sandboxID
	ep := b.endpointURL
	b.mu.Unlock()
	if !running || id == "" {
		return "", "", -1, fmt.Errorf("shipyard neo sandbox not running")
	}
	ep = strings.TrimSuffix(strings.TrimSpace(ep), "/")

	// Strip a leading "-c" so both ["-c", body] and [body] call styles work.
	start := 0
	if len(args) > 0 && args[0] == "-c" {
		start = 1
	}
	body := strings.Join(args[start:], " ")

	switch {
	case cmd == "python3" || cmd == "python":
		payload := map[string]interface{}{"code": body, "timeout": 300}
		resp, err := b.execPost(ctx, ep, "/v1/sandboxes/"+id+"/python/exec", payload)
		if err != nil {
			return "", "", -1, err
		}
		out, _ := resp["output"].(string)
		errText, _ := resp["error"].(string)
		success, _ := resp["success"].(bool)
		if !success {
			return out, errText, 1, nil
		}
		return out, errText, 0, nil
	default:
		payload := map[string]interface{}{
			"command": body,
			"timeout": 300,
			"cwd":     normalizeNeoCwd(workdir),
		}
		resp, err := b.execPost(ctx, ep, "/v1/sandboxes/"+id+"/shell/exec", payload)
		if err != nil {
			return "", "", -1, err
		}
		out, _ := resp["output"].(string)
		errText, _ := resp["error"].(string)
		code := 0
		if ec, ok := resp["exit_code"].(float64); ok {
			code = int(ec)
		} else if success, ok := resp["success"].(bool); ok && !success {
			code = 1
		}
		return out, errText, code, nil
	}
}

// ReadFile reads a file from the sandbox workspace (paths relative to /workspace).
func (b *ShipyardNeoBooter) ReadFile(ctx context.Context, path string) (string, error) {
	b.mu.Lock()
	running := b.running
	id := b.sandboxID
	ep := b.endpointURL
	b.mu.Unlock()
	if !running || id == "" {
		return "", fmt.Errorf("shipyard neo sandbox not running")
	}
	ep = strings.TrimSuffix(strings.TrimSpace(ep), "/")
	resp, err := b.get(ctx, ep, "/v1/sandboxes/"+id+"/filesystem/files", map[string]string{"path": path})
	if err != nil {
		return "", err
	}
	content, _ := resp["content"].(string)
	return content, nil
}

// WriteFile writes a file into the sandbox workspace.
func (b *ShipyardNeoBooter) WriteFile(ctx context.Context, path, content string) error {
	b.mu.Lock()
	running := b.running
	id := b.sandboxID
	ep := b.endpointURL
	b.mu.Unlock()
	if !running || id == "" {
		return fmt.Errorf("shipyard neo sandbox not running")
	}
	ep = strings.TrimSuffix(strings.TrimSpace(ep), "/")
	body := map[string]interface{}{"path": path, "content": content}
	_, err := b.put(ctx, ep, "/v1/sandboxes/"+id+"/filesystem/files", body)
	return err
}

// ListSkills discovers SKILL.md files under /workspace/skills via a shell find.
func (b *ShipyardNeoBooter) ListSkills(ctx context.Context) ([]skills.SandboxCacheEntry, error) {
	out, errText, code, err := b.Exec(ctx, "sh", []string{"-c", "find /workspace/skills -name SKILL.md -o -name skill.md 2>/dev/null"}, SandboxWorkdir)
	if err != nil || code != 0 {
		return nil, fmt.Errorf("list sandbox skills: %s%s", out, errText)
	}
	var entries []skills.SandboxCacheEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		dir := filepath.Base(filepath.Dir(line))
		content, err := b.ReadFile(ctx, line)
		if err != nil {
			continue
		}
		entries = append(entries, skills.SandboxCacheEntry{
			Name:        dir,
			Description: skills.ParseFrontmatterDescription(content),
			Path:        strings.ReplaceAll(line, "\\", "/"),
		})
	}
	return entries, nil
}

// normalizeNeoCwd converts a workdir into a Bay-relative path. Bay treats cwd
// as relative to /workspace, so "/workspace" and "/workspace/x" become "." and
// "x".
func normalizeNeoCwd(workdir string) string {
	wd := strings.TrimSpace(workdir)
	wd = strings.TrimPrefix(wd, SandboxWorkdir)
	wd = strings.TrimPrefix(wd, "/")
	if wd == "" {
		return "."
	}
	return wd
}

// ---- Bay HTTP helpers ----

func (b *ShipyardNeoBooter) do(ctx context.Context, client *http.Client, ep, method, path string, body interface{}, params map[string]string, contentType string) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		switch v := body.(type) {
		case []byte:
			rdr = bytes.NewReader(v)
		case io.Reader:
			rdr = v
		default:
			raw, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			rdr = bytes.NewReader(raw)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, ep+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.accessToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	q := req.URL.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("bay HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (b *ShipyardNeoBooter) get(ctx context.Context, ep, path string, params map[string]string) (map[string]interface{}, error) {
	data, err := b.do(ctx, b.client, ep, http.MethodGet, path, nil, params, "")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (b *ShipyardNeoBooter) post(ctx context.Context, ep, path string, body interface{}) (map[string]interface{}, error) {
	data, err := b.do(ctx, b.client, ep, http.MethodPost, path, body, nil, "application/json")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// execPost issues a POST on the exec client (no overall timeout; cancellation
// follows ctx), used by the shell/python exec endpoints whose commands may run
// up to the request's timeout field (300s).
func (b *ShipyardNeoBooter) execPost(ctx context.Context, ep, path string, body interface{}) (map[string]interface{}, error) {
	data, err := b.do(ctx, b.execClient, ep, http.MethodPost, path, body, nil, "application/json")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (b *ShipyardNeoBooter) put(ctx context.Context, ep, path string, body interface{}) (map[string]interface{}, error) {
	data, err := b.do(ctx, b.client, ep, http.MethodPut, path, body, nil, "application/json")
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (b *ShipyardNeoBooter) delete(ep, id string) {
	// Use an independent short-timeout context: callers may pass a ctx that is
	// already canceled (e.g. the failure paths in Start), which would make the
	// DELETE fail instantly and leak the remote sandbox until Bay's TTL.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = b.do(ctx, b.client, ep, http.MethodDelete, "/v1/sandboxes/"+id, nil, nil, "")
}

// discoverBayCredentials mirrors _discover_bay_credentials: it looks for a Bay
// api_key in credentials.json under BAY_DATA_DIR, the working directory, or
// ./data.
func discoverBayCredentials() string {
	home, _ := os.UserHomeDir()
	candidates := []string{
		filepath.Join(os.Getenv("BAY_DATA_DIR"), "credentials.json"),
		filepath.Join(home, ".config", "bay", "credentials.json"),
		"credentials.json",
		filepath.Join("data", "credentials.json"),
	}
	for _, p := range candidates {
		if p == "" {
			continue
		}
		// #nosec G304 -- p is one of a fixed list of local credential paths.
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var m map[string]interface{}
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if key, ok := m["api_key"].(string); ok && key != "" {
			return key
		}
	}
	return ""
}

// ensure ShipyardNeoBooter implements Booter.
var _ Booter = (*ShipyardNeoBooter)(nil)
