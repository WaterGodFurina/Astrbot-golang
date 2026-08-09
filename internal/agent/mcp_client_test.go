package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if tools[0].Name != "hello" || tools[1].Name != "echo" {
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
