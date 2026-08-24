package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
)

// TestBotsStats: /api/v1/bots/stats lists enabled platforms with proactive meta.
func TestBotsStats(t *testing.T) {
	s := &Server{}
	// Use a real config manager holding the platform list.
	cm := config.NewConfigManager()
	cfg := config.NewConfig("")
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

// TestChildProcessMemoryMB: 子进程 RSS 统计——spawn 一个常驻子进程后，
// childProcessMemoryMB 必须统计到其 RSS > 0（覆盖 gRPC 插件子进程统计路径）。
func TestChildProcessMemoryMB(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动子进程: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(100 * time.Millisecond) // 等子进程就绪
	if mb := childProcessMemoryMB(); mb <= 0 {
		t.Fatalf("childProcessMemoryMB = %d, want > 0 (subprocess RSS must be counted)", mb)
	}
}

// TestGetBaseStatsMemoryIncludesChildren: getBaseStats 的 memory 结构必须含
// process/plugins/total/system 四段，total = process + plugins。
func TestGetBaseStatsMemoryIncludesChildren(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动子进程: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()
	time.Sleep(100 * time.Millisecond)

	stats := s.getBaseStats(0)
	mem, ok := stats["memory"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing memory block: %#v", stats["memory"])
	}
	for _, k := range []string{"process", "plugins", "total", "system"} {
		if _, ok := mem[k]; !ok {
			t.Fatalf("memory block missing %q: %#v", k, mem)
		}
	}
	proc, _ := mem["process"].(int)
	plug, _ := mem["plugins"].(int)
	total, _ := mem["total"].(int)
	if plug <= 0 {
		t.Fatalf("plugins memory = %d, want > 0", plug)
	}
	if total != proc+plug {
		t.Fatalf("total (%d) != process (%d) + plugins (%d)", total, proc, plug)
	}
}
