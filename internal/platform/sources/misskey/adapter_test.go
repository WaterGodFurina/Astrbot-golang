// Package misskey - Misskey 平台适配器单元测试。
// 测试不连接真实网络：消息解析 / 发送参数构造 / streaming 帧解析用纯函数与 mock 验证；
// go-misskey 的 API 调用不测试。
package misskey

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/gorilla/websocket"
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

	// 连接后订阅成功并记录 channel_id；重连会清空旧 channel 映射，避免残留
	server := startTestWSServer(t)
	streaming.instanceURL = server.URL
	if !streaming.Connect() {
		t.Fatal("连接失败")
	}
	defer streaming.Disconnect()

	channelID, err := streaming.SubscribeChannel("main", nil)
	if err != nil {
		t.Fatalf("已连接订阅失败: %v", err)
	}
	if _, ok := streaming.channels[channelID]; !ok {
		t.Fatalf("channel_id 未记录: %v", streaming.channels)
	}
	if len(streaming.channels) != 1 {
		t.Fatalf("订阅后 channels 数量错误: %d", len(streaming.channels))
	}

	streaming.UnsubscribeChannel(channelID)
	if _, ok := streaming.channels[channelID]; ok {
		t.Fatal("退订后 channel_id 应被移除")
	}
}

// startTestWSServer 启动一个接受 WebSocket 升级并等待断开连接的本地测试服务端。
func startTestWSServer(t *testing.T) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
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

// ---------- M-30：数值配置兼容 float64 ----------

func TestIntVal(t *testing.T) {
	cfg := map[string]interface{}{
		"int":    5,
		"float":  6.0,
		"number": json.Number("7"),
		"str":    "8",
	}
	if v := intVal(cfg, "int", 0); v != 5 {
		t.Fatalf("int 值错误: %d", v)
	}
	if v := intVal(cfg, "float", 0); v != 6 {
		t.Fatalf("float64 值错误: %d", v)
	}
	if v := intVal(cfg, "number", 0); v != 7 {
		t.Fatalf("json.Number 值错误: %d", v)
	}
	if v := intVal(cfg, "str", 9); v != 9 {
		t.Fatalf("非法类型应回落默认值: %d", v)
	}
	if v := intVal(cfg, "missing", 10); v != 10 {
		t.Fatalf("缺失键应回落默认值: %d", v)
	}
}

func TestNewReadsFloatConfig(t *testing.T) {
	// JSON 反序列化后数值均为 float64，配置必须生效而非回落默认值
	a := New(map[string]interface{}{
		"id":                          "misskey",
		"misskey_instance_url":        "https://misskey.example",
		"misskey_token":               "t",
		"max_message_length":          float64(1200),
		"misskey_download_timeout":    float64(20),
		"misskey_download_chunk_size": float64(8192),
		"misskey_max_download_bytes":  float64(10485760),
		"misskey_upload_concurrency":  float64(5),
	}, nil, nil)
	if a.maxMessageLength != 1200 {
		t.Fatalf("max_message_length 错误: %d", a.maxMessageLength)
	}
	if a.downloadTimeout != 20 {
		t.Fatalf("download_timeout 错误: %d", a.downloadTimeout)
	}
	if a.downloadChunkSize != 8192 {
		t.Fatalf("download_chunk_size 错误: %d", a.downloadChunkSize)
	}
	if a.maxDownloadBytes != 10485760 {
		t.Fatalf("max_download_bytes 错误: %d", a.maxDownloadBytes)
	}

	// 未配置时应回落默认值
	def := New(nil, nil, nil)
	if def.maxMessageLength != 3000 || def.downloadTimeout != 15 ||
		def.downloadChunkSize != 64*1024 || def.maxDownloadBytes != 0 {
		t.Fatalf("默认值错误: %d %d %d %d", def.maxMessageLength, def.downloadTimeout, def.downloadChunkSize, def.maxDownloadBytes)
	}
	// 默认不允许不安全的 TLS 降级
	if def.allowInsecureDownloads {
		t.Fatal("allow_insecure_downloads 默认应为关闭")
	}

	// upload_concurrency 经 intVal 生效且受上限约束
	a.config["misskey_upload_concurrency"] = float64(99)
	if v := intVal(a.config, "misskey_upload_concurrency", defaultUploadConcurrency); v != 99 {
		t.Fatalf("upload_concurrency 错误: %d", v)
	}
}

// ---------- M-31：pong 续期读超时 ----------

func TestKeepalivePongHandlerExtendsReadDeadline(t *testing.T) {
	server := startTestWSServer(t)
	streaming := NewStreamingClient(server.URL, "token")
	if !streaming.Connect() {
		t.Fatal("连接失败")
	}
	defer streaming.Disconnect()

	// 模拟 Listen 安装的 pong 处理器
	streaming.conn.SetPongHandler(keepalivePongHandler(streaming.conn))
	// 先将读超时设到 300ms 后，然后由 pong 续期
	streaming.conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))

	readResult := make(chan error, 1)
	go func() {
		_, _, err := streaming.conn.ReadMessage()
		readResult <- err
	}()

	time.Sleep(100 * time.Millisecond)
	if err := keepalivePongHandler(streaming.conn)(""); err != nil {
		t.Fatalf("pong 处理失败: %v", err)
	}

	select {
	case err := <-readResult:
		t.Fatalf("pong 续期后读仍返回错误（读超时未刷新）: %v", err)
	case <-time.After(400 * time.Millisecond):
		// 仍在阻塞读取，说明 pong 已将读超时续期
	}
	// 关闭连接以解除阻塞中的读 goroutine
	streaming.conn.Close()
}

func TestStreamingStaysAliveWithPongs(t *testing.T) {
	// 服务端对每条 ping 回 pong；客户端 Listen 期间不应因读超时断开
	upgrader := websocket.Upgrader{
		CheckOrigin: func(*http.Request) bool { return true },
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetPingHandler(func(appData string) error {
			return conn.WriteControl(websocket.PongMessage, []byte(appData), time.Now().Add(time.Second))
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	streaming := NewStreamingClient(server.URL, "token")
	if !streaming.Connect() {
		t.Fatal("连接失败")
	}
	defer streaming.Disconnect()

	done := make(chan struct{})
	go func() {
		streaming.Listen()
		close(done)
	}()
	// 覆盖 2 个心跳周期：若 pong 不续期读超时，Listen 会提前返回
	select {
	case <-done:
		t.Fatal("收到 pong 后连接仍被误判失联")
	case <-time.After(2500 * time.Millisecond):
	}
}

// ---------- M-48：重连清空旧 channel 映射 ----------

func TestConnectClearsChannels(t *testing.T) {
	server := startTestWSServer(t)
	streaming := NewStreamingClient(server.URL, "token")
	if !streaming.Connect() {
		t.Fatal("连接失败")
	}
	defer streaming.Disconnect()
	if _, err := streaming.SubscribeChannel("main", nil); err != nil {
		t.Fatalf("订阅失败: %v", err)
	}
	if len(streaming.channels) != 1 {
		t.Fatalf("订阅后 channels 数量错误: %d", len(streaming.channels))
	}
	// 重连：旧 channel_id 必须被清空
	if !streaming.Connect() {
		t.Fatal("重连失败")
	}
	if len(streaming.channels) != 0 {
		t.Fatalf("重连后 channels 未清空: %v", streaming.channels)
	}
}

// ---------- M-49：SSRF 校验 ----------

func TestValidateDownloadURL(t *testing.T) {
	api := NewMisskeyAPI("https://misskey.example", "t", false, 15, 64*1024, 0)

	// 非 http(s) scheme 拒绝
	if err := api.validateDownloadURL("ftp://example.com/a.png"); err == nil {
		t.Fatal("非 http(s) URL 应被拒绝")
	}
	// 环回地址拒绝
	if err := api.validateDownloadURL("http://127.0.0.1/a.png"); err == nil {
		t.Fatal("环回地址应被拒绝")
	}
	// 私网地址拒绝
	if err := api.validateDownloadURL("http://192.168.1.10/a.png"); err == nil {
		t.Fatal("私网地址应被拒绝")
	}
	// 链路本地/云元数据地址拒绝
	if err := api.validateDownloadURL("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatal("云元数据地址应被拒绝")
	}
	// 本机 misskey 实例域名放行（自建内网实例仍可下载本站文件）
	if err := api.validateDownloadURL("https://misskey.example/files/1.png"); err != nil {
		t.Fatalf("实例域名应放行: %v", err)
	}
}

func TestDownloadRedirectGuard(t *testing.T) {
	api := NewMisskeyAPI("https://misskey.example", "t", false, 15, 64*1024, 0)
	client := api.downloadClient(true)
	if client.CheckRedirect == nil {
		t.Fatal("下载客户端应配置重定向校验")
	}
	req, _ := http.NewRequest("GET", "http://192.168.1.1/x", nil)
	if err := client.CheckRedirect(req, []*http.Request{req}); err == nil {
		t.Fatal("重定向到内网地址应被拒绝")
	}
	req2, _ := http.NewRequest("GET", "https://8.8.8.8/x", nil)
	if err := client.CheckRedirect(req2, []*http.Request{req2}); err != nil {
		t.Fatalf("重定向到公网地址应放行: %v", err)
	}
}

// ── L-36：Start 配置错误返回 error ───────────────────────────────

func TestStartReturnsErrorOnBadConfig(t *testing.T) {
	a := New(nil, nil, nil)
	if err := a.Start(context.Background()); err == nil {
		t.Fatal("配置不完整时 Start 应返回错误")
	}
}

// ── L-36：按 []rune 截断 ─────────────────────────────────────────

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("你好世界", 2); got != "你好" {
		t.Errorf("truncateRunes(你好世界,2) = %q", got)
	}
	if got := truncateRunes("hello", 10); got != "hello" {
		t.Errorf("truncateRunes(hello,10) = %q", got)
	}
	if got := truncateRunes("你好世界", 3); len([]rune(got)) != 3 {
		t.Errorf("截断结果应为 3 个字符，得到 %q (%d runes)", got, len([]rune(got)))
	}
}

// ── L-38：createdAt 时间戳解析 ───────────────────────────────────

func TestParseMisskeyCreatedAt(t *testing.T) {
	ts, ok := parseMisskeyCreatedAt("2024-01-02T03:04:05.000Z")
	if !ok || ts != 1704164645 {
		t.Fatalf("parseMisskeyCreatedAt = %d, ok=%v", ts, ok)
	}
	if _, ok := parseMisskeyCreatedAt("not-a-time"); ok {
		t.Fatal("非法时间应返回 false")
	}
	if _, ok := parseMisskeyCreatedAt(nil); ok {
		t.Fatal("nil 应返回 false")
	}
}

func TestConvertNoteMessageTimestamp(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	note := map[string]interface{}{
		"id":        "note-1",
		"text":      "@astrbot 你好",
		"createdAt": "2024-01-02T03:04:05.000Z",
		"user":      map[string]interface{}{"id": "user-1", "username": "alice"},
	}
	abm := adapter.convertMessage(note)
	if abm.Timestamp != 1704164645 {
		t.Fatalf("createdAt 应被解析为时间戳，得到 %d", abm.Timestamp)
	}
}
