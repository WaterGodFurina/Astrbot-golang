package satori

// 单元测试：事件解析 / 元素转换 / 发送构造 / 自消息过滤 / HTTP 请求。
// 网络调用仅通过 httptest 覆盖。

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	satorilib "github.com/FloatTech/satori-go"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// fakeEventBus 捕获发布的 core.Event，用于验证发布路径。
type fakeEventBus struct {
	events []*core.Event
}

func (f *fakeEventBus) Publish(e *core.Event) error {
	f.events = append(f.events, e)
	return nil
}

func newTestAdapter() *Adapter {
	return New(map[string]interface{}{
		"id":                    "satori_test",
		"satori_api_base_url":   "http://127.0.0.1:5140/satori/v1",
		"satori_token":          "test-token",
		"satori_endpoint":       "ws://127.0.0.1:5140/satori/v1/events",
		"satori_auto_reconnect": false,
	}, nil, nil)
}

// ---------------------------------------------------------------------------
// 事件解析
// ---------------------------------------------------------------------------

func TestConvertSatoriMessageGroup(t *testing.T) {
	messageData := map[string]interface{}{
		"id":      "msg-1",
		"content": "hello <at id=\"10001\"/> world <img src=\"https://example.com/a.png\"/>",
	}
	user := map[string]interface{}{"id": "u1", "nick": "小明"}
	channel := map[string]interface{}{"id": "c1"}
	guild := map[string]interface{}{"id": "g1", "name": "测试群"}
	login := map[string]interface{}{"user": map[string]interface{}{"id": "bot1"}, "platform": "qq"}

	abm := convertSatoriMessage(messageData, user, channel, guild, login, 1700000000, true)

	if abm.Type != platform.GroupMessage {
		t.Errorf("期望群消息，实际: %s", abm.Type)
	}
	if abm.GroupID() != "g1" {
		t.Errorf("期望 group_id=g1，实际: %s", abm.GroupID())
	}
	if abm.SessionID != "c1" {
		t.Errorf("期望 session_id=c1，实际: %s", abm.SessionID)
	}
	if abm.MessageID != "msg-1" {
		t.Errorf("期望 message_id=msg-1，实际: %s", abm.MessageID)
	}
	if abm.SelfID != "bot1" {
		t.Errorf("期望 self_id=bot1，实际: %s", abm.SelfID)
	}
	if abm.Sender.UserID != "u1" || abm.Sender.Nickname != "小明" {
		t.Errorf("发送者解析错误: %+v", abm.Sender)
	}
	if abm.Timestamp != 1700000000 {
		t.Errorf("期望时间戳 1700000000，实际: %d", abm.Timestamp)
	}

	// 消息链: Plain("hello ") + At + Plain(" world ") + Image
	if len(abm.Message) != 4 {
		t.Fatalf("期望 4 个组件，实际 %d: %+v", len(abm.Message), abm.Message)
	}
	if p, ok := abm.Message[0].(*message.Plain); !ok || p.Text != "hello " {
		t.Errorf("组件0 期望 Plain(\"hello \")，实际: %+v", abm.Message[0])
	}
	if at, ok := abm.Message[1].(*message.At); !ok || at.TargetID != "10001" {
		t.Errorf("组件1 期望 At(10001)，实际: %+v", abm.Message[1])
	}
	if img, ok := abm.Message[3].(*message.Image); !ok || img.URL != "https://example.com/a.png" {
		t.Errorf("组件3 期望 Image，实际: %+v", abm.Message[3])
	}
	if abm.MessageStr != "hello  world " {
		t.Errorf("期望消息文本 \"hello  world \"，实际: %q", abm.MessageStr)
	}
}

func TestConvertSatoriMessageFriend(t *testing.T) {
	messageData := map[string]interface{}{"id": "m2", "content": "私聊消息"}
	user := map[string]interface{}{"id": "u2", "name": "张三"}
	channel := map[string]interface{}{"id": "c2"}
	login := map[string]interface{}{"user": map[string]interface{}{"id": "bot1"}}

	abm := convertSatoriMessage(messageData, user, channel, nil, login, 0, false)

	if abm.Type != platform.FriendMessage {
		t.Errorf("期望私聊消息，实际: %s", abm.Type)
	}
	if abm.SessionID != "c2" {
		t.Errorf("期望 session_id=c2，实际: %s", abm.SessionID)
	}
	// guild 为 nil 时昵称回退到 name
	if abm.Sender.Nickname != "张三" {
		t.Errorf("期望昵称 张三，实际: %s", abm.Sender.Nickname)
	}
	// 无时间戳时使用当前时间
	if abm.Timestamp == 0 {
		t.Errorf("期望默认时间戳，实际为 0")
	}
}

func TestConvertSatoriMessageWithQuote(t *testing.T) {
	messageData := map[string]interface{}{
		"id":      "m3",
		"content": `<quote id="q1"><author id="u9" nick="被引用者"/>引用的内容</quote>after <at id="a1"/>`,
	}
	user := map[string]interface{}{"id": "u3", "nick": "小明"}
	channel := map[string]interface{}{"id": "c3"}
	guild := map[string]interface{}{"id": "g3"}
	login := map[string]interface{}{"user": map[string]interface{}{"id": "bot1"}}

	abm := convertSatoriMessage(messageData, user, channel, guild, login, 0, false)

	// 第一个组件应为 Reply（引用消息）
	if len(abm.Message) < 1 {
		t.Fatalf("期望至少 1 个组件")
	}
	reply, ok := abm.Message[0].(*message.Reply)
	if !ok {
		t.Fatalf("组件0 期望 Reply，实际: %+v", abm.Message[0])
	}
	if reply.MessageID != "q1" {
		t.Errorf("期望引用消息 id=q1，实际: %s", reply.MessageID)
	}
	// 引用内容由 quote 标签内部内容解析而来
	if !strings.Contains(reply.MessageStr, "引用的内容") {
		t.Errorf("期望引用内容包含 \"引用的内容\"，实际: %q", reply.MessageStr)
	}
	// XML 提取路径没有 author 信息（对齐 Python），使用默认发送者
	if reply.SenderID != "" || reply.SenderNick != "内容" {
		t.Errorf("期望默认发送者，实际: %s/%s", reply.SenderID, reply.SenderNick)
	}
	// 消息链还应包含 quote 之后的文本（Python 中序列化不匹配时 quote 保留、tail 仍被解析）
	if len(abm.Message) < 2 {
		t.Fatalf("期望至少 2 个组件")
	}
	if p, ok := abm.Message[1].(*message.Plain); !ok || !strings.Contains(p.Text, "after") {
		t.Errorf("组件1 期望包含 after 文本，实际: %+v", abm.Message[1])
	}
}

// TestConvertSatoriMessageWithQuoteObject 覆盖消息自带 quote 对象（含 author 信息）的路径。
func TestConvertSatoriMessageWithQuoteObject(t *testing.T) {
	messageData := map[string]interface{}{
		"id":      "m4",
		"content": "回复内容",
		"quote": map[string]interface{}{
			"id":      "orig-msg",
			"author":  map[string]interface{}{"id": "u9", "nick": "被引用者"},
			"content": "原始消息文本",
		},
	}
	user := map[string]interface{}{"id": "u3"}
	channel := map[string]interface{}{"id": "c3"}
	login := map[string]interface{}{"user": map[string]interface{}{"id": "bot1"}}

	abm := convertSatoriMessage(messageData, user, channel, nil, login, 0, false)

	reply, ok := abm.Message[0].(*message.Reply)
	if !ok {
		t.Fatalf("组件0 期望 Reply，实际: %+v", abm.Message[0])
	}
	if reply.MessageID != "orig-msg" {
		t.Errorf("期望引用 id=orig-msg，实际: %s", reply.MessageID)
	}
	if reply.SenderID != "u9" || reply.SenderNick != "被引用者" {
		t.Errorf("期望引用发送者 u9/被引用者，实际: %s/%s", reply.SenderID, reply.SenderNick)
	}
	if !strings.Contains(reply.MessageStr, "原始消息文本") {
		t.Errorf("期望引用内容，实际: %q", reply.MessageStr)
	}
}

// ---------------------------------------------------------------------------
// 元素转换
// ---------------------------------------------------------------------------

func TestParseSatoriElements(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		expected []message.Component
	}{
		{
			name:    "纯文本",
			content: "hello world",
			expected: []message.Component{
				&message.Plain{Text: "hello world"},
			},
		},
		{
			name:    "at 元素",
			content: `<at id="12345"/>`,
			expected: []message.Component{
				&message.At{TargetID: "12345", Name: "12345"},
			},
		},
		{
			name:    "at 仅 name",
			content: `<at name="test"/>`,
			expected: []message.Component{
				&message.At{TargetID: "test", Name: "test"},
			},
		},
		{
			name:    "图片元素",
			content: `<img src="https://example.com/x.png"/>`,
			expected: []message.Component{
				&message.Image{URL: "https://example.com/x.png"},
			},
		},
		{
			name:    "文件元素",
			content: `<file src="https://example.com/f.pdf" name="报告"/>`,
			expected: []message.Component{
				&message.File{Name: "报告", URL: "https://example.com/f.pdf"},
			},
		},
		{
			name:    "语音元素",
			content: `<audio src="https://example.com/v.wav"/>`,
			expected: []message.Component{
				&message.Record{URL: "https://example.com/v.wav"},
			},
		},
		{
			name:    "表情元素（有名称）",
			content: `<face name="大笑"/>`,
			expected: []message.Component{
				&message.Plain{Text: "[表情:大笑]"},
			},
		},
		{
			name:    "表情元素（有 id 与类型）",
			content: `<face id="100" type="1"/>`,
			expected: []message.Component{
				&message.Plain{Text: "[表情ID:100,类型:1]"},
			},
		},
		{
			name:    "ark 卡片",
			content: `<ark data="&#123;&quot;x&quot;:1&#125;"/>`,
			expected: []message.Component{
				&message.Plain{Text: `[ARK卡片数据: {"x":1}]`},
			},
		},
		{
			name:    "未知标签递归",
			content: `<custom><foo/>inner</custom>`,
			expected: []message.Component{
				&message.Plain{Text: "inner"},
			},
		},
		{
			name:    "混合内容",
			content: `a<at id="1"/>b`,
			expected: []message.Component{
				&message.Plain{Text: "a"},
				&message.At{TargetID: "1", Name: "1"},
				&message.Plain{Text: "b"},
			},
		},
		{
			name:    "命名空间前缀",
			content: `<at:at id="ns1"/>`,
			expected: []message.Component{
				&message.At{TargetID: "ns1", Name: "ns1"},
			},
		},
		{
			name:    "解析失败回退纯文本",
			content: `含 & 未转义 <的文本`,
			expected: []message.Component{
				&message.Plain{Text: `含 & 未转义 <的文本`},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSatoriElements(tc.content)
			if len(got) != len(tc.expected) {
				t.Fatalf("期望 %d 个组件，实际 %d: %+v", len(tc.expected), len(got), got)
			}
			for i, exp := range tc.expected {
				if got[i].Type() != exp.Type() {
					t.Fatalf("组件 %d 类型不匹配: 期望 %s 实际 %s", i, exp.Type(), got[i].Type())
				}
				// 类型一致时逐字段比较（序列化比较最稳妥）
				if got[i].String() != exp.String() {
					t.Fatalf("组件 %d 内容不匹配: 期望 %q 实际 %q", i, exp.String(), got[i].String())
				}
				// 图片/文件等再精确比对 URL 字段
				switch g := got[i].(type) {
				case *message.Image:
					if e, ok := exp.(*message.Image); ok && g.URL != e.URL {
						t.Fatalf("组件 %d Image.URL 不匹配: 期望 %q 实际 %q", i, e.URL, g.URL)
					}
				case *message.File:
					if e, ok := exp.(*message.File); ok && (g.Name != e.Name || g.URL != e.URL) {
						t.Fatalf("组件 %d File 不匹配: %+v vs %+v", i, e, g)
					}
				case *message.At:
					if e, ok := exp.(*message.At); ok && (g.TargetID != e.TargetID || g.Name != e.Name) {
						t.Fatalf("组件 %d At 不匹配: %+v vs %+v", i, e, g)
					}
				}
			}
		})
	}
}

func TestExtractQuoteElement(t *testing.T) {
	content := `<quote id="q1"><author id="u9" nick="小明"/>引用内容</quote>之后文本`
	qi := extractQuoteElement(content)
	if qi == nil {
		t.Fatalf("期望提取到 quote")
	}
	if id, _ := qi.quote["id"].(string); id != "q1" {
		t.Errorf("期望 quote id=q1，实际: %v", qi.quote["id"])
	}

	// 序列化包含子元素 tail（对齐 Python ET.tostring），与原文一致时 quote 被移除
	if strings.Contains(qi.contentWithoutQuote, "<quote") {
		t.Errorf("期望移除 quote 标签，实际: %q", qi.contentWithoutQuote)
	}
	if !strings.Contains(qi.contentWithoutQuote, "之后文本") {
		t.Errorf("期望保留后续文本，实际: %q", qi.contentWithoutQuote)
	}

	// 纯文本 quote（无子元素、无 tail）时序列化与原文一致，应被移除
	simple := `<quote id="q2">简单引用</quote>之后`
	qi2 := extractQuoteElement(simple)
	if qi2 == nil {
		t.Fatalf("期望提取到简单 quote")
	}
	if id, _ := qi2.quote["id"].(string); id != "q2" {
		t.Errorf("期望 quote id=q2，实际: %v", qi2.quote["id"])
	}
	if strings.Contains(qi2.contentWithoutQuote, "<quote") {
		t.Errorf("期望移除简单 quote 标签，实际: %q", qi2.contentWithoutQuote)
	}
	if !strings.Contains(qi2.contentWithoutQuote, "之后") {
		t.Errorf("期望保留后续文本，实际: %q", qi2.contentWithoutQuote)
	}

	// 正则回退路径（非法 XML 触发：属性中未转义的 &）
	broken := `bad <quote id="q3" data="a&b">内 容</quote>`
	qi3 := extractQuoteElement(broken)
	if qi3 == nil {
		t.Fatalf("期望正则回退提取到 quote")
	}
	if id, _ := qi3.quote["id"].(string); id != "q3" {
		t.Errorf("期望 quote id=q3，实际: %v", qi3.quote["id"])
	}
	if !strings.Contains(qi3.contentWithoutQuote, "bad") {
		t.Errorf("期望保留前缀文本，实际: %q", qi3.contentWithoutQuote)
	}
	if strings.Contains(qi3.contentWithoutQuote, "<quote") {
		t.Errorf("期望正则路径移除 quote 标签，实际: %q", qi3.contentWithoutQuote)
	}

	// 无 quote 返回 nil
	if qi4 := extractQuoteElement("plain text"); qi4 != nil {
		t.Errorf("期望 nil，实际: %+v", qi4)
	}
}

// ---------------------------------------------------------------------------
// 发送构造
// ---------------------------------------------------------------------------

func TestBuildSatoriContent(t *testing.T) {
	pngBase64 := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nfake"))
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Plain{Text: "a & b <c>"},
		&message.At{TargetID: "10001"},
		&message.At{Name: "张三"},
		&message.Image{Base64: pngBase64},
		&message.File{Name: "文档", URL: "https://example.com/d.pdf"},
		&message.Reply{MessageID: "r1"},
	}}
	content := buildSatoriContent(chain)

	expectParts := []string{
		"a &amp; b &lt;c&gt;",
		`<at id="10001"/>`,
		`<at name="张三"/>`,
		`<img src="data:image/png;base64,` + pngBase64 + `"/>`,
		`<file src="https://example.com/d.pdf" name="文档"/>`,
		`<reply id="r1"/>`,
	}
	for _, part := range expectParts {
		if !strings.Contains(content, part) {
			t.Errorf("期望内容包含 %q，实际: %s", part, content)
		}
	}
}

func TestConvertNodeToSatori(t *testing.T) {
	node := &message.Node{
		UIN:  "10001",
		Name: "小明",
		Content: []message.Component{
			&message.Plain{Text: "转发内容"},
		},
	}
	got := convertNodeToSatori(node)
	expected := `<message><author id="10001" name="小明"/>转发内容</message>`
	if got != expected {
		t.Errorf("期望 %q，实际 %q", expected, got)
	}

	// 空内容回退
	empty := convertNodeToSatori(&message.Node{})
	if !strings.Contains(empty, "[转发消息]") {
		t.Errorf("期望默认转发内容，实际: %s", empty)
	}

	// 合并转发
	nodes := &message.Nodes{Nodes: []*message.Node{
		{UIN: "1", Name: "A", Content: []message.Component{&message.Plain{Text: "x"}}},
		{UIN: "2", Name: "B", Content: []message.Component{&message.Plain{Text: "y"}}},
	}}
	nodesContent := convertNodesToSatori(nodes)
	if !strings.HasPrefix(nodesContent, `<message forward>`) || !strings.HasSuffix(nodesContent, `</message>`) {
		t.Errorf("期望合并转发包裹，实际: %s", nodesContent)
	}
	if !strings.Contains(nodesContent, `id="1"`) || !strings.Contains(nodesContent, `id="2"`) {
		t.Errorf("期望包含两个转发节点，实际: %s", nodesContent)
	}
}

// ---------------------------------------------------------------------------
// HTTP 发送
// ---------------------------------------------------------------------------

func TestSendHTTPRequest(t *testing.T) {
	var gotPath, gotAuth, gotPlatform, gotUserID string
	var gotBody map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotPlatform = r.Header.Get("satori-platform")
		gotUserID = r.Header.Get("satori-user-id")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"new-msg"}`))
	}))
	defer srv.Close()

	a := New(map[string]interface{}{
		"id":                  "t",
		"satori_api_base_url": srv.URL + "/satori/v1",
		"satori_token":        "tok123",
	}, nil, nil)
	// 模拟 READY 后的登录信息
	a.mu.Lock()
	a.logins = []satorilib.Login{{Platform: "qq", SelfID: "bot1", User: &satorilib.User{ID: "bot1"}}}
	a.mu.Unlock()

	result := a.sendHTTPRequest("POST", "/message.create", map[string]interface{}{
		"channel_id": "c1",
		"content":    `<at id="1"/>hi`,
	}, "", "")

	if result["id"] != "new-msg" {
		t.Errorf("期望响应 id=new-msg，实际: %+v", result)
	}
	if gotPath != "/satori/v1/message.create" {
		t.Errorf("期望路径 /satori/v1/message.create，实际: %s", gotPath)
	}
	if gotAuth != "Bearer tok123" {
		t.Errorf("期望 Bearer tok123，实际: %s", gotAuth)
	}
	if gotPlatform != "qq" || gotUserID != "bot1" {
		t.Errorf("期望登录路由头 qq/bot1，实际: %s/%s", gotPlatform, gotUserID)
	}
	if gotBody["channel_id"] != "c1" || gotBody["content"] != `<at id="1"/>hi` {
		t.Errorf("期望请求体 {channel_id, content}，实际: %+v", gotBody)
	}
}

// ---------------------------------------------------------------------------
// 自消息过滤与发布
// ---------------------------------------------------------------------------

func TestHandleEventPublishAndSelfFilter(t *testing.T) {
	bus := &fakeEventBus{}
	a := New(map[string]interface{}{"id": "t"}, nil, nil)
	a.SetEventBus(bus)

	// 机器人自己发的消息（user.id == login.user.id）应被过滤
	a.handleEvent(map[string]interface{}{
		"type": "message-created",
		"user": map[string]interface{}{"id": "bot1"},
		"login": map[string]interface{}{
			"user": map[string]interface{}{"id": "bot1"},
		},
		"message": map[string]interface{}{
			"id":      "self-msg",
			"content": "hello",
		},
		"channel": map[string]interface{}{"id": "c1"},
	})
	if len(bus.events) != 0 {
		t.Errorf("期望过滤机器人自己的消息，实际发布 %d 条", len(bus.events))
	}

	// 他人消息应发布
	a.handleEvent(map[string]interface{}{
		"type": "message-created",
		"user": map[string]interface{}{"id": "u1", "nick": "小明"},
		"login": map[string]interface{}{
			"user": map[string]interface{}{"id": "bot1"},
		},
		"message": map[string]interface{}{
			"id":      "m1",
			"content": "在吗",
		},
		"channel":   map[string]interface{}{"id": "c1"},
		"guild":     map[string]interface{}{"id": "g1"},
		"timestamp": float64(1700000000),
	})
	if len(bus.events) != 1 {
		t.Fatalf("期望发布 1 条消息，实际 %d", len(bus.events))
	}
	ev := bus.events[0]
	if ev.Source.Platform != "satori" || !ev.Source.IsGroup {
		t.Errorf("期望平台 satori 群消息，实际: %+v", ev.Source)
	}
	if ev.Source.SenderID != "u1" {
		t.Errorf("期望发送者 u1，实际: %s", ev.Source.SenderID)
	}
	if ev.MessageObj == nil || ev.MessageObj.MessageID != "m1" {
		t.Errorf("期望 message_id=m1，实际: %+v", ev.MessageObj)
	}
	if ev.MessageStr != "在吗" {
		t.Errorf("期望消息文本，实际: %q", ev.MessageStr)
	}

	// 非 message-created 事件应忽略
	a.handleEvent(map[string]interface{}{"type": "guild-added"})
	if len(bus.events) != 1 {
		t.Errorf("非消息事件不应发布，实际 %d 条", len(bus.events))
	}
}
