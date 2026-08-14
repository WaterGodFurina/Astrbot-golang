package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

type fakeAdapter struct {
	id string
}

func (a *fakeAdapter) ID() string                      { return a.id }
func (a *fakeAdapter) Type() string                    { return "fake" }
func (a *fakeAdapter) Start(ctx context.Context) error { return nil }
func (a *fakeAdapter) Stop() error                     { return nil }
func (a *fakeAdapter) Send(sessionID string, chain *message.MessageChain) error {
	return nil
}

func TestExecuteSendMessageRestrictsToCurrentSession(t *testing.T) {
	pm := platform.NewPlatformManager()
	pm.Register(&fakeAdapter{id: "qq"})
	s := NewProcessStage()
	s.platformMgr = pm

	event := &core.Event{
		Source: core.EventSource{Platform: "qq", ConvID: "group1"},
	}
	messages := []interface{}{
		map[string]interface{}{"type": "plain", "text": "hello"},
	}

	// No session: send to current session succeeds.
	if out := s.executeSendMessage(event, map[string]interface{}{"messages": messages}); !strings.Contains(out, "已发送") {
		t.Errorf("send to current session failed: %q", out)
	}

	// Same platform + session spelled out succeeds.
	if out := s.executeSendMessage(event, map[string]interface{}{
		"messages": messages,
		"session":  "qq:GroupMessage:group1",
	}); !strings.Contains(out, "已发送") {
		t.Errorf("send to explicit current session failed: %q", out)
	}

	// Different platform must be rejected.
	if out := s.executeSendMessage(event, map[string]interface{}{
		"messages": messages,
		"session":  "telegram:GroupMessage:group1",
	}); !strings.Contains(out, "cannot target platform") {
		t.Errorf("cross-platform send not blocked: %q", out)
	}

	// Same platform, different session must be rejected.
	if out := s.executeSendMessage(event, map[string]interface{}{
		"messages": messages,
		"session":  "qq:GroupMessage:othergroup",
	}); !strings.Contains(out, "cannot target session") {
		t.Errorf("cross-session send not blocked: %q", out)
	}
}
