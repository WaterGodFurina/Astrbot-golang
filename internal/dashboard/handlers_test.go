package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/config"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
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
			var v map[string]interface{}
			if err := json.Unmarshal([]byte(body), &v); err != nil {
				t.Fatalf("invalid json: %v", err)
			}
			data, ok := v["data"].(map[string]interface{})
			if !ok {
				t.Fatalf("expected data object, got: %T", v["data"])
			}
			if len(data) == 0 {
				t.Errorf("expected non-empty data object, got: %s", body)
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

// TestGetPluginListNilLegacyManager guards against a typed-nil *plugin.Manager
// (legacy_plugin_mode=false) causing getPluginList to panic on a nil receiver.
func TestGetPluginListNilLegacyManager(t *testing.T) {
	s := NewServer(0, "/tmp/test_pw.json")
	defer s.Stop()
	var nilMgr *plugin.Manager
	s.pluginMgr = nilMgr // typed-nil inside interface{}

	list := s.getPluginList()
	if list == nil {
		t.Fatal("getPluginList should return a non-nil list even with a nil legacy manager")
	}
	byID := s.pluginByID("anything")
	if byID == nil {
		t.Fatal("pluginByID should return a non-nil map with a nil legacy manager")
	}
	if failed := s.pluginFailed(); failed == nil {
		t.Fatal("pluginFailed should return a non-nil map with a nil legacy manager")
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
