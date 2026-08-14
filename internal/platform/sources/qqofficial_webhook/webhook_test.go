package qqofficial_webhook

// 单元测试：签名校验 / 消息解析 / webhook 回调全流程（httptest）。
// 对齐 qo_webhook_server.py 与 qqofficial_platform_adapter.py 的行为。

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// fakeEventBus 捕获发布的 core.Event。
type fakeEventBus struct {
	events []*core.Event
}

func (f *fakeEventBus) Publish(e *core.Event) error {
	f.events = append(f.events, e)
	return nil
}

func newWebhookAdapter(bus platform.EventBus) *Adapter {
	a := New(map[string]interface{}{
		"id":                   "qq_webhook_test",
		"appid":                "test-appid",
		"secret":               "test-secret",
		"unified_webhook_mode": true,
		"webhook_uuid":         "uuid-1",
	}, nil, nil)
	if bus != nil {
		a.SetEventBus(bus)
	}
	return a
}

// ---------------------------------------------------------------------------
// 签名校验
// ---------------------------------------------------------------------------

func TestEd25519Signature(t *testing.T) {
	secret := "my-secret"
	timestamp := "1700000000"
	payload := []byte(`{"op":0,"t":"group_message_create"}`)

	sig, err := signQQWebhookPayload(secret, timestamp, payload)
	if err != nil {
		t.Fatalf("签名失败: %v", err)
	}
	if !verifyQQWebhookSignature(secret, timestamp, sig, payload) {
		t.Errorf("期望签名校验通过")
	}

	// 篡改载荷应校验失败
	tampered := []byte(`{"op":0,"t":"c2c_message_create"}`)
	if verifyQQWebhookSignature(secret, timestamp, sig, tampered) {
		t.Errorf("期望篡改载荷校验失败")
	}

	// 篡改时间戳应校验失败
	if verifyQQWebhookSignature(secret, "1700000001", sig, payload) {
		t.Errorf("期望篡改时间戳校验失败")
	}

	// 空时间戳/空签名应失败
	if verifyQQWebhookSignature(secret, "", sig, payload) {
		t.Errorf("期望空时间戳校验失败")
	}
	if verifyQQWebhookSignature(secret, timestamp, "", payload) {
		t.Errorf("期望空签名校验失败")
	}

	// 非十六进制签名应失败
	if verifyQQWebhookSignature(secret, timestamp, "zzzz", payload) {
		t.Errorf("期望非法十六进制签名校验失败")
	}

	// 长度错误的签名应失败
	badLen := strings.Repeat("ab", 32)
	if verifyQQWebhookSignature(secret, timestamp, badLen, payload) {
		t.Errorf("期望长度错误的签名校验失败")
	}

	// 不同 secret 应失败
	otherSig, _ := signQQWebhookPayload("other-secret", timestamp, payload)
	if verifyQQWebhookSignature(secret, timestamp, otherSig, payload) {
		t.Errorf("期望错误 secret 校验失败")
	}

	// 空 secret 无法签名/校验
	if _, err := signQQWebhookPayload("", timestamp, payload); err == nil {
		t.Errorf("期望空 secret 签名报错")
	}

	// 短 secret 自加倍长（与 Python _build_ed25519_seed 一致）
	sig2, err := signQQWebhookPayload("ab", timestamp, payload)
	if err != nil {
		t.Fatalf("短 secret 签名失败: %v", err)
	}
	if !verifyQQWebhookSignature("ab", timestamp, sig2, payload) {
		t.Errorf("期望短 secret 校验通过")
	}
}

// ---------------------------------------------------------------------------
// 消息解析（复用 qqofficial 的解析逻辑）
// ---------------------------------------------------------------------------

func TestParseGroupMessage(t *testing.T) {
	d := map[string]interface{}{
		"id":           "msg-1",
		"group_openid": "grp1",
		"content":      "hello <@bot123> world",
		"message_type": float64(1),
		"author": map[string]interface{}{
			"member_openid": "mem1",
			"username":      "小明",
		},
		"mentions": []interface{}{
			map[string]interface{}{"id": "bot123", "is_you": true, "username": "我的机器人"},
		},
		"attachments": []interface{}{
			map[string]interface{}{"content_type": "image/png", "url": "example.com/a.png", "filename": "a.png"},
		},
	}
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindGroup, false)

	if abm.Type != platform.GroupMessage {
		t.Errorf("期望群消息，实际: %s", abm.Type)
	}
	if abm.MessageID != "msg-1" {
		t.Errorf("期望 msg-1，实际: %s", abm.MessageID)
	}
	if abm.GroupID() != "grp1" {
		t.Errorf("期望群 id grp1，实际: %s", abm.GroupID())
	}
	if abm.Sender.UserID != "mem1" || abm.Sender.Nickname != "小明" {
		t.Errorf("发送者解析错误: %+v", abm.Sender)
	}
	// self_id 取被 @ 的机器人 id
	if abm.SelfID != "bot123" {
		t.Errorf("期望 self_id=bot123，实际: %s", abm.SelfID)
	}
	// 消息文本应移除 @ 占位符
	if abm.MessageStr != "hello  world" {
		t.Errorf("期望文本 hello  world，实际: %q", abm.MessageStr)
	}
	// 组件: At(bot) + Plain + Image
	if len(abm.Message) != 3 {
		t.Fatalf("期望 3 个组件，实际 %d: %+v", len(abm.Message), abm.Message)
	}
	at, ok := abm.Message[0].(*message.At)
	if !ok || at.TargetID != "bot123" || at.Name != "我的机器人" {
		t.Errorf("组件0 期望 At(bot123)，实际: %+v", abm.Message[0])
	}
	if img, ok := abm.Message[2].(*message.Image); !ok || img.URL != "https://example.com/a.png" {
		t.Errorf("组件2 期望图片（补 https 前缀），实际: %+v", abm.Message[2])
	}
}

func TestParseGroupAtForceMention(t *testing.T) {
	// forceGroupMention=true（群 @ 消息事件）时即使没有 mentions 也插入 At
	d := map[string]interface{}{
		"id":           "msg-2",
		"group_openid": "grp2",
		"content":      "在吗",
		"author":       map[string]interface{}{"member_openid": "mem2"},
	}
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindGroup, true)
	if abm.SelfID != "qq_official" {
		t.Errorf("期望默认 self_id=qq_official，实际: %s", abm.SelfID)
	}
	at, ok := abm.Message[0].(*message.At)
	if !ok || at.TargetID != "qq_official" {
		t.Errorf("期望强制提及组件，实际: %+v", abm.Message[0])
	}
}

func TestParseC2CMessage(t *testing.T) {
	d := map[string]interface{}{
		"id":      "msg-3",
		"content": "你好",
		"author":  map[string]interface{}{"user_openid": "user1", "username": "张三"},
	}
	abm := parseFromQQOfficial(d, platform.FriendMessage, kindC2C, false)

	if abm.Type != platform.FriendMessage {
		t.Errorf("期望私聊消息，实际: %s", abm.Type)
	}
	if abm.Sender.UserID != "user1" {
		t.Errorf("期望发送者 user1，实际: %s", abm.Sender.UserID)
	}
	if abm.SelfID != "unknown_selfid" {
		t.Errorf("期望 self_id=unknown_selfid，实际: %s", abm.SelfID)
	}
	if abm.MessageStr != "你好" {
		t.Errorf("期望文本，实际: %q", abm.MessageStr)
	}
	// 组件: At(qq_official) + Plain
	if len(abm.Message) != 2 {
		t.Fatalf("期望 2 个组件，实际 %d", len(abm.Message))
	}
	if _, ok := abm.Message[0].(*message.At); !ok {
		t.Errorf("组件0 期望 At，实际: %+v", abm.Message[0])
	}
}

func TestParseChannelMessage(t *testing.T) {
	// 频道消息：消息链应包含 At + Plain 文本组件（M-54 回归）。
	d := map[string]interface{}{
		"id":         "msg-c1",
		"channel_id": "ch1",
		"content":    "频道消息",
		"author":     map[string]interface{}{"id": "u1", "username": "小明"},
		"mentions": []interface{}{
			map[string]interface{}{"id": "bot1"},
		},
	}
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindChannel, false)

	if abm.MessageStr != "频道消息" {
		t.Errorf("期望文本，实际: %q", abm.MessageStr)
	}
	// 组件：At(qq_official) + Plain
	if len(abm.Message) != 2 {
		t.Fatalf("期望 2 个组件，实际 %d: %+v", len(abm.Message), abm.Message)
	}
	if at, ok := abm.Message[0].(*message.At); !ok || at.TargetID != "qq_official" {
		t.Errorf("组件0 期望 At(qq_official)，实际: %+v", abm.Message[0])
	}
	if p, ok := abm.Message[1].(*message.Plain); !ok || p.Text != "频道消息" {
		t.Errorf("组件1 期望 Plain(频道消息)，实际: %+v", abm.Message[1])
	}
}

func TestParseDirectMessage(t *testing.T) {
	// 频道私聊消息：消息链应包含 At + Plain 文本组件（M-54 回归）。
	d := map[string]interface{}{
		"id":      "msg-d1",
		"content": "私聊内容",
		"author":  map[string]interface{}{"id": "u2", "username": "张三"},
	}
	abm := parseFromQQOfficial(d, platform.FriendMessage, kindDirect, false)

	if abm.MessageStr != "私聊内容" {
		t.Errorf("期望文本，实际: %q", abm.MessageStr)
	}
	if len(abm.Message) != 2 {
		t.Fatalf("期望 2 个组件，实际 %d: %+v", len(abm.Message), abm.Message)
	}
	if at, ok := abm.Message[0].(*message.At); !ok || at.TargetID != "qq_official" {
		t.Errorf("组件0 期望 At(qq_official)，实际: %+v", abm.Message[0])
	}
	if p, ok := abm.Message[1].(*message.Plain); !ok || p.Text != "私聊内容" {
		t.Errorf("组件1 期望 Plain(私聊内容)，实际: %+v", abm.Message[1])
	}
}

func TestParseFaceMessage(t *testing.T) {
	// ext 为 base64 编码的 {"text":"[大笑]"}
	ext := base64.StdEncoding.EncodeToString([]byte(`{"text":"[大笑]"}`))
	content := `hello <faceType=4,faceId="1",ext="` + ext + `"> world`
	got := parseFaceMessage(content)
	if !strings.Contains(got, "[表情:") {
		t.Errorf("期望表情解析，实际: %q", got)
	}
	// 解析失败回退 [表情]
	got2 := parseFaceMessage(`x <faceType=4,faceId="1"> y`)
	if !strings.Contains(got2, "[表情]") {
		t.Errorf("期望默认表情文本，实际: %q", got2)
	}
}

func TestParseQuotedGroupMessage(t *testing.T) {
	// message_type == 103 时解析引用消息
	d := map[string]interface{}{
		"id":           "msg-4",
		"group_openid": "grp4",
		"content":      "回复你",
		"message_type": float64(103),
		"author":       map[string]interface{}{"member_openid": "mem4", "username": "小明"},
		"msg_elements": []interface{}{
			map[string]interface{}{
				"id":      "quoted-1",
				"content": "原始消息",
			},
		},
	}
	abm := parseFromQQOfficial(d, platform.GroupMessage, kindGroup, false)
	reply, ok := abm.Message[0].(*message.Reply)
	if !ok {
		t.Fatalf("组件0 期望 Reply，实际: %+v", abm.Message[0])
	}
	if reply.MessageID != "quoted-1" {
		t.Errorf("期望引用 id=quoted-1，实际: %s", reply.MessageID)
	}
	if reply.MessageStr != "原始消息" {
		t.Errorf("期望引用内容，实际: %q", reply.MessageStr)
	}
}

// ---------------------------------------------------------------------------
// Webhook 回调全流程（httptest）
// ---------------------------------------------------------------------------

// doWebhookRequest 发送一个带签名的 webhook 回调请求。
func doWebhookRequest(t *testing.T, a *Adapter, payload map[string]interface{}, sign bool) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	if sign {
		timestamp := strconv.FormatInt(time.Now().Unix(), 10)
		sig, err := signQQWebhookPayload(a.secret, timestamp, body)
		if err != nil {
			t.Fatalf("签名失败: %v", err)
		}
		req.Header.Set(signatureTimestampHeader, timestamp)
		req.Header.Set(signatureHeader, sig)
	}
	w := httptest.NewRecorder()
	a.WebhookCallback(w, req)
	return w
}

func TestWebhookValidation(t *testing.T) {
	a := newWebhookAdapter(nil)
	eventTS := strconv.FormatInt(time.Now().Unix(), 10)
	w := doWebhookRequest(t, a, map[string]interface{}{
		"op": float64(13),
		"d": map[string]interface{}{
			"event_ts":    eventTS,
			"plain_token": "abc123",
		},
	}, false)

	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if resp["plain_token"] != "abc123" {
		t.Errorf("期望原样返回 plain_token，实际: %v", resp["plain_token"])
	}
	signature, _ := resp["signature"].(string)
	// 验证响应签名：sign(secret, event_ts + plain_token)
	expected, err := signQQWebhookPayload(a.secret, "", []byte(eventTS+"abc123"))
	if err != nil {
		t.Fatalf("期望签名失败: %v", err)
	}
	if signature != expected {
		t.Errorf("验证签名不匹配: %s vs %s", signature, expected)
	}
}

func TestWebhookValidationStaleEventTS(t *testing.T) {
	a := newWebhookAdapter(nil)
	// 过期 event_ts 应被拒绝
	w := doWebhookRequest(t, a, map[string]interface{}{
		"op": float64(13),
		"d": map[string]interface{}{
			"event_ts":    "1700000000",
			"plain_token": "abc123",
		},
	}, false)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("期望过期 event_ts 返回 401，实际 %d", w.Code)
	}
}

func TestWebhookValidationRateLimit(t *testing.T) {
	a := newWebhookAdapter(nil)
	// 短时间内连续请求触发限速
	for i := 0; i <= validationMaxRatePerMin; i++ {
		w := doWebhookRequest(t, a, map[string]interface{}{
			"op": float64(13),
			"d": map[string]interface{}{
				"event_ts":    strconv.FormatInt(time.Now().Unix(), 10),
				"plain_token": "abc123",
			},
		}, false)
		if i < validationMaxRatePerMin {
			if w.Code != http.StatusOK {
				t.Fatalf("第 %d 次请求期望 200，实际 %d", i, w.Code)
			}
		} else if w.Code != http.StatusUnauthorized {
			t.Fatalf("超限请求期望 401，实际 %d", w.Code)
		}
	}
}

func TestWebhookInvalidJSON(t *testing.T) {
	a := newWebhookAdapter(nil)
	req := httptest.NewRequest(http.MethodPost, webhookPath, strings.NewReader("not-json"))
	w := httptest.NewRecorder()
	a.WebhookCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("期望 400，实际 %d", w.Code)
	}
}

func TestWebhookBadSignature(t *testing.T) {
	a := newWebhookAdapter(nil)
	// 不签名的回调应返回 401
	w := doWebhookRequest(t, a, map[string]interface{}{
		"op": float64(0),
		"t":  "group_message_create",
		"d":  map[string]interface{}{"id": "m1"},
	}, false)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("期望 401，实际 %d", w.Code)
	}
}

func TestWebhookStaleTimestampRejected(t *testing.T) {
	a := newWebhookAdapter(nil)
	payload := map[string]interface{}{
		"id": "event-stale",
		"op": float64(0),
		"t":  "group_message_create",
		"d":  map[string]interface{}{"id": "m-stale"},
	}
	// 时间戳过旧的合法签名回调应被拒绝（防重放）。
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	timestamp := "1700000000"
	sig, _ := signQQWebhookPayload(a.secret, timestamp, body)
	req.Header.Set(signatureTimestampHeader, timestamp)
	req.Header.Set(signatureHeader, sig)
	w := httptest.NewRecorder()
	a.WebhookCallback(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("期望过期时间戳返回 401，实际 %d", w.Code)
	}

	// 非数字时间戳同样应被拒绝。
	req2 := httptest.NewRequest(http.MethodPost, webhookPath, bytes.NewReader(body))
	timestamp2 := "not-a-number"
	sig2, _ := signQQWebhookPayload(a.secret, timestamp2, body)
	req2.Header.Set(signatureTimestampHeader, timestamp2)
	req2.Header.Set(signatureHeader, sig2)
	w2 := httptest.NewRecorder()
	a.WebhookCallback(w2, req2)
	if w2.Code != http.StatusUnauthorized {
		t.Errorf("期望非法时间戳返回 401，实际 %d", w2.Code)
	}
}

func TestWebhookGroupMessageFlow(t *testing.T) {
	bus := &fakeEventBus{}
	a := newWebhookAdapter(bus)

	payload := map[string]interface{}{
		"id": "event-1",
		"op": float64(0),
		"t":  "group_message_create",
		"d": map[string]interface{}{
			"id":           "msg-10",
			"group_openid": "grp10",
			"content":      "hello world",
			"author": map[string]interface{}{
				"member_openid": "mem10",
				"username":      "小明",
				"union_openid":  "union-10",
			},
		},
	}
	w := doWebhookRequest(t, a, payload, true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["opcode"] != float64(12) {
		t.Errorf("期望 opcode 12，实际: %v", resp["opcode"])
	}

	// 应发布一条群消息事件
	if len(bus.events) != 1 {
		t.Fatalf("期望发布 1 条消息，实际 %d", len(bus.events))
	}
	ev := bus.events[0]
	if ev.Source.Platform != "qq_official_webhook" {
		t.Errorf("期望平台 qq_official_webhook，实际: %s", ev.Source.Platform)
	}
	if !ev.Source.IsGroup {
		t.Errorf("期望群消息，实际: %+v", ev.Source)
	}
	if ev.Source.ConvID != "grp10" || ev.Source.SenderID != "mem10" {
		t.Errorf("会话信息错误: %+v", ev.Source)
	}
	if ev.MessageObj == nil || ev.MessageObj.MessageID != "msg-10" {
		t.Errorf("消息对象错误: %+v", ev.MessageObj)
	}
	// 附加字段应注入 Metadata
	if ev.Metadata["union_openid"] != "union-10" {
		t.Errorf("期望 union_openid 附加字段，实际: %v", ev.Metadata["union_openid"])
	}
	// 会话场景应记录为 group（用于发送路由）
	a.mu.Lock()
	scene := a.sessionScene["grp10"]
	lastMsg := a.sessionLastMsg["grp10"]
	a.mu.Unlock()
	if scene != "group" {
		t.Errorf("期望会话场景 group，实际: %s", scene)
	}
	if lastMsg != "msg-10" {
		t.Errorf("期望记录最后消息 id，实际: %s", lastMsg)
	}
}

func TestWebhookDedup(t *testing.T) {
	bus := &fakeEventBus{}
	a := newWebhookAdapter(bus)

	payload := map[string]interface{}{
		"id": "event-dup",
		"op": float64(0),
		"t":  "c2c_message_create",
		"d": map[string]interface{}{
			"id":      "msg-20",
			"content": "你好",
			"author":  map[string]interface{}{"user_openid": "user20"},
		},
	}
	// 第一次推送：正常处理
	w := doWebhookRequest(t, a, payload, true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	// 第二次推送（相同 event id）：去重跳过
	w2 := doWebhookRequest(t, a, payload, true)
	if w2.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w2.Code)
	}
	if len(bus.events) != 1 {
		t.Errorf("期望去重后仅发布 1 条消息，实际 %d", len(bus.events))
	}
}

func TestWebhookUnknownEvent(t *testing.T) {
	bus := &fakeEventBus{}
	a := newWebhookAdapter(bus)
	payload := map[string]interface{}{
		"id": "event-x",
		"op": float64(0),
		"t":  "some_unknown_event",
		"d":  map[string]interface{}{"id": "msg-x"},
	}
	w := doWebhookRequest(t, a, payload, true)
	if w.Code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", w.Code)
	}
	if len(bus.events) != 0 {
		t.Errorf("未知事件不应发布，实际 %d 条", len(bus.events))
	}
}

// TestWebhookFullHTTPServer 使用真实 HTTP 服务器走一遍完整回调流程。
func TestWebhookFullHTTPServer(t *testing.T) {
	bus := &fakeEventBus{}
	a := newWebhookAdapter(bus)
	a.unifiedWebhookMode = false

	srv := httptest.NewServer(http.HandlerFunc(a.WebhookCallback))
	defer srv.Close()

	payload := map[string]interface{}{
		"id": "event-http",
		"op": float64(0),
		"t":  "group_at_message_create",
		"d": map[string]interface{}{
			"id":           "msg-30",
			"group_openid": "grp30",
			"content":      "在吗",
			"author": map[string]interface{}{
				"member_openid": "mem30",
				"username":      "小明",
			},
		},
	}
	body, _ := json.Marshal(payload)
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	sig, _ := signQQWebhookPayload(a.secret, timestamp, body)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+webhookPath, bytes.NewReader(body))
	req.Header.Set(signatureTimestampHeader, timestamp)
	req.Header.Set(signatureHeader, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.StatusCode)
	}

	// 群 @ 消息：强制提及，发布一条群消息
	if len(bus.events) != 1 {
		t.Fatalf("期望发布 1 条消息，实际 %d", len(bus.events))
	}
	ev := bus.events[0]
	if ev.Source.ConvID != "grp30" || !ev.Source.IsGroup {
		t.Errorf("会话信息错误: %+v", ev.Source)
	}
	if !strings.Contains(ev.MessageStr, "在吗") {
		t.Errorf("期望消息文本，实际: %q", ev.MessageStr)
	}
}
