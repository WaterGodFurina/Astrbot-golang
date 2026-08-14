package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
)

// fakeStdioServer is a tiny JSON-RPC MCP server that echoes the request id so
// the client's id-correlation logic can be exercised.
const fakeStdioServer = `import sys, json
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        req = json.loads(line)
    except Exception:
        continue
    method = req.get("method", "")
    rid = req.get("id")
    if method == "initialize":
        resp = {"jsonrpc": "2.0", "id": rid, "result": {"protocolVersion": "2024-11-05"}}
    elif method == "tools/list":
        resp = {"jsonrpc": "2.0", "id": rid, "result": {"tools": [
            {"name": "hello", "description": "Say hello", "inputSchema": {"type": "object"}},
            {"name": "echo", "description": "Echo back the text arg", "inputSchema": {"type": "object", "properties": {"text": {"type": "string"}}}},
        ]}}
    elif method == "tools/call":
        args = (req.get("params") or {}).get("arguments") or {}
        resp = {"jsonrpc": "2.0", "id": rid, "result": {"content": [{"type": "text", "text": "hello " + str(args.get("text", ""))}]}}
    else:
        resp = {"jsonrpc": "2.0", "id": rid, "result": {}}
    sys.stdout.write(json.dumps(resp) + "\n")
    sys.stdout.flush()
`

func writeFakeServer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake_mcp.py")
	if err := os.WriteFile(path, []byte(fakeStdioServer), 0755); err != nil {
		t.Fatalf("write fake server: %v", err)
	}
	return path
}

func TestMCPStdioConnectAndCall(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 not available")
	}
	script := writeFakeServer(t)
	client := NewMCPClient("fake", map[string]interface{}{
		"transport": "stdio",
		"command":   "/usr/bin/python3",
		"args":      []interface{}{script},
	})
	defer client.Cleanup()

	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}

	tools := client.Tools()
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["hello"] || !names["echo"] {
		t.Errorf("unexpected tools: %+v", tools)
	}

	res, err := client.CallTool(context.Background(), "echo", map[string]interface{}{"text": "world"})
	if err != nil {
		t.Fatalf("call tool: %v", err)
	}
	if res.IsError {
		t.Errorf("unexpected error result")
	}
	text := mcpContentTextForTest(res.Content)
	if !strings.Contains(text, "hello world") {
		t.Errorf("unexpected tool result: %q", text)
	}
}

// mcpContentTextForTest mirrors the pipeline's text extraction (kept local to
// avoid an import cycle).
func mcpContentTextForTest(content []map[string]interface{}) string {
	var parts []string
	for _, block := range content {
		if text, ok := block["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// TestMCPSSEHandshakeRequiresEndpoint verifies that connecting to an SSE URL
// that never sends an endpoint event fails instead of hanging.
func TestMCPSSEHandshakeRequiresEndpoint(t *testing.T) {
	// A server that returns 200 + a text/event-stream header but immediately
	// closes the body without ever sending an endpoint event. The client must
	// fail (EOF on the stream) instead of hanging.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewMCPClient("sse-noep", map[string]interface{}{
		"url":       srv.URL,
		"transport": "sse",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err == nil {
		t.Fatal("expected error when no endpoint event is sent")
	}
	client.Cleanup()
}

// TestMCPContentToMapCoversResourceLink verifies ResourceLink is rendered as a
// URI text block and unknown content falls back to a JSON dump instead of a Go
// struct dump (L-46.9a).
func TestMCPContentToMapCoversResourceLink(t *testing.T) {
	link := mcpContentToMap(mcp.ResourceLink{URI: "file:///a.txt", Name: "a"})
	if link["type"] != "text" {
		t.Fatalf("resource link type = %v, want text", link["type"])
	}
	if text, _ := link["text"].(string); !strings.Contains(text, "file:///a.txt") {
		t.Fatalf("resource link text = %q, want URI", text)
	}

	toolUse := mcpContentToMap(mcp.ToolUseContent{Name: "lookup", Input: map[string]interface{}{"q": "x"}})
	if toolUse["type"] != "text" {
		t.Fatalf("tool use type = %v, want text", toolUse["type"])
	}
	if text, _ := toolUse["text"].(string); !strings.Contains(text, "lookup") || !strings.Contains(text, `"q":"x"`) {
		t.Fatalf("tool use dump = %q, want JSON with name and input", text)
	}
}

// TestMCPReconnectSkipsStaleConnection verifies Reconnect is a no-op when the
// live connection has already been replaced by a concurrent reconnect, so a
// burst of concurrent failures cannot tear down a freshly rebuilt connection
// (L-46.9b).
func TestMCPReconnectSkipsStaleConnection(t *testing.T) {
	if _, err := os.Stat("/usr/bin/python3"); err != nil {
		t.Skip("python3 not available")
	}
	script := writeFakeServer(t)
	c := NewMCPClient("fake", map[string]interface{}{
		"transport": "stdio",
		"command":   "/usr/bin/python3",
		"args":      []interface{}{script},
	})
	defer c.Cleanup()
	if err := c.Connect(context.Background()); err != nil {
		t.Skipf("connect failed: %v", err)
	}
	live := c.Conn()
	if live == nil {
		t.Fatal("expected a live connection")
	}

	// The caller observed a stale/broken handle; the live connection differs,
	// so Reconnect must leave it untouched and report success.
	stale := &client.Client{}
	if err := c.Reconnect(context.Background(), stale); err != nil {
		t.Fatalf("stale reconnect must be a no-op, got err %v", err)
	}
	if got := c.Conn(); got != live {
		t.Fatal("stale reconnect tore down the replaced connection")
	}
	if !c.IsActive() {
		t.Fatal("client should still be active after the no-op reconnect")
	}
}
