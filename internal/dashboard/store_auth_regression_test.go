// Regression tests for M-06 / M-07 / M-09.
package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// ── M-06: mcpStore / personaStore must hand out deep copies ─────

func TestMCPGetReturnsDeepCopy(t *testing.T) {
	ms := newMCPStore(t.TempDir())
	nested := map[string]interface{}{
		"transport": "sse",
		"url":       "http://example.com",
		"headers":   map[string]interface{}{"Authorization": "Bearer x"},
		"args":      []interface{}{"a", "b"},
	}
	if err := ms.upsert("srv", nested); err != nil {
		t.Fatal(err)
	}

	got := ms.get("srv")
	got["active"] = false
	got["headers"].(map[string]interface{})["Authorization"] = "mutated"
	got["args"].([]interface{})[0] = "zzz"

	again := ms.get("srv")
	if active, _ := again["active"].(bool); !active {
		t.Fatal("mutation of returned map leaked into the store")
	}
	if h := again["headers"].(map[string]interface{})["Authorization"]; h != "Bearer x" {
		t.Fatalf("nested map mutation leaked into the store: %v", h)
	}
	args := again["args"].([]interface{})
	if args[0] != "a" || args[1] != "b" {
		t.Fatalf("slice mutation leaked into the store: %v", args)
	}
}

func TestMCPListReturnsDeepCopy(t *testing.T) {
	ms := newMCPStore(t.TempDir())
	if err := ms.upsert("srv", map[string]interface{}{
		"transport": "sse",
		"headers":   map[string]interface{}{"Authorization": "Bearer x"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, info := range ms.list() {
		info["active"] = false
		info["headers"].(map[string]interface{})["Authorization"] = "mutated"
	}
	got := ms.get("srv")
	if active, _ := got["active"].(bool); !active {
		t.Fatal("mutation of listed map leaked into the store")
	}
	if h := got["headers"].(map[string]interface{})["Authorization"]; h != "Bearer x" {
		t.Fatalf("nested map mutation leaked via list: %v", h)
	}
}

func TestPersonaFoldersReturnDeepCopy(t *testing.T) {
	ps := newPersonaStore(t.TempDir())
	if err := ps.upsertFolder(map[string]interface{}{"folder_id": "f1", "name": "root"}); err != nil {
		t.Fatal(err)
	}
	for _, f := range ps.listFolders(nil) {
		f["sort_order"] = 999.0
		f["name"] = "mutated"
	}
	g := ps.getFolder("f1")
	g["name"] = "mutated"
	g["sort_order"] = 777.0

	lst := ps.listFolders(nil)
	if len(lst) != 1 {
		t.Fatalf("expected 1 folder, got %d", len(lst))
	}
	if name, _ := lst[0]["name"].(string); name != "root" {
		t.Fatalf("mutation leaked into folder store: %v", name)
	}

	// reorder writes the stored map under the lock; readers must not see torn data
	if err := ps.reorder([]map[string]interface{}{{"id": "f1", "type": "folder", "sort_order": 5.0}}); err != nil {
		t.Fatal(err)
	}
	if so, _ := ps.getFolder("f1")["sort_order"].(float64); so != 5.0 {
		t.Fatalf("sort_order = %v, want 5.0", so)
	}
}

// ── M-07: TOTP recovery must keep the secret / not downgrade 2FA ─

func TestRegenerateRecoveryCodesKeepsSecret(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	secret, _, _, err := s.auth.GenerateTOTP()
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.auth.EnableTOTP(code) {
		t.Fatal("failed to enable TOTP")
	}

	newCodes, err := s.auth.RegenerateRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(newCodes) == 0 {
		t.Fatal("no recovery codes generated")
	}
	if got := s.auth.TOTPSecret(); got != secret {
		t.Fatalf("secret changed after regenerating codes: got %q want %q", got, secret)
	}
	if !s.auth.TOTPEnabled() {
		t.Fatal("TOTP must stay enabled after regenerating codes")
	}
	// 旧恢复码作废、新恢复码有效
	if ok, _ := s.auth.VerifyTOTPEx(code); !ok {
		t.Fatal("authenticator code must still validate after regenerating codes")
	}
	if ok, used := s.auth.VerifyTOTPEx(newCodes[0]); !ok || !used {
		t.Fatalf("new recovery code must validate as recovery, ok=%v used=%v", ok, used)
	}
}

func TestTOTPRecoveryEndpointKeepsSecret(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	secret, _, _, err := s.auth.GenerateTOTP()
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.auth.EnableTOTP(code) {
		t.Fatal("failed to enable TOTP")
	}

	req := authedRequest(t, s, http.MethodPost, "/api/auth/totp/recovery", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid response: %v (%s)", err, w.Body.String())
	}
	if st, _ := v["status"].(string); st != "ok" {
		t.Fatalf("recovery failed: %s", w.Body.String())
	}
	data, _ := v["data"].(map[string]interface{})
	codes, _ := data["recovery_codes"].([]interface{})
	if len(codes) == 0 {
		t.Fatal("recovery endpoint returned no codes")
	}
	if got := s.auth.TOTPSecret(); got != secret {
		t.Fatalf("recovery endpoint changed the secret: got %q want %q", got, secret)
	}
	if !s.auth.TOTPEnabled() {
		t.Fatal("recovery endpoint must not disable TOTP")
	}
}

func TestTOTPSetupRejectedWhenEnabled(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	secret, _, _, err := s.auth.GenerateTOTP()
	if err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !s.auth.EnableTOTP(code) {
		t.Fatal("failed to enable TOTP")
	}

	// setup 第一步在 TOTP 已启用时必须拒绝，且不得覆盖现有密钥
	req := authedRequest(t, s, http.MethodPost, "/api/auth/totp/setup", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid response: %v (%s)", err, w.Body.String())
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Fatalf("expected setup step-1 to be rejected while TOTP enabled, got: %s", w.Body.String())
	}
	if got := s.auth.TOTPSecret(); got != secret {
		t.Fatalf("setup step-1 must not regenerate the secret while enabled: got %q want %q", got, secret)
	}
	if !s.auth.TOTPEnabled() {
		t.Fatal("setup step-1 must not disable TOTP while enabled")
	}
}

// ── M-09: market fetch failures must surface explicit errors ────

func TestFetchPluginMarketHTTPErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	if _, err := s.fetchPluginMarket(srv.URL, false); err == nil {
		t.Fatal("expected error for non-200 upstream with no cache")
	}
}

func TestFetchPluginMarketDecodeErrorReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	if _, err := s.fetchPluginMarket(srv.URL, false); err == nil {
		t.Fatal("expected error for decode failure with no cache")
	}
}

// TestPluginLogoPublicWithoutToken: 插件 logo 端点必须对未认证请求放行
// （WebUI 用 <img src> 直接加载，浏览器无法携带 Authorization header），
// 而其他 plugins 端点仍要求认证。
func TestPluginLogoPublicWithoutToken(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	// 未认证访问 plugins/logo：放行（走到 handler，无插件时 404 而非 401）。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins/logo?plugin_id=nonexistent", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code == http.StatusUnauthorized {
		t.Fatalf("plugins/logo must not require auth (img tags carry no header), got 401")
	}

	// 未认证访问其他 plugins 端点：仍 401。
	req = httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("plugins list must still require auth, got %d", w.Code)
	}
}
