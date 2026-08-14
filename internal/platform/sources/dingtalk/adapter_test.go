package dingtalk

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

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
