package agent

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestSSELiveAgainstModelscope(t *testing.T) {
	url := os.Getenv("ASTRBOT_MCP_SSE_TEST_URL")
	if url == "" {
		t.Skip("set ASTRBOT_MCP_SSE_TEST_URL to run live test")
	}
	c := NewMCPClient("live", map[string]interface{}{"url": url, "transport": "sse"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Logf("tools: %d", len(c.Tools()))
	for _, tool := range c.Tools() {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}
	c.Cleanup()
}
