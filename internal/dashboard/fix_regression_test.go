// Regression tests for M-01 / M-02 / M-03 / M-05.
package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
)

// ── M-01: nil map body must not panic ─────────────────────────

func TestPersonaStoreNilMapUpsert(t *testing.T) {
	ps := newPersonaStore(filepath.Join(t.TempDir(), "personas.json"))
	if err := ps.upsertPersona(nil); err != nil {
		t.Fatalf("upsertPersona(nil): %v", err)
	}
	if err := ps.upsertFolder(nil); err != nil {
		t.Fatalf("upsertFolder(nil): %v", err)
	}
	if got := ps.listPersonas(nil); len(got) != 1 {
		t.Errorf("expected 1 persona after nil upsert, got %d", len(got))
	}
	if got := ps.listFolders(nil); len(got) != 1 {
		t.Errorf("expected 1 folder after nil upsert, got %d", len(got))
	}
}

func TestNilBodyHandlersDoNotPanic(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/providers"},
		{http.MethodPost, "/api/personas"},
		{http.MethodPut, "/api/personas/by-id?persona_id=test"},
		{http.MethodPost, "/api/persona-folders"},
		{http.MethodPut, "/api/persona-folders/folder1"},
	}
	for _, c := range cases {
		req := authedRequest(t, s, c.method, c.path, nil)
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Body.Len() == 0 {
			t.Errorf("%s %s: empty response body", c.method, c.path)
			continue
		}
		var v map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Errorf("%s %s: invalid json response: %v", c.method, c.path, err)
		}
		if st, _ := v["status"].(string); st == "" {
			t.Errorf("%s %s: response missing status: %s", c.method, c.path, w.Body.String())
		}
	}
}

// ── M-02: PUT /conversations/{id}/messages must replace history ─

func TestConversationMessagesPutReplacesHistory(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	mgr := conversation.NewManager(nil)
	s.conversationMgr = mgr
	conv := mgr.NewConversation("test:group:g1", "test")
	mgr.AppendHistory("test:group:g1", "user", "old msg")
	if got := len(mgr.FindByCID(conv.CID).History); got != 1 {
		t.Fatalf("setup: expected 1 history entry, got %d", got)
	}

	body := `{"history":[{"role":"user","content":"new msg"},{"role":"assistant","content":"reply"}]}`
	req := authedRequest(t, s, http.MethodPut, "/api/conversations/"+conv.CID+"/messages", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid response: %v (%s)", err, w.Body.String())
	}
	if st, _ := v["status"].(string); st != "ok" {
		t.Fatalf("expected ok, got %s: %s", st, w.Body.String())
	}
	got := mgr.FindByCID(conv.CID)
	if len(got.History) != 2 || got.History[0]["content"] != "new msg" || got.History[1]["content"] != "reply" {
		t.Errorf("history was not replaced: %v", got.History)
	}
}

// ── M-03: outbound SSRF guards ────────────────────────────────

func TestKBImportURLRejectsPrivateIP(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	body := `{"url":"http://169.254.169.254/latest/meta-data/"}`
	req := authedRequest(t, s, http.MethodPost, "/api/knowledge-bases/kb1/documents/import-url", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Fatalf("expected error for private URL, got: %s", w.Body.String())
	}
}

func TestKBImportURLRejectsRedirectToPrivate(t *testing.T) {
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/steal", http.StatusFound)
	}))
	defer redirector.Close()

	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	body := `{"url":"` + redirector.URL + `/doc"}`
	req := authedRequest(t, s, http.MethodPost, "/api/knowledge-bases/kb1/documents/import-url", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "error" {
		t.Fatalf("expected redirect-to-private to be rejected, got: %s", w.Body.String())
	}
}

func TestKBImportURLDownloadsPublic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello kb content"))
	}))
	defer upstream.Close()

	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	body := `{"url":"` + upstream.URL + `/doc.txt"}`
	req := authedRequest(t, s, http.MethodPost, "/api/knowledge-bases/kb1/documents/import-url", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if st, _ := v["status"].(string); st != "ok" {
		t.Fatalf("expected ok for public URL, got: %s", w.Body.String())
	}
}

func TestTestMCPServerRejectsPrivateURL(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()
	if err := s.mcp.upsert("bad", map[string]interface{}{"transport": "sse", "url": "http://169.254.169.254/x"}); err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, s, http.MethodPost, "/api/mcp/servers/bad/test", strings.NewReader(`{"server_name":"bad"}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	data, _ := v["data"].(map[string]interface{})
	if ok, _ := data["success"].(bool); ok {
		t.Fatalf("expected success=false for private MCP url, got: %s", w.Body.String())
	}
}

func TestRegistrationSSRFGuards(t *testing.T) {
	if _, _, err := larkPostRegistration(context.Background(), "http://169.254.169.254/oauth/v1/app/registration", url.Values{}); err == nil {
		t.Error("lark: expected error for private endpoint")
	}
	if _, err := (&Server{}).qqOfficialStart(map[string]interface{}{"qqofficial_bind_host": "10.0.0.1"}); err == nil {
		t.Error("qqofficial: expected error for private host")
	}
	if _, err := (&Server{}).weixinOCRegistration("start", map[string]interface{}{"weixin_oc_base_url": "http://192.168.1.1"}, ""); err == nil {
		t.Error("weixin_oc: expected error for private base url")
	}
}

// ── M-05: zip extraction size limits ──────────────────────────

func TestInstallSkillFromZipRejectsOversizedFile(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 1<<20)
	for i := 0; i < 65; i++ {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := installSkillFromZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), t.TempDir(), "bomb"); err == nil {
		t.Fatal("expected oversized zip to be rejected")
	}
}

func TestInstallSkillFromZipValidPackage(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	entry, err := zw.Create("my-skill/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("---\nname: hello_skill\n---\n# Hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	skillsRoot := t.TempDir()
	name, err := installSkillFromZip(bytes.NewReader(buf.Bytes()), int64(buf.Len()), skillsRoot, "hello_skill")
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if name != "hello_skill" {
		t.Errorf("installed name = %q, want hello_skill", name)
	}
}
