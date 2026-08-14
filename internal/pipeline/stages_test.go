package pipeline

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuiltinToolsSchema(t *testing.T) {
	tools := builtinTools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 built-in tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		fn, ok := tool["function"].(map[string]interface{})
		if !ok {
			t.Fatalf("tool missing function object: %v", tool)
		}
		name, _ := fn["name"].(string)
		names[name] = true
		if fn["description"] == "" {
			t.Errorf("tool %q missing description", name)
		}
	}
	if !names["get_current_time"] || !names["web_fetch"] {
		t.Errorf("unexpected tool set: %v", names)
	}
}

func TestExecuteGetCurrentTime(t *testing.T) {
	out := executeGetCurrentTime("Asia/Shanghai")
	if !strings.Contains(out, "Asia/Shanghai") {
		t.Errorf("timezone not applied: %q", out)
	}
	if !strings.Contains(out, "20") { // year 2026 includes "20"; sanity date format check
		t.Errorf("unexpected output format: %q", out)
	}
	// Invalid timezone falls back to local without erroring.
	out2 := executeGetCurrentTime("Not/AZone")
	if out2 == "" {
		t.Errorf("expected non-empty local time")
	}
}

func TestExecuteWebFetch(t *testing.T) {
	cases := []struct {
		name, url, want string
	}{
		{"empty", "", "错误"},
		{"non-http", "ftp://example.com/x", "错误"},
		{"loopback", "http://127.0.0.1/", "环回"},
		{"localhost", "http://localhost/", "环回"},
		{"private", "http://10.0.0.1/", "私网"},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/", "元数据"},
		{"ipv6 loopback", "http://[::1]/", "环回"},
	}
	for _, c := range cases {
		if out := executeWebFetch(c.url, 1000); !strings.Contains(out, c.want) {
			t.Errorf("executeWebFetch(%q) = %q, want substring %q", c.url, out, c.want)
		}
	}
}

func TestValidateWebFetchHost(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.8.8.8", "10.0.0.1", "172.16.0.1", "172.31.255.255",
		"192.168.1.1", "169.254.169.254", "169.254.1.1", "::1", "fe80::1",
		"0.0.0.0", "100.64.0.1", "100.100.100.200",
	}
	for _, h := range blocked {
		if err := validateWebFetchHost(h); err == nil {
			t.Errorf("expected %q to be rejected", h)
		}
	}
	allowed := []string{"1.1.1.1", "8.8.8.8", "9.9.9.9", "208.67.222.222"}
	for _, h := range allowed {
		if err := validateWebFetchHost(h); err != nil {
			t.Errorf("expected %q to be allowed, got %v", h, err)
		}
	}
}

func TestValidateWebFetchURL(t *testing.T) {
	if _, err := validateWebFetchURL("ftp://example.com/x"); err == nil {
		t.Errorf("expected non-http scheme rejected")
	}
	if _, err := validateWebFetchURL("http://127.0.0.1/x"); err == nil {
		t.Errorf("expected loopback rejected")
	}
	u, err := validateWebFetchURL("http://1.1.1.1/page#frag")
	if err != nil {
		t.Fatalf("expected public url allowed: %v", err)
	}
	if strings.Contains(u, "#") {
		t.Errorf("fragment not stripped: %q", u)
	}
}

func TestWebFetchRedirectGuard(t *testing.T) {
	req := httptest.NewRequest("GET", "http://1.1.1.1/", nil)
	if err := webFetchRedirectGuard(req, nil); err != nil {
		t.Errorf("expected public redirect allowed, got %v", err)
	}
	req2 := httptest.NewRequest("GET", "http://127.0.0.1/", nil)
	if err := webFetchRedirectGuard(req2, nil); err == nil {
		t.Errorf("expected loopback redirect rejected")
	}
	req3 := httptest.NewRequest("GET", "http://8.8.8.8/", nil)
	if err := webFetchRedirectGuard(req3, make([]*http.Request, maxRedirects)); err == nil {
		t.Errorf("expected too-many-redirects rejected")
	}
	req4 := httptest.NewRequest("GET", "ftp://1.1.1.1/", nil)
	if err := webFetchRedirectGuard(req4, nil); err == nil {
		t.Errorf("expected non-http redirect rejected")
	}
}

func TestAllowedWebFetchContentType(t *testing.T) {
	allowed := []string{"text/html", "text/html; charset=utf-8", "text/plain", "application/json", "application/xml", ""}
	for _, ct := range allowed {
		if !allowedWebFetchContentType(ct) {
			t.Errorf("expected %q to be allowed", ct)
		}
	}
	blocked := []string{"image/png", "image/jpeg", "application/octet-stream", "application/pdf", "application/zip"}
	for _, ct := range blocked {
		if allowedWebFetchContentType(ct) {
			t.Errorf("expected %q to be rejected", ct)
		}
	}
}

func TestExecuteBuiltinToolDispatch(t *testing.T) {
	res, handled := executeBuiltinTool("get_current_time", map[string]interface{}{})
	if !handled || res == "" {
		t.Errorf("get_current_time not dispatched: handled=%v res=%q", handled, res)
	}
	if _, handled := executeBuiltinTool("unknown_tool", nil); handled {
		t.Errorf("unknown tool should not be handled")
	}
}

func TestMCPContentText(t *testing.T) {
	content := []map[string]interface{}{
		{"type": "text", "text": "hello"},
		{"type": "text", "text": "world"},
		{"type": "image", "data": "base64..."},
	}
	if out := mcpContentText(content); out != "hello\nworld" {
		t.Errorf("expected joined text, got %q", out)
	}
	if out := mcpContentText(nil); out != "" {
		t.Errorf("expected empty for nil, got %q", out)
	}
}
