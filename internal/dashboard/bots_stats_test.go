package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
)

// TestBotsStats: /api/v1/bots/stats lists enabled platforms with proactive meta.
func TestBotsStats(t *testing.T) {
	s := &Server{}
	// Use a real config manager holding the platform list.
	cm := config.NewConfigManager()
	cfg := config.NewConfig("", nil)
	_ = cfg.Set("platform", []interface{}{
		map[string]interface{}{"id": "default_1", "type": "qq_official", "enable": true},
		map[string]interface{}{"id": "tg", "type": "telegram", "enable": false},
		map[string]interface{}{"id": "wc", "type": "webchat", "enable": true},
	})
	cm.Register("default", cfg)
	s.configMgr = cm

	req := httptest.NewRequest(http.MethodGet, "/api/v1/bots/stats", nil)
	w := httptest.NewRecorder()
	s.handleBots(w, req, []string{"stats"})

	var resp struct {
		Data struct {
			Platforms []map[string]interface{} `json:"platforms"`
			Summary   map[string]interface{}   `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Platforms) != 3 {
		t.Fatalf("expected 3 platforms, got %d", len(resp.Data.Platforms))
	}
	// qq_official has proactive meta.
	foundProactive := false
	for _, p := range resp.Data.Platforms {
		meta, _ := p["meta"].(map[string]interface{})
		if meta == nil {
			t.Fatalf("platform missing meta: %v", p)
		}
		if v, _ := meta["support_proactive_message"].(bool); v {
			foundProactive = true
		}
		if p["type"] == "qq_official" && meta["name"] != "qq_official" {
			t.Errorf("meta.name: %v", meta["name"])
		}
	}
	if !foundProactive {
		t.Error("no platform advertises support_proactive_message")
	}
	if int(resp.Data.Summary["total"].(float64)) != 3 {
		t.Errorf("summary.total: %v", resp.Data.Summary["total"])
	}
}
