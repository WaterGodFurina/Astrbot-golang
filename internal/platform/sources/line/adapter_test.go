package line

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ── 签名校验 ─────────────────────────────────────────────────────

func TestVerifySignature(t *testing.T) {
	secret := "test-channel-secret"
	client, err := NewLineAPIClient("token", secret)
	if err != nil {
		t.Fatalf("NewLineAPIClient failed: %v", err)
	}
	body := []byte(`{"events":[]}`)
	sig := hmacBase64(secret, body)

	if !client.VerifySignature(body, sig) {
		t.Error("合法签名应通过校验")
	}
	if client.VerifySignature(body, "wrong-signature") {
		t.Error("错误签名不应通过校验")
	}
	if client.VerifySignature(body, "") {
		t.Error("空签名不应通过校验")
	}
	if client.VerifySignature([]byte("tampered"), sig) {
		t.Error("篡改请求体后签名不应通过校验")
	}
}

// ── 消息转换 ─────────────────────────────────────────────────────

func TestParseTextWithMentions(t *testing.T) {
	// "你好 @Alice 再见 @Bob" 带两个提及
	text := "你好 @Alice 再见 @Bob"
	mention := map[string]interface{}{
		"mentionees": []interface{}{
			map[string]interface{}{"index": float64(3), "length": float64(6), "type": "user", "userId": "U1001"},
			map[string]interface{}{"index": float64(13), "length": float64(4), "type": "user", "userId": "U1002"},
		},
	}
	components := parseTextWithMentions(text, mention)
	if len(components) != 4 {
		t.Fatalf("期望 4 个组件（Plain/At/Plain/At），实际 %d 个", len(components))
	}
	if p, ok := components[0].(*message.Plain); !ok || p.Text != "你好 " {
		t.Errorf("组件 0 应为 Plain 你好 ，实际 %v", components[0])
	}
	if at, ok := components[1].(*message.At); !ok || at.TargetID != "U1001" || at.Name != "Alice" {
		t.Errorf("组件 1 应为 At(U1001, Alice)，实际 %v", components[1])
	}
	if p, ok := components[2].(*message.Plain); !ok || p.Text != " 再见 " {
		t.Errorf("组件 2 应为 Plain 再见 ，实际 %v", components[2])
	}
	if at, ok := components[3].(*message.At); !ok || at.TargetID != "U1002" || at.Name != "Bob" {
		t.Errorf("组件 3 应为 At(U1002, Bob)，实际 %v", components[3])
	}
}

func TestParseTextWithMentionsOutOfOrder(t *testing.T) {
	text := "a @X b @Y"
	mention := map[string]interface{}{
		"mentionees": []interface{}{
			map[string]interface{}{"index": float64(7), "length": float64(2), "type": "user", "userId": "U2"},
			map[string]interface{}{"index": float64(2), "length": float64(2), "type": "user", "userId": "U1"},
		},
	}
	components := parseTextWithMentions(text, mention)
	if len(components) != 4 {
		t.Fatalf("期望 4 个组件，实际 %d 个", len(components))
	}
	if at, ok := components[1].(*message.At); !ok || at.TargetID != "U1" {
		t.Errorf("乱序提及应按 index 排序，组件 1 应为 At(U1)，实际 %v", components[1])
	}
	if at, ok := components[3].(*message.At); !ok || at.TargetID != "U2" {
		t.Errorf("组件 3 应为 At(U2)，实际 %v", components[3])
	}
}

func TestConvertMessageGroup(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	event := map[string]interface{}{
		"type":           "message",
		"mode":           "active",
		"webhookEventId": "WEV-1",
		"timestamp":      float64(1700000000123),
		"replyToken":     "RT-1",
		"source": map[string]interface{}{
			"type":    "group",
			"groupId": "G1001",
			"userId":  "U1001",
		},
		"message": map[string]interface{}{
			"id":   "MSG-1",
			"type": "text",
			"text": "hello",
		},
	}
	abm := a.convertMessage(event)
	if abm == nil {
		t.Fatal("convertMessage 返回 nil")
	}
	if abm.Type != platform.GroupMessage {
		t.Errorf("群消息类型应为 GroupMessage，实际 %v", abm.Type)
	}
	if abm.SessionID != "G1001" || abm.Group.GroupID != "G1001" {
		t.Errorf("会话 ID 应为 G1001，实际 session=%s group=%s", abm.SessionID, abm.Group.GroupID)
	}
	if abm.MessageID != "MSG-1" {
		t.Errorf("消息 ID 应为 MSG-1，实际 %s", abm.MessageID)
	}
	if abm.Timestamp != 1700000000 {
		t.Errorf("时间戳应转换为秒 1700000000，实际 %d", abm.Timestamp)
	}
	if abm.MessageStr != "hello" {
		t.Errorf("MessageStr 应为 hello，实际 %q", abm.MessageStr)
	}
	// replyToken 应被记录
	if token := a.takeReplyToken("G1001"); token != "RT-1" {
		t.Errorf("replyToken 应为 RT-1，实际 %q", token)
	}
}

func TestConvertMessageSkipsNonMessage(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	if abm := a.convertMessage(map[string]interface{}{"type": "follow"}); abm != nil {
		t.Error("非 message 事件应返回 nil")
	}
	if abm := a.convertMessage(map[string]interface{}{"type": "message", "mode": "standby", "message": map[string]interface{}{"type": "text", "text": "x"}}); abm != nil {
		t.Error("standby 模式消息应返回 nil")
	}
}

// ── 事件去重 ─────────────────────────────────────────────────────

func TestDuplicateEvent(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	if a.isDuplicateEvent("E1") {
		t.Error("首次出现不应判为重复")
	}
	if !a.isDuplicateEvent("E1") {
		t.Error("第二次出现应判为重复")
	}
	if a.isDuplicateEvent("E2") {
		t.Error("不同事件 ID 不应判为重复")
	}
}

// ── 发送消息构造 ─────────────────────────────────────────────────

func TestBuildLineMessages(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Plain{Text: "  你好 LINE  "},
		&message.At{Name: "Alice", TargetID: "U1"},
		&message.Image{URL: "https://example.com/a.png"},
		&message.Plain{Text: " 结尾 "},
	}}
	ctx := context.Background()
	messages, err := a.buildLineMessages(ctx, chain)
	if err != nil {
		t.Fatalf("buildLineMessages: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("期望 4 条消息，实际 %d", len(messages))
	}
	// Plain 应 trim 后作为 text
	textMsg, ok := messages[0].(*messaging_api.TextMessage)
	if !ok || textMsg.Text != "你好 LINE" {
		t.Errorf("消息 0 应为 TextMessage(你好 LINE)，实际 %v", messages[0])
	}
	// At 应转为 @名字 文本
	atMsg, ok := messages[1].(*messaging_api.TextMessage)
	if !ok || atMsg.Text != "@Alice" {
		t.Errorf("消息 1 应为 TextMessage(@Alice)，实际 %v", messages[1])
	}
	// Image 的 https 直连
	imgMsg, ok := messages[2].(*messaging_api.ImageMessage)
	if !ok || imgMsg.OriginalContentUrl != "https://example.com/a.png" {
		t.Errorf("消息 2 应为 ImageMessage(https URL)，实际 %v", messages[2])
	}
	// 超过 5 条时截断
	big := &message.MessageChain{}
	for i := 0; i < 7; i++ {
		big.Chain = append(big.Chain, &message.Plain{Text: "x"})
	}
	messages, err = a.buildLineMessages(ctx, big)
	if err != nil {
		t.Fatalf("buildLineMessages(大链): %v", err)
	}
	if len(messages) != 5 {
		t.Errorf("超过 5 条应截断为 5 条，实际 %d", len(messages))
	}
}

func TestBuildLineMessagesFileService(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	// 本地图片文件 → 注册到内置文件服务
	tmpFile := t.TempDir() + "/local.png"
	if err := writeTestFile(tmpFile, []byte("fake-png")); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Image{Path: tmpFile},
		&message.File{Path: tmpFile, Name: "doc.bin"},
	}}
	messages, err := a.buildLineMessages(context.Background(), chain)
	if err != nil {
		t.Fatalf("buildLineMessages: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("期望 2 条消息，实际 %d", len(messages))
	}
	imgMsg, ok := messages[0].(*messaging_api.ImageMessage)
	if !ok {
		t.Fatalf("消息 0 应为 ImageMessage，实际 %T", messages[0])
	}
	if !strings.HasPrefix(imgMsg.OriginalContentUrl, "http://127.0.0.1:") ||
		!strings.Contains(imgMsg.OriginalContentUrl, "/api/file/") {
		t.Errorf("本地文件应注册到文件服务，实际 %q", imgMsg.OriginalContentUrl)
	}
	// 通过文件服务 URL 实际拉取内容验证
	resp, err := http.Get(imgMsg.OriginalContentUrl)
	if err != nil {
		t.Fatalf("拉取文件服务内容失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("文件服务应返回 200，实际 %d", resp.StatusCode)
	}

	fileMsg, ok := messages[1].(*fileMessage)
	if !ok {
		t.Fatalf("消息 1 应为 fileMessage，实际 %T", messages[1])
	}
	if fileMsg.FileName != "doc.bin" || fileMsg.FileSize != int64(len("fake-png")) {
		t.Errorf("fileMessage 元数据不正确: %+v", fileMsg)
	}
	if fileMsg.OriginalContentURL == "" {
		t.Error("fileMessage 应包含 originalContentUrl")
	}
}

// TestBuildLineMessagesCallbackBase 验证配置 callback_api_base 时媒体 URL 使用公网地址 (对应 H-19)。
func TestBuildLineMessagesCallbackBase(t *testing.T) {
	a := New(map[string]interface{}{
		"id":                   "line-test",
		"channel_access_token": "test-token",
		"channel_secret":       "secret",
		"callback_api_base":    "https://bot.example.com",
	}, nil, nil)
	tmpFile := t.TempDir() + "/local.png"
	if err := writeTestFile(tmpFile, []byte("fake-png")); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Image{Path: tmpFile},
	}}
	messages, err := a.buildLineMessages(context.Background(), chain)
	if err != nil {
		t.Fatalf("buildLineMessages: %v", err)
	}
	if len(messages) != 1 {
		t.Fatalf("期望 1 条消息，实际 %d", len(messages))
	}
	imgMsg, ok := messages[0].(*messaging_api.ImageMessage)
	if !ok {
		t.Fatalf("消息 0 应为 ImageMessage，实际 %T", messages[0])
	}
	if !strings.HasPrefix(imgMsg.OriginalContentUrl, "https://bot.example.com/api/file/") {
		t.Errorf("配置 callback_api_base 后媒体 URL 应使用公网 HTTPS 地址，实际 %q", imgMsg.OriginalContentUrl)
	}
}

// ── 文件名提取 ───────────────────────────────────────────────────

func TestExtractFilenameFromDisposition(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`attachment; filename="report.pdf"`, "report.pdf"},
		{`attachment; filename*=UTF-8''%E6%96%87%E4%BB%B6.txt`, "文件.txt"},
		// 与 Python 一致：按分号顺序匹配，先命中 filename= 即返回
		{`attachment; filename="a.txt"; filename*=UTF-8''b.txt`, "a.txt"},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractFilenameFromDisposition(c.in); got != c.want {
			t.Errorf("extractFilenameFromDisposition(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// ── 发送 HTTP（httptest）─────────────────────────────────────────

func TestReplyMessageHTTP(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/bot/message/reply" {
			t.Errorf("路径应为 /v2/bot/message/reply，实际 %s", r.URL.Path)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-token" {
			t.Errorf("Authorization 头不正确: %q", auth)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("解码请求体失败: %v", err)
		}
		got = body
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client, err := NewLineAPIClient("test-token", "secret")
	if err != nil {
		t.Fatalf("NewLineAPIClient: %v", err)
	}
	client.WithEndpointOverride(srv.URL)

	ok := client.ReplyMessage(context.Background(), "RT-1", []messaging_api.MessageInterface{
		&messaging_api.TextMessage{Text: "hi"},
	})
	if !ok {
		t.Fatal("ReplyMessage 应成功")
	}
	if got["replyToken"] != "RT-1" {
		t.Errorf("replyToken 应为 RT-1，实际 %v", got["replyToken"])
	}
	messages := got["messages"].([]interface{})
	if len(messages) != 1 {
		t.Fatalf("messages 应为 1 条，实际 %d", len(messages))
	}
	msg := messages[0].(map[string]interface{})
	if msg["type"] != "text" || msg["text"] != "hi" {
		t.Errorf("消息内容不正确: %v", msg)
	}
}

// ── Webhook 回调（httptest）──────────────────────────────────────

func TestWebhookCallback(t *testing.T) {
	secret := "hook-secret"
	client, err := NewLineAPIClient("token", secret)
	if err != nil {
		t.Fatalf("NewLineAPIClient: %v", err)
	}
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	a.lineAPI = client

	body := []byte(`{"destination":"U-bot","events":[{"type":"message","webhookEventId":"E1","replyToken":"RT1","timestamp":1700000000000,"source":{"type":"user","userId":"U1"},"message":{"type":"text","text":"hi"}}]}`)

	req := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(string(body)))
	req.Header.Set("x-line-signature", hmacBase64(secret, body))
	w := httptest.NewRecorder()
	a.WebhookCallback(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("合法请求应返回 200，实际 %d", w.Code)
	}
	if a.destination != "U-bot" {
		t.Errorf("destination 应为 U-bot，实际 %q", a.destination)
	}
	// 重复事件（同一 webhookEventId）应被去重跳过——通过内部状态验证
	if !a.isDuplicateEvent("E1") {
		t.Error("事件 E1 应已被记录为已处理")
	}

	// 错误签名 → 400
	req2 := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader(string(body)))
	req2.Header.Set("x-line-signature", "bad")
	w2 := httptest.NewRecorder()
	a.WebhookCallback(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Errorf("错误签名应返回 400，实际 %d", w2.Code)
	}

	// 非法 JSON → 400
	req3 := httptest.NewRequest(http.MethodPost, "/callback", strings.NewReader("{not json"))
	req3.Header.Set("x-line-signature", hmacBase64(secret, []byte("{not json")))
	w3 := httptest.NewRecorder()
	a.WebhookCallback(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d", w3.Code)
	}
}

// ── 辅助 ─────────────────────────────────────────────────────────

func writeTestFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0o644)
}

func TestNewAdapterMissingConfig(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	if a.lineAPI != nil {
		t.Error("缺少 channel_access_token/channel_secret 时 lineAPI 应为 nil")
	}
	if err := a.Send("G1", &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "hi"}}}); err == nil {
		t.Error("lineAPI 未初始化时 Send 应返回错误")
	}
}

// ── L-33：缺失字段不 panic、不产生 "<nil>" ──────────────────────

func TestStringVal(t *testing.T) {
	if got := stringVal(nil); got != "" {
		t.Errorf("stringVal(nil) = %q，期望空串", got)
	}
	if got := stringVal("x"); got != "x" {
		t.Errorf("stringVal(\"x\") = %q", got)
	}
	if got := stringVal(123); got != "" {
		t.Errorf("stringVal(123) = %q，期望空串", got)
	}
}

func TestConvertMessageMissingFields(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	// 缺失 webhookEventId / userId / message.id / deliveryContext：
	// 旧代码会 panic（类型断言）或产生 "<nil>" 字面量。
	event := map[string]interface{}{
		"type":    "message",
		"mode":    "active",
		"source":  map[string]interface{}{"type": "user"},
		"message": map[string]interface{}{"type": "text", "text": "hi"},
	}
	abm := a.convertMessage(event)
	if abm == nil {
		t.Fatal("convertMessage 不应返回 nil")
	}
	if abm.MessageID == "" {
		t.Error("MessageID 应回退到随机值而非空")
	}
	if strings.Contains(abm.Sender.UserID, "<nil>") || strings.Contains(abm.SessionID, "<nil>") || strings.Contains(abm.MessageID, "<nil>") {
		t.Errorf("缺失字段不应产生 <nil> 字面量: %+v", abm)
	}
}

// ── L-35：replyToken TTL 清理 ────────────────────────────────────

func TestSweepReplyTokens(t *testing.T) {
	a := New(map[string]interface{}{"id": "line-test"}, nil, nil)
	a.rtMu.Lock()
	a.replyTokens["fresh"] = replyTokenEntry{token: "T1", at: time.Now()}
	a.replyTokens["stale"] = replyTokenEntry{token: "T2", at: time.Now().Add(-2 * replyTokenTTL)}
	a.rtMu.Unlock()

	a.sweepReplyTokens()

	a.rtMu.Lock()
	defer a.rtMu.Unlock()
	if _, ok := a.replyTokens["fresh"]; !ok {
		t.Error("未过期的 replyToken 不应被清理")
	}
	if _, ok := a.replyTokens["stale"]; ok {
		t.Error("过期 replyToken 应被清理")
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

// ── L-32：临时媒体文件清理 ───────────────────────────────────────

func TestStoreTempContent(t *testing.T) {
	path := storeTempContent("test", "msg-1", []byte("content"), ".txt", "file.txt")
	if path == "" {
		t.Fatal("storeTempContent 返回空路径")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("临时文件应存在: %v", err)
	}
	if !strings.HasPrefix(filepath.Base(path), "file") {
		t.Errorf("文件名前缀应保留原文件名: %s", filepath.Base(path))
	}
	_ = os.Remove(path)
}

// TestMediaServerTokenSweep 验证过期 token 被清除、owned 临时文件被删除（L-32/L-35）。
func TestMediaServerTokenSweep(t *testing.T) {
	ms := &mediaServer{tokens: map[string]mediaTokenEntry{}}

	ownedFile := filepath.Join(t.TempDir(), "owned.bin")
	if err := os.WriteFile(ownedFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherFile := filepath.Join(t.TempDir(), "other.bin")
	if err := os.WriteFile(otherFile, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}

	ms.tokenMu.Lock()
	ms.tokens["owned"] = mediaTokenEntry{path: ownedFile, owned: true, at: time.Now().Add(-2 * mediaTokenTTL)}
	ms.tokens["other"] = mediaTokenEntry{path: otherFile, owned: false, at: time.Now().Add(-2 * mediaTokenTTL)}
	ms.tokenMu.Unlock()

	// 触发一次注册以执行 sweep
	if _, err := ms.registerFile(filepath.Join(t.TempDir(), "live.bin"), false); err != nil {
		t.Fatal(err)
	}

	ms.tokenMu.RLock()
	_, ownedExists := ms.tokens["owned"]
	_, otherExists := ms.tokens["other"]
	ms.tokenMu.RUnlock()
	if ownedExists || otherExists {
		t.Error("过期 token 应被清除")
	}
	if _, err := os.Stat(ownedFile); !os.IsNotExist(err) {
		t.Errorf("owned 临时文件应在 token 过期时被删除: %v", err)
	}
	if _, err := os.Stat(otherFile); err != nil {
		t.Errorf("非 owned 文件不应被删除: %v", err)
	}
}

// ── L-35：readBody 区分 io.EOF 与其他读取错误 ────────────────────

// chunkErrReader 在返回数据的同时返回非 EOF 错误，模拟响应体读取中断。
type chunkErrReader struct {
	readErr error
}

func (r *chunkErrReader) Read(p []byte) (int, error) {
	n := copy(p, []byte("partial-data"))
	return n, r.readErr
}

func (r *chunkErrReader) Close() error { return nil }

func TestReadBodyNonEOFError(t *testing.T) {
	resp := &http.Response{Body: &chunkErrReader{readErr: errors.New("boom")}}
	got := readBody(resp)
	if string(got) != "partial-data" {
		t.Errorf("非 EOF 读取错误时不应吞掉已读部分，得到 %q", string(got))
	}

	resp2 := &http.Response{Body: &chunkErrReader{readErr: io.EOF}}
	got2 := readBody(resp2)
	if string(got2) != "partial-data" {
		t.Errorf("EOF 结束时应保留已读部分，得到 %q", string(got2))
	}
}
