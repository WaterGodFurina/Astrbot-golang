package slack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// slackTestSignature 构造 Slack 签名头（v0）。
func slackTestSignature(secret, timestamp, body string) string {
	sigBase := fmt.Sprintf("v0:%s:%s", timestamp, body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(sigBase))
	return "v0=" + fmt.Sprintf("%x", mac.Sum(nil))
}

// ── 签名校验 ─────────────────────────────────────────────────────

func TestVerifySlackSignature(t *testing.T) {
	secret := "test-signing-secret"
	body := `{"type":"url_verification","challenge":"abc123"}`
	ts := "1700000000"
	sig := slackTestSignature(secret, ts, body)

	if !verifySlackSignature(secret, []byte(body), ts, sig) {
		t.Error("合法签名应通过校验")
	}
	if verifySlackSignature(secret, []byte(body), ts, "v0=wrong") {
		t.Error("错误签名不应通过校验")
	}
	if verifySlackSignature(secret, []byte(body+"x"), ts, sig) {
		t.Error("篡改请求体后不应通过校验")
	}
	if verifySlackSignature(secret, []byte(body), "", sig) {
		t.Error("缺少时间戳不应通过校验")
	}
}

// ── blocks 解析（收消息）─────────────────────────────────────────

func TestParseBlocksRichText(t *testing.T) {
	blocks := []interface{}{
		map[string]interface{}{
			"type": "rich_text",
			"elements": []interface{}{
				map[string]interface{}{
					"type": "rich_text_section",
					"elements": []interface{}{
						map[string]interface{}{"type": "text", "text": "你好 "},
						map[string]interface{}{"type": "user", "user_id": "U123"},
						map[string]interface{}{"type": "text", "text": " 再见"},
						map[string]interface{}{"type": "emoji", "name": "smile"},
					},
				},
			},
		},
	}
	components := parseBlocks(blocks)
	if len(components) != 3 {
		t.Fatalf("期望 3 个组件（Plain/At/Plain），实际 %d", len(components))
	}
	if p, ok := components[0].(*message.Plain); !ok || p.Text != "你好 " {
		t.Errorf("组件 0 应为 Plain(你好 )，实际 %v", components[0])
	}
	if at, ok := components[1].(*message.At); !ok || at.TargetID != "U123" {
		t.Errorf("组件 1 应为 At(U123)，实际 %v", components[1])
	}
	if p, ok := components[2].(*message.Plain); !ok || p.Text != " 再见:smile:" {
		t.Errorf("组件 2 应为 Plain( 再见:smile:)，实际 %v", components[2])
	}
}

func TestParseBlocksSection(t *testing.T) {
	blocks := []interface{}{
		map[string]interface{}{
			"type": "section",
			"text": map[string]interface{}{"type": "mrkdwn", "text": "*粗体* 文本"},
		},
	}
	components := parseBlocks(blocks)
	if len(components) != 1 {
		t.Fatalf("期望 1 个组件，实际 %d", len(components))
	}
	if p, ok := components[0].(*message.Plain); !ok || p.Text != "*粗体* 文本" {
		t.Errorf("section 文本解析不正确: %v", components[0])
	}
}

func TestParseBlocksRichTextList(t *testing.T) {
	blocks := []interface{}{
		map[string]interface{}{
			"type": "rich_text",
			"elements": []interface{}{
				map[string]interface{}{
					"type": "rich_text_list",
					"elements": []interface{}{
						map[string]interface{}{
							"type": "rich_text_section",
							"elements": []interface{}{
								map[string]interface{}{"type": "text", "text": "第一项"},
							},
						},
						map[string]interface{}{
							"type": "rich_text_section",
							"elements": []interface{}{
								map[string]interface{}{"type": "text", "text": "第二项"},
							},
						},
					},
				},
			},
		},
	}
	components := parseBlocks(blocks)
	if len(components) != 1 {
		t.Fatalf("期望 1 个组件，实际 %d", len(components))
	}
	p, ok := components[0].(*message.Plain)
	if !ok || p.Text != "• 第一项\n• 第二项" {
		t.Errorf("列表解析不正确: %q", p.Text)
	}
}

// ── 消息转换 ─────────────────────────────────────────────────────

func TestConvertMessageWithMentions(t *testing.T) {
	a := New(map[string]interface{}{"id": "slack-test"}, nil, nil)
	a.botSelfID = "BOT"
	// client 为 nil 时用户名回退为用户 ID
	event := map[string]interface{}{
		"type":          "message",
		"user":          "U1001",
		"channel":       "C1001",
		"client_msg_id": "CM1",
		"ts":            "1700000000.123",
		"text":          "你好 <@U1002> 世界",
	}
	abm := a.convertMessage(event)
	if abm == nil {
		t.Fatal("convertMessage 返回 nil")
	}
	if abm.Type != platform.GroupMessage {
		t.Errorf("频道消息应为群消息，实际 %v", abm.Type)
	}
	if abm.SessionID != "C1001" {
		t.Errorf("群会话应为频道 ID，实际 %s", abm.SessionID)
	}
	if abm.MessageID != "CM1" {
		t.Errorf("消息 ID 应为 client_msg_id，实际 %s", abm.MessageID)
	}
	if abm.Timestamp != 1700000000 {
		t.Errorf("时间戳应为 1700000000，实际 %d", abm.Timestamp)
	}
	// 组件：At(U1002) + Plain(你好  世界)
	if len(abm.Message) != 2 {
		t.Fatalf("期望 2 个组件，实际 %d", len(abm.Message))
	}
	if at, ok := abm.Message[0].(*message.At); !ok || at.TargetID != "U1002" {
		t.Errorf("组件 0 应为 At(U1002)，实际 %v", abm.Message[0])
	}
	if p, ok := abm.Message[1].(*message.Plain); !ok || p.Text != "你好  世界" {
		t.Errorf("组件 1 应为清理提及后的文本，实际 %v", abm.Message[1])
	}
}

func TestConvertMessageSkipsBot(t *testing.T) {
	a := New(map[string]interface{}{"id": "slack-test"}, nil, nil)
	// 机器人消息 / 编辑 / 删除应被过滤，不会走到 convertMessage
	event := map[string]interface{}{
		"type":          "message",
		"subtype":       "bot_message",
		"bot_id":        "B123",
		"user":          "U1",
		"channel":       "C1",
		"text":          "bot 消息",
		"client_msg_id": "CM2",
	}
	a.processIncomingEvent(event)
	a.processIncomingEvent(map[string]interface{}{
		"type": "message", "subtype": "message_changed", "text": "x",
	})
	a.processIncomingEvent(map[string]interface{}{
		"type": "message", "subtype": "message_deleted", "text": "x",
	})
	// 合法的普通消息（无 client 时用户名回退）不 panic
	a.processIncomingEvent(map[string]interface{}{
		"type": "message", "user": "U1", "channel": "C1", "text": "ok",
	})
}

func TestConvertMessagePrivateChatFallback(t *testing.T) {
	// client 为 nil 时 isIMChannel 返回 false → 默认群组
	a := New(map[string]interface{}{"id": "slack-test"}, nil, nil)
	event := map[string]interface{}{
		"type": "message", "user": "U1", "channel": "D1",
		"text": "私聊", "client_msg_id": "CM3",
	}
	abm := a.convertMessage(event)
	if abm == nil || abm.Type != platform.GroupMessage {
		t.Errorf("无 client 时应默认按群组处理，实际 %v", abm)
	}
}

// ── 发送 blocks 构造 ─────────────────────────────────────────────

func TestParseSlackBlocks(t *testing.T) {
	a := New(map[string]interface{}{"id": "slack-test"}, nil, nil)
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Plain{Text: "第一段"},
		&message.Plain{Text: "第二段"},
		&message.Image{URL: "https://example.com/pic.png"},
		&message.Plain{Text: "尾部"},
	}}
	blocks, text := parseSlackBlocks(context.Background(), chain, a.client)
	if text != "" {
		t.Errorf("存在 blocks 时 text 应为空，实际 %q", text)
	}
	// Plain 合并为 1 个块 + Image 1 个块 + 尾部 1 个块 = 3
	if len(blocks) != 3 {
		t.Fatalf("期望 3 个块，实际 %d", len(blocks))
	}
	sec, ok := blocks[0].(*slack.SectionBlock)
	if !ok || sec.Text.Text != "第一段第二段" {
		t.Errorf("块 0 应为合并文本块，实际 %v", blocks[0])
	}
	img, ok := blocks[1].(*slack.ImageBlock)
	if !ok || img.ImageURL != "https://example.com/pic.png" {
		t.Errorf("块 1 应为 ImageBlock，实际 %v", blocks[1])
	}
}

func TestParseSlackBlocksTextOnly(t *testing.T) {
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Plain{Text: "纯文本"},
	}}
	// 与 Python 一致：末尾文本也会生成一个 section 块，text 为空
	blocks, text := parseSlackBlocks(context.Background(), chain, nil)
	if len(blocks) != 1 {
		t.Fatalf("纯文本应生成 1 个 section 块，实际 %d", len(blocks))
	}
	if text != "" {
		t.Errorf("存在 blocks 时 text 应为空，实际 %q", text)
	}
	sec, ok := blocks[0].(*slack.SectionBlock)
	if !ok || sec.Text.Text != "纯文本" {
		t.Errorf("文本块内容不正确: %v", blocks[0])
	}
}

// ── Webhook 回调（httptest）──────────────────────────────────────

func TestWebhookCallbackURLVerification(t *testing.T) {
	secret := "hook-secret"
	server := NewSlackWebhookServer(secret, "/slack/events", nil)
	body := `{"type":"url_verification","challenge":"3eZbrw1aBBa2OEYoErbO2g"}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackTestSignature(secret, ts, body))
	w := httptest.NewRecorder()
	server.HandleCallback(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("url_verification 应返回 200，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "3eZbrw1aBBa2OEYoErbO2g") {
		t.Errorf("响应应包含 challenge，实际 %s", w.Body.String())
	}
}

func TestWebhookCallbackInvalidSignature(t *testing.T) {
	secret := "hook-secret"
	server := NewSlackWebhookServer(secret, "/slack/events", nil)
	body := `{"type":"event_callback","event":{}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)

	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", "v0=bad")
	w := httptest.NewRecorder()
	server.HandleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("错误签名应返回 400，实际 %d", w.Code)
	}

	// 缺少头
	req2 := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	w2 := httptest.NewRecorder()
	server.HandleCallback(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("缺少头应返回 400，实际 %d", w2.Code)
	}
}

func TestWebhookCallbackStaleTimestamp(t *testing.T) {
	secret := "hook-secret"
	server := NewSlackWebhookServer(secret, "/slack/events", nil)
	body := `{"type":"event_callback","event":{"type":"message","text":"replay"}}`

	// 过旧时间戳的合法签名回调应被拒绝（防重放）。
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	stale := "1700000000"
	req.Header.Set("X-Slack-Request-Timestamp", stale)
	req.Header.Set("X-Slack-Signature", slackTestSignature(secret, stale, body))
	w := httptest.NewRecorder()
	server.HandleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("过期时间戳应返回 400，实际 %d", w.Code)
	}
}

func TestWebhookCallbackBodyTooLarge(t *testing.T) {
	secret := "hook-secret"
	server := NewSlackWebhookServer(secret, "/slack/events", nil)
	big := strings.Repeat("a", maxWebhookBodySize+1024)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(big))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackTestSignature(secret, ts, big))
	w := httptest.NewRecorder()
	server.HandleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("超限请求体应返回 400，实际 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "too large") {
		t.Errorf("超限请求体应返回大小超限错误，实际 %s", w.Body.String())
	}
}

func TestWebhookCallbackEventDispatch(t *testing.T) {
	secret := "hook-secret"
	got := ""
	server := NewSlackWebhookServer(secret, "/slack/events", func(eventData map[string]interface{}) {
		if ev, ok := eventData["event"].(map[string]interface{}); ok {
			got, _ = ev["text"].(string)
		}
	})
	body := `{"type":"event_callback","event":{"type":"message","text":"hello slack","user":"U1","channel":"C1","client_msg_id":"CM9"}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, "/slack/events", strings.NewReader(body))
	req.Header.Set("X-Slack-Request-Timestamp", ts)
	req.Header.Set("X-Slack-Signature", slackTestSignature(secret, ts, body))
	w := httptest.NewRecorder()
	server.HandleCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("合法事件应返回 200，实际 %d", w.Code)
	}
	if got != "hello slack" {
		t.Errorf("事件处理函数应收到消息，实际 %q", got)
	}
}

// ── Start 非阻塞（统一 webhook 模式）──────────────────────────────

func TestStartUnifiedWebhookModeNonBlocking(t *testing.T) {
	a := &Adapter{
		connectionMode:     "webhook",
		unifiedWebhookMode: true,
		webhookUUID:        "uuid-1",
		stopCh:             make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- a.startWebhookMode(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("统一 webhook 模式 Start 应返回 nil，实际 %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("统一 webhook 模式 Start 不应阻塞")
	}
}

// ── 会话 ID 处理 ─────────────────────────────────────────────────

func TestChannelIDFromSession(t *testing.T) {
	if got := channelIDFromSession("C12345"); got != "C12345" {
		t.Errorf("无下划线会话应原样返回，实际 %s", got)
	}
	if got := channelIDFromSession("default_C999"); got != "C999" {
		t.Errorf("带下划线会话应取最后一段，实际 %s", got)
	}
}

// channelIDFromSession 对应 Python 的 session_id.split("_")[-1] 逻辑。
func channelIDFromSession(sessionID string) string {
	if strings.Contains(sessionID, "_") {
		return sessionID[strings.LastIndex(sessionID, "_")+1:]
	}
	return sessionID
}

// ── 文件下载大小上限（M-50 回归）────────────────────────────────

func TestLimitedWriter(t *testing.T) {
	var buf bytes.Buffer
	lw := &limitedWriter{w: &buf, remaining: 4}
	if n, err := lw.Write([]byte("1234")); err != nil || n != 4 {
		t.Fatalf("未超限写入应成功: n=%d err=%v", n, err)
	}
	if _, err := lw.Write([]byte("5")); err == nil {
		t.Error("超过上限的写入应报错")
	}
	if buf.String() != "1234" {
		t.Errorf("期望仅写入 1234，实际 %q", buf.String())
	}
}

func TestGetFileBase64RejectsOversize(t *testing.T) {
	// 服务端返回超过上限大小的文件内容，下载应失败而非无界读取。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxFileDownloadSize+1))
	}))
	defer srv.Close()

	a := New(map[string]interface{}{"id": "slack-test"}, nil, nil)
	a.client = slack.New("x", slack.OptionHTTPClient(&http.Client{}))
	if _, err := a.getFileBase64(context.Background(), srv.URL); err == nil {
		t.Error("超过大小上限的 Slack 文件下载应报错")
	}
}

// ── socket/webhook 字段竞态回归（race detector 验证）────────────────

// runStartStopRace concurrently drives one startXxx (which assigns
// a.socket/a.webhook/socketCancel) against repeated Stop calls, so the write
// under lock and the read+consume under lock are exercised under -race.
func runStartStopRace(t *testing.T, a *Adapter, start func()) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		start()
	}()
	for i := 0; i < 50; i++ {
		if err := a.Stop(); err != nil {
			t.Fatalf("Stop 返回错误: %v", err)
		}
	}
	wg.Wait()
}

// TestSocketModeStartStopConcurrent verifies that assigning a.socket and
// a.socketCancel under a.mu does not race with Stop's locked read, and that
// the cancel is consumed exactly once (subsequent Stops are no-ops).
func TestSocketModeStartStopConcurrent(t *testing.T) {
	a := New(map[string]interface{}{
		"id":                    "slack-race",
		"bot_token":             "xoxb-test",
		"app_token":             "xapp-test",
		"slack_connection_mode": "socket",
	}, nil, nil)
	if a.client == nil {
		t.Fatal("adapter 应初始化 client")
	}
	// Pre-cancelled ctx makes socketmode.RunContext exit immediately without
	// dialing Slack; the test only exercises the field-sync/lifecycle.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	defer cancel()

	runStartStopRace(t, a, func() {
		_ = a.startSocketMode(ctx)
	})

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.socketCancel != nil {
		t.Error("Stop 后 socketCancel 应被置 nil（防止重复取消）")
	}
	if a.socket == nil {
		t.Error("socket 客户端应已创建")
	}
}

// TestWebhookModeStartStopConcurrent mirrors the socket test for the webhook
// field (a.webhook), in unified mode so no HTTP server is started.
func TestWebhookModeStartStopConcurrent(t *testing.T) {
	a := New(map[string]interface{}{
		"id":                    "slack-race",
		"bot_token":             "xoxb-test",
		"signing_secret":        "secret",
		"slack_connection_mode": "webhook",
		"unified_webhook_mode":  true,
		"webhook_uuid":          "uuid-1",
	}, nil, nil)

	runStartStopRace(t, a, func() {
		_ = a.startWebhookMode(context.Background())
	})
}
