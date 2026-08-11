package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestSSELiveCallTool connects to a real MCP SSE server and invokes a tool
// (network test; skipped unless ASTRBOT_MCP_SSE_TEST_URL is set).
func TestSSELiveCallTool(t *testing.T) {
	url := os.Getenv("ASTRBOT_MCP_SSE_TEST_URL")
	if url == "" {
		t.Skip("set ASTRBOT_MCP_SSE_TEST_URL to run live test")
	}
	c := NewMCPClient("live", map[string]interface{}{"url": url, "transport": "sse"})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(c.Tools()) == 0 {
		t.Fatal("no tools")
	}
	res, err := c.CallTool(ctx, c.Tools()[0].Name, map[string]interface{}{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("empty tool result: %+v", res)
	}
	c.Cleanup()
}
