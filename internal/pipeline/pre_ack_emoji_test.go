package pipeline

import (
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
)

// fakeReactor records reaction calls.
type fakeReactor struct {
	platform.PlatformAdapter
	reactions []string
}

func (f *fakeReactor) ID() string   { return "fake" }
func (f *fakeReactor) Type() string { return "lark" }

func (f *fakeReactor) React(sessionID, messageID, emoji string) error {
	f.reactions = append(f.reactions, emoji)
	return nil
}

// TestPreAckEmojiTriggered: enabled pre_ack_emoji on a supported platform
// sends a reaction for woken messages.
func TestPreAckEmojiTriggered(t *testing.T) {
	pm := platform.NewPlatformManager()
	reactor := &fakeReactor{}
	pm.Register(reactor)

	s := NewPreProcessStage()
	s.config = map[string]interface{}{
		"platform_specific": map[string]interface{}{
			"lark": map[string]interface{}{
				"pre_ack_emoji": map[string]interface{}{
					"enable": true,
					"emojis": []interface{}{"Typing"},
				},
			},
		},
	}
	s.platformMgr = pm

	ev := &core.Event{
		Source:            core.EventSource{Platform: "lark"},
		IsAtOrWakeCommand: true,
		MessageObj:        &core.MessageObj{MessageID: "m1"},
	}
	s.applyPreAckEmoji(ev)
	time.Sleep(100 * time.Millisecond)
	if len(reactor.reactions) != 1 || reactor.reactions[0] != "Typing" {
		t.Errorf("expected 1 reaction Typing, got %v", reactor.reactions)
	}
}

// TestPreAckEmojiDisabled: disabled config sends nothing.
func TestPreAckEmojiDisabled(t *testing.T) {
	pm := platform.NewPlatformManager()
	reactor := &fakeReactor{}
	pm.Register(reactor)

	s := NewPreProcessStage()
	s.config = map[string]interface{}{
		"platform_specific": map[string]interface{}{
			"lark": map[string]interface{}{
				"pre_ack_emoji": map[string]interface{}{
					"enable": false,
					"emojis": []interface{}{"Typing"},
				},
			},
		},
	}
	s.platformMgr = pm
	ev := &core.Event{
		Source:            core.EventSource{Platform: "lark"},
		IsAtOrWakeCommand: true,
		MessageObj:        &core.MessageObj{MessageID: "m1"},
	}
	s.applyPreAckEmoji(ev)
	if len(reactor.reactions) != 0 {
		t.Errorf("disabled pre_ack_emoji must not react, got %v", reactor.reactions)
	}
}

// TestPreAckEmojiUnsupportedPlatform: telegram-only emoji config on an
// unsupported platform sends nothing.
func TestPreAckEmojiUnsupportedPlatform(t *testing.T) {
	pm := platform.NewPlatformManager()
	reactor := &fakeReactor{}
	pm.Register(reactor)

	s := NewPreProcessStage()
	s.config = map[string]interface{}{
		"platform_specific": map[string]interface{}{
			"aiocqhttp": map[string]interface{}{
				"pre_ack_emoji": map[string]interface{}{
					"enable": true,
					"emojis": []interface{}{"👋"},
				},
			},
		},
	}
	s.platformMgr = pm
	ev := &core.Event{
		Source:            core.EventSource{Platform: "aiocqhttp"},
		IsAtOrWakeCommand: true,
		MessageObj:        &core.MessageObj{MessageID: "m1"},
	}
	s.applyPreAckEmoji(ev)
	if len(reactor.reactions) != 0 {
		t.Errorf("aiocqhttp is not a supported reaction platform, got %v", reactor.reactions)
	}
}
