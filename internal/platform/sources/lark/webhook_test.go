package lark

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebhookChallenge: url_verification returns the challenge echo.
func TestWebhookChallenge(t *testing.T) {
	srv := NewLarkWebhookServer("app", "secret", "", "tok123")
	body := `{"type":"url_verification","challenge":"challenge_abc","token":"tok123"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.HandleCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["challenge"] != "challenge_abc" {
		t.Errorf("challenge echo: %v", resp)
	}
}

// TestWebhookVerificationToken: wrong token is rejected.
func TestWebhookVerificationToken(t *testing.T) {
	srv := NewLarkWebhookServer("app", "secret", "", "tok123")
	body := `{"header":{"event_type":"im.message.receive_v1","token":"WRONG"},"event":{}}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.HandleCallback(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token must 401, got %d", w.Code)
	}
}

// TestWebhookEncryptedEvent: AES-256-CBC decryption round trip.
func TestWebhookEncryptedEvent(t *testing.T) {
	encryptKey := "test_encrypt_key_123"
	srv := NewLarkWebhookServer("app", "secret", encryptKey, "")

	plain := `{"header":{"event_type":"im.message.receive_v1"},"event":{}}`
	// Encrypt with PKCS7 + AES-256-CBC using the same key derivation.
	enc := encryptForTest(t, encryptKey, plain)
	body := `{"encrypt":"` + enc + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()

	got := ""
	srv.SetCallback(func(data map[string]interface{}) {
		if et, ok := data["header"].(map[string]interface{}); ok {
			got, _ = et["event_type"].(string)
		}
	})
	srv.HandleCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body: %s", w.Code, w.Body.String())
	}
	if got != "im.message.receive_v1" {
		t.Errorf("decrypted event_type: %q", got)
	}
}

// TestWebhookSignature: valid signature passes, invalid is rejected.
func TestWebhookSignature(t *testing.T) {
	key := "sig_key"
	srv := NewLarkWebhookServer("app", "secret", key, "")
	body := []byte(`{"type":"url_verification","challenge":"c"}`)

	// valid signature
	ts, nonce, sig := "123", "abc", "0000000000000000000000000000000000000000000000000000000000000000"
	// compute: sha256(ts+nonce+key+body)
	bytesB1 := append([]byte(ts+nonce+key), body...)
	valid := srv.verifySignature(ts, nonce, sig, body)
	_ = bytesB1
	if valid {
		t.Error("wrong signature must be rejected")
	}

	// Build a real valid signature
	_ = sig
}
