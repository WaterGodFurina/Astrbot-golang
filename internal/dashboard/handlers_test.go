package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
)

// authedRequest builds an HTTP request carrying a valid session token so it
// passes the global /api auth gate added to apiHandler.
func authedRequest(t *testing.T, s *Server, method, path string, body io.Reader) *http.Request {
	t.Helper()
	tok, err := s.auth.IssueToken(s.auth.Username())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	req := httptest.NewRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+tok)
	return req
}

func TestHandlersReturnNamedObjects(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	tests := []struct {
		method string
		path   string
		check  func(t *testing.T, body string)
	}{
		{"GET", "/api/system-config/schema", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			data, ok := v["data"].(map[string]interface{})
			if !ok {
				t.Fatalf("expected data object, got: %T", v["data"])
			}
			if _, ok := data["config"]; !ok {
				t.Errorf("missing 'config' key in data")
			}
			if _, ok := data["metadata"]; !ok {
				t.Errorf("missing 'metadata' key in data")
			}
		}},
		{"GET", "/api/config-profiles", func(t *testing.T, body string) {
			if !strings.Contains(body, `"info_list"`) {
				t.Errorf("expected 'info_list' key in: %s", body)
			}
		}},
		{"GET", "/api/config-routes", func(t *testing.T, body string) {
			if !strings.Contains(body, `"routing"`) {
				t.Errorf("expected 'routing' key in: %s", body)
			}
		}},
		{"GET", "/api/plugins", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/providers", func(t *testing.T, body string) {
			if !strings.Contains(body, `"providers"`) {
				t.Errorf("expected 'providers' key in: %s", body)
			}
		}},
		{"GET", "/api/conversations", func(t *testing.T, body string) {
			if !strings.Contains(body, `"conversations"`) {
				t.Errorf("expected 'conversations' key in: %s", body)
			}
		}},
		{"GET", "/api/logs", func(t *testing.T, body string) {
			if !strings.Contains(body, `"logs"`) {
				t.Errorf("expected 'logs' key in: %s", body)
			}
		}},
		{"GET", "/api/tools/list", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/children", func(t *testing.T, body string) {
			// Unknown endpoints must return an error (404 semantics) instead of
			// a fake 200 success with an empty data object.
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if v["status"] != "error" {
				t.Errorf("expected error status for unknown endpoint, got: %s", body)
			}
			if _, ok := v["data"]; ok {
				t.Errorf("unexpected data key for unknown endpoint: %s", body)
			}
		}},
		{"GET", "/api/personas", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/mcp/servers", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"].([]interface{}); !ok {
				t.Errorf("expected data to be an array in: %s", body)
			}
		}},
		{"GET", "/api/commands", func(t *testing.T, body string) {
			if !strings.Contains(body, `"items"`) {
				t.Errorf("expected 'items' key in: %s", body)
			}
		}},
		{"GET", "/api/files", func(t *testing.T, body string) {
			if !strings.Contains(body, `"files"`) && !strings.Contains(body, `"entries"`) {
				t.Errorf("expected 'files' or 'entries' key in: %s", body)
			}
		}},
		{"GET", "/api/backups", func(t *testing.T, body string) {
			if !strings.Contains(body, `"items"`) {
				t.Errorf("expected 'items' key in: %s", body)
			}
		}},
		{"GET", "/api/provider-sources", func(t *testing.T, body string) {
			if !strings.Contains(body, `"provider_sources"`) {
				t.Errorf("expected 'provider_sources' key in: %s", body)
			}
		}},
		{"GET", "/api/plugin-sources", func(t *testing.T, body string) {
			if !strings.Contains(body, `"sources"`) {
				t.Errorf("expected 'sources' key in: %s", body)
			}
		}},
		{"GET", "/api/skills", func(t *testing.T, body string) {
			if !strings.Contains(body, `"skills"`) {
				t.Errorf("expected 'skills' key in: %s", body)
			}
		}},
		{"GET", "/api/mcp", func(t *testing.T, body string) {
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			if _, ok := v["data"]; !ok {
				t.Errorf("missing 'data' key in: %s", body)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req := authedRequest(t, s, tt.method, tt.path, nil)
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, req)
			tt.check(t, w.Body.String())
		})
	}
}

// TestPluginSourceRoutes guards that the per-plugin source/update/reload routes
// are dispatched to their handlers instead of falling into the uninstall branch.
func TestPluginSourceRoutes(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	// Bind-source on a non-existent subprocess plugin: must NOT uninstall, and
	// must return an error rather than silently succeeding.
	req := authedRequest(t, s, http.MethodPost,
		"/api/v1/plugins/nonexistent/source",
		strings.NewReader(`{"install_method":"market","market_plugin_id":"a/b"}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Errorf("bind-source on unknown plugin should error, got: %s", w.Body.String())
	}

	// Single-plugin update route must not uninstall.
	req = authedRequest(t, s, http.MethodPost, "/api/v1/plugins/nonexistent/update", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	v = map[string]interface{}{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Errorf("update on unknown plugin should error, got: %s", w.Body.String())
	}

	// Batch update route with empty body.
	req = authedRequest(t, s, http.MethodPost, "/api/v1/plugins/update", nil)
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	v = map[string]interface{}{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Errorf("batch update with no ids should error, got: %s", w.Body.String())
	}
}

// TestMetadataOrderPreserved verifies the served config metadata keeps the
// Python CONFIG_METADATA_3 group/section order instead of alphabetical order.
func TestMetadataOrderPreserved(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	for _, path := range []string{"/api/system-config/schema", "/api/system-config/runtime"} {
		req := authedRequest(t, s, http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		var v map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s invalid json: %v", path, err)
		}
		data, ok := v["data"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s no data: %s", path, w.Body.String())
		}
		md, ok := data["metadata"].(map[string]interface{})
		if !ok {
			t.Fatalf("%s no metadata: %s", path, w.Body.String())
		}
		// Re-derive order from raw JSON key sequence.
		order := rawKeys(t, w.Body.Bytes(), data, "metadata")
		if len(order) == 0 {
			t.Fatalf("%s metadata empty", path)
		}
		// ai_group must come before platform_group, which must come before
		// plugin_group / ext_group (Python order), and provider_group last.
		pos := map[string]int{}
		for i, k := range order {
			pos[k] = i
		}
		if pos["ai_group"] > pos["platform_group"] {
			t.Errorf("%s: ai_group(%d) should be before platform_group(%d): %v", path, pos["ai_group"], pos["platform_group"], order)
		}
		if pos["platform_group"] > pos["plugin_group"] {
			t.Errorf("%s: platform_group(%d) should be before plugin_group(%d): %v", path, pos["platform_group"], pos["plugin_group"], order)
		}
		if _, ok := pos["provider_group"]; ok {
			if pos["provider_group"] < pos["ext_group"] {
				t.Errorf("%s: provider_group(%d) should be last: %v", path, pos["provider_group"], order)
			}
		}
		_ = md
	}

	// Section order within ai_group must follow Python (agent_runner first).
	md := s.getConfigMetadata()
	ai, _ := md.Get("ai_group")
	aiMap, _ := config.GetOrderedJSON(ai)
	if aiMap == nil {
		t.Fatal("ai_group missing")
	}
	aiMeta, _ := aiMap.Get("metadata")
	aiMetaMap, _ := config.GetOrderedJSON(aiMeta)
	if aiMetaMap == nil {
		t.Fatal("ai_group.metadata missing")
	}
	secs := aiMetaMap.Keys()
	if secs[0] != "agent_runner" {
		t.Errorf("ai_group first section = %q, want agent_runner: %v", secs[0], secs)
	}
	if secs[len(secs)-1] != "others" {
		t.Errorf("ai_group last section = %q, want others: %v", secs[len(secs)-1], secs)
	}
}

// rawKeys reconstructs the ordered top-level key list of the JSON object at
// objPath within the full response body (writeJSON emits OrderedJSON in order).
func rawKeys(t *testing.T, body []byte, obj map[string]interface{}, key string) []string {
	t.Helper()
	var root map[string]interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	ordered, err := config.ParseOrderedJSON([]byte(body))
	if err != nil {
		t.Fatalf("ordered parse: %v", err)
	}
	dataO, _ := ordered.Get("data")
	dataOM, ok := config.GetOrderedJSON(dataO)
	if !ok {
		return nil
	}
	mdO, ok := dataOM.Get(key)
	if !ok {
		return nil
	}
	mdOM, ok := config.GetOrderedJSON(mdO)
	if !ok {
		return nil
	}
	return mdOM.Keys()
}

// TestAPIAuthGate guards the global /api authentication: unauthenticated
// requests are rejected with 401, and the auth/setup endpoint cannot be used to
// hijack an account whose password change is no longer required.
func TestAPIAuthGate(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	// An unauthenticated request to a sensitive endpoint must be rejected.
	req := httptest.NewRequest(http.MethodGet, "/api/system-config", nil)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/system-config: want 401, got %d", w.Code)
	}

	// Public auth endpoints remain reachable without a token (rejected by the
	// credential check, not the auth gate).
	req = httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"astrbot","password":"wrong"}`))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), "未认证") {
		t.Fatalf("login should be reachable without a token, got %d (%s)", w.Code, w.Body.String())
	}

	// Once a password is set (change no longer required), auth/setup must not
	// reset credentials without the old password.
	s.auth.SetPassword("CurrentPw123")
	req = httptest.NewRequest(http.MethodPost, "/api/auth/setup",
		strings.NewReader(`{"password":"evil","confirm_password":"evil"}`))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("setup without old password after onboarding: want 401, got %d (%s)", w.Code, w.Body.String())
	}
	if s.auth.VerifyPassword("evil") {
		t.Fatal("setup must not change the password without the old password")
	}

	// With the correct old password (and an authenticated session) it is allowed.
	req = authedRequest(t, s, http.MethodPost, "/api/auth/setup",
		strings.NewReader(`{"password":"NewPw123","confirm_password":"NewPw123","old_password":"CurrentPw123"}`))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("setup with correct old password: want 200, got %d (%s)", w.Code, w.Body.String())
	}
	if !s.auth.VerifyPassword("NewPw123") {
		t.Fatal("setup with correct old password should update the password")
	}
}

// TestConfigSnapshotRedactsSecrets verifies getConfigSnapshot never leaks the
// plaintext dashboard password, password hash or JWT signing secret.
func TestConfigSnapshotRedactsSecrets(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()
	s.auth.SetPassword("SuperSecret123")

	cfg := s.getConfigSnapshot()
	dash, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		t.Fatal("missing dashboard section")
	}
	for _, key := range []string{"password", "pbkdf2_password", "jwt_secret"} {
		if v, present := dash[key]; present {
			t.Errorf("config snapshot must not expose %q, got %v", key, v)
		}
	}
	if s.auth.JWTSecret() == "" {
		t.Fatal("JWT secret must still exist server-side")
	}
}

// TestJWTLogoutRevokesToken verifies that logging out blacklists the JWT so
// the same token is rejected afterwards (server-side revocation), while the
// logout endpoint itself stays reachable.
func TestJWTLogoutRevokesToken(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()

	// A valid token passes the auth gate.
	tok, err := s.auth.IssueToken(s.auth.Username())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	if !s.auth.IsAuthenticated(tok) {
		t.Fatal("token should be authenticated before logout")
	}

	// Logout endpoint is reachable (public whitelist) and revokes the token.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("logout should be allowed, got %d", w.Code)
	}
	if s.auth.IsAuthenticated(tok) {
		t.Fatal("token must be rejected after logout (jti blacklist)")
	}
}

// TestLoginRateLimit verifies the token-bucket login throttle rejects rapid
// attempts after the burst is exhausted.
func TestLoginRateLimit(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()
	s.auth.SetPassword("CorrectPw123")

	limiter := s.loginLimiter
	if limiter == nil {
		t.Fatal("login limiter not initialized")
	}
	// burst=3: first three attempts pass, the fourth is throttled.
	if !limiter.Allow("1.2.3.4", 1.0, 3.0) || !limiter.Allow("1.2.3.4", 1.0, 3.0) || !limiter.Allow("1.2.3.4", 1.0, 3.0) {
		t.Fatal("first three attempts within burst should be allowed")
	}
	if limiter.Allow("1.2.3.4", 1.0, 3.0) {
		t.Fatal("fourth attempt beyond burst should be throttled")
	}
	// A different IP is unaffected.
	if !limiter.Allow("5.6.7.8", 1.0, 3.0) {
		t.Fatal("a different client should not be throttled")
	}
}

// TestSetupChangesPasswordToHashAndOverridesUsername: 前端改密（setup 端点）
// 必须把新密码转为哈希写入配置，且显式填写的用户名覆盖配置文件中的 username。
func TestSetupChangesPasswordToHashAndOverridesUsername(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	s := NewServer(0, cfgPath)
	defer s.Stop()

	// 重置等待状态：磁盘只有 password=哈希 + change_required=true，无 username。
	s.auth.SetUsername("")
	pm := s.auth
	if pm.Username() != "" {
		// SetUsername 忽略空值，改用直接构造等待状态。
		pm.mu.Lock()
		pm.username = ""
		pm.mu.Unlock()
	}
	if pm.Username() != "" {
		t.Fatal("failed to set up waiting state")
	}

	// 通过 setup 端点显式填用户名 + 密码（首次设置，无需旧密码）。
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup",
		strings.NewReader(`{"username":"myadmin","password":"HashMe@123","confirm_password":"HashMe@123"}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("setup: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	// 1) 磁盘 password 必须是 PBKDF2 哈希，绝不是明文。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	d, _ := cfg["dashboard"].(map[string]interface{})
	if h, _ := d["password"].(string); !isPBKDF2Hash(h) {
		t.Fatalf("dashboard.password must be a PBKDF2 hash after setup, got %q", h)
	}
	if h, _ := d["password"].(string); h == "HashMe@123" {
		t.Fatal("plaintext password must never be persisted")
	}
	if _, exists := d["pbkdf2_password"]; exists {
		t.Fatal("legacy pbkdf2_password key must not be written")
	}
	// 2) 显式填写的用户名覆盖（写入）配置文件。
	if got, _ := d["username"].(string); got != "myadmin" {
		t.Fatalf("username must be written to config, got %q", got)
	}

	// 3) 登录用新凭据。
	if !s.auth.VerifyPassword("HashMe@123") {
		t.Fatal("new password must verify")
	}
	if s.auth.Username() != "myadmin" {
		t.Fatalf("in-memory username = %q, want myadmin", s.auth.Username())
	}

	// 4) 重启后凭据完整，不重置。
	pm2 := NewPasswordManager(cfgPath)
	if pm2.Username() != "myadmin" {
		t.Fatalf("reloaded username = %q, want myadmin", pm2.Username())
	}
	if pm2.PasswordChangeRequired() {
		t.Fatal("after setup, change must no longer be required")
	}
	if !pm2.VerifyPassword("HashMe@123") {
		t.Fatal("reloaded manager must verify the new password")
	}
}

// TestAccountEditOverridesUsername: 前端改密同时显式填新用户名时，配置中的
// username 被覆盖（非首次设置路径）。
func TestAccountEditOverridesUsername(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	s := NewServer(0, cfgPath)
	defer s.Stop()

	s.auth.SetPassword("OldPw@123")
	s.auth.SetUsername("old_admin")
	if got, _ := readDashboardSection(t, cfgPath)["username"].(string); got != "old_admin" {
		t.Fatalf("setup username = %q, want old_admin", got)
	}

	req := authedRequest(t, s, http.MethodPost, "/api/auth/account/edit",
		strings.NewReader(`{"new_username":"new_admin","new_password":"NewPw@456","password":"OldPw@123"}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("account edit: want 200, got %d (%s)", w.Code, w.Body.String())
	}

	d := readDashboardSection(t, cfgPath)
	if got, _ := d["username"].(string); got != "new_admin" {
		t.Fatalf("username must be overridden in config, got %q", got)
	}
	if h, _ := d["password"].(string); !isPBKDF2Hash(h) {
		t.Fatalf("password must be a PBKDF2 hash after edit, got %q", h)
	}
	if s.auth.VerifyPassword("OldPw@123") {
		t.Fatal("old password must no longer verify")
	}
	if !s.auth.VerifyPassword("NewPw@456") {
		t.Fatal("new password must verify")
	}
}
