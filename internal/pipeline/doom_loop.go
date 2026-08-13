package pipeline

import (
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// doomLoopThreshold is the number of consecutive same-tool calls before we
// pause the tool and ask the session owner for confirmation.
const doomLoopThreshold = 5

// doomWhitelist lists tools that are exempt from doom-loop detection: they are
// inherently iterative (each invocation is a distinct user-visible action) and
// frequent repetition is legitimate.
var doomWhitelist = map[string]bool{
	"astrbot_execute_shell": true,
	"astrbot_shell_session": true,
}

// doomTracker tracks per-session tool-call repetition.
type doomTracker struct {
	lastTool     string
	count        int
	pausedTool   string // tool paused by a doom loop, waiting for owner confirmation
	askSender    string // the sender who triggered the loop (only they may answer)
	resumePrompt string // original request text to resume after confirmation
}

// doomConfirmResult classifies a doom-confirmation message.
type doomConfirmResult int

const (
	doomNotConsumed doomConfirmResult = iota // no paused tool for this session
	doomResumed                             // asker confirmed -> re-run the original request
	doomDeclined                            // asker declined or another sender -> stop
)

// maybeHandleDoomConfirm is called at the start of Process: if this session has
// a paused tool and the current message comes from the SAME sender who
// triggered it (checked by UMO + sender id), a confirmation reply resumes the
// original request. No other user can confirm.
func (s *ProcessStage) maybeHandleDoomConfirm(event *core.Event) doomConfirmResult {
	s.doomMu.Lock()
	tr, ok := s.doomTrackers[event.UnifiedMsgOrigin()]
	if !ok || tr.pausedTool == "" {
		s.doomMu.Unlock()
		return doomNotConsumed
	}
	if event.Source.SenderID != tr.askSender {
		s.doomMu.Unlock()
		return doomNotConsumed // not the asker; treat as a normal message
	}
	paused := tr.pausedTool
	resume := tr.resumePrompt
	text := strings.ToLower(strings.TrimSpace(event.MessageStr))
	confirm := strings.Contains(text, "继续") || strings.Contains(text, "继续执行") ||
		strings.Contains(text, "continue") || strings.Contains(text, "是") ||
		strings.Contains(text, "yes")
	if confirm {
		tr.pausedTool = ""
		tr.lastTool = ""
		tr.count = 0
		s.doomMu.Unlock()
		// Replace the confirmation message with the original request so the
		// pipeline re-runs it (tool paused state cleared).
		if resume != "" {
			event.PlainText = resume
			event.MessageStr = resume
		}
		s.replyText(event, "已解除工具 "+paused+" 的暂停，正在继续执行。")
		return doomResumed
	}
	s.doomMu.Unlock()
	s.replyText(event, "已停止工具 "+paused+" 的执行。如需继续，请回复“继续”。")
	return doomDeclined
}

// checkDoomLoop tracks consecutive same-tool calls. Returns false when the tool
// is paused (a doom loop was detected and the owner has been asked), in which
// case the caller should stop executing tools.
func (s *ProcessStage) checkDoomLoop(event *core.Event, toolName string) bool {
	// Whitelisted tools are exempt from repetition detection.
	if doomWhitelist[toolName] {
		return true
	}
	umo := event.UnifiedMsgOrigin()
	s.doomMu.Lock()
	defer s.doomMu.Unlock()
	tr := s.doomTrackers[umo]
	if tr == nil {
		tr = &doomTracker{}
		s.doomTrackers[umo] = tr
	}
	// If this exact tool is already paused, refuse to run it.
	if tr.pausedTool == toolName {
		return false
	}
	if tr.lastTool == toolName {
		tr.count++
	} else {
		tr.lastTool = toolName
		tr.count = 1
	}
	if tr.count >= doomLoopThreshold {
		tr.pausedTool = toolName
		tr.askSender = event.Source.SenderID
		tr.resumePrompt = event.PlainText
		// Ask the session owner (async; only they may answer).
		s.askDoomConfirm(event, toolName)
		return false
	}
	return true
}

// askDoomConfirm sends a confirmation request to the session owner.
func (s *ProcessStage) askDoomConfirm(event *core.Event, toolName string) {
	text := "检测到工具 " + toolName + " 已连续调用 " + itoa(doomLoopThreshold) +
		" 次，可能是重复/死循环。已暂停该工具的执行。\n" +
		"如需继续执行，请由本次请求的发起者回复“继续”；回复其他内容将保持停止。"
	s.replyText(event, text)
}

// replyText sends a plain-text reply to the event's conversation.
func (s *ProcessStage) replyText(event *core.Event, text string) {
	if s.platformMgr == nil {
		return
	}
	chain := message.NewMessageChain(&message.Plain{Text: text})
	_ = s.platformMgr.Send(event.Source.Platform, event.Source.ConvID, chain)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
