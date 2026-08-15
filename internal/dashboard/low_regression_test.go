// Regression tests for the L-01 .. L-13 low-severity dashboard fixes.
package dashboard

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/gorilla/websocket"
)

// ── L-01: /stat/start-time must return process start time ───

func TestStartTimeReturnsProcessStart(t *testing.T) {
	s := &Server{startTime: time.Now().Add(-2 * time.Hour)}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/stat/start-time", nil)
	s.handleStat(w, r, []string{"start-time"})
	var v struct {
		Data struct {
			StartTime int64 `json:"start_time"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if v.Data.StartTime != s.startTime.Unix() {
		t.Fatalf("start_time = %d, want %d", v.Data.StartTime, s.startTime.Unix())
	}
}

// ── L-02: auth == nil defensive branches ─────────────────────

func TestHandleCheckNilAuthDoesNotPanic(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/auth/check", nil)
	s.handleCheck(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	data := v["data"].(map[string]interface{})
	if data["loggedin"] != false {
		t.Errorf("loggedin = %v, want false", data["loggedin"])
	}
	if u, _ := data["username"].(string); u != "" {
		t.Errorf("username = %q, want empty", u)
	}
}

func TestHandleLoginNilAuthReturnsError(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"u","password":"p"}`))
	s.handleLogin(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (no guest dead-ticket)", w.Code)
	}
}

// ── L-03: chat_sessions.json must be written 0600 ────────────

func TestChatStoreFilePermission(t *testing.T) {
	cs := newChatStore(t.TempDir())
	if _, err := cs.createSession("test"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cs.path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Fatalf("chat_sessions.json perm = %o, want 600", perm)
	}
}

// ── L-04: clientIP uses the rightmost trusted XFF hop ────────

func TestClientIPRightmostXFF(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 7.7.7.7")
	if got := clientIP(r, true); got != "7.7.7.7" {
		t.Fatalf("trustProxy clientIP = %q, want 7.7.7.7 (rightmost)", got)
	}
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 7.7.7.7, ")
	if got := clientIP(r, true); got != "7.7.7.7" {
		t.Fatalf("trailing comma clientIP = %q, want 7.7.7.7", got)
	}
	r.Header.Set("X-Forwarded-For", "6.6.6.6")
	if got := clientIP(r, false); got != "10.0.0.5" {
		t.Fatalf("no-trust clientIP = %q, want remote addr 10.0.0.5", got)
	}
}

// ── L-05: base64URLSafe must produce URL-safe base64, not hex ─

func TestBase64URLSafeIsBase64(t *testing.T) {
	got := base64URLSafe(16)
	b, err := base64.URLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("base64URLSafe(%q) is not valid URL-safe base64: %v", got, err)
	}
	if len(b) != 16 {
		t.Fatalf("decoded length = %d, want 16", len(b))
	}
}

// ── L-06: persona store persists through the atomic writer ───

func TestPersonaStoreAtomicSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	ps := newPersonaStore(dir)
	if err := ps.upsertPersona(map[string]interface{}{"name": "p1"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(ps.path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "p1") {
		t.Fatalf("persona not persisted: %s", string(data))
	}
	ps2 := newPersonaStore(dir)
	if got := ps2.listPersonas(nil); len(got) != 1 {
		t.Fatalf("reload: expected 1 persona, got %d", len(got))
	}
}

// ── L-08: system-config PUT must reject bad bodies ───────────

func TestSystemConfigPutRejectsBadBody(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()
	cases := []string{
		"{not-json",
		"null",
		"",
	}
	for _, body := range cases {
		req := authedRequest(t, s, http.MethodPut, "/api/system-config", strings.NewReader(body))
		w := httptest.NewRecorder()
		s.mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
	}
}

// ── L-09: legacy /api/config set|update must not be a fake no-op

func TestLegacyConfigSetNotImplemented(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()
	req := authedRequest(t, s, http.MethodPost, "/api/config/set", strings.NewReader(`{"ai_group":{"enable":false}}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

// ── L-10: personas move/reorder must surface save errors ─────

func TestPersonaMoveReorderStillSucceeds(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()
	if err := s.personas.upsertPersona(map[string]interface{}{"persona_id": "p1", "name": "x"}); err != nil {
		t.Fatal(err)
	}
	req := authedRequest(t, s, http.MethodPost, "/api/personas/move", strings.NewReader(`{"persona_id":"p1","folder_id":""}`))
	w := httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("move status = %d, want 200", w.Code)
	}
	req = authedRequest(t, s, http.MethodPost, "/api/personas/reorder", strings.NewReader(`{"items":[{"id":"p1","type":"persona","sort_order":3}]}`))
	w = httptest.NewRecorder()
	s.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reorder status = %d, want 200", w.Code)
	}
	if got := s.personas.getPersona("p1"); got["sort_order"] != 3.0 {
		t.Fatalf("sort_order = %v, want 3", got["sort_order"])
	}
}

// ── L-11: server must send periodic WebSocket pings ──────────

func TestWebSocketServerSendsPing(t *testing.T) {
	old := wsPingInterval
	wsPingInterval = 150 * time.Millisecond
	defer func() { wsPingInterval = old }()

	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()
	ts := httptest.NewServer(s.mux)
	defer ts.Close()

	tok, err := s.auth.IssueToken(s.auth.Username())
	if err != nil {
		t.Fatal(err)
	}
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/v1/unified-chat/ws?token=" + url.QueryEscape(tok)
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	received := make(chan struct{}, 1)
	conn.SetPingHandler(func(appData string) error {
		select {
		case received <- struct{}{}:
		default:
		}
		return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
	})
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	select {
	case <-received:
	case <-time.After(5 * time.Second):
		t.Fatal("no server ping received within 5s")
	}
}

// ── L-12: same-session concurrent runs must not cross-talk ───

func TestChatStreamAdapterRoutesToEarliestActiveRun(t *testing.T) {
	a := newChatStreamAdapter()
	ch1, seq1 := a.subscribe("s1")
	ch2, seq2 := a.subscribe("s1")
	if seq1 >= seq2 {
		t.Fatalf("subscription order broken: seq1=%d seq2=%d", seq1, seq2)
	}
	if err := a.Send("s1", message.PlainChain("reply-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch1:
		if chainPlainText(got) != "reply-1" {
			t.Fatalf("run1 got %q, want reply-1", chainPlainText(got))
		}
	default:
		t.Fatal("earliest subscriber did not receive its own reply")
	}
	select {
	case <-ch2:
		t.Fatal("run2 subscriber leaked run1's reply")
	default:
	}

	// Run1 completes and unsubscribes; run2's reply must only reach ch2.
	a.unsubscribe("s1", seq1, ch1)
	if err := a.Send("s1", message.PlainChain("reply-2")); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-ch2:
		if chainPlainText(got) != "reply-2" {
			t.Fatalf("run2 got %q, want reply-2", chainPlainText(got))
		}
	default:
		t.Fatal("run2 subscriber did not receive its own reply")
	}
	select {
	case <-ch1:
		t.Fatal("unsubscribed run1 channel received a reply")
	default:
	}
}

// ── L-13: kbTasks store KBID and tolerate nil records ────────

func TestRecordKBTaskStoresKBIDAndNilSafe(t *testing.T) {
	s := &Server{}
	s.kbTasks = make(map[string]*kbUploadTask)
	s.recordKBTask(nil)
	s.recordKBTask(&kbUploadTask{TaskID: "t1", KBID: "kb1", Status: "processing"})
	got := s.getKBTask("t1")
	if got == nil {
		t.Fatal("task t1 not stored")
	}
	if got.KBID != "kb1" {
		t.Fatalf("KBID = %q, want kb1", got.KBID)
	}
}

// ── handleChatSessions "messages" GET must return real history ──

func TestChatSessionsMessagesReturnsHistory(t *testing.T) {
	cs := newChatStore(t.TempDir())
	sess, err := cs.createSession("test")
	if err != nil {
		t.Fatal(err)
	}
	cs.appendMessage(sess.SessionID, map[string]interface{}{"role": "user", "content": "hello"})
	cs.appendMessage(sess.SessionID, map[string]interface{}{"role": "assistant", "content": "world"})

	s := &Server{chat: cs}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/chat/sessions/"+sess.SessionID+"/messages", nil)
	s.handleChatSessions(w, r, []string{sess.SessionID, "messages"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Data struct {
			Messages []map[string]interface{} `json:"messages"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (was a stub returning empty)", len(resp.Data.Messages))
	}
	if resp.Data.Messages[0]["content"] != "hello" {
		t.Errorf("first message content = %v", resp.Data.Messages[0]["content"])
	}
}

// ── skillFilePath: ".." inside a name is legal; real traversal stays blocked ──

func TestSkillFilePathAllowsDotsInsideName(t *testing.T) {
	// "a..b" is a legitimate single-segment directory name, not traversal.
	p, err := skillFilePath("a..b", "file.txt")
	if err != nil {
		t.Fatalf("skillFilePath(a..b) should be allowed: %v", err)
	}
	if !strings.HasSuffix(p, string(os.PathSeparator)+"a..b"+string(os.PathSeparator)+"file.txt") {
		t.Errorf("unexpected path: %s", p)
	}

	// Actual traversal vectors remain blocked: skillName separators / exact
	// ".." are rejected, and relPath ".." is neutralized by Clean + the
	// containment check so the result never escapes the skill root.
	if _, err := skillFilePath("..", "x"); err == nil {
		t.Error("exact '..' must be rejected")
	}
	if _, err := skillFilePath("../evil", "x"); err == nil {
		t.Error("separator traversal name must be rejected")
	}
	root, _ := filepath.Abs(filepath.Join("data", "skills", "ok"))
	p2, err := skillFilePath("ok", "../../etc/passwd")
	if err != nil {
		t.Fatalf("relPath '..' should be neutralized, not error: %v", err)
	}
	if !strings.HasPrefix(p2, root+string(os.PathSeparator)) {
		t.Errorf("relPath traversal escaped skill root: %s", p2)
	}
}
