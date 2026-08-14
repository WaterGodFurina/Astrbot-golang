// Package misskey - Misskey 平台适配器单元测试。
// 测试不连接真实网络：消息解析 / 发送参数构造 / streaming 帧解析用纯函数与 mock 验证；
// go-misskey 的 API 调用不测试。
package misskey

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ---------- 消息链序列化（serialize_message_chain） ----------

func TestSerializeMessageChain(t *testing.T) {
	// 纯文本
	text, hasAt := serializeMessageChain([]message.Component{
		&message.Plain{Text: "hello "},
		&message.Plain{Text: "world"},
	})
	if text != "hello world" || hasAt {
		t.Fatalf("纯文本序列化错误: %q, hasAt=%v", text, hasAt)
	}

	// At 组件：优先使用 name
	text, hasAt = serializeMessageChain([]message.Component{
		&message.At{TargetID: "user-1", Name: "alice"},
	})
	if text != "@alice" || !hasAt {
		t.Fatalf("At 序列化错误: %q, hasAt=%v", text, hasAt)
	}

	// At 无 name 时使用 TargetID
	text, _ = serializeMessageChain([]message.Component{
		&message.At{TargetID: "user-1"},
	})
	if text != "@user-1" {
		t.Fatalf("At 无 name 序列化错误: %q", text)
	}

	// 图片/文件占位符
	text, _ = serializeMessageChain([]message.Component{
		&message.Image{URL: "https://x/y.png"},
		&message.File{Name: "doc.pdf"},
	})
	if text != "[图片][文件]" {
		t.Fatalf("图片/文件占位符错误: %q", text)
	}

	// Node 转发消息内的组件
	text, _ = serializeMessageChain([]message.Component{
		&message.Node{Content: []message.Component{
			&message.Plain{Text: "n1"},
			&message.Plain{Text: "n2"},
		}},
	})
	if text != "n1n2" {
		t.Fatalf("Node 内容序列化错误: %q", text)
	}
}

// ---------- 发送参数构造 ----------

func TestMessagePayloadBuilder(t *testing.T) {
	var b MessagePayloadBuilder

	// 聊天消息负载
	chat := b.BuildChatPayload("user-1", "hi", "file-1")
	if chat["toUserId"] != "user-1" || chat["text"] != "hi" || chat["fileId"] != "file-1" {
		t.Fatalf("聊天负载错误: %v", chat)
	}
	// 无文件时不带 fileId
	chat = b.BuildChatPayload("user-1", "hi", "")
	if _, ok := chat["fileId"]; ok {
		t.Fatalf("无文件时不应带 fileId: %v", chat)
	}

	// 房间消息负载
	room := b.BuildRoomPayload("room-1", "hello", "")
	if room["toRoomId"] != "room-1" || room["text"] != "hello" {
		t.Fatalf("房间负载错误: %v", room)
	}

	// 发帖负载
	note := b.BuildNotePayload("text", []string{"f1", "f2"}, map[string]interface{}{"visibility": "specified"})
	if note["text"] != "text" || note["visibility"] != "specified" {
		t.Fatalf("发帖负载错误: %v", note)
	}
	fileIDs, _ := note["fileIds"].([]string)
	if len(fileIDs) != 2 {
		t.Fatalf("fileIds 错误: %v", note["fileIds"])
	}
}

func TestAddAtMentionIfNeeded(t *testing.T) {
	userInfo := map[string]interface{}{"username": "alice", "nickname": "爱丽丝"}

	// 无 at 时自动添加 @username
	if got := AddAtMentionIfNeeded("你好", userInfo, false); got != "@alice\n你好" {
		t.Fatalf("自动添加提及错误: %q", got)
	}

	// 已包含 at 时不重复添加
	if got := AddAtMentionIfNeeded("@alice 你好", userInfo, true); got != "@alice 你好" {
		t.Fatalf("已含提及不应重复添加: %q", got)
	}

	// 无 username 时不添加（避免生成 @<user_id>）
	if got := AddAtMentionIfNeeded("你好", map[string]interface{}{"username": ""}, false); got != "你好" {
		t.Fatalf("无 username 不应添加提及: %q", got)
	}
}

func TestSessionIDHelpers(t *testing.T) {
	if !IsValidUserSessionID("chat%user-1") {
		t.Fatal("chat%user-1 应为有效用户会话")
	}
	if IsValidUserSessionID("note%user-1") {
		t.Fatal("note%user-1 不是用户会话")
	}
	if IsValidUserSessionID("chat%unknown") {
		t.Fatal("chat%unknown 不是有效会话")
	}
	if !IsValidRoomSessionID("room%room-1") {
		t.Fatal("room%room-1 应为有效房间会话")
	}
	if ExtractUserIDFromSessionID("chat%user-1") != "user-1" {
		t.Fatal("提取用户 ID 错误")
	}
	if ExtractRoomIDFromSessionID("room%room-1") != "room-1" {
		t.Fatal("提取房间 ID 错误")
	}
	if ExtractRoomIDFromSessionID("chat%user-1") != "chat%user-1" {
		t.Fatal("非房间会话应原样返回")
	}
}

func TestResolveMessageVisibility(t *testing.T) {
	// 从 user_cache 解析：specified 时合并 visible_user_ids 与收发双方
	userCache := map[string]map[string]interface{}{
		"user-1": {
			"visibility":       "specified",
			"visible_user_ids": []string{"user-2"},
		},
	}
	visibility, visibleIDs := resolveMessageVisibility("user-1", userCache, "bot-1", nil, "public")
	if visibility != "specified" {
		t.Fatalf("visibility 错误: %s", visibility)
	}
	seen := make(map[string]bool)
	for _, uid := range visibleIDs {
		seen[uid] = true
	}
	if !seen["user-1"] || !seen["bot-1"] || !seen["user-2"] {
		t.Fatalf("visible_user_ids 错误: %v", visibleIDs)
	}

	// 从 raw_message 解析（fallback）
	rawMessage := map[string]interface{}{
		"visibility":     "followers",
		"userId":         "sender-1",
		"visibleUserIds": []interface{}{"u1", "u2"},
	}
	visibility, _ = resolveMessageVisibility("", nil, "bot-1", rawMessage, "public")
	if visibility != "followers" {
		t.Fatalf("raw_message fallback visibility 错误: %s", visibility)
	}

	// 默认可见性
	visibility, _ = resolveMessageVisibility("", nil, "bot-1", nil, "public")
	if visibility != "public" {
		t.Fatalf("默认可见性错误: %s", visibility)
	}
}

// ---------- 消息转换（note / chat / room） ----------

func TestConvertNoteMessage(t *testing.T) {
	adapter := New(map[string]interface{}{
		"id": "misskey", "misskey_instance_url": "https://misskey.example", "misskey_token": "t",
	}, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	note := map[string]interface{}{
		"id":   "note-1",
		"text": "@astrbot 你好",
		"user": map[string]interface{}{"id": "user-1", "username": "alice", "name": "爱丽丝"},
		"files": []interface{}{
			map[string]interface{}{"url": "https://x/a.png", "name": "a.png", "type": "image/png"},
			map[string]interface{}{"url": "https://x/b.mp4", "name": "b.mp4", "type": "video/mp4"},
		},
	}
	abm := adapter.convertMessage(note)
	if abm.Type != platform.OtherMessage {
		t.Fatalf("note 消息类型错误: %s", abm.Type)
	}
	if abm.SessionID != "note%user-1" {
		t.Fatalf("session_id 错误: %s", abm.SessionID)
	}
	if abm.Sender.UserID != "user-1" || abm.Sender.Nickname != "爱丽丝" {
		t.Fatalf("发送者错误: %+v", abm.Sender)
	}
	// 组件: At + Plain + Image + Video
	if len(abm.Message) != 4 {
		t.Fatalf("组件数量错误: %d (%v)", len(abm.Message), abm.Message)
	}
	if abm.Message[0].Type() != message.CompAt {
		t.Fatalf("第一个组件应为 At: %v", abm.Message[0])
	}
	if abm.Message[2].Type() != message.CompImage {
		t.Fatalf("第三个组件应为 Image: %v", abm.Message[2])
	}
	if abm.Message[3].Type() != message.CompVideo {
		t.Fatalf("第四个组件应为 Video: %v", abm.Message[3])
	}
	// message_str 应包含剩余文本与文件描述
	if abm.MessageStr != "你好 图片[a.png] 视频[b.mp4]" {
		t.Fatalf("message_str 错误: %q", abm.MessageStr)
	}
	// 用户缓存应记录 reply_to_note_id
	userInfo := adapter.userCache["user-1"]
	if userInfo["reply_to_note_id"] != "note-1" {
		t.Fatalf("用户缓存 reply_to_note_id 错误: %v", userInfo)
	}
}

func TestConvertPollMessage(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"

	note := map[string]interface{}{
		"id":   "note-2",
		"text": "投票",
		"user": map[string]interface{}{"id": "user-1", "username": "alice"},
		"poll": map[string]interface{}{
			"multiple": true,
			"choices": []interface{}{
				map[string]interface{}{"text": "A", "votes": 3.0},
				map[string]interface{}{"text": "B", "votes": 1.0},
			},
		},
	}
	abm := adapter.convertMessage(note)
	foundPoll := false
	for _, comp := range abm.Message {
		if plain, ok := comp.(*message.Plain); ok && contains(plain.Text, "[投票]") {
			foundPoll = true
		}
	}
	if !foundPoll {
		t.Fatalf("投票文本未附加到消息: %v", abm.Message)
	}
	raw, _ := abm.RawMessage.(map[string]interface{})
	if _, ok := raw["poll"]; !ok {
		t.Fatal("raw_message 中应包含 poll")
	}
}

func TestConvertChatMessage(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"

	chat := map[string]interface{}{
		"id":         "msg-1",
		"fromUserId": "user-1",
		"fromUser":   map[string]interface{}{"id": "user-1", "username": "alice"},
		"text":       "私聊消息",
		"files": []interface{}{
			map[string]interface{}{"url": "https://x/c.txt", "name": "c.txt", "type": "text/plain"},
		},
	}
	abm := adapter.convertChatMessage(chat)
	if abm.Type != platform.FriendMessage {
		t.Fatalf("私聊类型错误: %s", abm.Type)
	}
	if abm.SessionID != "chat%user-1" {
		t.Fatalf("session_id 错误: %s", abm.SessionID)
	}
	if abm.MessageStr != "私聊消息" {
		t.Fatalf("message_str 错误: %q", abm.MessageStr)
	}
	if len(abm.Message) != 2 {
		t.Fatalf("组件数量错误: %d", len(abm.Message))
	}
}

func TestConvertRoomMessage(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	room := map[string]interface{}{
		"id":         "msg-2",
		"fromUserId": "user-1",
		"fromUser":   map[string]interface{}{"id": "user-1", "username": "alice"},
		"toRoomId":   "room-1",
		"toRoom":     map[string]interface{}{"name": "测试房间"},
		"text":       "@astrbot 群聊消息",
	}
	abm := adapter.convertRoomMessage(room)
	if abm.Type != platform.GroupMessage {
		t.Fatalf("群聊类型错误: %s", abm.Type)
	}
	if abm.SessionID != "room%room-1" {
		t.Fatalf("session_id 错误: %s", abm.SessionID)
	}
	if abm.Group == nil || abm.Group.GroupID != "room-1" {
		t.Fatalf("Group 错误: %+v", abm.Group)
	}
	// 提及机器人时应为 At + Plain
	if len(abm.Message) != 2 || abm.Message[0].Type() != message.CompAt {
		t.Fatalf("群聊提及组件错误: %v", abm.Message)
	}
	if abm.MessageStr != "群聊消息" {
		t.Fatalf("message_str 错误: %q", abm.MessageStr)
	}
	// 房间缓存
	if _, ok := adapter.userCache["room:room-1"]; !ok {
		t.Fatal("房间信息未缓存")
	}
}

func TestIsBotMentioned(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	// 文本提及
	if !adapter.isBotMentioned(map[string]interface{}{"text": "hi @astrbot"}) {
		t.Fatal("文本提及应命中")
	}
	// mentions 数组命中
	if !adapter.isBotMentioned(map[string]interface{}{
		"text":     "hi",
		"mentions": []interface{}{"bot-1"},
	}) {
		t.Fatal("mentions 数组应命中")
	}
	// 未提及
	if adapter.isBotMentioned(map[string]interface{}{"text": "hi"}) {
		t.Fatal("未提及不应命中")
	}
	// 回复机器人的 note：需文本也提及
	replyNote := map[string]interface{}{
		"text": "hello",
		"reply": map[string]interface{}{
			"user": map[string]interface{}{"id": "bot-1"},
		},
	}
	if adapter.isBotMentioned(replyNote) {
		t.Fatal("回复机器人但文本未提及不应命中")
	}
	replyNote["text"] = "@astrbot hello"
	if !adapter.isBotMentioned(replyNote) {
		t.Fatal("回复机器人且文本提及应命中")
	}
}

// ---------- streaming 帧解析 ----------

func TestStreamingFrameParsing(t *testing.T) {
	streaming := NewStreamingClient("https://misskey.example", "token")

	notifications := 0
	chatMessages := 0
	debugs := 0
	streaming.AddMessageHandler("notification", func(data map[string]interface{}) {
		notifications++
	})
	streaming.AddMessageHandler("main:notification", func(data map[string]interface{}) {
		notifications++
	})
	streaming.AddMessageHandler("newChatMessage", func(data map[string]interface{}) {
		chatMessages++
	})
	streaming.AddMessageHandler("messaging:newChatMessage", func(data map[string]interface{}) {
		chatMessages++
	})
	streaming.AddMessageHandler("_debug", func(data map[string]interface{}) {
		debugs++
	})

	// 预注册 channel_id -> channel_type 映射
	streaming.channels["chan-1"] = "main"
	streaming.channels["chan-2"] = "messaging"

	// 1. channel 帧：main:notification
	streaming.HandleMessage(map[string]interface{}{
		"type": "channel",
		"body": map[string]interface{}{
			"id":   "chan-1",
			"type": "notification",
			"body": map[string]interface{}{"type": "mention"},
		},
	})
	if notifications != 1 {
		t.Fatalf("notification 处理器调用次数错误: %d", notifications)
	}

	// 2. channel 帧：messaging:newChatMessage
	streaming.HandleMessage(map[string]interface{}{
		"type": "channel",
		"body": map[string]interface{}{
			"id":   "chan-2",
			"type": "newChatMessage",
			"body": map[string]interface{}{"text": "hi"},
		},
	})
	if chatMessages != 1 {
		t.Fatalf("newChatMessage 处理器调用次数错误: %d", chatMessages)
	}

	// 3. 未知 channel 事件 → _debug
	streaming.HandleMessage(map[string]interface{}{
		"type": "channel",
		"body": map[string]interface{}{
			"id": "chan-1", "type": "unknownEvent", "body": map[string]interface{}{},
		},
	})
	if debugs != 1 {
		t.Fatalf("_debug 处理器调用次数错误: %d", debugs)
	}

	// 4. 直接消息（非 channel）→ 按事件类型分发
	streaming.HandleMessage(map[string]interface{}{
		"type": "notification",
		"body": map[string]interface{}{"type": "follow"},
	})
	if notifications != 2 {
		t.Fatalf("直接 notification 分发错误: %d", notifications)
	}
}

func TestStreamingSubscribeChannel(t *testing.T) {
	// 未连接时订阅应报错
	streaming := NewStreamingClient("https://misskey.example", "token")
	if _, err := streaming.SubscribeChannel("main", nil); err == nil {
		t.Fatal("未连接时订阅应报错")
	}

	// 期望频道记录（供重连后重订阅）
	streaming.desiredChannels["main"] = nil
	if len(streaming.desiredChannels) != 1 {
		t.Fatalf("desiredChannels 记录错误: %v", streaming.desiredChannels)
	}
}

func TestStreamURL(t *testing.T) {
	if got := streamURL("https://misskey.example", "tok"); got != "wss://misskey.example/streaming?i=tok" {
		t.Fatalf("https 转换错误: %s", got)
	}
	if got := streamURL("http://192.168.1.10:3000", "tok"); got != "ws://192.168.1.10:3000/streaming?i=tok" {
		t.Fatalf("http 转换错误: %s", got)
	}
}

func TestFormatPoll(t *testing.T) {
	poll := map[string]interface{}{
		"multiple": false,
		"choices": []interface{}{
			map[string]interface{}{"text": "A", "votes": 3.0},
			map[string]interface{}{"text": "B", "votes": 1.0},
		},
	}
	got := FormatPoll(poll)
	if !contains(got, "[投票]") || !contains(got, "单选") || !contains(got, "(1) A [3票]") {
		t.Fatalf("投票格式化错误: %q", got)
	}
	if FormatPoll(nil) != "" {
		t.Fatal("空 poll 应返回空串")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOfStr(s, sub) >= 0)
}

func indexOfStr(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
