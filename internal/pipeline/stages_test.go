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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><head><script>var x=1;</script><style>.a{}</style></head><body><h1>Hello</h1><p>World</p></body></html>"))
	}))
	defer srv.Close()

	out := executeWebFetch(srv.URL, 1000)
	if strings.Contains(out, "<h1>") {
		t.Errorf("HTML tags not stripped: %q", out)
	}
	if !strings.Contains(out, "Hello") || !strings.Contains(out, "World") {
		t.Errorf("expected text content: %q", out)
	}
	if strings.Contains(out, "var x") || strings.Contains(out, ".a{}") {
		t.Errorf("script/style content leaked: %q", out)
	}

	// Bad URL / errors handled gracefully.
	if out := executeWebFetch("", 100); !strings.Contains(out, "错误") {
		t.Errorf("expected error for empty url, got: %q", out)
	}
	if out := executeWebFetch("ftp://x", 100); !strings.Contains(out, "错误") {
		t.Errorf("expected error for non-http url, got: %q", out)
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
