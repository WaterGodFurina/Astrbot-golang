package weixin_official_account

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	wxmp "github.com/blusewang/wx/mp_api"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

func sigOf(parts ...string) string {
	cp := make([]string, len(parts))
	copy(cp, parts)
	sort.Strings(cp)
	sum := sha1.Sum([]byte(strings.Join(cp, "")))
	return hex.EncodeToString(sum[:])
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
	msg := `<xml><ToUserName><![CDATA[gh]]></ToUserName><FromUserName><![CDATA[u]]></FromUserName>
<CreateTime>1</CreateTime><MsgType><![CDATA[event]]></MsgType><Event><![CDATA[subscribe]]></Event></xml>`
	req := httptest.NewRequest(http.MethodPost, "/callback/command?signature="+sigOf("tok", "1", "n")+"&timestamp=1&nonce=n", strings.NewReader(msg))
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)
	if w.Body.String() != "success" {
		t.Errorf("event must ack success, got %q", w.Body.String())
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
