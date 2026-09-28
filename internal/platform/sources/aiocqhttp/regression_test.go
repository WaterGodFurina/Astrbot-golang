package aiocqhttp

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// TestBuildAioMessageStrMentions 覆盖非自身 @ 追加为 " @name(qq) "、
// 首个 @机器人 被跳过、@全体 不进入 message_str（对齐 Python at_parts）。
func TestBuildAioMessageStrMentions(t *testing.T) {
	chain := []message.Component{
		&message.Plain{Text: "hi "},
		&message.At{TargetID: "999", Name: "bot"},
		&message.At{TargetID: "123", Name: "Tom"},
		&message.Plain{Text: " yo"},
	}
	if got, want := buildAioMessageStr(chain, "999"), "hi  @Tom(123)  yo"; got != want {
		t.Errorf("message_str = %q, want %q", got, want)
	}

	all := []message.Component{
		&message.At{TargetID: "all", Name: "全体成员"},
		&message.Plain{Text: "ping"},
	}
	if got, want := buildAioMessageStr(all, "999"), "ping"; got != want {
		t.Errorf("at-all message_str = %q, want %q", got, want)
	}
}

// TestParseAtAllIsAtComponent 验证 @全体成员生成 At(qq="all") 而非 AtAll
// （对齐 Python adapter.py:350-353）。
func TestParseAtAllIsAtComponent(t *testing.T) {
	a := &Adapter{}
	segs := []interface{}{
		map[string]interface{}{
			"type": "at",
			"data": map[string]interface{}{"qq": "all"},
		},
	}
	comps, _ := a.parseOneBotSegments(segs, 0, "")
	if len(comps) != 1 {
		t.Fatalf("components = %d, want 1", len(comps))
	}
	at, ok := comps[0].(*message.At)
	if !ok {
		t.Fatalf("expected *message.At, got %T", comps[0])
	}
	if at.TargetID != "all" {
		t.Errorf("TargetID = %q, want all", at.TargetID)
	}
}
