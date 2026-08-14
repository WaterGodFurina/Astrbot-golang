package lark

import (
	"context"
	"testing"
	"time"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

func strPtr(s string) *string { return &s }

// newReceiveEvent 构造一个 im.message.receive_v1 事件。
func newReceiveEvent(chatID, chatType string) *larkim.P2MessageReceiveV1 {
	return &larkim.P2MessageReceiveV1{
		Event: &larkim.P2MessageReceiveV1Data{
			Message: &larkim.EventMessage{
				ChatId:      strPtr(chatID),
				ChatType:    strPtr(chatType),
				MessageId:   strPtr("om_msg_1"),
				MessageType: strPtr("text"),
				Content:     strPtr(`{"text":"hello"}`),
				CreateTime:  strPtr("1700000000000"),
			},
			Sender: &larkim.EventSender{
				SenderId: &larkim.UserId{OpenId: strPtr("ou_user_1")},
			},
		},
	}
}

// captureStage 捕获进入管线的首个事件。
type captureStage struct{ ch chan *core.Event }

// Name 返回阶段名。
func (s *captureStage) Name() string { return "capture" }

// Process 把事件放入 channel 并终止管线。
func (s *captureStage) Process(_ context.Context, event *core.Event) (*core.StageResult, error) {
	select {
	case s.ch <- event:
	default:
	}
	return &core.StageResult{Continue: false}, nil
}

// runCaptureAdapter 构造一个捕获事件的 EventBus 并返回适配器。
func runCaptureAdapter(t *testing.T) (*Adapter, <-chan *core.Event, context.CancelFunc) {
	t.Helper()
	bus := core.NewEventBus(16)
	scheduler := core.NewPipelineScheduler("test")
	captured := make(chan *core.Event, 1)
	scheduler.AddStage(&captureStage{ch: captured})
	bus.RegisterScheduler("test", scheduler)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = bus.Start(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	a := New(map[string]interface{}{"app_id": "app", "app_secret": "sec"}, nil, nil)
	a.EventBus = bus
	a.botOpenID = "ou_bot_1"
	return a, captured, cancel
}

// TestConvertMsgGroupChatID 验证群聊消息 GroupID/SessionID/ConvID 使用真实 chat_id (对应 H-17)。
func TestConvertMsgGroupChatID(t *testing.T) {
	a, captured, _ := runCaptureAdapter(t)
	a.convertMsg(newReceiveEvent("oc_group_1", "group"))

	select {
	case e := <-captured:
		if e.Source.ConvID != "oc_group_1" {
			t.Errorf("群消息 ConvID 应为真实 chat_id %q, 实际 %q", "oc_group_1", e.Source.ConvID)
		}
		if !e.Source.IsGroup {
			t.Errorf("群消息 IsGroup 应为 true")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时未收到事件")
	}
}

// TestConvertMsgP2PSessionID 验证私聊消息 SessionID 仍为发送者 open_id。
func TestConvertMsgP2PSessionID(t *testing.T) {
	a, captured, _ := runCaptureAdapter(t)
	a.convertMsg(newReceiveEvent("oc_p2p_1", "p2p"))

	select {
	case e := <-captured:
		if e.Source.ConvID != "ou_user_1" {
			t.Errorf("私聊消息 ConvID 应为发送者 open_id, 实际 %q", e.Source.ConvID)
		}
		if e.Source.IsGroup {
			t.Errorf("私聊消息 IsGroup 应为 false")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时未收到事件")
	}
}

// TestWebhookEventID 验证 event_id 提取: schema v2 从 header 读取, 兼容旧格式顶层 (对应 M-29)。
func TestWebhookEventID(t *testing.T) {
	v2 := map[string]interface{}{
		"header": map[string]interface{}{
			"event_id":   "evt_v2",
			"event_type": "im.message.receive_v1",
		},
		"event": map[string]interface{}{},
	}
	if id := webhookEventID(v2); id != "evt_v2" {
		t.Errorf("schema v2 event_id 应从 header 读取, got %q", id)
	}

	legacy := map[string]interface{}{"event_id": "evt_v1"}
	if id := webhookEventID(legacy); id != "evt_v1" {
		t.Errorf("旧格式 event_id 应从顶层读取, got %q", id)
	}

	hybrid := map[string]interface{}{
		"header":   map[string]interface{}{"event_type": "x"},
		"event_id": "evt_top",
	}
	if id := webhookEventID(hybrid); id != "evt_top" {
		t.Errorf("header.event_id 为空时应回退顶层, got %q", id)
	}
}
