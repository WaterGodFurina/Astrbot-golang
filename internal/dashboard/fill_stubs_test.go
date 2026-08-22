package dashboard

// 针对补齐的三个 OpenAPI 端点的回归测试：
//   - POST /conversations/export（JSONL 导出 + 归属校验）
//   - GET  /subagents/available-tools
//   - 插件页面 asset_token 安全链（签发/校验/防篡改/防跨插件/过期）

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
)

// ── conversations export ─────────────────────────────────────

func TestConversationsExportReturnsJSONL(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	mgr := conversation.NewManager(nil)
	s.conversationMgr = mgr
	conv := mgr.NewConversation("test:group:g1", "test")
	mgr.AppendHistory("test:group:g1", "user", "hi")

	body := `{"conversations":[{"user_id":"test:group:g1","cid":"` + conv.CID + `"}]}`
	req := authedRequest(t, s, http.MethodPost, "/api/v1/conversations/export", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "jsonl") {
		t.Errorf("unexpected content type: %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("expected attachment disposition, got %q", cd)
	}
	firstLine := strings.SplitN(w.Body.String(), "\n", 2)[0]
	var rec map[string]interface{}
	if err := json.Unmarshal([]byte(firstLine), &rec); err != nil {
		t.Fatalf("first line not jsonl: %v (%q)", err, w.Body.String())
	}
	if rec["cid"] != conv.CID {
		t.Errorf("wrong cid in export: %v", rec["cid"])
	}
	if rec["user_id"] != "test:group:g1" {
		t.Errorf("wrong user_id in export: %v", rec["user_id"])
	}
}

func TestConversationsExportRejectsForeignOwner(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	mgr := conversation.NewManager(nil)
	s.conversationMgr = mgr
	conv := mgr.NewConversation("test:group:g1", "test")

	// user_id 与会话 umo 不匹配：不得导出他人会话。
	body := `{"conversations":[{"user_id":"other:group:g1","cid":"` + conv.CID + `"}]}`
	req := authedRequest(t, s, http.MethodPost, "/api/v1/conversations/export", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "没有成功导出任何对话") {
		t.Errorf("expected export failure message, got %s", w.Body.String())
	}
}

func TestConversationsExportRejectsEmptyList(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	req := authedRequest(t, s, http.MethodPost, "/api/v1/conversations/export",
		strings.NewReader(`{"conversations":[]}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (%s)", w.Code, w.Body.String())
	}
}

// ── subagents available-tools ────────────────────────────────

func TestSubagentAvailableToolsListsBuiltin(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	req := authedRequest(t, s, http.MethodGet, "/api/v1/subagents/available-tools", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d (%s)", w.Code, w.Body.String())
	}
	var v struct {
		Data struct {
			Tools []map[string]interface{} `json:"tools"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v (%s)", err, w.Body.String())
	}
	if len(v.Data.Tools) == 0 {
		t.Errorf("expected non-empty tools list (builtin), got %s", w.Body.String())
	}
	for _, tool := range v.Data.Tools {
		if strings.HasPrefix(tool["name"].(string), "transfer_to_") {
			t.Errorf("subagent internal tool leaked: %v", tool["name"])
		}
	}
}

// ── 插件页面 asset_token 安全链 ───────────────────────────────

func TestPluginPageAssetTokenRoundTrip(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	tok := s.issuePluginPageAssetToken("p1", "home")
	if !s.validPluginPageAssetToken(tok, "p1", "home") {
		t.Fatal("valid token rejected")
	}
	if s.validPluginPageAssetToken(tok, "p2", "home") {
		t.Error("cross-plugin token must be rejected")
	}
	if s.validPluginPageAssetToken(tok, "p1", "other") {
		t.Error("cross-page token must be rejected")
	}
	if s.validPluginPageAssetToken(tok+"x", "p1", "home") {
		t.Error("tampered token must be rejected")
	}
	if s.validPluginPageAssetToken("", "p1", "home") {
		t.Error("empty token must be rejected")
	}
}

func TestPluginPageAssetTokenExpires(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	// 用同一签名方案手工构造一个已过期 token，验证 exp 校验生效。
	payload := "p1\x00home\x00" + strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.pluginPageSecret()))
	mac.Write([]byte(payload))
	tok := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if s.validPluginPageAssetToken(tok, "p1", "home") {
		t.Error("expired token must be rejected")
	}
}

func TestPluginPageNameAndDirSanitizers(t *testing.T) {
	for _, bad := range []string{"", "..", ".", "../x", "a/b", "a\\b", ".hidden"} {
		if _, err := normalizePluginPageName(bad); err == nil {
			t.Errorf("normalizePluginPageName(%q) should fail", bad)
		}
	}
	if got, err := normalizePluginPageName("  主页 "); err != nil || got != "主页" {
		t.Errorf("normalizePluginPageName trim failed: %q %v", got, err)
	}
	for in, want := range map[string]string{"..": "_", ".": "_", "...": "_", "a/b": "a_b", "a\\b": "a_b", "ok-id_1.2": "ok-id_1.2"} {
		if got := safePluginDirID(in); got != want {
			t.Errorf("safePluginDirID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPluginPageContentRequiresAssetCredential(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	// 无 token 无 Cookie：鉴权门必须拒绝（401），不得落入文件读取。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/page-content/p1/home/", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated asset request must 401, got %d (%s)", w.Code, w.Body.String())
	}
}
