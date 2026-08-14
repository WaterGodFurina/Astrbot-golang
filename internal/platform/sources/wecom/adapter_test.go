package wecom

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// fakeEventBus 测试用事件总线，记录发布的事件。
type fakeEventBus struct {
	mu     sync.Mutex
	events []*core.Event
}

func (f *fakeEventBus) Publish(event *core.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, event)
	return nil
}

func (f *fakeEventBus) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.events)
}

func (f *fakeEventBus) last() *core.Event {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.events) == 0 {
		return nil
	}
	return f.events[len(f.events)-1]
}

// newTestAdapter 构造测试适配器（不发起真实网络请求）。
func newTestAdapter(t *testing.T, bus platform.EventBus, extra map[string]interface{}) *Adapter {
	t.Helper()
	config := map[string]interface{}{
		"id":                   "wecom",
		"type":                 "wecom",
		"corpid":               "ww1234567890abcdef",
		"secret":               "test_secret",
		"token":                "test_token",
		"encoding_aes_key":     testEncodingAESKey(t),
		"api_base_url":         "https://qyapi.weixin.qq.com/cgi-bin/",
		"callback_server_host": "127.0.0.1",
		"port":                 6195,
	}
	for k, v := range extra {
		config[k] = v
	}
	a := New(config, map[string]interface{}{}, nil)
	a.EventBus = bus
	return a
}

// testEncodingAESKey 生成 43 位 EncodingAESKey。
func testEncodingAESKey(t *testing.T) string {
	t.Helper()
	c := makeTestCrypto(t)
	return c.encodingAESKeyString(t)
}

// TestAdapterURLVerification GET 验证请求返回解密后的 echostr。
func TestAdapterURLVerification(t *testing.T) {
	a := newTestAdapter(t, &fakeEventBus{}, nil)
	a.crypto, _ = NewWXBizMsgCrypt("test_token", testEncodingAESKey(t), "ww1234567890abcdef")

	echostr, err := a.crypto.Encrypt("echo_this_123")
	if err != nil {
		t.Fatal(err)
	}
	timestamp := "1700000000"
	nonce := "noncexyz"
	sig := a.crypto.GetSignature(timestamp, nonce, echostr)

	req := httptest.NewRequest(http.MethodGet, "/callback/command?"+
		"msg_signature="+sig+"&timestamp="+timestamp+"&nonce="+nonce+"&echostr="+url.QueryEscape(echostr), nil)
	w := httptest.NewRecorder()
	a.handleVerify(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: %d body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "echo_this_123" {
		t.Errorf("验证结果: got %q want %q", got, "echo_this_123")
	}
}

// TestAdapterURLVerificationBadSignature 错误签名应返回 400。
func TestAdapterURLVerificationBadSignature(t *testing.T) {
	a := newTestAdapter(t, &fakeEventBus{}, nil)
	a.crypto, _ = NewWXBizMsgCrypt("test_token", testEncodingAESKey(t), "ww1234567890abcdef")
	req := httptest.NewRequest(http.MethodGet, "/callback/command?msg_signature=bad&timestamp=1&nonce=2&echostr=3", nil)
	w := httptest.NewRecorder()
	a.handleVerify(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("错误签名应返回 400，got %d", w.Code)
	}
}

// TestAdapterCallbackTextMessage POST 回调文本消息被解密并发布事件。
func TestAdapterCallbackTextMessage(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, nil)
	a.crypto, _ = NewWXBizMsgCrypt("test_token", testEncodingAESKey(t), "ww1234567890abcdef")

	plain := `<xml><ToUserName><![CDATA[corpid]]></ToUserName><FromUserName><![CDATA[zhangsan]]></FromUserName><CreateTime>1348831860</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[你好AstrBot]]></Content><MsgId>111222333</MsgId><AgentID>1000002</AgentID></xml>`
	enc, _ := a.crypto.Encrypt(plain)
	xmlBody := `<xml><Encrypt><![CDATA[` + enc + `]]></Encrypt></xml>`
	timestamp := "1700000001"
	nonce := "nonce1"
	sig := a.crypto.GetSignature(timestamp, nonce, enc)

	req := httptest.NewRequest(http.MethodPost, "/callback/command?msg_signature="+sig+"&timestamp="+timestamp+"&nonce="+nonce,
		strings.NewReader(xmlBody))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "success" {
		t.Fatalf("回调响应异常: %d %s", w.Code, w.Body.String())
	}
	// convertMessage 已改为异步执行，等待事件发布
	deadline := time.Now().Add(2 * time.Second)
	for bus.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bus.count() != 1 {
		t.Fatalf("应发布 1 个事件，got %d", bus.count())
	}
	ev := bus.last()
	if ev.MessageStr != "你好AstrBot" {
		t.Errorf("消息内容: %q", ev.MessageStr)
	}
	if ev.Source.SenderID != "zhangsan" || ev.Source.ConvID != "zhangsan" {
		t.Errorf("发送者信息异常: %+v", ev.Source)
	}
	if ev.Source.Platform != "wecom" || ev.Source.SelfID != "1000002" {
		t.Errorf("平台信息异常: %+v", ev.Source)
	}
	if a.getAgentID() != "1000002" {
		t.Errorf("agent_id 应被记录: %q", a.getAgentID())
	}
}

// TestAdapterCallbackBadSignature POST 回调签名错误返回 400。
func TestAdapterCallbackBadSignature(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, nil)
	a.crypto, _ = NewWXBizMsgCrypt("test_token", testEncodingAESKey(t), "ww1234567890abcdef")
	req := httptest.NewRequest(http.MethodPost, "/callback/command?msg_signature=bad&timestamp=1&nonce=2",
		strings.NewReader(`<xml><Encrypt><![CDATA[xxx]]></Encrypt></xml>`))
	w := httptest.NewRecorder()
	a.handleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("错误签名应返回 400，got %d", w.Code)
	}
	if bus.count() != 0 {
		t.Error("不应发布事件")
	}
}

// TestWebhookPlatformInterface 统一 Webhook 模式：GET/POST 分发。
func TestWebhookPlatformInterface(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, map[string]interface{}{"webhook_uuid": "abc-123", "unified_webhook_mode": true})
	var _ platform.WebhookPlatform = a // 编译期断言接口实现
	if a.WebhookUUID() != "abc-123" {
		t.Errorf("WebhookUUID: %q", a.WebhookUUID())
	}

	a.crypto, _ = NewWXBizMsgCrypt("test_token", testEncodingAESKey(t), "ww1234567890abcdef")

	// GET 验证
	echostr, _ := a.crypto.Encrypt("unified_echo")
	ts := "1700000002"
	nonce := "nn1"
	sig := a.crypto.GetSignature(ts, nonce, echostr)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/webhooks/platforms/abc-123?msg_signature="+sig+"&timestamp="+ts+"&nonce="+nonce+"&echostr="+url.QueryEscape(echostr), nil)
	w := httptest.NewRecorder()
	a.WebhookCallback(w, req)
	if w.Body.String() != "unified_echo" {
		t.Errorf("GET 分发异常: %q", w.Body.String())
	}

	// POST 消息
	plain := `<xml><ToUserName><![CDATA[corpid]]></ToUserName><FromUserName><![CDATA[lisi]]></FromUserName><CreateTime>1348831860</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[统一Webhook测试]]></Content><MsgId>999888777</MsgId><AgentID>1000002</AgentID></xml>`
	enc, _ := a.crypto.Encrypt(plain)
	xmlBody := `<xml><Encrypt><![CDATA[` + enc + `]]></Encrypt></xml>`
	ts2 := "1700000003"
	sig2 := a.crypto.GetSignature(ts2, nonce, enc)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/platforms/abc-123?msg_signature="+sig2+"&timestamp="+ts2+"&nonce="+nonce, strings.NewReader(xmlBody))
	w2 := httptest.NewRecorder()
	a.WebhookCallback(w2, req2)
	if w2.Body.String() != "success" {
		t.Errorf("POST 分发异常: %q", w2.Body.String())
	}
	// convertMessage 已改为异步执行，等待事件发布
	deadline := time.Now().Add(2 * time.Second)
	for bus.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bus.count() != 1 || bus.last().MessageStr != "统一Webhook测试" {
		t.Errorf("事件异常: %d", bus.count())
	}
}

// TestRESTClientGetTokenAndSend 使用 httptest 模拟企业微信 API：gettoken 缓存与 message/send。
func TestRESTClientGetTokenAndSend(t *testing.T) {
	var mu sync.Mutex
	tokenCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/gettoken"):
			tokenCalls++
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"TOKEN_ABC","expires_in":7200}`))
		case strings.HasSuffix(r.URL.Path, "/message/send"):
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			if r.URL.Query().Get("access_token") != "TOKEN_ABC" {
				w.WriteHeader(500)
				w.Write([]byte(`{"errcode":-1,"errmsg":"no token"}`))
				return
			}
			if body["msgtype"] != "text" || body["agentid"] != "1000002" || body["touser"] != "zhangsan" {
				w.WriteHeader(500)
				w.Write([]byte(`{"errcode":-2,"errmsg":"bad payload"}`))
				return
			}
			text, _ := body["text"].(map[string]interface{})
			if text["content"] != "你好" {
				w.WriteHeader(500)
				w.Write([]byte(`{"errcode":-3,"errmsg":"bad content"}`))
				return
			}
			w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"errcode":404,"errmsg":"not found"}`))
		}
	}))
	defer srv.Close()

	client := NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")
	if err := client.SendText(t.Context(), "1000002", "zhangsan", "你好"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	// 第二次调用应使用缓存的 token
	if err := client.SendText(t.Context(), "1000002", "zhangsan", "你好"); err != nil {
		t.Fatalf("第二次发送失败: %v", err)
	}
	mu.Lock()
	calls := tokenCalls
	mu.Unlock()
	if calls != 1 {
		t.Errorf("gettoken 应只调用 1 次（缓存），got %d", calls)
	}
}

// TestRESTClientErrorCode API 错误返回 WeChatClientError（含 errcode）。
func TestRESTClientErrorCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gettoken") {
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"T","expires_in":7200}`))
			return
		}
		w.Write([]byte(`{"errcode":40096,"errmsg":"invalid external userid"}`))
	}))
	defer srv.Close()

	client := NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")
	err := client.SendText(t.Context(), "1000002", "bad_user", "hi")
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !IsErrCode(err, 40096) {
		t.Errorf("IsErrCode(40096) 应为 true, got %v", err)
	}
}

// TestRESTClientUploadMedia 素材上传 multipart 请求构造正确。
func TestRESTClientUploadMedia(t *testing.T) {
	tmpFile := t.TempDir() + "/test.jpg"
	content := []byte("fake-image-bytes")
	if err := os.WriteFile(tmpFile, content, 0644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gettoken"):
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"T","expires_in":7200}`))
		case strings.HasSuffix(r.URL.Path, "/media/upload"):
			if r.URL.Query().Get("type") != "image" {
				t.Errorf("type 参数: %q", r.URL.Query().Get("type"))
			}
			file, header, err := r.FormFile("media")
			if err != nil {
				t.Errorf("缺少 media 表单字段: %v", err)
				w.Write([]byte(`{"errcode":-1,"errmsg":"no media"}`))
				return
			}
			defer file.Close()
			got, _ := io.ReadAll(file)
			if !bytes.Equal(got, content) {
				t.Errorf("上传内容不一致")
			}
			if header.Filename != "test.jpg" {
				t.Errorf("文件名: %q", header.Filename)
			}
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","media_id":"MEDIA_1","type":"image","created_at":"1407783380"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	client := NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")
	mediaID, err := client.UploadMedia(t.Context(), "image", tmpFile)
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if mediaID != "MEDIA_1" {
		t.Errorf("media_id: %q", mediaID)
	}
}

// TestSendChainAppMode 应用模式发送：文本分块与消息发送（httptest）。
func TestSendChainAppMode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/gettoken") {
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"T","expires_in":7200}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/message/send") {
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			text, _ := body["text"].(map[string]interface{})
			fmt.Fprintf(w, `{"errcode":0,"errmsg":"ok","content":"%v"}`, text["content"])
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	a := &Adapter{client: NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")}
	// 长文本分块发送：共 4000 字符应分成 2 条
	longText := strings.Repeat("a", 2040) + "。" + strings.Repeat("b", 1960)
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: longText}}}
	if err := a.sendChain(chain, "1000002", "zhangsan"); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
}

// TestSendChainURLOnlyFileVideo URL-only 文件/视频组件应能解析下载并发送（回归 M-45）。
func TestSendChainURLOnlyFileVideo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/file.bin":
			w.Write([]byte("file-bytes"))
		case r.URL.Path == "/video.mp4":
			w.Write([]byte("video-bytes"))
		case strings.HasSuffix(r.URL.Path, "/gettoken"):
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"T","expires_in":7200}`))
		case strings.HasSuffix(r.URL.Path, "/media/upload"):
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","media_id":"MEDIA_1","type":"image","created_at":"1407783380"}`))
		case strings.HasSuffix(r.URL.Path, "/message/send"):
			w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	a := &Adapter{client: NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")}
	chain := &message.MessageChain{Chain: []message.Component{
		&message.File{URL: srv.URL + "/file.bin"},
		&message.Video{URL: srv.URL + "/video.mp4"},
	}}
	if err := a.sendChain(chain, "1000002", "zhangsan"); err != nil {
		t.Fatalf("URL-only 文件/视频发送失败: %v", err)
	}
}

// TestHandleKFMsgOrEventCursorAdvance KF 同步游标逐页推进且逐条转换（回归 M-46）。
func TestHandleKFMsgOrEventCursorAdvance(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, map[string]interface{}{"kf_name": "客服"})

	var mu sync.Mutex
	var cursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/gettoken"):
			w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"T","expires_in":7200}`))
		case strings.HasSuffix(r.URL.Path, "/kf/sync_msg"):
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			cursor, _ := body["cursor"].(string)
			mu.Lock()
			cursors = append(cursors, cursor)
			mu.Unlock()
			if cursor == "" {
				w.Write([]byte(`{"errcode":0,"errmsg":"ok","has_more":1,"next_cursor":"cur_1","msg_list":[` +
					`{"msgtype":"text","external_userid":"wm_1","open_kfid":"wk","msgid":"m1","text":{"content":"A"}},` +
					`{"msgtype":"text","external_userid":"wm_1","open_kfid":"wk","msgid":"m2","text":{"content":"B"}}]}`))
			} else {
				w.Write([]byte(`{"errcode":0,"errmsg":"ok","has_more":0,"next_cursor":"","msg_list":[` +
					`{"msgtype":"text","external_userid":"wm_1","open_kfid":"wk","msgid":"m3","text":{"content":"C"}}]}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	a.client = NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")

	msg := &WecomMessage{Type: "event", Event: "kf_msg_or_event", Token: "tok", OpenKfID: "wk"}
	a.handleKFMsgOrEvent(msg)

	if bus.count() != 3 {
		t.Fatalf("应逐条处理 3 条消息，got %d", bus.count())
	}
	mu.Lock()
	got := make([]string, len(cursors))
	copy(got, cursors)
	mu.Unlock()
	if len(got) != 2 || got[0] != "" || got[1] != "cur_1" {
		t.Errorf("游标应推进为 [\"\", cur_1]，got %v", got)
	}
}

// TestSendChainKFModeError 客服模式 Send 返回错误（不支持主动发送）。
func TestSendChainKFModeError(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, map[string]interface{}{"kf_name": "我的客服"})
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}}
	if err := a.Send("ext_user", chain); err == nil {
		t.Error("客服模式主动发送应返回错误")
	}
}

// TestSendNoAgentID 未收到过消息（无 agent_id）时 Send 返回错误。
func TestSendNoAgentID(t *testing.T) {
	a := newTestAdapter(t, &fakeEventBus{}, nil)
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}}
	if err := a.Send("user", chain); err == nil {
		t.Error("无 agent_id 时应返回错误")
	}
}

// TestKFTextDedup 15 秒内重复客服文本去重。
func TestKFTextDedup(t *testing.T) {
	a := newTestAdapter(t, &fakeEventBus{}, nil)
	if a.isDuplicateKFText("session1", "  hello  ") {
		t.Error("首次不应判重")
	}
	if !a.isDuplicateKFText("session1", "hello") {
		t.Error("15 秒内重复应判重")
	}
	if a.isDuplicateKFText("session2", "hello") {
		t.Error("不同会话不应判重")
	}
	if a.isDuplicateKFText("session1", "  ") {
		t.Error("空白文本不应判重")
	}
}

// TestConvertKFTextMessage 微信客服文本消息转换。
func TestConvertKFTextMessage(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, map[string]interface{}{"kf_name": "客服"})
	msg := map[string]interface{}{
		"msgtype":         "text",
		"external_userid": "wm_xxx",
		"open_kfid":       "wk_open",
		"msgid":           "msgid_1",
		"text":            map[string]interface{}{"content": " 客服你好 "},
	}
	a.convertKFMessage(msg)
	if bus.count() != 1 {
		t.Fatalf("应发布 1 个事件，got %d", bus.count())
	}
	ev := bus.last()
	if ev.MessageStr != "客服你好" {
		t.Errorf("消息内容: %q", ev.MessageStr)
	}
	if ev.Source.ConvID != "wm_xxx" || ev.Source.SenderID != "wm_xxx" {
		t.Errorf("会话信息异常: %+v", ev.Source)
	}
	if ev.Source.SelfID != "wk_open" {
		t.Errorf("self_id 应为 open_kfid: %q", ev.Source.SelfID)
	}
	// 去重后第二次转换不再发布
	a.convertKFMessage(msg)
	if bus.count() != 1 {
		t.Errorf("重复消息应被去重，got %d", bus.count())
	}
}

// TestConvertMessageText 普通应用文本消息转换。
func TestConvertMessageText(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, nil)
	msg := &WecomMessage{
		Type:    "text",
		Content: "你好",
		Source:  "zhangsan",
		ID:      "msg_1",
		Time:    1700000000,
		Agent:   "1000002",
	}
	a.convertMessage(msg)
	if bus.count() != 1 {
		t.Fatalf("应发布 1 个事件，got %d", bus.count())
	}
	ev := bus.last()
	if ev.MessageStr != "你好" || ev.Source.SelfID != "1000002" {
		t.Errorf("事件异常: %+v", ev)
	}
	if a.getAgentID() != "1000002" {
		t.Error("agent_id 未记录")
	}
}

// TestConvertMessageUnsupported 未支持的消息类型不应发布事件。
func TestConvertMessageUnsupported(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAdapter(t, bus, nil)
	msg := &WecomMessage{Type: "video", Source: "u", Agent: "1"}
	a.convertMessage(msg)
	if bus.count() != 0 {
		t.Error("未支持类型不应发布事件")
	}
}

// TestRESTClientTokenInvalidRetry 回归 L-45.1：收到 40014/42001 时清空 access_token
// 缓存并重新获取后重试一次。
func TestRESTClientTokenInvalidRetry(t *testing.T) {
	for _, code := range []int{40014, 42001} {
		t.Run(fmt.Sprintf("errcode_%d", code), func(t *testing.T) {
			var mu sync.Mutex
			tokenCalls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case strings.HasSuffix(r.URL.Path, "/gettoken"):
					tokenCalls++
					if tokenCalls == 1 {
						w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"TOKEN_OLD","expires_in":7200}`))
					} else {
						w.Write([]byte(`{"errcode":0,"errmsg":"ok","access_token":"TOKEN_NEW","expires_in":7200}`))
					}
				case strings.HasSuffix(r.URL.Path, "/message/send"):
					switch r.URL.Query().Get("access_token") {
					case "TOKEN_OLD":
						fmt.Fprintf(w, `{"errcode":%d,"errmsg":"token invalid"}`, code)
					case "TOKEN_NEW":
						w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
					default:
						w.WriteHeader(500)
						w.Write([]byte(`{"errcode":-1,"errmsg":"unexpected token"}`))
					}
				default:
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()

			client := NewWeChatClient("corpid", "secret", srv.URL+"/cgi-bin/")
			if err := client.SendText(t.Context(), "1000002", "zhangsan", "hi"); err != nil {
				t.Fatalf("发送失败: %v", err)
			}
			mu.Lock()
			calls := tokenCalls
			mu.Unlock()
			if calls != 2 {
				t.Errorf("token 失效后应重新获取 access_token，gettoken 调用次数 got %d want 2", calls)
			}
		})
	}
}

// TestWecomServerStartBindFailure 回归 L-45.4：端口占用时 Start 应返回错误而不是仅记日志。
func TestWecomServerStartBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	a := newTestAdapter(t, &fakeEventBus{}, nil)
	s := &WecomServer{adapter: a}
	if err := s.Start(t.Context(), "127.0.0.1", port); err == nil {
		t.Error("端口占用时 Start 应返回错误")
	}
}

// TestAppMessageDedup 回归 L-45.5：应用消息按 MsgId 做短窗口去重。
func TestAppMessageDedup(t *testing.T) {
	a := newTestAdapter(t, &fakeEventBus{}, nil)
	m1 := &WecomMessage{Type: "text", ID: "msg_1", Content: "a", Source: "u", Agent: "1"}
	m2 := &WecomMessage{Type: "text", ID: "msg_1", Content: "a", Source: "u", Agent: "1"}
	m3 := &WecomMessage{Type: "text", ID: "msg_2", Content: "b", Source: "u", Agent: "1"}
	if a.isDuplicateAppMessage(m1) {
		t.Error("首次不应判重")
	}
	if !a.isDuplicateAppMessage(m2) {
		t.Error("同一 MsgId 短时间内应判重")
	}
	if a.isDuplicateAppMessage(m3) {
		t.Error("不同 MsgId 不应判重")
	}
	if a.isDuplicateAppMessage(&WecomMessage{Type: "text"}) {
		t.Error("无 MsgId 的消息不应判重")
	}
}
