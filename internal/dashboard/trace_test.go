package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// TestTraceSettingsGet: GET returns the config trace_enable.
func TestTraceSettingsGet(t *testing.T) {
	s := &Server{}
	s.configMgr = nil
	// configMgr nil -> default true (getConfigData returns nil).
	req := httptest.NewRequest(http.MethodGet, "/api/v1/trace/settings", nil)
	w := httptest.NewRecorder()
	s.handleTrace(w, req, []string{"settings"})
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if v, ok := resp.Data["trace_enable"].(bool); !ok || !v {
		t.Errorf("trace_enable: %v", resp.Data)
	}
}

// TestLogHistoryIncludesTrace: a published trace appears in /api/v1/logs.
func TestLogHistoryIncludesTrace(t *testing.T) {
	log.GetDefault().PublishTrace(map[string]interface{}{
		"type": "trace", "span_id": "hist1", "action": "astr_agent_prepare",
		"fields": map[string]interface{}{"persona_id": "x"},
	})
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/logs", nil)
	w := httptest.NewRecorder()
	s.handleLogs(w, req, []string{"history"})
	var resp struct {
		Data struct {
			Logs []map[string]interface{} `json:"logs"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range resp.Data.Logs {
		if typ, _ := item["type"].(string); typ == "trace" {
			if sid, _ := item["span_id"].(string); sid == "hist1" {
				found = true
			}
		}
	}
	if !found {
		t.Error("trace entry not present in log history")
	}
}

// TestTraceSSEStream: the SSE stream emits trace entries with the right shape,
// verified over a real HTTP server (ResponseRecorder lacks a Flusher).
func TestTraceSSEStream(t *testing.T) {
	s := &Server{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.handleLogStream(w, r)
	}))
	defer srv.Close()

	// Publish AFTER the client connects so the entry is streamed, not only in
	// history (handleLogStream has no history replay).
	streamDone := make(chan string, 1)
	go func() {
		resp, err := http.Get(srv.URL)
		if err != nil {
			streamDone <- ""
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 64*1024)
		n, _ := resp.Body.Read(buf)
		streamDone <- string(buf[:n])
	}()

	time.Sleep(300 * time.Millisecond)
	log.GetDefault().PublishTrace(map[string]interface{}{
		"type": "trace", "span_id": "sse1", "action": "agent_tool_call",
		"fields": map[string]interface{}{"tool_name": "web_fetch"},
	})

	select {
	case body := <-streamDone:
		if !strings.Contains(body, "trace") || !strings.Contains(body, "sse1") {
			t.Errorf("SSE stream must contain the trace payload, got: %.200s", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no SSE data received")
	}
}
