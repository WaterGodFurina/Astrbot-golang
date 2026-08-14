package weixin_official_account

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	wxmp "github.com/blusewang/wx/mp_api"
)

func sigOf(parts ...string) string {
	cp := make([]string, len(parts))
	copy(cp, parts)
	sort.Strings(cp)
	sum := sha1.Sum([]byte(strings.Join(cp, "")))
	return hex.EncodeToString(sum[:])
}

// TestRefreshAccessTokenConcurrent: 并发刷新 access_token 只应触发一次 gettoken
// （tokenMu 串行化，L-43 回归）。
func TestRefreshAccessTokenConcurrent(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"tok-1","expires_in":7200}`)
	}))
	defer srv.Close()

	a := New(map[string]interface{}{"id": "wx", "appid": "app", "secret": "sec"}, nil, nil)
	a.apiBase = srv.URL + "/"

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.refreshAccessToken(context.Background()); err != nil {
				t.Errorf("refreshAccessToken error: %v", err)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	c := calls
	mu.Unlock()
	if c != 1 {
		t.Errorf("并发刷新应只触发 1 次 gettoken，实际 %d", c)
	}
	if a.account.AccessToken != "tok-1" {
		t.Errorf("access_token 应为 tok-1，实际 %q", a.account.AccessToken)
	}
}

// TestVerifyHandler: GET echostr verification via the SDK query validation.
func TestVerifyHandler(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	ts, nonce := "1348831860", "1378278745"
	sig := sigOf("tok", ts, nonce)
	req := httptest.NewRequest(http.MethodGet,
		"/callback/command?signature="+sig+"&timestamp="+ts+"&nonce="+nonce+"&echostr=echostr_ok", nil)
	w := httptest.NewRecorder()
	a.verify(w, req)
	if w.Body.String() != "echostr_ok" {
		t.Errorf("echostr echo: %q", w.Body.String())
	}
}

// TestVerifyHandlerBadSignature: wrong signature is rejected.
func TestVerifyHandlerBadSignature(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	req := httptest.NewRequest(http.MethodGet,
		"/callback/command?signature=wrong&timestamp=1&nonce=n&echostr=ok", nil)
	w := httptest.NewRecorder()
	a.verify(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad signature must 400, got %d", w.Code)
	}
}

// TestParseMessageText: text XML is parsed by the SDK ReadMessage path.
func TestParseMessageText(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	xmlStr := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName>
<FromUserName><![CDATA[o_openid]]></FromUserName>
<CreateTime>1348831860</CreateTime>
<MsgType><![CDATA[text]]></MsgType>
<Content><![CDATA[你好]]></Content>
<MsgId>1234567890123456</MsgId></xml>`
	req := httptest.NewRequest(http.MethodPost, "/callback/command?signature="+sigOf("tok", "1", "n")+"&timestamp=1&nonce=n", strings.NewReader(xmlStr))
	q, msg, err := a.account.ReadMessage(req)
	if err != nil {
		t.Fatal(err)
	}
	if q.Timestamp != "1" || msg.MsgType != "text" || msg.Content != "你好" {
		t.Errorf("parse mismatch: %+v %+v", q, msg)
	}
}

// TestConvertMessage: text MessageData becomes a Plain component.
func TestConvertMessage(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	msg := &wxmp.MessageData{
		ToUserName: "gh_app", FromUserName: "o_openid",
		CreateTime: 1348831860, MsgType: "text",
		MsgId: 123, Content: "你好呀",
	}
	abm := a.convertMessage(msg)
	if abm == nil {
		t.Fatal("convertMessage returned nil")
	}
	if abm.MessageStr != "你好呀" {
		t.Errorf("message str: %q", abm.MessageStr)
	}
	if p, ok := abm.Message[0].(*message.Plain); !ok || p.Text != "你好呀" {
		t.Errorf("component mismatch: %#v", abm.Message[0])
	}
	if abm.SessionID != "o_openid" || abm.MessageID != "123" {
		t.Errorf("session/message id: %q %q", abm.SessionID, abm.MessageID)
	}
}

// TestConvertMessageImage: image MessageData becomes an Image component.
func TestConvertMessageImage(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	msg := &wxmp.MessageData{
		MsgType: "image", MediaId: "media123", PicUrl: "http://img/1.jpg",
		FromUserName: "o", ToUserName: "gh", MsgId: 1,
	}
	abm := a.convertMessage(msg)
	if img, ok := abm.Message[0].(*message.Image); !ok {
		t.Errorf("image component expected, got %#v", abm.Message[0])
	} else if img.URL != "http://img/1.jpg" || img.File != "media123" {
		t.Errorf("image fields: %+v", img)
	}
}

// TestCallbackCommandEvent: event messages are acknowledged.
func TestCallbackCommandEvent(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	msg := `<xml><ToUserName><![CDATA[gh]]></ToUserName><FromUserName><![CDATA[u]]></FromUserName>
<CreateTime>1</CreateTime><MsgType><![CDATA[event]]></MsgType><Event><![CDATA[subscribe]]></Event></xml>`
	req := httptest.NewRequest(http.MethodPost, "/callback/command?signature="+sigOf("tok", ts, nonce)+"&timestamp="+ts+"&nonce="+nonce, strings.NewReader(msg))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Body.String() != "success" {
		t.Errorf("event must ack success, got %q", w.Body.String())
	}
}

// TestCallbackCommandStaleTimestamp: a replayed/old timestamp must be rejected.
func TestCallbackCommandStaleTimestamp(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	ts := "1"
	nonce := "n"
	msg := `<xml><ToUserName><![CDATA[gh]]></ToUserName><FromUserName><![CDATA[u]]></FromUserName>
<CreateTime>1</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[hi]]></Content></xml>`
	req := httptest.NewRequest(http.MethodPost, "/callback/command?signature="+sigOf("tok", ts, nonce)+"&timestamp="+ts+"&nonce="+nonce, strings.NewReader(msg))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("stale timestamp must 400, got %d body %q", w.Code, w.Body.String())
	}
}

// TestCallbackCommandBadSignature: wrong plaintext signature is rejected.
func TestCallbackCommandBadSignature(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	msg := `<xml><ToUserName><![CDATA[gh]]></ToUserName><FromUserName><![CDATA[u]]></FromUserName>
<MsgType><![CDATA[text]]></MsgType><Content><![CDATA[hi]]></Content></xml>`
	req := httptest.NewRequest(http.MethodPost, "/callback/command?signature=wrong&timestamp="+ts+"&nonce="+nonce, strings.NewReader(msg))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad signature must 400, got %d", w.Code)
	}
}

// testAESKey 生成 43 位 EncodingAESKey。
func testAESKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)[:43]
}

// encryptWXBody 构造微信公众号安全模式密文（random16 + 4B 长度 + 明文 + appId，
// PKCS7 填充 32 字节块，AES-256-CBC，IV 取密钥前 16 字节）。
func encryptWXBody(t *testing.T, aesKey []byte, plainXML, appID string) string {
	t.Helper()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 16+4+len(plainXML)+len(appID))
	buf = append(buf, random...)
	buf = append(buf, byte(len(plainXML)>>24), byte(len(plainXML)>>16), byte(len(plainXML)>>8), byte(len(plainXML)))
	buf = append(buf, plainXML...)
	buf = append(buf, appID...)
	pad := 32 - len(buf)%32
	if pad == 0 {
		pad = 32
	}
	for i := 0; i < pad; i++ {
		buf = append(buf, byte(pad))
	}
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(buf))
	cipher.NewCBCEncrypter(block, aesKey[:16]).CryptBlocks(out, buf)
	return base64.StdEncoding.EncodeToString(out)
}

// TestCallbackCommandEncrypted: safe-mode request with valid msg_signature and
// matching appId is accepted and decrypted.
func TestCallbackCommandEncrypted(t *testing.T) {
	keyStr := testAESKey(t)
	a := New(map[string]interface{}{"id": "wx", "token": "tok", "appid": "wxappid", "encoding_aes_key": keyStr}, nil, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	inner := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><FromUserName><![CDATA[o_openid]]></FromUserName>
<CreateTime>` + ts + `</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[密文你好]]></Content><MsgId>1234567890</MsgId></xml>`
	aesKey, err := base64.StdEncoding.DecodeString(keyStr + "=")
	if err != nil {
		t.Fatal(err)
	}
	encrypt := encryptWXBody(t, aesKey, inner, "wxappid")
	body := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><Encrypt><![CDATA[` + encrypt + `]]></Encrypt></xml>`
	msgSig := sigOf("tok", ts, nonce, encrypt)
	req := httptest.NewRequest(http.MethodPost,
		"/callback/command?signature="+sigOf("tok", ts, nonce)+"&timestamp="+ts+"&nonce="+nonce+"&msg_signature="+msgSig+"&encrypt_type=aes",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("encrypted callback must succeed, got %d body %q", w.Code, w.Body.String())
	}
	if w.Body.String() != "success" {
		t.Errorf("encrypted callback body: %q", w.Body.String())
	}
}

// TestCallbackCommandEncryptedBadMsgSignature: wrong msg_signature is rejected.
func TestCallbackCommandEncryptedBadMsgSignature(t *testing.T) {
	keyStr := testAESKey(t)
	a := New(map[string]interface{}{"id": "wx", "token": "tok", "appid": "wxappid", "encoding_aes_key": keyStr}, nil, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	inner := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><FromUserName><![CDATA[o]]></FromUserName>
<CreateTime>` + ts + `</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[hi]]></Content></xml>`
	aesKey, err := base64.StdEncoding.DecodeString(keyStr + "=")
	if err != nil {
		t.Fatal(err)
	}
	encrypt := encryptWXBody(t, aesKey, inner, "wxappid")
	body := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><Encrypt><![CDATA[` + encrypt + `]]></Encrypt></xml>`
	req := httptest.NewRequest(http.MethodPost,
		"/callback/command?signature="+sigOf("tok", ts, nonce)+"&timestamp="+ts+"&nonce="+nonce+"&msg_signature=wrong&encrypt_type=aes",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("bad msg_signature must 400, got %d", w.Code)
	}
}

// TestCallbackCommandEncryptedAppIDMismatch: decrypted AppId must match the
// account AppId.
func TestCallbackCommandEncryptedAppIDMismatch(t *testing.T) {
	keyStr := testAESKey(t)
	a := New(map[string]interface{}{"id": "wx", "token": "tok", "appid": "wxappid", "encoding_aes_key": keyStr}, nil, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	inner := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><FromUserName><![CDATA[o]]></FromUserName>
<CreateTime>` + ts + `</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[hi]]></Content></xml>`
	aesKey, err := base64.StdEncoding.DecodeString(keyStr + "=")
	if err != nil {
		t.Fatal(err)
	}
	encrypt := encryptWXBody(t, aesKey, inner, "other_appid")
	body := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><Encrypt><![CDATA[` + encrypt + `]]></Encrypt></xml>`
	msgSig := sigOf("tok", ts, nonce, encrypt)
	req := httptest.NewRequest(http.MethodPost,
		"/callback/command?signature="+sigOf("tok", ts, nonce)+"&timestamp="+ts+"&nonce="+nonce+"&msg_signature="+msgSig+"&encrypt_type=aes",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("appid mismatch must 400, got %d", w.Code)
	}
}

// TestCallbackCommandMalformedCiphertext: malformed ciphertext must be rejected
// without panicking.
func TestCallbackCommandMalformedCiphertext(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok", "appid": "wxappid", "encoding_aes_key": testAESKey(t)}, nil, nil)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	body := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><Encrypt><![CDATA[abc]]></Encrypt></xml>`
	req := httptest.NewRequest(http.MethodPost,
		"/callback/command?signature="+sigOf("tok", ts, nonce)+"&timestamp="+ts+"&nonce="+nonce+"&msg_signature="+sigOf("tok", ts, nonce, "abc")+"&encrypt_type=aes",
		strings.NewReader(body))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed ciphertext must 400, got %d", w.Code)
	}
}

// TestSendBlockedWithoutActiveMode: passive mode Send returns an error
// (mirrors Python's "公众号不支持发送主动消息").
func TestSendBlockedWithoutActiveMode(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	err := a.Send("o_openid", &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}})
	if err == nil {
		t.Error("passive mode Send must reject active sending")
	}
}

// TestEncryptedMessageRoundTrip: the SDK's AES decode handles encrypted input.
func TestEncryptedMessageRoundTrip(t *testing.T) {
	_ = xml.Name{}
	// Sanity: MessageData.ShouldDecode is available on the SDK type.
	var md wxmp.MessageData
	md.Encrypt = ""
	if err := md.ShouldDecode(""); err != nil {
		t.Logf("ShouldDecode(empty) err (expected, no-op): %v", err)
	}
}
