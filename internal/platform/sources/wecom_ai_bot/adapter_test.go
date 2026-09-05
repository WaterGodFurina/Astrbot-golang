package wecom_ai_bot

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// fakeEventBus 测试用事件总线。
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

// testEncodingAESKey 生成 43 位 EncodingAESKey。
func testEncodingAESKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)[:43]
}

// newTestAIBotAdapter 构造测试适配器。
func newTestAIBotAdapter(t *testing.T, bus platform.EventBus, extra map[string]interface{}) *Adapter {
	t.Helper()
	config := map[string]interface{}{
		"id":                           "wecom_ai_bot",
		"type":                         "wecom_ai_bot",
		"wecom_ai_bot_connection_mode": "webhook",
		"wecom_ai_bot_name":            "测试机器人",
		"wecomaibot_token":             "test_token",
		"wecomaibot_encoding_aes_key":  testEncodingAESKey(t),
		"callback_server_host":         "127.0.0.1",
		"port":                         6198,
	}
	for k, v := range extra {
		config[k] = v
	}
	a := New(config, map[string]interface{}{}, nil)
	a.EventBus = bus
	return a
}

// TestAIBotURLVerification GET 验证请求。
func TestAIBotURLVerification(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAIBotAdapter(t, bus, nil)
	echostr := ""
	// 使用 API 客户端的加解密器构造 echostr
	_, echostr = a.apiClient.wxcpt.encrypt("echo_verify_123")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "nonce123"
	_, sig := a.apiClient.wxcpt.GetSHA1(timestamp, nonce, echostr)

	req := httptest.NewRequest(http.MethodGet, "/webhook/wecom-ai-bot?"+
		"msg_signature="+sig+"&timestamp="+timestamp+"&nonce="+nonce+"&echostr="+url.QueryEscape(echostr), nil)
	w := httptest.NewRecorder()
	a.server.handleVerify(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: %d body: %s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "echo_verify_123" {
		t.Errorf("验证结果: %q", got)
	}
}

// TestAIBotURLVerificationMissingParams 缺少参数返回 400。
func TestAIBotURLVerificationMissingParams(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/webhook/wecom-ai-bot?msg_signature=x", nil)
	w := httptest.NewRecorder()
	a.server.handleVerify(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("缺少参数应返回 400，got %d", w.Code)
	}
}

// TestAIBotCallbackTextMessage POST 消息回调：初始响应文本加密返回。
func TestAIBotCallbackTextMessage(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAIBotAdapter(t, bus, map[string]interface{}{
		"wecomaibot_init_respond_text": "收到，正在处理",
	})
	// 启动队列监听器（Start 的一部分，测试中手动启动）
	a.queueMgr.SetListener(a.handleQueuedMessage)
	crypt := a.apiClient.wxcpt

	// 构造明文消息并加密
	plain := `{"chattype":"single","msgtype":"text","text":{"content":"你好"},"from":{"userid":"user_1"}}`
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	ret, encrypted := crypt.EncryptMsg(plain, "nonce1", ts)
	if ret != MsgCryptOK {
		t.Fatal("加密失败")
	}
	postData := []byte(`{"encrypt":"` + extractEncryptField(t, encrypted) + `"}`)
	_, sig := crypt.GetSHA1(ts, "nonce1", extractEncryptField(t, encrypted))

	req := httptest.NewRequest(http.MethodPost, "/webhook/wecom-ai-bot?msg_signature="+sig+"&timestamp="+ts+"&nonce=nonce1",
		strings.NewReader(string(postData)))
	w := httptest.NewRecorder()
	a.server.handleCallback(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("状态码: %d body: %s", w.Code, w.Body.String())
	}

	// 解密响应，应为初始响应文本的 stream 消息
	respBody := w.Body.String()
	if respBody == "success" {
		t.Fatal("应返回加密的初始响应")
	}
	var decrypted string
	// 先以无效签名调用解密，验证其优雅失败（不 panic），结果无需使用。
	_, _ = crypt.DecryptMsg([]byte(respBody), "", "", "")
	// 重新用响应中的签名验证
	_, sig2 := crypt.GetSHA1("1700000001", "nonce1", extractEncryptField(t, respBody))
	ret, decrypted = crypt.DecryptMsg([]byte(respBody), sig2, "1700000001", "nonce1")
	if ret != MsgCryptOK {
		t.Fatalf("解密响应失败: %d", ret)
	}
	var resp map[string]interface{}
	json.Unmarshal([]byte(decrypted), &resp)
	stream, _ := resp["stream"].(map[string]interface{})
	if resp["msgtype"] != "stream" || stream["content"] != "收到，正在处理" {
		t.Errorf("初始响应异常: %v", resp)
	}

	// 队列应收到消息并发布事件（异步，等待）
	deadline := time.Now().Add(2 * time.Second)
	for bus.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bus.count() != 1 {
		t.Fatalf("应发布 1 个事件，got %d", bus.count())
	}
	ev := bus.last()
	if ev.MessageStr != "你好" || ev.Source.ConvID != "wecom_ai_bot_wecomai_user_1" {
		t.Errorf("事件异常: %+v", ev)
	}
	if ev.Source.Platform != "wecom_ai_bot" {
		t.Errorf("平台: %q", ev.Source.Platform)
	}
}

// TestAIBotCallbackBadSignature 错误签名返回 400。
func TestAIBotCallbackBadSignature(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAIBotAdapter(t, bus, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/wecom-ai-bot?msg_signature=bad&timestamp=1&nonce=2",
		strings.NewReader(`{"encrypt":"abc"}`))
	w := httptest.NewRecorder()
	a.server.handleCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("错误签名应返回 400，got %d", w.Code)
	}
	if bus.count() != 0 {
		t.Error("不应发布事件")
	}
}

// TestConvertMessage 消息转换：混合消息 + At 处理 + 图片 base64。
func TestConvertMessage(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	payload := &QueueItem{
		MessageData: map[string]interface{}{
			"chattype": "group",
			"chatid":   "chat_1",
			"msgtype":  "mixed",
			"from":     map[string]interface{}{"userid": "user_1"},
			"mixed": map[string]interface{}{
				"msg_item": []interface{}{
					map[string]interface{}{"msgtype": "text", "text": map[string]interface{}{"content": "@测试机器人 你好"}},
					map[string]interface{}{"msgtype": "text", "text": map[string]interface{}{"content": "世界"}},
				},
			},
		},
		SessionID: "wecom_ai_bot_wecomai_chat_1",
	}
	abm := a.convertMessage(payload)
	if abm == nil {
		t.Fatal("转换失败")
	}
	if abm.Type != platform.GroupMessage {
		t.Errorf("群聊消息类型: %v", abm.Type)
	}
	if abm.SessionID != "wecom_ai_bot_wecomai_chat_1" {
		t.Errorf("会话 ID: %q", abm.SessionID)
	}
	if abm.Sender.UserID != "user_1" {
		t.Errorf("发送者: %+v", abm.Sender)
	}
	// At 组件在前
	first, ok := abm.Message[0].(*message.At)
	if !ok || first.Name != "测试机器人" {
		t.Errorf("At 组件异常: %v", abm.Message[0])
	}
	// 文本中 @机器人 已被移除
	if strings.Contains(abm.MessageStr, "@测试机器人") {
		t.Errorf("文本不应包含 @机器人: %q", abm.MessageStr)
	}
}

// TestConvertMessageImage 图片消息转换（不含下载 URL 时返回占位文本）。
func TestConvertMessageImage(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	payload := &QueueItem{
		MessageData: map[string]interface{}{
			"chattype": "single",
			"msgtype":  "image",
			"from":     map[string]interface{}{"userid": "u1"},
			"image":    map[string]interface{}{},
		},
		SessionID: "wecom_ai_bot_wecomai_u1",
	}
	abm := a.convertMessage(payload)
	if abm == nil || abm.MessageStr != "[未知消息]" {
		t.Errorf("图片消息转换异常: %+v", abm)
	}
}

// TestExtractSessionID 会话 ID 提取：群聊/单聊。
func TestExtractSessionID(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	group := a.extractSessionID(map[string]interface{}{
		"chattype": "group", "chatid": "g1",
	})
	if group != "wecom_ai_bot_wecomai_g1" {
		t.Errorf("群聊会话: %q", group)
	}
	single := a.extractSessionID(map[string]interface{}{
		"from": map[string]interface{}{"userid": "u2"},
	})
	if single != "wecom_ai_bot_wecomai_u2" {
		t.Errorf("单聊会话: %q", single)
	}
	defaultSession := a.extractSessionID(map[string]interface{}{})
	if defaultSession != "wecom_ai_bot_wecomai_default_user" {
		t.Errorf("默认会话: %q", defaultSession)
	}
}

// TestQueueMgr 队列管理器：入队、监听回调、待处理响应、清理。
func TestQueueMgr(t *testing.T) {
	mgr := NewWecomAIQueueMgr()
	var mu sync.Mutex
	var received *QueueItem
	cb := func(item *QueueItem) {
		mu.Lock()
		received = item
		mu.Unlock()
	}
	mgr.SetListener(cb)

	// 入队后监听器回调
	q := mgr.GetOrCreateQueue("stream_1")
	q <- &QueueItem{Type: "plain", Data: "hello", SessionID: "stream_1"}
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		r := received
		mu.Unlock()
		if r != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	gotReceived := received
	mu.Unlock()
	if gotReceived == nil || gotReceived.Data != "hello" {
		t.Fatalf("监听器未收到消息: %+v", gotReceived)
	}

	// 输出队列与待处理响应
	bq := mgr.GetOrCreateBackQueue("stream_1")
	bq <- &QueueItem{Type: "plain", Data: "x", Streaming: true}
	if !mgr.HasBackQueue("stream_1") {
		t.Error("输出队列应存在")
	}
	mgr.SetPendingResponse("stream_1", map[string]string{"nonce": "n"})
	p := mgr.GetPendingResponse("stream_1")
	if p == nil || p.CallbackParams["nonce"] != "n" {
		t.Error("待处理响应异常")
	}

	// 移除并标记完成
	mgr.RemoveQueues("stream_1", true)
	if mgr.HasBackQueue("stream_1") || mgr.HasQueue("stream_1") {
		t.Error("队列应被移除")
	}
	if !mgr.IsStreamFinished("stream_1", 60) {
		t.Error("流应标记为完成")
	}
}

// TestHandleTextMessageDoesNotBlockReadLoop 回归 H-29：回调消息投递到独立 handler
// goroutine，即使 handler 阻塞（如 SendCommand 等待响应），读循环也不会被卡住。
func TestHandleTextMessageDoesNotBlockReadLoop(t *testing.T) {
	blocker := make(chan struct{})
	handlerCalled := make(chan struct{}, 1)
	c := NewWecomAIBotLongConnectionClient("bot1", "sec1", "wss://example.com", 30, nil)
	c.messageHandler = func(payload map[string]interface{}) {
		handlerCalled <- struct{}{}
		<-blocker
	}
	handlerCh := make(chan map[string]interface{}, 4)
	handlerDone := make(chan struct{})
	go c.handlerLoop(handlerCh, handlerDone)
	defer func() {
		close(handlerCh)
		<-handlerDone
	}()

	cb := map[string]interface{}{
		"cmd":     "aibot_msg_callback",
		"headers": map[string]interface{}{"req_id": "r1"},
		"body":    map[string]interface{}{"msgtype": "text"},
	}
	data, err := json.Marshal(cb)
	if err != nil {
		t.Fatal(err)
	}
	c.handleTextMessage(string(data), handlerCh)
	select {
	case <-handlerCalled:
	case <-time.After(time.Second):
		t.Fatal("handler 未被调用")
	}
	done := make(chan struct{})
	go func() {
		c.handleTextMessage(string(data), handlerCh)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("读循环不应被阻塞")
	}
	close(blocker)
}

// TestStreamPollImageCacheUntilFinish 图片仅随 finish 帧返回（对齐本体
// wecomai_adapter.py:310-321）：非 finish 轮询先缓存图片不下发，finish
// 轮询一次性携带且不重复（原回归 M-47 的"非 finish 携带图片"行为已按
// 本体对齐反转）。
func TestStreamPollImageCacheUntilFinish(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	crypt := a.apiClient.wxcpt

	imgData := []byte("fake-image-bytes")
	imgB64 := base64.StdEncoding.EncodeToString(imgData)
	bq := a.queueMgr.GetOrCreateBackQueue("sid_stream")
	bq <- &QueueItem{Type: "image", ImageData: imgB64}

	nonce := "nonce_poll"
	timestamp := "1700000100"
	// 图片已入队，文本与结束尚未入队 → 非 finish 轮询不返回图片，先缓存
	resp, err := a.processMessage(map[string]interface{}{
		"msgtype": "stream",
		"stream":  map[string]interface{}{"id": "sid_stream"},
	}, map[string]string{"nonce": nonce, "timestamp": timestamp})
	if err != nil {
		t.Fatalf("非 finish 轮询失败: %v", err)
	}
	if resp != "" {
		t.Fatalf("非 finish 轮询不应下发图片，应返回空响应: %s", resp)
	}
	a.streamCacheMu.Lock()
	cached := a.streamImageCache["sid_stream"]
	a.streamCacheMu.Unlock()
	if len(cached) != 1 || cached[0] != imgB64 {
		t.Fatalf("图片应缓存至 finish，got %v", cached)
	}

	// 文本 + 结束入队，finish 轮询：返回文本并一次性携带缓存图片
	bq <- &QueueItem{Type: "plain", Data: "你好", Streaming: true}
	bq <- &QueueItem{Type: "end", Data: "", Streaming: false}
	resp2, err := a.processMessage(map[string]interface{}{
		"msgtype": "stream",
		"stream":  map[string]interface{}{"id": "sid_stream"},
	}, map[string]string{"nonce": nonce, "timestamp": timestamp})
	if err != nil {
		t.Fatalf("finish 轮询失败: %v", err)
	}
	if resp2 == "" {
		t.Fatal("finish 轮询应返回文本")
	}
	_, sig2 := crypt.GetSHA1(timestamp, nonce, extractEncryptField(t, resp2))
	_, decrypted2 := crypt.DecryptMsg([]byte(resp2), sig2, timestamp, nonce)
	var out2 map[string]interface{}
	if err := json.Unmarshal([]byte(decrypted2), &out2); err != nil {
		t.Fatalf("解密 finish 响应解析失败: %v", err)
	}
	stream2, _ := out2["stream"].(map[string]interface{})
	if stream2["finish"] != true {
		t.Errorf("finish 轮询 finish 应为 true: %v", stream2["finish"])
	}
	if stream2["content"] != "你好" {
		t.Errorf("finish 轮询 content: %v", stream2["content"])
	}
	items2, _ := stream2["msg_item"].([]interface{})
	if len(items2) != 1 {
		t.Fatalf("finish 轮询应一次性携带缓存的 1 个图片，got %d: %v", len(items2), items2)
	}
	item, _ := items2[0].(map[string]interface{})
	img, _ := item["image"].(map[string]interface{})
	if item["msgtype"] != MSGTypeImage || img["base64"] != imgB64 {
		t.Errorf("finish 轮询图片异常: %v", item)
	}
	a.streamCacheMu.Lock()
	leftover := a.streamImageCache["sid_stream"]
	a.streamCacheMu.Unlock()
	if len(leftover) != 0 {
		t.Errorf("finish 后图片缓存应清空: %v", leftover)
	}
}

// TestSendToBackQueue Send 逻辑：正常回复（存在待响应上下文）文本进入输出队列。
func TestSendToBackQueue(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAIBotAdapter(t, bus, nil)
	streamID := "wecom_ai_bot_wecomai_user_1_abcdefghij"
	a.sessionStreamMap.Store("wecom_ai_bot_wecomai_user_1", streamID)
	// 注入待响应上下文，模拟用户消息刚到、尚未收尾的正常回复场景。
	a.queueMgr.SetPendingResponse(streamID, map[string]string{"req_id": "req_1"})

	chain := &message.MessageChain{Chain: []message.Component{
		&message.Plain{Text: "回复内容"},
	}}
	if err := a.Send("wecom_ai_bot_wecomai_user_1", chain); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	bq := a.queueMgr.GetOrCreateBackQueue(streamID)
	select {
	case item := <-bq:
		if item.Type != "plain" || item.Data != "回复内容" {
			t.Errorf("输出队列内容异常: %+v", item)
		}
	case <-time.After(time.Second):
		t.Fatal("输出队列超时")
	}
}

// TestSendProactiveWithoutWebhookErrors 主动消息（无待响应上下文）且未配置
// 消息推送 webhook 时应报错返回（对齐本体 wecomai_adapter.py:564-583 RuntimeError），
// 不再静默降级。
func TestSendProactiveWithoutWebhookErrors(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	chain := &message.MessageChain{Chain: []message.Component{
		&message.Plain{Text: "主动消息"},
	}}
	err := a.Send("wecom_ai_bot_wecomai_no_ctx", chain)
	if err == nil {
		t.Fatal("未配置 webhook 的主动消息发送应返回错误")
	}
	if !strings.Contains(err.Error(), "未配置企业微信消息推送 Webhook URL") {
		t.Fatalf("错误信息不符: %v", err)
	}
}

// TestWebhookClientSend 消息推送 webhook 客户端（httptest）。
func TestWebhookClientSend(t *testing.T) {
	var got map[string]interface{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	client, err := NewWecomAIBotWebhookClient(srv.URL + "?key=testkey")
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if err := client.SendPayload(t.Context(), map[string]interface{}{
		"msgtype":     "markdown_v2",
		"markdown_v2": map[string]interface{}{"content": "hello"},
	}); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	if got["msgtype"] != "markdown_v2" {
		t.Errorf("推送载荷异常: %v", got)
	}
}

// TestWebhookClientErrors 无效 URL / 错误码 / 缺少 key。
func TestWebhookClientErrors(t *testing.T) {
	if _, err := NewWecomAIBotWebhookClient(""); err == nil {
		t.Error("空 URL 应报错")
	}
	if _, err := NewWecomAIBotWebhookClient("https://example.com/send"); err == nil {
		t.Error("缺少 key 应报错")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errcode":93000,"errmsg":"invalid key"}`))
	}))
	defer srv.Close()
	client, err := NewWecomAIBotWebhookClient(srv.URL + "?key=k")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendPayload(t.Context(), map[string]interface{}{"msgtype": "text"}); err == nil {
		t.Error("errcode != 0 应报错")
	}
}

// TestWebhookClientMarkdownSplit markdown 分块（4096 字节）。
func TestWebhookClientMarkdownSplit(t *testing.T) {
	client, err := NewWecomAIBotWebhookClient("https://example.com/send?key=k")
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("a", 9000)
	chunks := client.SplitMarkdownV2Content(content, 4096)
	if len(chunks) != 3 {
		t.Errorf("应分为 3 块，got %d", len(chunks))
	}
	for _, c := range chunks {
		if len(c) > 4096 {
			t.Errorf("块超过 4096 字节: %d", len(c))
		}
	}
	empty := client.SplitMarkdownV2Content("", 4096)
	if len(empty) != 0 {
		t.Error("空内容应返回空列表")
	}
}

// TestSendOnlyWebhookMode 仅 webhook 推送模式：Send 直接推送到 webhook 并标记流完成。
func TestSendOnlyWebhookMode(t *testing.T) {
	var mu sync.Mutex
	pushCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pushCount++
		mu.Unlock()
		w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer srv.Close()

	a := newTestAIBotAdapter(t, &fakeEventBus{}, map[string]interface{}{
		"msg_push_webhook_url":         srv.URL + "?key=k",
		"only_use_webhook_url_to_send": true,
	})
	if a.webhookClient == nil {
		t.Fatal("webhook 客户端未初始化")
	}
	a.sessionStreamMap.Store("sid_sess", "sid_stream")
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "推送消息"}}}
	if err := a.Send("sid_sess", chain); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
	mu.Lock()
	n := pushCount
	mu.Unlock()
	if n == 0 {
		t.Error("应推送 webhook")
	}
	// 输出队列应写入 complete 标记（由 webhook 轮询聚合时消费并标记流完成）
	bq := a.queueMgr.GetOrCreateBackQueue("sid_stream")
	deadline := time.Now().Add(2 * time.Second)
	seenComplete := false
	for len(bq) > 0 {
		item := <-bq
		if item.Type == "complete" {
			seenComplete = true
			break
		}
	}
	_ = deadline
	if !seenComplete {
		// 再次尝试等待
		for time.Now().Before(deadline) && len(bq) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
		for len(bq) > 0 {
			item := <-bq
			if item.Type == "complete" {
				seenComplete = true
				break
			}
		}
	}
	if !seenComplete {
		t.Error("输出队列应包含 complete 标记")
	}
}

// TestWebhookPlatformInterface 统一 Webhook 模式接口实现。
func TestWebhookPlatformInterface(t *testing.T) {
	bus := &fakeEventBus{}
	a := newTestAIBotAdapter(t, bus, map[string]interface{}{
		"webhook_uuid":         "ai-123",
		"unified_webhook_mode": true,
	})
	var _ platform.WebhookPlatform = a
	if a.WebhookUUID() != "ai-123" {
		t.Errorf("WebhookUUID: %q", a.WebhookUUID())
	}
	// 长连接模式不应接受 webhook 回调
	longConn := newTestAIBotAdapter(t, bus, map[string]interface{}{
		"wecom_ai_bot_connection_mode": "long_connection",
		"wecomaibot_ws_bot_id":         "bot1",
		"wecomaibot_ws_secret":         "sec1",
	})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{}"))
	longConn.WebhookCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("长连接模式回调应返回 400，got %d", w.Code)
	}
}

// TestCleanupOrphanedBackQueue 回归 L-45.2：无 pendingResponse 且长时间未使用的
// 输出队列应被清理，避免满 512 条后 Send 永久阻塞。
func TestCleanupOrphanedBackQueue(t *testing.T) {
	mgr := NewWecomAIQueueMgr()
	bq := mgr.GetOrCreateBackQueue("orphan_stream")
	bq <- &QueueItem{Type: "plain", Data: "x"}
	mgr.mu.Lock()
	mgr.backQueueLastUsed["orphan_stream"] = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()
	mgr.CleanupExpiredResponses(300)
	if mgr.HasBackQueue("orphan_stream") {
		t.Error("孤立输出队列应被清理")
	}
}

// TestCleanupKeepsActiveBackQueue 有 pendingResponse 的输出队列不应被清理。
func TestCleanupKeepsActiveBackQueue(t *testing.T) {
	mgr := NewWecomAIQueueMgr()
	mgr.GetOrCreateBackQueue("active_stream")
	mgr.SetPendingResponse("active_stream", map[string]string{"nonce": "n"})
	mgr.mu.Lock()
	mgr.backQueueLastUsed["active_stream"] = time.Now().Add(-time.Hour)
	mgr.mu.Unlock()
	mgr.CleanupExpiredResponses(300)
	if !mgr.HasBackQueue("active_stream") {
		t.Error("有 pendingResponse 的输出队列不应被清理")
	}
}

// TestBackQueueWriteTimeoutOnFullQueue 回归 L-45.2：满队列写入应在超时后返回
// false 而不是永久阻塞。
func TestBackQueueWriteTimeoutOnFullQueue(t *testing.T) {
	old := queueWriteTimeout
	queueWriteTimeout = 50 * time.Millisecond
	defer func() { queueWriteTimeout = old }()

	q := make(chan *QueueItem, 1)
	q <- &QueueItem{Type: "plain", Data: "x"}
	start := time.Now()
	if trySendBackQueueItem(q, &QueueItem{Type: "end"}) {
		t.Error("满队列写入应超时返回 false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("满队列写入不应永久阻塞，耗时 %v", elapsed)
	}
}

// TestStreamPlainCacheCleanup 回归 L-45.2：已无输出队列的 streamPlainCache 条目应被清理。
func TestStreamPlainCacheCleanup(t *testing.T) {
	a := newTestAIBotAdapter(t, &fakeEventBus{}, nil)
	a.queueMgr.GetOrCreateBackQueue("s1")
	a.streamCacheMu.Lock()
	a.streamPlainCache["s1"] = "content"
	a.streamPlainCache["s2"] = "content"
	a.streamCacheMu.Unlock()
	a.cleanupStreamPlainCache()
	a.streamCacheMu.Lock()
	defer a.streamCacheMu.Unlock()
	if _, ok := a.streamPlainCache["s1"]; !ok {
		t.Error("有输出队列的缓存不应被清理")
	}
	if _, ok := a.streamPlainCache["s2"]; ok {
		t.Error("无输出队列的缓存应被清理")
	}
}

// TestAIBotServerStartBindFailure 回归 L-45.4：端口占用时 Start 应返回错误而不是仅记日志。
func TestAIBotServerStartBindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	srv := NewWecomAIBotServer("127.0.0.1", port, nil, nil)
	if err := srv.Start(); err == nil {
		t.Error("端口占用时 Start 应返回错误")
	}
}
