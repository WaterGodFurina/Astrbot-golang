package weixin_official_account

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// stubStage 捕获管线事件并将结果文本注入事件（模拟 LLM 回复）。
type stubStage struct {
	replyText string
	got       chan string
	adapter   *Adapter // 模拟 RespondStage：经 adapter.Send 投递回复
}

func (s *stubStage) Name() string { return "stub" }

func (s *stubStage) Process(ctx context.Context, event *core.Event) (*core.StageResult, error) {
	if s.got != nil {
		s.got <- event.MessageStr
	}
	if s.replyText != "" && s.adapter != nil {
		// 模拟 RespondStage：管线结果经 PlatformManager → adapter.Send 投递。
		chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: s.replyText}}}
		_ = s.adapter.Send(event.Source.ConvID, chain)
	}
	return &core.StageResult{Continue: false}, nil
}

// TestPassiveWindowImmediateReply: 管线在窗口内完成时回加密/明文 XML 而非占位符。
func TestPassiveWindowImmediateReply(t *testing.T) {
	bus := core.NewEventBus(10)
	sched := core.NewPipelineScheduler("wx")
	stub := &stubStage{replyText: "管线回复", got: make(chan string, 1)}
	sched.AddStage(stub)
	bus.RegisterScheduler("wx", sched)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	t.Cleanup(func() { bus.Stop() })

	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	a.SetEventBus(bus)
	stub.adapter = a

	req := buildPassiveRequest(a, "o_user", "你好", "9001")
	w := httptest.NewRecorder()
	a.callbackCommand(w, req)

	select {
	case got := <-stub.got:
		if got != "你好" {
			t.Errorf("管线应收到消息: %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("管线未收到消息")
	}

	body := w.Body.String()
	if strings.Contains(body, "正在思考") {
		t.Fatalf("窗口内完成不应返回占位符: %q", body)
	}
	if !strings.Contains(body, "管线回复") {
		t.Fatalf("被动回复应携带管线结果: %q", body)
	}
}

// slowStage 管线阶段：阻塞直到测试放行（模拟慢 LLM），放行后经 Send 投递回复。
type slowStage struct {
	release chan struct{}
	got     chan string
	adapter *Adapter
}

func (s *slowStage) Name() string { return "slow" }

func (s *slowStage) Process(ctx context.Context, event *core.Event) (*core.StageResult, error) {
	s.got <- event.MessageStr
	<-s.release
	if s.adapter != nil {
		chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: "管线回复"}}}
		_ = s.adapter.Send(event.Source.ConvID, chain)
	}
	return &core.StageResult{Continue: false}, nil
}

// TestPassiveWindowTimeoutPlaceholder: 管线超时时返回【正在思考…】占位符，
// 且管线结果留存于缓冲，用户下次触发时弹出。
func TestPassiveWindowTimeoutPlaceholder(t *testing.T) {
	bus := core.NewEventBus(10)
	sched := core.NewPipelineScheduler("wx")
	release := make(chan struct{})
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	a.SetEventBus(bus)
	stub := &slowStage{release: release, got: make(chan string, 1), adapter: a}
	sched.AddStage(stub)
	bus.RegisterScheduler("wx", sched)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	t.Cleanup(func() { bus.Stop() })

	req := buildPassiveRequest(a, "o_user", "慢消息", "9002")
	w := httptest.NewRecorder()
	start := time.Now()
	a.callbackCommand(w, req)
	if elapsed := time.Since(start); elapsed < 3*time.Second || elapsed > 5*time.Second {
		t.Fatalf("应在 4s 窗口末尾返回，实际耗时 %v", elapsed)
	}
	body := w.Body.String()
	if !strings.Contains(body, "正在思考") || !strings.Contains(body, "慢消息") {
		t.Fatalf("超时应返回含预览的占位符: %q", body)
	}

	// 管线放行 → 回复经 Send 入缓存；用户再触发 → 弹出。
	select {
	case <-stub.got:
	case <-time.After(2 * time.Second):
		t.Fatal("管线未收到消息")
	}
	close(release)
	// 等待管线完成（worker goroutine flush pending → cached）。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if st := a.getUserState("o_user"); st != nil {
			st.mu.Lock()
			ready := len(st.cached) > 0
			st.mu.Unlock()
			if ready {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("管线放行后回复未进入缓存")
		}
		time.Sleep(20 * time.Millisecond)
	}
	w2 := httptest.NewRecorder()
	a.callbackCommand(w2, buildPassiveRequest(a, "o_user", "继续", "9006"))
	if !strings.Contains(w2.Body.String(), "管线回复") {
		t.Fatalf("用户再触发应弹出缓存的管线回复: %q", w2.Body.String())
	}
}

// TestPassiveDuplicateMsgIDRetry: 同 msg_id 重试复用缓存，不重复进入管线。
func TestPassiveDuplicateMsgIDRetry(t *testing.T) {
	bus := core.NewEventBus(10)
	sched := core.NewPipelineScheduler("wx")
	stub := &stubStage{replyText: "首次回复", got: make(chan string, 2)}
	sched.AddStage(stub)
	bus.RegisterScheduler("wx", sched)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	t.Cleanup(func() { bus.Stop() })

	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	a.SetEventBus(bus)
	stub.adapter = a

	// 第一次请求：等待窗口内管线完成并回显结果。
	w1 := httptest.NewRecorder()
	a.callbackCommand(w1, buildPassiveRequest(a, "o_user", "问题", "9003"))
	select {
	case <-stub.got:
	case <-time.After(5 * time.Second):
		t.Fatal("管线未收到消息")
	}
	if !strings.Contains(w1.Body.String(), "首次回复") {
		t.Fatalf("首次请求应返回回复: %q", w1.Body.String())
	}

	// 第二次请求：同 msg_id（微信重试）→ 排重表命中缓存条目，管线仅收到 1 条。
	w2 := httptest.NewRecorder()
	a.callbackCommand(w2, buildPassiveRequest(a, "o_user", "问题", "9003"))
	select {
	case got := <-stub.got:
		t.Fatalf("重试不应重复进入管线: %q", got)
	case <-time.After(500 * time.Millisecond):
	}
}

// TestPassiveCachedXMLTrigger: 占位符后用户再发消息 → 弹出缓冲的完整回复。
func TestPassiveCachedXMLTrigger(t *testing.T) {
	bus := core.NewEventBus(10)
	sched := core.NewPipelineScheduler("wx")
	stub := &stubStage{replyText: strings.Repeat("迟", 1100), got: make(chan string, 2)}
	sched.AddStage(stub)
	bus.RegisterScheduler("wx", sched)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = bus.Start(ctx) }()
	t.Cleanup(func() { bus.Stop() })

	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	a.SetEventBus(bus)
	stub.adapter = a

	// 第一次：管线窗口内完成 → 返回第一段并附加"缓冲中"提示。
	w1 := httptest.NewRecorder()
	a.callbackCommand(w1, buildPassiveRequest(a, "o_user", "长问题", "9004"))
	select {
	case <-stub.got:
	case <-time.After(5 * time.Second):
		t.Fatal("管线未收到消息")
	}
	body1 := w1.Body.String()
	if !strings.Contains(body1, "迟") || !strings.Contains(body1, "后续消息还在缓冲中") {
		t.Fatalf("首次窗口应返回首段并附加缓冲提示: %q", body1)
	}

	// 用户再发任意消息 → 弹出第二段（缓存弹出不发布新事件，对齐本体 cached_xml 分支）。
	w2 := httptest.NewRecorder()
	a.callbackCommand(w2, buildPassiveRequest(a, "o_user", "继续", "9005"))
	select {
	case got := <-stub.got:
		t.Fatalf("缓存弹出不应再次进入管线: %q", got)
	case <-time.After(300 * time.Millisecond):
	}
	body2 := w2.Body.String()
	if !strings.Contains(body2, "迟") || strings.Contains(body2, "后续消息还在缓冲中") {
		t.Fatalf("末段弹出不应附加提示: %q", body2)
	}
	// 缓冲耗尽 → 用户缓冲应被清理。
	if st := a.getUserState("o_user"); st != nil {
		t.Fatalf("缓冲耗尽后应清理用户缓冲")
	}
}
