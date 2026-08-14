package lark

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
)

// LarkWebhookServer implements the Lark webhook event subscription:
// challenge verification, AES-256-CBC decryption (lark_encrypt_key) and
// SHA256 signature verification. Ported 1:1 from lark/server.py.
type LarkWebhookServer struct {
	appID       string
	appSecret   string
	encryptKey  string
	verifyToken string
	cipher      cipher.Block
	callback    func(map[string]interface{})
}

// NewLarkWebhookServer creates a webhook server.
func NewLarkWebhookServer(appID, appSecret, encryptKey, verifyToken string) *LarkWebhookServer {
	s := &LarkWebhookServer{
		appID:       appID,
		appSecret:   appSecret,
		encryptKey:  encryptKey,
		verifyToken: verifyToken,
	}
	if encryptKey != "" {
		key := sha256.Sum256([]byte(encryptKey))
		s.cipher, _ = aes.NewCipher(key[:])
	}
	return s
}

// SetCallback registers the event callback.
func (s *LarkWebhookServer) SetCallback(cb func(map[string]interface{})) {
	s.callback = cb
}

// verifySignature checks X-Lark-Signature (timestamp + nonce + encrypt_key + body).
func (s *LarkWebhookServer) verifySignature(timestamp, nonce, signature string, body []byte) bool {
	bytesB1 := append([]byte(timestamp+nonce+s.encryptKey), body...)
	sum := sha256.Sum256(bytesB1)
	return hex.EncodeToString(sum[:]) == signature
}

// decryptEvent AES-256-CBC decrypts an encrypted event payload.
func (s *LarkWebhookServer) decryptEvent(encrypted string) (map[string]interface{}, error) {
	if s.cipher == nil {
		return nil, errNoEncryptKey
	}
	enc, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, err
	}
	if len(enc) < aes.BlockSize {
		return nil, errBadPadding
	}
	iv := enc[:aes.BlockSize]
	ct := enc[aes.BlockSize:]
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(s.cipher, iv).CryptBlocks(pt, ct)
	// PKCS7 unpad
	padLen := int(pt[len(pt)-1])
	if padLen <= 0 || padLen > aes.BlockSize || padLen > len(pt) {
		return nil, errBadPadding
	}
	pt = pt[:len(pt)-padLen]
	var data map[string]interface{}
	if err := json.Unmarshal(pt, &data); err != nil {
		return nil, err
	}
	return data, nil
}

var (
	errNoEncryptKey = errWebhook("未配置 encrypt_key，无法解密事件")
	errBadPadding   = errWebhook("事件解密失败")
)

type errWebhook string

func (e errWebhook) Error() string { return string(e) }

// HandleCallback handles the unified webhook callback request
// (mirrors lark/server.py handle_callback).
func (s *LarkWebhookServer) HandleCallback(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeWebhookError(w, http.StatusBadRequest, "Invalid request body")
		return
	}
	var eventData map[string]interface{}
	if err := json.Unmarshal(body, &eventData); err != nil || eventData == nil {
		writeWebhookError(w, http.StatusBadRequest, "Invalid JSON")
		return
	}

	// Signature verification when encrypt_key is configured.
	if s.encryptKey != "" {
		timestamp := r.Header.Get("X-Lark-Request-Timestamp")
		nonce := r.Header.Get("X-Lark-Request-Nonce")
		signature := r.Header.Get("X-Lark-Signature")
		if timestamp != "" && nonce != "" && signature != "" {
			if !s.verifySignature(timestamp, nonce, signature, body) {
				writeWebhookError(w, http.StatusUnauthorized, "Invalid signature")
				return
			}
		}
	}

	// Encrypted events.
	if enc, ok := eventData["encrypt"].(string); ok && enc != "" {
		dec, err := s.decryptEvent(enc)
		if err != nil {
			writeWebhookError(w, http.StatusBadRequest, "Decryption failed")
			return
		}
		eventData = dec
	}

	// Verification token check.
	if s.verifyToken != "" {
		token := ""
		if header, ok := eventData["header"].(map[string]interface{}); ok {
			token, _ = header["token"].(string)
		}
		if token == "" {
			token, _ = eventData["token"].(string)
		}
		if token != s.verifyToken {
			writeWebhookError(w, http.StatusUnauthorized, "Invalid verification token")
			return
		}
	}

	// URL verification (challenge).
	if eventType, _ := eventData["type"].(string); eventType == "url_verification" {
		challenge, _ := eventData["challenge"].(string)
		logger.Info("收到飞书 challenge 验证请求: %s", challenge)
		writeJSON(w, map[string]interface{}{"challenge": challenge})
		return
	}

	if s.callback != nil {
		s.callback(eventData)
	}
	writeJSON(w, map[string]interface{}{})
}

func writeJSON(w http.ResponseWriter, data map[string]interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(data)
}

func writeWebhookError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": msg})
}
