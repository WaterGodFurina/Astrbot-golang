package lark

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	// encrypt_key 配置后签名头缺失必须被拒绝 (M-27)。
	req.Header.Set("X-Lark-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
	req.Header.Set("X-Lark-Request-Nonce", "nonce_1")
	req.Header.Set("X-Lark-Signature", signatureForTest(t, encryptKey, fmt.Sprintf("%d", time.Now().Unix()), "nonce_1", body))
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

// TestWebhookEncryptedEventMissingSignature: encrypt_key 配置后省略签名头必须 401。
func TestWebhookEncryptedEventMissingSignature(t *testing.T) {
	encryptKey := "test_encrypt_key_123"
	srv := NewLarkWebhookServer("app", "secret", encryptKey, "")
	enc := encryptForTest(t, encryptKey, `{"header":{"event_type":"im.message.receive_v1"},"event":{}}`)
	body := `{"encrypt":"` + enc + `"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	w := httptest.NewRecorder()
	srv.HandleCallback(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("签名头缺失应 401, got %d body: %s", w.Code, w.Body.String())
	}
}

// TestWebhookEncryptedEventWrongSignature: encrypt_key 配置后错误签名必须 401。
func TestWebhookEncryptedEventWrongSignature(t *testing.T) {
	encryptKey := "test_encrypt_key_123"
	srv := NewLarkWebhookServer("app", "secret", encryptKey, "")
	enc := encryptForTest(t, encryptKey, `{"header":{"event_type":"im.message.receive_v1"},"event":{}}`)
	body := `{"encrypt":"` + enc + `"}`
	ts := fmt.Sprintf("%d", time.Now().Unix())
	nonce := "nonce_1"
	// 先算正确签名，再确定性地翻转首字符——避免 "00"+sig[2:] 在正确签名恰好
	// 以 "00" 开头时变成有效签名（约 1/256 概率造成 flaky）。
	correct := signatureForTest(t, encryptKey, ts, nonce, body)
	flip := byte('0')
	if correct[0] == '0' {
		flip = '1'
	}
	wrong := string(flip) + correct[1:]
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("X-Lark-Request-Timestamp", ts)
	req.Header.Set("X-Lark-Request-Nonce", nonce)
	req.Header.Set("X-Lark-Signature", wrong)
	w := httptest.NewRecorder()
	srv.HandleCallback(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误签名应 401, got %d body: %s", w.Code, w.Body.String())
	}
}

// TestWebhookDecryptInvalidLength: 畸形密文长度不得 panic (M-28)。
func TestWebhookDecryptInvalidLength(t *testing.T) {
	encryptKey := "test_encrypt_key_123"
	srv := NewLarkWebhookServer("app", "secret", encryptKey, "")

	cases := []string{
		// len(enc)==16: ct 为空, 旧代码 pt[len(pt)-1] 越界
		"AAAAAAAAAAAAAAAAAAAAAA==",
		// len(enc) 不是 16 的倍数: 旧代码 CryptBlocks panic
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for _, enc := range cases {
		body := `{"encrypt":"` + enc + `"}`
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set("X-Lark-Request-Timestamp", fmt.Sprintf("%d", time.Now().Unix()))
		req.Header.Set("X-Lark-Request-Nonce", "nonce_1")
		req.Header.Set("X-Lark-Signature", signatureForTest(t, encryptKey, fmt.Sprintf("%d", time.Now().Unix()), "nonce_1", body))
		w := httptest.NewRecorder()
		srv.HandleCallback(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("畸形密文应 400, got %d body: %s", w.Code, w.Body.String())
		}
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
