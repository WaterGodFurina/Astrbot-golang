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

// TestExecuteSendMessageBlocksUnsafeMedia: send_message_to_user 的 url/path
// 必须过 SSRF 与路径范围校验（bug.md 4.3）。
func TestExecuteSendMessageBlocksUnsafeMedia(t *testing.T) {
	pm := platform.NewPlatformManager()
	pm.Register(&fakeAdapter{id: "qq"})
	s := NewProcessStage()
	s.platformMgr = pm

	event := &core.Event{
		Source: core.EventSource{Platform: "qq", ConvID: "group1"},
	}
	send := func(media map[string]interface{}) string {
		return s.executeSendMessage(event, map[string]interface{}{
			"messages": []interface{}{media},
		})
	}

	// SSRF: cloud metadata / loopback URLs must be rejected.
	if out := send(map[string]interface{}{"type": "image", "url": "http://169.254.169.254/latest/meta-data/"}); !strings.Contains(out, "不安全") {
		t.Errorf("metadata URL not blocked: %q", out)
	}
	if out := send(map[string]interface{}{"type": "image", "url": "http://127.0.0.1:8080/x"}); !strings.Contains(out, "不安全") {
		t.Errorf("loopback URL not blocked: %q", out)
	}
	// 非 http(s) 协议拒绝。
	if out := send(map[string]interface{}{"type": "image", "url": "file:///etc/passwd"}); !strings.Contains(out, "不安全") {
		t.Errorf("file:// URL not blocked: %q", out)
	}

	// 任意文件读取：/etc/passwd 不在允许目录内。
	if out := send(map[string]interface{}{"type": "image", "path": "/etc/passwd"}); !strings.Contains(out, "不安全") {
		t.Errorf("host path not blocked: %q", out)
	}
	if out := send(map[string]interface{}{"type": "file", "path": "/etc/shadow", "name": "x"}); !strings.Contains(out, "不安全") {
		t.Errorf("host file path not blocked: %q", out)
	}
}
