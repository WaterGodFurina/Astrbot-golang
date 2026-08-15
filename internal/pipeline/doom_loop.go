package pipeline

import (
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// doomLoopThreshold is the number of consecutive same-tool calls before we
// pause the tool and ask the session owner for confirmation.
const doomLoopThreshold = 5

// doomTrackerTTL is how long an idle session's doom tracker is kept before it
// is pruned. Paused sessions (waiting for owner confirmation) are never
// pruned, so the doom-confirm flow survives the TTL.
const doomTrackerTTL = 30 * time.Minute

// doomWhitelist lists tools that are exempt from doom-loop detection. 当前为
// 空：shell 工具也参与检测，但按命令内容做键（见 doomLoopKey），不会因为
// 顺序执行不同命令的常规工作流而误判。
var doomWhitelist = map[string]bool{}

// doomLoopKey derives the repetition key for a tool call. For shell execution
// tools the command text is part of the key: a loop re-issuing the SAME command
// is caught by the detector, while sequentially running different commands (a
// normal coding workflow) is not.
func doomLoopKey(toolName string, args map[string]interface{}) string {
	switch toolName {
	case "astrbot_execute_shell":
		return toolName + "\x00" + argString(args, "command")
	case "astrbot_shell_session":
		return toolName + "\x00" + argString(args, "action") + "\x00" + argString(args, "command")
	}
	return toolName
}

// doomTracker tracks per-session tool-call repetition.
type doomTracker struct {
	lastTool     string
	count        int
	pausedTool   string // tool paused by a doom loop, waiting for owner confirmation
	askSender    string // the sender who triggered the loop (only they may answer)
	resumePrompt string // original request text to resume after confirmation
	lastSeen     time.Time
}

// doomConfirmResult classifies a doom-confirmation message.
type doomConfirmResult int

const (
	doomNotConsumed doomConfirmResult = iota // no paused tool for this session
	doomResumed                              // asker confirmed -> re-run the original request
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
	tr.lastSeen = time.Now()
	if event.Source.SenderID != tr.askSender {
		s.doomMu.Unlock()
		return doomNotConsumed // not the asker; treat as a normal message
	}
	paused := tr.pausedTool
	resume := tr.resumePrompt
	text := strings.ToLower(strings.TrimSpace(event.MessageStr))
	// Whole-message confirmation: only an exact confirm reply resumes the
	// paused tool, so ordinary messages (e.g. "是的"、"继续读") cannot replay it.
	confirmed := text == "继续" || text == "继续执行" || text == "continue" ||
		text == "是" || text == "yes"
	if confirmed {
		tr.pausedTool = ""
		tr.lastTool = ""
		tr.count = 0
		tr.askSender = ""
		tr.resumePrompt = ""
		s.doomMu.Unlock()
		// Replace the confirmation message with the original request so the
		// pipeline re-runs it (tool paused state cleared).
		if resume != "" {
			event.PlainText = resume
			event.MessageStr = resume
		}
		// Mark the event as woken so the resume reaches the LLM stage even in
		// group chats where a bare-text reply is not otherwise a wake command.
		event.IsAtOrWakeCommand = true
		event.SetExtra("llm_wake", true)
		s.replyText(event, "已解除工具 "+paused+" 的暂停，正在继续执行。")
		return doomResumed
	}
	// Declined or any other reply from the asker: clear the paused state so
	// the session is no longer blocked, and let the message flow through the
	// normal pipeline.
	tr.pausedTool = ""
	tr.lastTool = ""
	tr.count = 0
	tr.askSender = ""
	tr.resumePrompt = ""
	s.doomMu.Unlock()
	s.replyText(event, "已停止工具 "+paused+" 的执行。如需继续，请回复“继续”。")
	return doomNotConsumed
}

// resetDoomLoopCount clears the same-tool repetition counters for a session at
// the start of a new agent request, so counts never leak across request
// boundaries. The paused state (and its asker/resume prompt) is preserved.
func (s *ProcessStage) resetDoomLoopCount(umo string) {
	s.doomMu.Lock()
	defer s.doomMu.Unlock()
	if tr := s.doomTrackers[umo]; tr != nil {
		tr.lastTool = ""
		tr.count = 0
		tr.lastSeen = time.Now()
	}
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
	// Lazy TTL sweep: drop trackers of sessions idle beyond the TTL before
	// touching this session's entry.
	s.pruneDoomTrackers()
	tr := s.doomTrackers[umo]
	if tr == nil {
		tr = &doomTracker{}
		s.doomTrackers[umo] = tr
	}
	tr.lastSeen = time.Now()
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

// pruneDoomTrackers removes trackers for sessions that have been idle longer
// than doomTrackerTTL. Sessions with an active pause (pending owner
// confirmation) are kept so the doom-confirm flow cannot be broken by the
// sweep. Caller must hold doomMu.
func (s *ProcessStage) pruneDoomTrackers() {
	cutoff := time.Now().Add(-doomTrackerTTL)
	for umo, tr := range s.doomTrackers {
		if tr.pausedTool == "" && tr.lastSeen.Before(cutoff) {
			delete(s.doomTrackers, umo)
		}
	}
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
