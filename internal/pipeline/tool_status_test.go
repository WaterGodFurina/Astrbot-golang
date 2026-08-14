package pipeline

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// fakeToolStatusAdapter records message chains sent via Send.
type fakeToolStatusAdapter struct {
	platform.PlatformAdapter
	sent []string
}

func (f *fakeToolStatusAdapter) ID() string   { return "toolstatus" }
func (f *fakeToolStatusAdapter) Type() string { return "qq_official" }

func (f *fakeToolStatusAdapter) Send(sessionID string, chain *message.MessageChain) error {
	text := ""
	for _, c := range chain.Chain {
		if p, ok := c.(*message.Plain); ok {
			text += p.Text
		}
	}
	f.sent = append(f.sent, text)
	return nil
}

// TestSendToolStatus: 工具状态消息经 platformMgr.Send 送达会话。
func TestSendToolStatus(t *testing.T) {
	pm := platform.NewPlatformManager()
	adapter := &fakeToolStatusAdapter{}
	pm.Register(adapter)

	s := &ProcessStage{platformMgr: pm}
	ev := &core.Event{Source: core.EventSource{Platform: "toolstatus", ConvID: "conv1"}}
	s.sendToolStatus(ev, toolStatusCall("get_weather"))
	if len(adapter.sent) != 1 || adapter.sent[0] != "🔨 调用工具: get_weather" {
		t.Fatalf("want status sent, got %v", adapter.sent)
	}

	// 组合 status + result（show_tool_use_status + show_tool_call_result）。
	s.sendToolStatus(ev, toolStatusCall("get_weather")+"\n"+toolStatusResult("sunny 26°C"))
	if len(adapter.sent) != 2 || adapter.sent[1] != "🔨 调用工具: get_weather\n📎 返回结果: sunny 26°C" {
		t.Fatalf("want combined status+result, got %v", adapter.sent)
	}
}
