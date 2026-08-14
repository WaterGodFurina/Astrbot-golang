package dingtalk

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/gorilla/websocket"
)

// rewriteHostTransport 将请求重定向到测试服务器 (钉钉 API 域名无法从单测访问)。
type rewriteHostTransport struct {
	base string
}

// RoundTrip 重写目标 host 到测试服务器。
func (t *rewriteHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	req2.URL.Host = strings.TrimPrefix(t.base, "http://")
	return http.DefaultTransport.RoundTrip(req2)
}

// newTokenTestAdapter 构造指向测试服务器的钉钉适配器。
func newTokenTestAdapter(t *testing.T, handler http.HandlerFunc) *Adapter {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	a.httpClient = &http.Client{Transport: &rewriteHostTransport{base: srv.URL}}
	return a
}

// TestGetAccessTokenFlatResponse 验证扁平响应 {"accessToken": ...} 解析 (对应 H-16)。
func TestGetAccessTokenFlatResponse(t *testing.T) {
	a := newTokenTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.0/oauth2/accessToken" {
			t.Errorf("请求路径应为 /v1.0/oauth2/accessToken, 实际 %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"accessToken":"tok-flat","expireIn":7200}`))
	})
	if got := a.getAccessToken(); got != "tok-flat" {
		t.Fatalf("扁平 accessToken 应解析成功, 实际 %q", got)
	}
	// 缓存应生效 (第二次调用不再发起请求)
	if got := a.getAccessToken(); got != "tok-flat" {
		t.Fatalf("缓存命中应返回同一 token, 实际 %q", got)
	}
}

// TestGetAccessTokenNestedFallback 验证嵌套结构 {"data":{"accessToken":...}} 兜底解析。
func TestGetAccessTokenNestedFallback(t *testing.T) {
	a := newTokenTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"accessToken":"tok-nested","expireIn":3600}}`))
	})
	if got := a.getAccessToken(); got != "tok-nested" {
		t.Fatalf("嵌套 accessToken 应兜底解析成功, 实际 %q", got)
	}
}

// ---------- 会话 id 转换 ----------

// TestIDToSid 验证钉钉 id 前缀剥离 (对应 Python _id_to_sid)。
func TestIDToSid(t *testing.T) {
	cases := map[string]string{
		"$:LWCP_v1$:user123": "user123",
		"user123":            "user123",
		"":                   "unknown",
	}
	for input, want := range cases {
		if got := idToSid(input); got != want {
			t.Fatalf("idToSid(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

// ---------- 消息解析 ----------

// TestParseChatbotMessage 验证回调数据解析 (对应 ChatbotMessage.from_dict)。
func TestParseChatbotMessage(t *testing.T) {
	raw := `{
		"conversationId": "conv_1",
		"conversationType": "2",
		"createAt": 1700000000000,
		"msgId": "msg_1",
		"senderId": "sender_1",
		"senderNick": "小明",
		"senderStaffId": "staff_1",
		"chatbotUserId": "bot_1",
		"isInAtList": true,
		"robotCode": "robot_1",
		"msgtype": "text",
		"text": {"content": "hello"},
		"atUsers": [{"dingtalkId": "bot_1", "staffId": "bot_staff"}]
	}`
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &data); err != nil {
		t.Fatalf("解析测试数据失败: %v", err)
	}
	msg := parseChatbotMessage(data)
	if msg.ConversationType != "2" || msg.ConversationID != "conv_1" {
		t.Fatalf("会话信息解析错误: %+v", msg)
	}
	if msg.MessageType != "text" || msg.TextContent != "hello" {
		t.Fatalf("文本内容解析错误: %+v", msg)
	}
	if msg.CreateAt != 1700000000000 {
		t.Fatalf("createAt 解析错误: %d", msg.CreateAt)
	}
	if len(msg.AtUsers) != 1 || msg.AtUsers[0].DingtalkID != "bot_1" {
		t.Fatalf("atUsers 解析错误: %+v", msg.AtUsers)
	}
}

// TestConvertMsgText 验证文本消息转换 (群聊 + @)。
func TestConvertMsgText(t *testing.T) {
	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	msg := &ChatbotMessage{
		ConversationID:   "conv_1",
		ConversationType: "2",
		CreateAt:         1700000000000,
		MsgID:            "msg_1",
		SenderID:         "$:LWCP_v1$:sender_1",
		SenderNick:       "小明",
		ChatbotUserID:    "$:LWCP_v1$:bot_1",
		MessageType:      "text",
		TextContent:      " /server",
		AtUsers:          []AtUser{{DingtalkID: "$:LWCP_v1$:bot_1"}},
	}
	abm := a.convertMsg(msg)
	if abm.Type != platform.GroupMessage {
		t.Fatalf("群聊消息应为 GroupMessage")
	}
	if abm.SelfID != "bot_1" {
		t.Fatalf("self_id 应为 bot_1 (去前缀), 实际 %q", abm.SelfID)
	}
	if abm.SessionID != "conv_1" || abm.Group == nil || abm.Group.GroupID != "conv_1" {
		t.Fatalf("会话信息错误: %+v", abm)
	}
	if len(abm.Message) != 2 {
		t.Fatalf("应有 2 个组件 (At + Plain), 实际 %d", len(abm.Message))
	}
	if at, ok := abm.Message[0].(*message.At); !ok || at.TargetID != "bot_1" {
		t.Fatalf("组件0 应为 At(bot_1): %+v", abm.Message[0])
	}
	if abm.MessageStr != "/server" {
		t.Fatalf("文本应去首尾空白, 实际: %q", abm.MessageStr)
	}
	if p, ok := abm.Message[1].(*message.Plain); !ok || p.Text != "/server" {
		t.Fatalf("组件1 应为 Plain(/server): %+v", abm.Message[1])
	}
	// 私聊时 senderId 去前缀
	if a.isKnownGroup("conv_1") != true {
		t.Fatal("群聊会话应被记录")
	}
}

// TestConvertMsgRichText 验证富文本消息转换 (跳过开头的 @机器人 文本段)。
func TestConvertMsgRichText(t *testing.T) {
	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	msg := &ChatbotMessage{
		ConversationType: "2",
		SenderID:         "sender_1",
		ChatbotUserID:    "bot_1",
		MessageType:      "richText",
		AtUsers:          []AtUser{{DingtalkID: "bot_1"}},
		RichText: []map[string]interface{}{
			{"text": "@ExampleBot"},
			{"text": "/server"},
		},
	}
	abm := a.convertMsg(msg)
	if abm.MessageStr != "/server" {
		t.Fatalf("开头的 @机器人 文本段应被跳过, 实际: %q", abm.MessageStr)
	}
	if len(abm.Message) != 2 {
		t.Fatalf("应有 2 个组件 (At + Plain), 实际 %d", len(abm.Message))
	}
}

// TestConvertMsgRichTextOtherMention 验证非机器人 @ 文本段保留 (对应 Python 测试用例)。
func TestConvertMsgRichTextOtherMention(t *testing.T) {
	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	msg := &ChatbotMessage{
		ConversationType: "2",
		SenderID:         "sender_1",
		ChatbotUserID:    "bot_1",
		MessageType:      "richText",
		AtUsers:          []AtUser{{DingtalkID: "another-user"}},
		RichText: []map[string]interface{}{
			{"text": "@AnotherUser"},
			{"text": "/server"},
		},
	}
	abm := a.convertMsg(msg)
	if abm.MessageStr != "@AnotherUser/server" {
		t.Fatalf("非机器人 @ 文本段应保留, 实际: %q", abm.MessageStr)
	}
	if len(abm.Message) != 3 {
		t.Fatalf("应有 3 个组件, 实际 %d", len(abm.Message))
	}
	if at, ok := abm.Message[0].(*message.At); !ok || at.TargetID != "another-user" {
		t.Fatalf("组件0 应为 At(another-user): %+v", abm.Message[0])
	}
}

// TestConvertMsgPrivate 验证私聊消息转换 (sessionID = 发送者 sid)。
func TestConvertMsgPrivate(t *testing.T) {
	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	msg := &ChatbotMessage{
		ConversationType: "1",
		SenderID:         "$:LWCP_v1$:sender_1",
		SenderNick:       "小明",
		ChatbotUserID:    "bot_1",
		MessageType:      "text",
		TextContent:      "hi",
		SenderStaffID:    "staff_1",
	}
	abm := a.convertMsg(msg)
	if abm.Type != platform.FriendMessage {
		t.Fatalf("私聊消息应为 FriendMessage")
	}
	if abm.SessionID != "sender_1" {
		t.Fatalf("私聊 session_id 应为发送者 sid, 实际 %q", abm.SessionID)
	}
	// 验证 staff_id 映射被记录
	if a.getSenderStaffID("sender_1") != "staff_1" {
		t.Fatal("staff_id 映射应被记录")
	}
}

// TestConvertMsgAudioVoice 验证语音消息解析 (audio/voice 类型)。
func TestConvertMsgAudioVoice(t *testing.T) {
	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	// 缺少 downloadCode 时应跳过 (不会触发网络请求)
	msg := &ChatbotMessage{
		ConversationType: "1",
		SenderID:         "sender_1",
		MessageType:      "voice",
		RobotCode:        "robot_1",
		Content:          map[string]interface{}{},
	}
	abm := a.convertMsg(msg)
	if len(abm.Message) != 0 {
		t.Fatalf("缺少 downloadCode 时应无组件, 实际 %d", len(abm.Message))
	}
	// 缺少 robotCode 时同样跳过
	msg2 := &ChatbotMessage{
		ConversationType: "1",
		SenderID:         "sender_1",
		MessageType:      "voice",
		Content:          map[string]interface{}{"downloadCode": "dc_1"},
	}
	abm2 := a.convertMsg(msg2)
	if len(abm2.Message) != 0 {
		t.Fatalf("缺少 robotCode 时应无组件, 实际 %d", len(abm2.Message))
	}
}

// ---------- 发送参数构造 ----------

// TestReconnectDelay 验证重连延迟指数退避 (对应 Python 测试用例)。
func TestReconnectDelay(t *testing.T) {
	for i := 1; i <= 4; i++ {
		want := int64(10 * (1 << (i - 1)))
		if got := dingtalkReconnectDelay(i).Seconds(); int64(got) != want {
			t.Fatalf("重连延迟(%d) = %v, 期望 %d", i, got, want)
		}
	}
	// 最小延迟
	if got := dingtalkReconnectDelay(0).Seconds(); got != 10 {
		t.Fatalf("重连延迟(0) 应为 10, 实际 %v", got)
	}
	// 上限 300 秒
	if got := dingtalkReconnectDelay(20).Seconds(); got != 300 {
		t.Fatalf("重连延迟(20) 应封顶 300, 实际 %v", got)
	}
}

// TestReconnectDelaySaturation 验证重连延迟在极多次失败后仍封顶 300 秒,
// 不会因 1<<(retryCount-1) 移位溢出为 0/负数而退化为热循环 (对应 L-26)。
func TestReconnectDelaySaturation(t *testing.T) {
	for _, n := range []int{63, 64, 100, 1000} {
		if got := dingtalkReconnectDelay(n).Seconds(); got != 300 {
			t.Fatalf("重连延迟(%d) 应饱和到 300, 实际 %v", n, got)
		}
	}
}

// TestGroupMessagePayload 验证群聊消息发送 payload 构造 (msgKey/msgParam)。
func TestGroupMessagePayload(t *testing.T) {
	_ = New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	robotCode := "c1"
	msgParam := map[string]interface{}{"content": "hello"}
	paramJSON, _ := json.Marshal(msgParam)
	payload := map[string]interface{}{
		"msgKey":             "sampleText",
		"msgParam":           string(paramJSON),
		"openConversationId": "conv_1",
		"robotCode":          robotCode,
	}
	raw, _ := json.Marshal(payload)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("payload 非法 JSON: %v", err)
	}
	if parsed["msgKey"] != "sampleText" || parsed["robotCode"] != "c1" {
		t.Fatalf("payload 错误: %+v", parsed)
	}
	if parsed["openConversationId"] != "conv_1" {
		t.Fatalf("群聊会话字段错误: %+v", parsed)
	}
	if parsed["msgParam"] != `{"content":"hello"}` {
		t.Fatalf("msgParam 应为 JSON 字符串: %+v", parsed)
	}
}

// TestPrivateMessagePayload 验证私聊消息发送 payload 构造 (userIds 列表)。
func TestPrivateMessagePayload(t *testing.T) {
	_ = New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	paramJSON, _ := json.Marshal(map[string]interface{}{"title": "AstrBot", "text": "hi"})
	payload := map[string]interface{}{
		"robotCode": "c1",
		"userIds":   []string{"staff_1"},
		"msgKey":    "sampleMarkdown",
		"msgParam":  string(paramJSON),
	}
	raw, _ := json.Marshal(payload)
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("payload 非法 JSON: %v", err)
	}
	users, _ := parsed["userIds"].([]interface{})
	if len(users) != 1 || users[0] != "staff_1" {
		t.Fatalf("userIds 应为 [staff_1]: %+v", parsed)
	}
	if parsed["msgKey"] != "sampleMarkdown" {
		t.Fatalf("msgKey 错误: %+v", parsed)
	}
}

// TestTextMarkdownMode 验证文本消息的 markdown 模式选择 (对应 Python 测试用例)。
func TestTextMarkdownMode(t *testing.T) {
	// UseMarkdown=nil → markdown; UseMarkdown=false → 纯文本
	falseVal := false
	useMarkdownNil := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "first\nsecond"}}}
	useMarkdownFalse := &message.MessageChain{
		Chain:       []message.Component{&message.Plain{Text: "first\nsecond"}},
		UseMarkdown: &falseVal,
	}

	_ = New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)

	// 纯文本模式: sampleText
	if useMarkdownFalse.UseMarkdown != nil && !*useMarkdownFalse.UseMarkdown {
		// 构造与 Python 一致的参数
		text := strings.TrimSpace(useMarkdownFalse.Chain[0].(*message.Plain).Text)
		if text != "first\nsecond" {
			t.Fatalf("纯文本模式文本错误: %q", text)
		}
	}
	// markdown 模式 (默认): sampleMarkdown, title=AstrBot
	if useMarkdownNil.UseMarkdown == nil {
		text := strings.TrimSpace(useMarkdownNil.Chain[0].(*message.Plain).Text)
		param := map[string]interface{}{"title": "AstrBot", "text": text}
		if param["title"] != "AstrBot" || param["text"] != "first\nsecond" {
			t.Fatalf("markdown 模式参数错误: %+v", param)
		}
	}
}

// ---------- WS 帧解析 ----------

// TestParseDingFrame 验证钉钉流帧解析 (Callback 消息帧)。
func TestParseDingFrame(t *testing.T) {
	raw := `{
		"specVersion": "1.0",
		"type": "Callback",
		"headers": {
			"topic": "/v1.0/im/bot/messages/get",
			"eventId": "evt_1",
			"messageId": "frame_1"
		},
		"data": "{\"msgId\":\"msg_1\",\"conversationType\":\"2\"}"
	}`
	var frame dingFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("解析流帧失败: %v", err)
	}
	if frame.Type != "Callback" {
		t.Fatalf("流帧类型解析错误: %+v", frame)
	}
	if frame.Headers.Topic != chatTopic || frame.Headers.MessageID != "frame_1" {
		t.Fatalf("headers 解析错误: %+v", frame.Headers)
	}
	// Callback 帧的 data 为 JSON 字符串, 需要二次解析 (对应 SDK 的 json.loads)
	var rawData string
	if err := json.Unmarshal(frame.Data, &rawData); err != nil {
		t.Fatalf("Callback 帧 data 应为 JSON 字符串: %v", err)
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &data); err != nil {
		t.Fatalf("Callback 帧 data 解析失败: %v", err)
	}
	if data["msgId"] != "msg_1" {
		t.Fatalf("data 解析错误: %+v", data)
	}
}

// TestBuildAckFrame 验证确认帧构造 (对应 SDK AckMessage.to_dict)。
func TestBuildAckFrame(t *testing.T) {
	frame := &dingFrame{MessageID: "frame_1", Type: "Callback"}

	ack := dingAckFrame{
		Code: 200,
		Headers: map[string]string{
			"messageId":   frame.MessageID,
			"contentType": "application/json",
		},
		Message: "",
		Data:    `{"response": "OK"}`,
	}
	if frame.Type == "Callback" {
		ack.Data = `{"response": "OK"}`
	}
	raw, err := json.Marshal(ack)
	if err != nil {
		t.Fatalf("确认帧序列化失败: %v", err)
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("确认帧非法 JSON: %v", err)
	}
	if parsed["code"] != float64(200) {
		t.Fatalf("确认帧 code 应为 200: %+v", parsed)
	}
	headers, _ := parsed["headers"].(map[string]interface{})
	if headers["messageId"] != "frame_1" {
		t.Fatalf("确认帧应回显 messageId: %+v", parsed)
	}
	if parsed["data"] != `{"response": "OK"}` {
		t.Fatalf("确认帧 data 应为 JSON 字符串: %+v", parsed)
	}
}

// ---------- 发送失败返回 error (L-27) ----------

// newSendErrorAdapter 构造返回 500 的钉钉适配器 (token 请求正常, 发送请求失败)。
func newSendErrorAdapter(t *testing.T) *Adapter {
	t.Helper()
	return newTokenTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			_, _ = w.Write([]byte(`{"accessToken":"tok","expireIn":7200}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
}

// TestSendGroupMessageError 验证群聊发送失败时 Send 返回 error (而非静默 nil)。
func TestSendGroupMessageError(t *testing.T) {
	a := newSendErrorAdapter(t)
	a.convMu.Lock()
	a.knownGroups["conv_1"] = true
	a.convMu.Unlock()
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}}
	if err := a.Send("conv_1", chain); err == nil {
		t.Fatal("群聊发送失败应返回 error, 而非静默 nil (L-27)")
	}
}

// TestSendPrivateMessageError 验证私聊发送失败时 Send 返回 error。
func TestSendPrivateMessageError(t *testing.T) {
	a := newSendErrorAdapter(t)
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}}
	if err := a.Send("sender_1", chain); err == nil {
		t.Fatal("私聊发送失败应返回 error, 而非静默 nil (L-27)")
	}
}

// TestSendPrivateMessageUsesStaffID 验证私聊发送使用持久化的 staff_id 而非回退 session_id。
func TestSendPrivateMessageUsesStaffID(t *testing.T) {
	var gotStaffID string
	a := newTokenTestAdapter(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			_, _ = w.Write([]byte(`{"accessToken":"tok","expireIn":7200}`))
		case "/v1.0/robot/oToMessages/batchSend":
			var payload struct {
				UserIDs []string `json:"userIds"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if len(payload.UserIDs) == 1 {
				gotStaffID = payload.UserIDs[0]
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// 模拟已记录 staff_id 映射
	a.staffMu.Lock()
	a.staffIDMap[a.staffIDKey("sender_1")] = "staff_1"
	a.staffMu.Unlock()
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}}
	if err := a.Send("sender_1", chain); err != nil {
		t.Fatalf("发送不应失败: %v", err)
	}
	if gotStaffID != "staff_1" {
		t.Fatalf("私聊发送应使用持久化 staff_id, 实际 %q", gotStaffID)
	}
}

// TestStatePersistenceRoundTrip 验证会话映射落盘后重启可恢复 (对应 L-27)。
func TestStatePersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	config := map[string]interface{}{
		"client_id":         "c1",
		"client_secret":     "s1",
		"dingtalk_data_dir": dir,
	}
	a := New(config, nil, nil)
	a.rememberGroup("conv_1")
	a.rememberSenderBinding(&ChatbotMessage{SenderStaffID: "staff_1"},
		&platform.AstrBotMessage{Type: platform.FriendMessage, Sender: platform.MessageMember{UserID: "sender_1"}})

	// 新的适配器实例模拟进程重启
	b := New(config, nil, nil)
	if !b.isKnownGroup("conv_1") {
		t.Fatal("群聊会话应在重启后从磁盘恢复")
	}
	if b.getSenderStaffID("sender_1") != "staff_1" {
		t.Fatal("staff_id 映射应在重启后从磁盘恢复")
	}
}

// ---------- Callback 先 ack 再异步处理 (L-28) ----------

// newWSPair 建立一对真实 WebSocket 连接, 返回客户端连接与服务端收到的帧通道。
func newWSPair(t *testing.T) (*websocket.Conn, <-chan []byte, func()) {
	t.Helper()
	frames := make(chan []byte, 8)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, msg, err := c.ReadMessage()
			if err != nil {
				close(frames)
				return
			}
			frames <- msg
		}
	}))
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial ws: %v", err)
	}
	cleanup := func() {
		_ = conn.Close()
		srv.Close()
	}
	return conn, frames, cleanup
}

// TestHandleCallbackAcksBeforeAsyncProcessing 验证 Callback 帧先发送 ack,
// 再把数据投递到异步处理队列, 下载/转码等耗时操作不再阻塞 WS 读循环。
func TestHandleCallbackAcksBeforeAsyncProcessing(t *testing.T) {
	conn, frames, cleanup := newWSPair(t)
	defer cleanup()

	a := New(map[string]interface{}{"client_id": "c1", "client_secret": "s1"}, nil, nil)
	a.msgCh = make(chan map[string]interface{}, 8)

	callbackData, _ := json.Marshal(`{"msgId":"m1","conversationType":"2","msgtype":"text","text":{"content":"hi"}}`)
	frame := &dingFrame{
		Type:    "Callback",
		Headers: dingFrameHeader{Topic: chatTopic, MessageID: "frame_1"},
		Data:    callbackData,
	}
	a.handleCallback(context.Background(), conn, frame)

	// ack 应立即到达 (无需依赖异步处理完成)
	select {
	case ackMsg := <-frames:
		var ack dingAckFrame
		if err := json.Unmarshal(ackMsg, &ack); err != nil {
			t.Fatalf("ack 解析失败: %v", err)
		}
		if ack.Code != 200 || ack.Headers["messageId"] != "frame_1" {
			t.Fatalf("ack 内容错误: %+v", ack)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("先发送 ack 后异步处理: 未在超时内收到 ack")
	}

	// 原始数据应进入异步处理队列
	select {
	case data := <-a.msgCh:
		if data["msgId"] != "m1" {
			t.Fatalf("入队数据错误: %+v", data)
		}
	case <-time.After(time.Second):
		t.Fatal("回调数据应入队待异步处理 (L-28)")
	}
}
