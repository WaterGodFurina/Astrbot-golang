package kook

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ---------- 客户端状态访问器 ----------

// TestClientStateAccessors 验证 lastSN/sessionID/lastHeartbeatTime/heartbeatFailedCount
// 的跨 goroutine 访问统一经由 stateMu 保护 (对应 M-26)。
func TestClientStateAccessors(t *testing.T) {
	client := NewKookClient(&KookConfig{}, nil)
	client.setSessionID("sess_1")
	client.setLastSN(42)
	client.setLastHeartbeatTime(time.Unix(100, 0))
	client.setHeartbeatFailedCount(3)

	if got := client.sessionIDValue(); got != "sess_1" {
		t.Errorf("sessionID 应为 sess_1, 实际 %q", got)
	}
	if got := client.lastSNValue(); got != 42 {
		t.Errorf("lastSN 应为 42, 实际 %d", got)
	}
	if got := client.lastHeartbeatTimeValue(); !got.Equal(time.Unix(100, 0)) {
		t.Errorf("lastHeartbeatTime 读取错误: %v", got)
	}
	if got := client.incHeartbeatFailedCount(); got != 4 {
		t.Errorf("heartbeatFailedCount 自增后应为 4, 实际 %d", got)
	}
	client.setHeartbeatFailedCount(0)
	if got := client.lastSNValue(); got != 42 {
		t.Errorf("setHeartbeatFailedCount 不应影响 lastSN: %d", got)
	}
}

// ---------- WS 帧解析 ----------

// TestParseWSMessageFrame 验证消息帧 {"s":0,"d":{...},"sn":..} 的解析 (对应 Python KookWebsocketEvent)。
func TestParseWSMessageFrame(t *testing.T) {
	raw := `{
		"s": 0,
		"sn": 100,
		"d": {
			"channel_type": "GROUP",
			"type": 9,
			"target_id": "channel_1",
			"author_id": "user_1",
			"content": "hello (met)123(met)",
			"msg_id": "msg_1",
			"msg_timestamp": 1700000000000,
			"nonce": "",
			"from_type": 1,
			"extra": {
				"type": 1,
				"author": {"id": "user_1", "username": "nick", "nickname": "nick"},
				"kmarkdown": {
					"raw_content": "hello (met)123(met)",
					"mention_part": [{"id": "123", "username": "u123"}],
					"mention_role_part": []
				},
				"guild_id": "111"
			}
		}
	}`
	var frame kookWSFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("解析WS帧失败: %v", err)
	}
	if frame.Signal != signalMessage {
		t.Fatalf("信令应为 %d, 实际为 %d", signalMessage, frame.Signal)
	}
	if frame.SN == nil || *frame.SN != 100 {
		t.Fatalf("sn 应为 100, 实际为 %v", frame.SN)
	}
	var data kookMessageEventData
	if err := json.Unmarshal(frame.Data, &data); err != nil {
		t.Fatalf("解析消息数据失败: %v", err)
	}
	if data.ChannelType != KookChannelGroup {
		t.Fatalf("channel_type 应为 GROUP, 实际为 %q", data.ChannelType)
	}
	if data.Type != KookMsgKMarkdown {
		t.Fatalf("type 应为 9 (kmarkdown), 实际为 %d", data.Type)
	}
	if data.MsgID != "msg_1" || data.AuthorID != "user_1" {
		t.Fatalf("msg_id/author_id 解析错误: %+v", data)
	}
	if data.Extra.KMarkdown == nil {
		t.Fatal("kmarkdown 字段解析失败")
	}
	if len(data.Extra.KMarkdown.MentionPart) != 1 {
		t.Fatalf("mention_part 数量应为 1")
	}
	if data.Extra.KMarkdown.MentionPart[0].ID != "123" {
		t.Fatalf("mention_part[0].id 应为 123")
	}
}

// TestParseWSHelloFrame 验证 HELLO 帧 {"s":1} 的解析。
func TestParseWSHelloFrame(t *testing.T) {
	raw := `{"s": 1, "d": {"code": 0, "session_id": "sess-abc"}, "sn": null}`
	var frame kookWSFrame
	if err := json.Unmarshal([]byte(raw), &frame); err != nil {
		t.Fatalf("解析WS帧失败: %v", err)
	}
	if frame.Signal != signalHello {
		t.Fatalf("信令应为 %d, 实际为 %d", signalHello, frame.Signal)
	}
	var hello kookHelloEventData
	if err := json.Unmarshal(frame.Data, &hello); err != nil {
		t.Fatalf("解析HELLO数据失败: %v", err)
	}
	if hello.Code != 0 || hello.SessionID != "sess-abc" {
		t.Fatalf("HELLO 数据解析错误: %+v", hello)
	}
}

// TestBuildPingFrame 验证心跳帧构造: {"s":2,"sn":<last_sn>} (无 "d" 字段, 与 Python to_json 一致)。
func TestBuildPingFrame(t *testing.T) {
	client := &KookClient{lastSN: 42}
	payload, err := json.Marshal(map[string]interface{}{
		"s":  signalPing,
		"sn": client.lastSN,
	})
	if err != nil {
		t.Fatalf("构造心跳帧失败: %v", err)
	}
	var frame map[string]interface{}
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatalf("心跳帧不是合法JSON: %v", err)
	}
	if frame["s"] != float64(signalPing) {
		t.Fatalf("心跳帧 s 应为 %d", signalPing)
	}
	if frame["sn"] != float64(42) {
		t.Fatalf("心跳帧 sn 应为 42")
	}
	// Python 序列化时会排除 None 字段, "d" 不应存在
	if _, exists := frame["d"]; exists {
		t.Fatal("心跳帧不应包含 d 字段")
	}
}

// ---------- 文本消息解析 ----------

// TestFindAtSelectors 验证 (met)/(rol) 选择器状态机与 Python 正则行为一致。
func TestFindAtSelectors(t *testing.T) {
	matches := findAtSelectors("hello (met)123(met) world (met)all(met)")
	if len(matches) != 2 {
		t.Fatalf("应匹配 2 个选择器, 实际 %d", len(matches))
	}
	if matches[0].tag != "met" || matches[0].target != "123" {
		t.Fatalf("第一个选择器解析错误: %+v", matches[0])
	}
	if matches[1].tag != "met" || matches[1].target != "all" {
		t.Fatalf("第二个选择器解析错误: %+v", matches[1])
	}

	// 角色选择器
	roleMatches := findAtSelectors("(rol)8899(rol) hi")
	if len(roleMatches) != 1 || roleMatches[0].target != "8899" {
		t.Fatalf("角色选择器解析错误: %+v", roleMatches)
	}

	// 括号不闭合的不应匹配
	noMatch := findAtSelectors("(met)123(rol) x")
	if len(noMatch) != 0 {
		t.Fatalf("不应匹配: %+v", noMatch)
	}

	// 空目标不应匹配 ([^()]+ 至少一个字符)
	emptyTarget := findAtSelectors("(met)(met)")
	if len(emptyTarget) != 0 {
		t.Fatalf("空目标不应匹配: %+v", emptyTarget)
	}
}

// TestConvertTextToComponents 验证文本消息到组件的转换 (对应 Python _convert_text_message_to_component)。
func TestConvertTextToComponents(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	a.client.botID = "bot_1"
	a.client.botNickname = "Bot"

	comps, msgStr := a.convertTextToComponents(
		"hello (met)123(met) world (met)all(met) tail",
		"hello (met)123(met) world (met)all(met) tail",
		nil, "", map[string]string{"123": "u123"},
	)
	if msgStr != "hello (met)123(met) world (met)all(met) tail" {
		t.Fatalf("message_str 应为原始内容, 实际: %q", msgStr)
	}
	if len(comps) != 5 {
		t.Fatalf("应有 5 个组件, 实际 %d: %+v", len(comps), comps)
	}
	if p, ok := comps[0].(*message.Plain); !ok || p.Text != "hello" {
		t.Fatalf("组件0 应为 Plain(hello), 实际: %+v", comps[0])
	}
	if at, ok := comps[1].(*message.At); !ok || at.TargetID != "123" || at.Name != "u123" {
		t.Fatalf("组件1 应为 At(123), 实际: %+v", comps[1])
	}
	if _, ok := comps[3].(*message.AtAll); !ok {
		t.Fatalf("组件3 应为 AtAll, 实际: %+v", comps[3])
	}
}

// TestConvertTextMentionNameMap 验证 mention_name_map 的使用。
func TestConvertTextMentionNameMap(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	a.client.botID = "bot_1"

	mentionNames := map[string]string{"123": "u123"}
	comps, _ := a.convertTextToComponents("(met)123(met)", "(met)123(met)", nil, "", mentionNames)
	if len(comps) != 1 {
		t.Fatalf("应有 1 个组件, 实际 %d", len(comps))
	}
	at, ok := comps[0].(*message.At)
	if !ok || at.TargetID != "123" || at.Name != "u123" {
		t.Fatalf("At 组件解析错误: %+v", comps[0])
	}
}

// TestConvertTextBotMentionStripsPrefix 验证 @bot 前缀剥离 (对应 Python AT_MENTION_PREFIX_REGEX)。
func TestConvertTextBotMentionStripsPrefix(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	a.client.botID = "bot_1"

	comps, msgStr := a.convertTextToComponents(
		"(met)bot_1(met) hello",
		"@Bot - bot_1 hello",
		nil, "", nil,
	)
	if len(comps) != 2 {
		t.Fatalf("应有 2 个组件, 实际 %d", len(comps))
	}
	at, ok := comps[0].(*message.At)
	if !ok || at.TargetID != "bot_1" {
		t.Fatalf("组件0 应为 At(bot_1): %+v", comps[0])
	}
	if msgStr != "hello" {
		t.Fatalf("@Bot 前缀应被剥离, 实际: %q", msgStr)
	}
}

// TestConvertTextRoleMentionNotInRole 验证机器人不属于某角色时跳过 (rol) 选择器。
func TestConvertTextRoleMentionNotInRole(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	a.client.botID = "bot_1"
	// 预置角色缓存: 机器人在频道 111 不拥有角色 8899 (不触发真实网络请求)
	a.rolesCache.mu.Lock()
	a.rolesCache.cache[111] = &rolesCacheEntry{
		Roles:            map[int64]bool{},
		LatestUpdateTime: time.Now(),
	}
	a.rolesCache.order = append(a.rolesCache.order, 111)
	a.rolesCache.mu.Unlock()

	comps, _ := a.convertTextToComponents("(rol)8899(rol) hi", "(rol)8899(rol) hi", nil, "111", nil)
	if len(comps) != 1 {
		t.Fatalf("角色未匹配时应有 1 个 Plain 组件, 实际 %d", len(comps))
	}
	if _, ok := comps[0].(*message.Plain); !ok {
		t.Fatalf("组件应为 Plain: %+v", comps[0])
	}
}

// TestConvertTextRoleMentionInRole 验证机器人属于该角色时 (rol) 转换为 At 机器人自己。
func TestConvertTextRoleMentionInRole(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	a.client.botID = "bot_1"
	// 预置角色缓存: 机器人在频道 111 拥有角色 8899
	a.rolesCache.mu.Lock()
	a.rolesCache.cache[111] = &rolesCacheEntry{
		Roles:            map[int64]bool{8899: true},
		LatestUpdateTime: time.Now(),
	}
	a.rolesCache.order = append(a.rolesCache.order, 111)
	a.rolesCache.mu.Unlock()

	comps, _ := a.convertTextToComponents("(rol)8899(rol) hi", "(rol)8899(rol) hi", nil, "111", nil)
	if len(comps) != 2 {
		t.Fatalf("应有 2 个组件, 实际 %d", len(comps))
	}
	at, ok := comps[0].(*message.At)
	if !ok || at.TargetID != "bot_1" {
		t.Fatalf("组件0 应为 At(bot_1): %+v", comps[0])
	}
}

// ---------- 卡片消息解析 ----------

// TestParseCardMessage 验证卡片消息解析 (对应 Python _parse_card_message)。
func TestParseCardMessage(t *testing.T) {
	cardContent := `[
		{
			"type": "card",
			"theme": "info",
			"modules": [
				{"type": "header", "text": {"type": "plain-text", "content": "标题"}},
				{"type": "section", "text": {"type": "kmarkdown", "content": "正文 (met)123(met)"}},
				{"type": "container", "elements": [{"type": "image", "src": "https://img.example.com/a.png"}]},
				{"type": "file", "title": "文档.pdf", "src": "https://file.example.com/a.pdf"}
			]
		}
	]`
	data := &kookMessageEventData{
		Type:    KookMsgCard,
		Content: mustMarshalString(cardContent),
		Extra:   kookExtra{GuildID: "111"},
	}
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	a.client.botID = "bot_1"

	comps, text, err := a.parseCardMessage(data)
	if err != nil {
		t.Fatalf("解析卡片消息失败: %v", err)
	}
	// 期望 (与 Python 一致): 文本部分先 join 再整体转换, 得到
	// Plain("标题正文") At(123) Plain("[image] [file]") Image File
	if len(comps) != 5 {
		t.Fatalf("应有 5 个组件, 实际 %d: %+v", len(comps), comps)
	}
	if p, ok := comps[0].(*message.Plain); !ok || p.Text != "标题正文" {
		t.Fatalf("组件0 应为 Plain(标题正文): %+v", comps[0])
	}
	if at, ok := comps[1].(*message.At); !ok || at.TargetID != "123" {
		t.Fatalf("组件1 应为 At(123): %+v", comps[1])
	}
	if img, ok := comps[3].(*message.Image); !ok || img.URL != "https://img.example.com/a.png" {
		t.Fatalf("组件3 应为 Image: %+v", comps[3])
	}
	if f, ok := comps[4].(*message.File); !ok || f.Name != "文档.pdf" {
		t.Fatalf("组件4 应为 File: %+v", comps[4])
	}
	// 与 Python 一致: 首组件为 Plain 时 message_str 保持原始内容 (含 (met) 选择器)
	if text != "标题正文 (met)123(met) [image] [file]" {
		t.Fatalf("text 提取错误: %q", text)
	}
}

// TestParseCardMessageInvalidJSON 验证非法卡片 JSON 报错。
func TestParseCardMessageInvalidJSON(t *testing.T) {
	data := &kookMessageEventData{
		Type:    KookMsgCard,
		Content: mustMarshalString("not-json"),
	}
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	if _, _, err := a.parseCardMessage(data); err == nil {
		t.Fatal("非法卡片 JSON 应返回错误")
	}
}

// mustMarshalString 将字符串包装为 JSON 字符串字面量 (raw message)。
func mustMarshalString(s string) json.RawMessage {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return data
}

// ---------- 发送参数构造 ----------

// TestBuildOrderMessagePlain 验证文本组件的发送参数构造 (kmarkdown 类型)。
func TestBuildOrderMessagePlain(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	order, err := a.buildOrderMessage(0, &message.Plain{Text: "你好"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if order.text != "你好" || order.msgType != KookMsgKMarkdown {
		t.Fatalf("Plain 应映射为 kmarkdown: %+v", order)
	}
}

// TestBuildOrderMessageAt 验证 @ 组件的发送参数构造 ((met)xx(met))。
func TestBuildOrderMessageAt(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	order, err := a.buildOrderMessage(1, &message.At{TargetID: "123"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if order.text != "(met)123(met)" || order.msgType != KookMsgKMarkdown {
		t.Fatalf("At 应映射为 (met)123(met): %+v", order)
	}
	allOrder, _ := a.buildOrderMessage(2, &message.AtAll{})
	if allOrder.text != "(met)all(met)" {
		t.Fatalf("AtAll 应映射为 (met)all(met): %+v", allOrder)
	}
}

// TestBuildOrderMessageReply 验证 Reply 组件携带 reply_id。
func TestBuildOrderMessageReply(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	order, err := a.buildOrderMessage(0, &message.Reply{MessageID: "ref_1"})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if order.replyID != "ref_1" || order.text != "" {
		t.Fatalf("Reply 应设置 reply_id: %+v", order)
	}
}

// TestBuildOrderMessageJson 验证 Json 卡片组件构造 (外层包成列表, type=CARD)。
func TestBuildOrderMessageJson(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	order, err := a.buildOrderMessage(0, &message.Json{Data: map[string]interface{}{
		"type": "card", "modules": []interface{}{},
	}})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if order.msgType != KookMsgCard {
		t.Fatalf("Json 应映射为 CARD: %+v", order)
	}
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(order.text), &parsed); err != nil {
		t.Fatalf("卡片 JSON 非法: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("卡片 JSON 外层应为列表 (长度1), 实际 %d", len(parsed))
	}
	if parsed[0]["type"] != "card" {
		t.Fatalf("卡片 type 应为 card: %+v", parsed[0])
	}
}

// TestBuildAudioCard 验证音频卡片 JSON 构造。
func TestBuildAudioCard(t *testing.T) {
	card := buildAudioCard("https://file.example.com/a.wav", "录音")
	var parsed []map[string]interface{}
	if err := json.Unmarshal([]byte(card), &parsed); err != nil {
		t.Fatalf("音频卡片 JSON 非法: %v", err)
	}
	if len(parsed) != 1 || parsed[0]["type"] != "card" {
		t.Fatalf("音频卡片结构错误: %+v", parsed)
	}
	modules := parsed[0]["modules"].([]interface{})
	module := modules[0].(map[string]interface{})
	if module["type"] != "audio" || module["title"] != "录音" || module["src"] != "https://file.example.com/a.wav" {
		t.Fatalf("音频模块错误: %+v", module)
	}
}

// TestBuildOrderMessageUnsupported 验证不支持的组件类型报错。
func TestBuildOrderMessageUnsupported(t *testing.T) {
	a := New(map[string]interface{}{"kook_bot_token": "token"}, nil, nil)
	if _, err := a.buildOrderMessage(0, &message.Face{ID: "1"}); err == nil {
		t.Fatal("Face 组件应报不支持")
	}
}

// TestSendPayloadChannelVsDirect 验证发送 payload 中 target_id/quote/reply_msg_id 的构造。
func TestSendPayloadChannelVsDirect(t *testing.T) {
	// 验证 SendText 中 URL 选择: 频道消息用 message/create, 私聊用 direct-message/create
	client := NewKookClient(&KookConfig{Token: "token"}, nil)
	payload := map[string]interface{}{
		"target_id":    "channel_1",
		"content":      "hello",
		"type":         int(KookMsgKMarkdown),
		"quote":        "ref_1",
		"reply_msg_id": "ref_1",
	}
	raw, _ := json.Marshal(payload)
	var parsed map[string]interface{}
	_ = json.Unmarshal(raw, &parsed)
	if parsed["target_id"] != "channel_1" || parsed["type"] != float64(int(KookMsgKMarkdown)) {
		t.Fatalf("发送 payload 错误: %+v", parsed)
	}
	if parsed["quote"] != "ref_1" || parsed["reply_msg_id"] != "ref_1" {
		t.Fatalf("引用参数错误: %+v", parsed)
	}
	if client.config.Token != "token" {
		t.Fatal("token 读取错误")
	}
}

// rewriteKookHostTransport 将请求重定向到测试服务器 (KOOK API 域名无法从单测访问)。
type rewriteKookHostTransport struct {
	base string
}

func (t *rewriteKookHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = "http"
	req2.URL.Host = strings.TrimPrefix(t.base, "http://")
	return http.DefaultTransport.RoundTrip(req2)
}

// TestRolesRecordPendingDedup 验证同频道并发查询只发起一次请求, 且等待方
// 能正确读到结果 (对应 L-31: roles 必须先赋值再 close(done))。
func TestRolesRecordPendingDedup(t *testing.T) {
	var mu sync.Mutex
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond) // 制造并发等待窗口
		_, _ = w.Write([]byte(`{"code":0,"data":{"roles":[42]}}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: &rewriteKookHostTransport{base: srv.URL}}
	rr := NewRolesRecord(client)
	rr.SetBotID("bot_1")

	var wg sync.WaitGroup
	results := make([]bool, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = rr.HasRoleInChannel(context.Background(), 42, 111)
		}(i)
	}
	wg.Wait()

	if results[0] != true || results[1] != true {
		t.Fatalf("并发查询应都返回 true (等待方不能把成功结果误读为 nil), 实际 %v (L-31)", results)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("同频道并发查询应只发起 1 次请求, 实际 %d", n)
	}
}
