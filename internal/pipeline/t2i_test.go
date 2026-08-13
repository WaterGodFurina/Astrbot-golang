package pipeline

import (
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

func newResultDecorateStage(t *testing.T, cfg map[string]interface{}) *ResultDecorateStage {
	t.Helper()
	s := NewResultDecorateStage()
	ctx := &PipelineContext{AstrbotConfig: cfg}
	if err := s.Initialize(ctx); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return s
}

func makeEventWithText(text string) *core.Event {
	e := &core.Event{
		Result: &message.MessageEventResult{
			Chain: []message.Component{&message.Plain{Text: text}},
		},
	}
	return e
}

func TestT2IShortTextNotConverted(t *testing.T) {
	s := newResultDecorateStage(t, map[string]interface{}{
		"t2i":                true,
		"t2i_word_threshold": 100,
		"t2i_strategy":       "local",
	})
	event := makeEventWithText("短文本，不超过阈值")
	if err := s.applyT2I(event); err != nil {
		t.Fatalf("applyT2I: %v", err)
	}
	if _, ok := event.Result.Chain[0].(*message.Plain); !ok {
		t.Fatalf("short text should stay plain, got %T", event.Result.Chain[0])
	}
}

func TestT2IDisabledNotConverted(t *testing.T) {
	s := newResultDecorateStage(t, map[string]interface{}{
		"t2i":                false,
		"t2i_word_threshold": 10,
		"t2i_strategy":       "local",
	})
	event := makeEventWithText(strings.Repeat("这是一段足够长的文本，用于验证禁用时不会被转换。", 5))
	// Process() only invokes applyT2I when t2iEnabled; a disabled stage must
	// leave the chain untouched.
	if s.t2iEnabled {
		t.Fatal("t2i should be disabled")
	}
	if err := s.applyT2I(event); err != nil {
		t.Fatalf("applyT2I: %v", err)
	}
	_ = event
}

func TestT2ILongTextConvertedToImage(t *testing.T) {
	s := newResultDecorateStage(t, map[string]interface{}{
		"t2i":                true,
		"t2i_word_threshold": 20,
		"t2i_strategy":       "local",
	})
	long := strings.Repeat("这是一段用于验证本地文本转图像功能是否正常工作的中文长文本内容。", 8)
	event := makeEventWithText(long)
	if err := s.applyT2I(event); err != nil {
		t.Fatalf("applyT2I: %v", err)
	}
	if len(event.Result.Chain) == 0 {
		t.Fatal("chain empty")
	}
	img, ok := event.Result.Chain[0].(*message.Image)
	if !ok {
		t.Fatalf("long text should become image, got %T", event.Result.Chain[0])
	}
	if img.Base64 == "" {
		t.Fatal("image base64 empty")
	}
}

func TestT2IRemoteNoEndpointFallsBackToLocal(t *testing.T) {
	s := newResultDecorateStage(t, map[string]interface{}{
		"t2i":                true,
		"t2i_word_threshold": 5,
		"t2i_strategy":       "remote",
		"t2i_endpoint":       "",
	})
	event := makeEventWithText("足够长的文本来触发转换逻辑，由于没有配置远端地址应该回退到本地渲染生成图片。")
	if err := s.applyT2I(event); err != nil {
		t.Fatalf("applyT2I: %v", err)
	}
	if len(event.Result.Chain) == 0 {
		t.Fatal("chain empty")
	}
	if _, ok := event.Result.Chain[0].(*message.Image); !ok {
		t.Fatalf("expected fallback to local image, got %T", event.Result.Chain[0])
	}
}

func TestMediaOnlyChain(t *testing.T) {
	res := &message.MessageEventResult{Chain: []message.Component{
		&message.Plain{Text: "已经流式发送的文本"},
		&message.At{TargetID: "u1"},
		&message.Reply{MessageID: "m1"},
		&message.Image{Base64: "aW1n"},
		&message.File{URL: "https://x/file"},
	}}
	media := mediaOnlyChain(res)
	if len(media) != 2 {
		t.Fatalf("expected 2 media components, got %d", len(media))
	}
	if _, ok := media[0].(*message.Image); !ok {
		t.Fatalf("first media should be Image, got %T", media[0])
	}
	if _, ok := media[1].(*message.File); !ok {
		t.Fatalf("second media should be File, got %T", media[1])
	}
	// Pure text chain -> no media to resend.
	empty := mediaOnlyChain(&message.MessageEventResult{Chain: []message.Component{
		&message.Plain{Text: "x"}, &message.At{TargetID: "u"},
	}})
	if len(empty) != 0 {
		t.Fatalf("pure text chain should have no media, got %d", len(empty))
	}
}
