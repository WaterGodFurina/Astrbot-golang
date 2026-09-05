// SSE chat streaming for the WebUI /chat page.
//
// The WebUI chat page sends a message via POST /api/v1/chat and expects a
// text/event-stream response (events: session_id -> user_message_saved ->
// run_started -> plain (streaming text) -> complete -> end). This mirrors
// astrbot/dashboard/services/chat_service.py build_chat_stream.
//
// To reuse the existing message pipeline we run the user message through the
// "default" pipeline scheduler as a webchat-platform event and register a
// lightweight platform adapter ("dashboard_chat") that captures the reply
// chain sent by the pipeline's RespondStage / streamSender and forwards it to
// a per-session SSE subscriber.
package dashboard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// chatStreamAdapter captures reply chains from the pipeline for a chat session.
// It implements platform.PlatformAdapter so platformMgr.Send("dashboard_chat",
// sessionID, chain) lands here and fans out to SSE subscribers.
//
// 同一 session 的多次并发发送可能同时存在多个订阅者。pipeline 的 Send 契约
// 只携带 sessionID、无法区分 reply 属于哪一次 run，因此按会话内注册顺序
// （seq）路由：reply 一定产生在对应 run 的 dispatch 过程中，而同一会话的
// run 在事件总线上串行分发，故取"仍存活且 seq 最小"的订阅者即当前正在
// dispatch 的 run。订阅者在收到 done 后立即退订，防止下一个 run 的回复
// 落进自己的 channel。
type chatStreamAdapter struct {
	mu          sync.Mutex
	seq         uint64
	subscribers map[string]map[uint64]chan *message.MessageChain // sessionID -> seq -> ch

	// persistMu 保护下方惰性绑定的落库依赖（bindPersistence 由每次 chat
	// run 注入，Send 在无订阅者兜底时读取）。
	persistMu sync.Mutex
	// chat/threads/registerAttachment 用于"无订阅者兜底落库"（对齐本体
	// webchat_adapter.send_by_session：无活跃流时直接持久化主动消息）。
	// chatStore/threadStore 不导出且 server.go 不可改动，因此由 chat run
	// 入口（runChatSSEStream / handleWSMessageSend）幂等绑定。
	chat               *chatStore
	threads            *threadStore
	registerAttachment func(srcPath, attachType, displayName string) (string, string, error)
}

// bindPersistence 幂等绑定落库依赖（重复绑定同值无害）。
func (a *chatStreamAdapter) bindPersistence(chat *chatStore, threads *threadStore, registerAttachment func(srcPath, attachType, displayName string) (string, string, error)) {
	a.persistMu.Lock()
	defer a.persistMu.Unlock()
	a.chat = chat
	a.threads = threads
	a.registerAttachment = registerAttachment
}

func newChatStreamAdapter() *chatStreamAdapter {
	return &chatStreamAdapter{subscribers: make(map[string]map[uint64]chan *message.MessageChain)}
}

func (a *chatStreamAdapter) ID() string   { return "dashboard_chat" }
func (a *chatStreamAdapter) Type() string { return "dashboard_chat" }

// Start/Stop are no-ops: the adapter is purely a reply sink.
func (a *chatStreamAdapter) Start(ctx context.Context) error { return nil }
func (a *chatStreamAdapter) Stop() error                     { return nil }

// Send forwards a reply chain to the earliest-registered still-active
// subscriber of the session. The channel set is copied under the lock so a
// concurrent unsubscribe (delete) can never race the map iteration.
func (a *chatStreamAdapter) Send(sessionID string, chain *message.MessageChain) error {
	a.mu.Lock()
	var bestSeq uint64
	var target chan *message.MessageChain
	for seq, ch := range a.subscribers[sessionID] {
		if target == nil || seq < bestSeq {
			bestSeq = seq
			target = ch
		}
	}
	a.mu.Unlock()
	if target == nil {
		// 无活跃订阅者：对齐本体 webchat_adapter.send_by_session 兜底——
		// 主动消息不再直接丢弃，而是落库到会话历史，保证重新打开会话可见。
		a.persistProactiveMessage(sessionID, chain)
		return nil
	}
	select {
	case target <- chain:
	default:
		// Subscriber not draining; drop rather than block the pipeline.
	}
	return nil
}

// persistProactiveMessage 把无订阅者收到的回复链按全组件持久化进会话历史
// （对齐本体 webchat_adapter._save_proactive_message：媒体组件登记为附件
// part，bot 身份落库）。会话不存在时 appendMessage 返回 false，静默放弃。
func (a *chatStreamAdapter) persistProactiveMessage(sessionID string, chain *message.MessageChain) {
	a.persistMu.Lock()
	chat, register := a.chat, a.registerAttachment
	a.persistMu.Unlock()
	if chat == nil || chain == nil || len(chain.Chain) == 0 {
		return
	}
	parts := chainToStorageParts(register, chain)
	if len(parts) == 0 {
		return
	}
	chat.appendMessage(sessionID, map[string]interface{}{
		"id":          fmt.Sprintf("b_%d", time.Now().UnixNano()),
		"session_id":  sessionID,
		"sender_id":   "bot",
		"sender_name": "bot",
		"role":        "assistant",
		"type":        "bot",
		"content":     map[string]interface{}{"type": "bot", "message": parts},
		"created_at":  time.Now().Format(time.RFC3339Nano),
	})
}

func (a *chatStreamAdapter) subscribe(sessionID string) (chan *message.MessageChain, uint64) {
	ch := make(chan *message.MessageChain, 64)
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subscribers[sessionID] == nil {
		a.subscribers[sessionID] = make(map[uint64]chan *message.MessageChain)
	}
	a.seq++
	a.subscribers[sessionID][a.seq] = ch
	return ch, a.seq
}

func (a *chatStreamAdapter) unsubscribe(sessionID string, seq uint64, ch chan *message.MessageChain) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if subs, ok := a.subscribers[sessionID]; ok {
		if subs[seq] == ch {
			delete(subs, seq)
		}
		if len(subs) == 0 {
			delete(a.subscribers, sessionID)
		}
	}
}

// chatStreamRequest describes one SSE chat run (POST /api/v1/chat 与
// regenerate/thread 消息共用同一管线)。
type chatStreamRequest struct {
	SessionID        string
	Parts            []map[string]interface{}
	Files            []interface{}
	SelectedProvider string
	SelectedModel    string
	Flags            map[string]interface{}
	// PersistUser 为 false 时（regenerate）不再落盘 user 消息（历史中已存在）。
	PersistUser bool
	// ThreadID 非空时消息写入线程历史而非会话历史（对齐 Python
	// platform_history_id="webchat_thread"）。
	ThreadID string
	// Extras 携带 action_type / llm_checkpoint_id / thread_selected_text，
	// 对齐本体 webchat_adapter.create_event 注入 event.extra 的字段
	// （pipeline 通过 event.GetExtra 消费）。
	Extras map[string]interface{}
	// LegacyFlags 是请求体遗留顶层 flag 字段（flags.* 未传时的回退源），
	// 与 Flags 一起交给 resolveWebchatRequestFlags。
	LegacyFlags map[string]interface{}
	// LLMCheckpointID 为本次 run 的 checkpoint id（落库/前端 message_saved 共用）。
	LLMCheckpointID string
}

// resolveWebchatRequestFlags 对齐本体 request_flags.resolve_webchat_request_flags：
// flags[key]（bool）优先，其次 payload 顶层同名字段（bool），否则默认 true。
// 三个 flag 未传时默认开启（Go decode 后缺字段是零值，需显式 resolve）。
func resolveWebchatRequestFlags(flags map[string]interface{}, payload map[string]interface{}) map[string]interface{} {
	resolved := make(map[string]interface{}, 3)
	for _, key := range webchatRequestFlagKeys {
		value := boolFlagValue(flags[key])
		if value == nil {
			value = boolFlagValue(payload[key])
		}
		if value == nil {
			value = true
		}
		resolved[key] = value
	}
	return resolved
}

// webchatRequestFlagKeys 对齐本体 WEBCHAT_REQUEST_FLAG_DEFAULTS（三者默认 true）。
var webchatRequestFlagKeys = []string{
	"enable_inline_genui",
	"enable_default_system_prompt",
	"enable_streaming",
}

// boolFlagValue 仅接受显式 bool，其余（含缺省/类型不符）返回 nil 触发回退。
func boolFlagValue(v interface{}) interface{} {
	if b, ok := v.(bool); ok {
		return b
	}
	return nil
}

// legacyFlagPayload 把请求体中的遗留顶层 flag 字段收集成 map（对齐本体
// payload 顶层 enable_* 回退字段），nil 字段不放入（表示未传）。
func legacyFlagPayload(inlineGenui, defaultPrompt, streaming *bool) map[string]interface{} {
	payload := make(map[string]interface{}, 3)
	if inlineGenui != nil {
		payload["enable_inline_genui"] = *inlineGenui
	}
	if defaultPrompt != nil {
		payload["enable_default_system_prompt"] = *defaultPrompt
	}
	if streaming != nil {
		payload["enable_streaming"] = *streaming
	}
	return payload
}

// handleChatSend streams a chat reply over SSE.
// POST /api/v1/chat  body: {session_id, message:[parts], selected_provider, selected_model, flags}
func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID        string                   `json:"session_id"`
		ConversationID   string                   `json:"conversation_id"`
		Message          []map[string]interface{} `json:"message"`
		Files            []interface{}            `json:"files"`
		SelectedProvider string                   `json:"selected_provider"`
		SelectedModel    string                   `json:"selected_model"`
		Flags            map[string]interface{}   `json:"flags"`
		SkipUserHistory  bool                     `json:"_skip_user_history"`
		// 遗留顶层 flag 字段：flags.* 未传时回退（本体 payload 顶层字段语义）。
		EnableInlineGenui         *bool `json:"enable_inline_genui"`
		EnableDefaultSystemPrompt *bool `json:"enable_default_system_prompt"`
		EnableStreaming           *bool `json:"enable_streaming"`
		// 对齐本体 webchat_adapter.create_event 注入 extra 的三个字段。
		ActionType         string `json:"action_type"`
		LLMCheckpointID    string `json:"_llm_checkpoint_id"`
		ThreadSelectedText string `json:"_thread_selected_text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("Invalid JSON body"))
		return
	}

	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = body.ConversationID
	}
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, apiError("Missing session_id"))
		return
	}
	parts := body.Message
	if len(parts) == 0 {
		parts = filePartsToMessage(body.Files)
	}
	// 对齐 Python 原版 webchat_message_parts_have_content：纯媒体消息
	// （仅图片/文件 part，无 plain 文本）也允许发送。
	if !partsHaveContent(parts, body.Files) {
		writeJSON(w, http.StatusBadRequest, apiError("Message content is empty"))
		return
	}
	// llm_checkpoint_id：请求显式携带时沿用（regenerate 续接 checkpoint），
	// 否则新生成（对齐本体 chat_service.build_chat_stream）。
	llmCheckpointID := body.LLMCheckpointID
	if llmCheckpointID == "" {
		llmCheckpointID = fmt.Sprintf("c_%d", time.Now().UnixNano())
	}
	s.runChatSSEStream(w, r, chatStreamRequest{
		SessionID:        sessionID,
		Parts:            parts,
		Files:            body.Files,
		SelectedProvider: body.SelectedProvider,
		SelectedModel:    body.SelectedModel,
		Flags:            body.Flags,
		// 编辑后"继续对话"（continueEditedMessage）会带 _skip_user_history：
		// user 消息已存在于历史中，不再重复落盘（对齐 Python _skip_user_history）。
		PersistUser: !body.SkipUserHistory,
		Extras: map[string]interface{}{
			"action_type":          body.ActionType,
			"llm_checkpoint_id":    llmCheckpointID,
			"thread_selected_text": body.ThreadSelectedText,
		},
		// 遗留顶层 flag 字段与显式 flags 一并交给 resolveWebchatRequestFlags。
		LegacyFlags:     legacyFlagPayload(body.EnableInlineGenui, body.EnableDefaultSystemPrompt, body.EnableStreaming),
		LLMCheckpointID: llmCheckpointID,
	})
}

// partsHaveContent 校验消息有真实内容（plain 文本或媒体 part），对齐
// webchat_message_parts_have_content；files 兜底用于纯附件消息。
func partsHaveContent(parts []map[string]interface{}, files []interface{}) bool {
	if len(parts) == 0 && len(files) > 0 {
		return true
	}
	text := plainTextFromParts(parts)
	if text == "" && !partsHaveMediaContent(parts) {
		return false
	}
	return true
}

// runChatSSEStream runs a chat reply through the pipeline and streams it over
// SSE (events: session_id -> user_message_saved -> run_started -> plain ->
// complete -> end), mirroring Python build_chat_stream. The run is registered
// in the session run registry so POST /chat/sessions/{id}/stop can cancel it.
func (s *Server) runChatSSEStream(w http.ResponseWriter, r *http.Request, req chatStreamRequest) {
	sessionID := req.SessionID

	// 惰性绑定落库依赖：无订阅者兜底（persistProactiveMessage）需要
	// chat store 与附件登记函数（server.go 构造处不可改动，在此幂等注入）。
	s.chatAdapter.bindPersistence(s.chat, s.threads, s.registerReplyAttachment)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, apiError("streaming not supported"))
		return
	}

	// Persist the user message (skipped for regenerate: the message already
	// exists in history).
	savedUserID := fmt.Sprintf("u_%d", time.Now().UnixNano())
	llmCheckpointID := req.LLMCheckpointID
	if llmCheckpointID == "" {
		llmCheckpointID = fmt.Sprintf("c_%d", time.Now().UnixNano())
	}
	if req.PersistUser {
		userRecord := map[string]interface{}{
			"id":          savedUserID,
			"session_id":  sessionID,
			"sender_id":   "dashboard",
			"sender_name": "dashboard",
			"role":        "user",
			"type":        "user",
			"content":     map[string]interface{}{"type": "user", "message": req.Parts},
			"created_at":  time.Now().Format(time.RFC3339Nano),
		}
		if req.ThreadID != "" {
			s.threads.appendThreadMessage(req.ThreadID, userRecord)
		} else {
			s.chat.appendMessage(sessionID, userRecord)
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// session_id event first (matches Python build_chat_stream).
	sendSSE(w, flusher, map[string]interface{}{
		"type":       "session_id",
		"data":       nil,
		"session_id": sessionID,
	})
	if req.PersistUser {
		sendSSE(w, flusher, map[string]interface{}{
			"type": "user_message_saved",
			"data": map[string]interface{}{
				"id":                savedUserID,
				"created_at":        time.Now().Format(time.RFC3339Nano),
				"llm_checkpoint_id": llmCheckpointID,
			},
		})
	}
	runID := fmt.Sprintf("r_%d", time.Now().UnixNano())
	sendSSE(w, flusher, map[string]interface{}{
		"type": "run_started",
		"data": map[string]interface{}{"run_id": runID},
	})

	// 注册 run 取消（stop 端点按 session 取消），并绑定到本请求上下文。
	runCtx, runCancel := context.WithCancel(r.Context())
	s.registerChatRun(sessionID, runID, runCancel)
	defer s.unregisterChatRun(sessionID, runID)

	// 订阅管线回复；同时用 sink 累积全组件 parts，run 结束时把完整
	// 组件列表（媒体+文本）持久化进会话历史（对齐本体全组件落库）。
	ch, subSeq := s.chatAdapter.subscribe(sessionID)
	defer s.chatAdapter.unsubscribe(sessionID, subSeq, ch)
	botSink := newChainPartsSink()

	// Run the user message through the pipeline; done is closed when the
	// event finishes processing.
	done := s.processChatEvent(runCtx, sessionID, runID, plainTextFromParts(req.Parts), req)
	if done == nil {
		sendSSE(w, flusher, map[string]interface{}{"type": "error", "data": "对话管道不可用"})
		sendSSE(w, flusher, map[string]interface{}{"type": "end", "data": nil})
		return
	}

	// Stream reply chains until the pipeline finishes or the client disconnects.
	var full strings.Builder
	deadline := time.After(300 * time.Second)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			sendSSE(w, flusher, map[string]interface{}{"type": "end", "data": nil})
			return
		case <-done:
			// 本 run 的回复在 done 触发前已全部送入 ch；先退订再排空，
			// 避免同一 session 下一个 run 的回复落到本 channel。
			s.chatAdapter.unsubscribe(sessionID, subSeq, ch)
			for {
				select {
				case chain := <-ch:
					s.emitChainSSE(w, flusher, &full, chain, botSink)
				default:
					goto done
				}
			}
		done:
			// Persist the bot reply into the chat session/thread store: 全组件
			// （plain/image/record/file/video）一并落库，对齐本体
			// message_parts_helper 全组件持久化语义（此前仅存纯文本）。
			if parts := botSink.storageParts(); len(parts) > 0 {
				botID := fmt.Sprintf("b_%d", time.Now().UnixNano())
				botRecord := map[string]interface{}{
					"id":          botID,
					"session_id":  sessionID,
					"sender_id":   "bot",
					"sender_name": "bot",
					"role":        "assistant",
					"type":        "bot",
					"content": map[string]interface{}{
						"type":    "bot",
						"message": parts,
					},
					"created_at": time.Now().Format(time.RFC3339Nano),
				}
				if req.ThreadID != "" {
					s.threads.appendThreadMessage(req.ThreadID, botRecord)
				} else {
					s.chat.appendMessage(sessionID, botRecord)
				}
				sendSSE(w, flusher, map[string]interface{}{
					"type": "message_saved",
					"data": map[string]interface{}{"id": botID, "created_at": time.Now().Format(time.RFC3339Nano)},
				})
			}
			// complete 帧只做完成信号（文本兜底由前端处理），帧格式不变。
			sendSSE(w, flusher, map[string]interface{}{"type": "complete", "data": full.String()})
			sendSSE(w, flusher, map[string]interface{}{"type": "end", "data": nil})
			return
		case chain := <-ch:
			s.emitChainSSE(w, flusher, &full, chain, botSink)
		}
	}
}

// emitChainSSE sends SSE frames for a reply chain. 对齐本体 webchat_event._send
// 分帧语义：逐组件独立发帧——plain 文本帧；Json 组件序列化为 plain 帧；
// image/record/video/file 媒体帧（文件登记进 webui_files 后发
// "[TYPE]存储名[|显示名]"，前端按 stored filename 走文件服务拉取）。
// image 的 base64/URL 保持旧帧格式（data URL / URL 直传）——帧格式不变，
// 只补此前完全丢失的 record/file/video/json 帧。所有组件同步累积进 sink，
// 供 run 结束时全组件持久化。
func (s *Server) emitChainSSE(w http.ResponseWriter, flusher http.Flusher, full *strings.Builder, chain *message.MessageChain, sink *chainPartsSink) {
	if chain == nil {
		return
	}
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			if c.Text == "" {
				continue
			}
			full.WriteString(c.Text)
			sink.absorbPlain(c.Text)
			sendSSE(w, flusher, map[string]interface{}{"type": "plain", "data": c.Text, "chain_type": "text"})
		case *message.Json:
			// 对齐本体：Json 组件序列化为 plain 帧输出。
			if b, err := json.Marshal(c.Data); err == nil && len(b) > 0 {
				full.WriteString(string(b))
				sink.absorbPlain(string(b))
				sendSSE(w, flusher, map[string]interface{}{"type": "plain", "data": string(b), "chain_type": "text"})
			}
		case *message.Image:
			if data := chainImageDataURL(c); data != "" {
				sendSSE(w, flusher, map[string]interface{}{"type": "image", "data": data})
				s.persistImageForSink(sink, c)
				continue
			}
			s.emitMediaSSE(w, flusher, sink, c.Path, c.File, "image", "")
		case *message.Record:
			s.emitMediaSSE(w, flusher, sink, c.Path, c.File, "record", "")
		case *message.Video:
			s.emitMediaSSE(w, flusher, sink, c.Path, c.URL, "video", "")
		case *message.File:
			s.emitMediaSSE(w, flusher, sink, c.Path, c.URL, "file", c.Name)
		}
	}
}

// emitMediaSSE 登记媒体组件为附件、发出对齐本体 webchat_event._send 的媒体帧
// （data="[TYPE]存储名[|显示名]"）并累积进 sink。登记失败时：URL 源回退
// 直传帧（组件不丢），本地源静默跳过。
func (s *Server) emitMediaSSE(w http.ResponseWriter, flusher http.Flusher, sink *chainPartsSink, primary, secondary, attachType, displayName string) {
	frame := s.mediaReplyFrame(sink, primary, secondary, attachType, displayName)
	if frame == nil {
		return
	}
	sendSSE(w, flusher, frame)
}

// persistImageForSink 把 base64/URL 形态的图片也登记成附件 part（仅用于
// 持久化回放；帧本身保持旧格式直传）。登记失败静默忽略。
func (s *Server) persistImageForSink(sink *chainPartsSink, img *message.Image) {
	if sink == nil || img == nil {
		return
	}
	var id, stored string
	var err error
	if img.Base64 != "" {
		id, stored, err = registerBase64Image(s.registerReplyAttachment, img.Base64)
	} else {
		id, stored, err = s.registerReplyAttachment(img.URL, "image", "")
	}
	if err != nil || id == "" {
		return
	}
	sink.absorbAttachment("image", id, stored, "")
}

// chainImageDataURL returns the inline display source for an image: base64
// payloads become data URLs (no file service needed), https URLs pass through.
// Local files return "" so callers fall back to the attachment channel.
func chainImageDataURL(img *message.Image) string {
	if img == nil {
		return ""
	}
	switch {
	case img.Base64 != "":
		return "data:image/png;base64," + img.Base64
	case strings.HasPrefix(img.URL, "http://"), strings.HasPrefix(img.URL, "https://"):
		return img.URL
	}
	return ""
}

// mediaReplyFrame 把媒体组件登记进 webui_files（复用现有附件通道）并生成
// 对齐本体的媒体帧。登记成功时同步把附件 part 累积进 sink（bot 持久化）。
func (s *Server) mediaReplyFrame(sink *chainPartsSink, primary, secondary, attachType, displayName string) map[string]interface{} {
	src := firstMediaPath(primary, secondary)
	if src == "" {
		return nil
	}
	prefix := "[IMAGE]"
	switch attachType {
	case "record":
		prefix = "[RECORD]"
	case "video":
		prefix = "[VIDEO]"
	case "file":
		prefix = "[FILE]"
	}
	id, stored, display := registerMediaIntoSink(s.registerReplyAttachment, sink, primary, secondary, attachType, displayName)
	if id == "" {
		if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
			return nil
		}
		// 登记失败回退：URL 直传帧，前端尽力解析；不入持久化 parts。
		return map[string]interface{}{"type": attachType, "data": prefix + src}
	}
	data := prefix + stored
	if attachType == "file" && display != "" && stored != display {
		data = prefix + stored + "|" + display
	}
	return map[string]interface{}{"type": attachType, "data": data}
}

// firstMediaPath 返回第一个可用的媒体源（本地路径或 URL），跳过空值。
func firstMediaPath(paths ...string) string {
	for _, p := range paths {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return ""
}

// registerMediaIntoSink 把媒体组件（本地文件或远端 URL）登记进 webui_files
// 并把附件 part 累积进 sink（bot 持久化），返回 attachment_id / 存储名 /
// 显示名；登记失败返回空 id。发帧与纯落库两条路径共用。
func registerMediaIntoSink(register func(srcPath, attachType, displayName string) (string, string, error), sink *chainPartsSink, primary, secondary, attachType, displayName string) (string, string, string) {
	src := firstMediaPath(primary, secondary)
	if src == "" || register == nil {
		return "", "", ""
	}
	id, stored, err := register(src, attachType, displayName)
	if err != nil || id == "" {
		return "", "", ""
	}
	display := strings.TrimSpace(displayName)
	if display == "" {
		display = stored
	}
	sink.absorbAttachment(attachType, id, display, stored)
	return id, stored, display
}

// chainToStorageParts 把整条回复链转换为全组件存储 parts（对齐本体
// message_chain_to_storage_message_parts：plain/json → plain part，
// 媒体组件登记为附件 part）。供主动消息兜底落库使用。
func chainToStorageParts(register func(srcPath, attachType, displayName string) (string, string, error), chain *message.MessageChain) []map[string]interface{} {
	sink := newChainPartsSink()
	if chain != nil {
		for _, comp := range chain.Chain {
			switch c := comp.(type) {
			case *message.Plain:
				sink.absorbPlain(c.Text)
			case *message.Json:
				if b, err := json.Marshal(c.Data); err == nil && len(b) > 0 {
					sink.absorbPlain(string(b))
				}
			case *message.Image:
				if chainImageDataURL(c) == "" {
					registerMediaIntoSink(register, sink, c.Path, c.File, "image", "")
				} else if c.Base64 != "" {
					if id, stored, err := registerBase64Image(register, c.Base64); err == nil {
						sink.absorbAttachment("image", id, stored, "")
					}
				}
			case *message.Record:
				registerMediaIntoSink(register, sink, c.Path, c.File, "record", "")
			case *message.Video:
				registerMediaIntoSink(register, sink, c.Path, c.URL, "video", "")
			case *message.File:
				registerMediaIntoSink(register, sink, c.Path, c.URL, "file", c.Name)
			}
		}
	}
	return sink.storageParts()
}

// registerBase64Image 把 base64 图片（裸 base64 或 data URL）解码写入
// webui_files 并登记，用于 bot 回复图片的持久化。
func registerBase64Image(register func(srcPath, attachType, displayName string) (string, string, error), b64 string) (string, string, error) {
	payload := strings.TrimSpace(b64)
	if payload == "" {
		return "", "", fmt.Errorf("empty base64 payload")
	}
	ext := ".png"
	if strings.HasPrefix(payload, "data:") {
		if i := strings.Index(payload, ","); i >= 0 {
			header := strings.ToLower(payload[:i])
			switch {
			case strings.Contains(header, "jpeg"), strings.Contains(header, "jpg"):
				ext = ".jpg"
			case strings.Contains(header, "gif"):
				ext = ".gif"
			case strings.Contains(header, "webp"):
				ext = ".webp"
			}
			payload = payload[i+1:]
		}
	}
	data, decErr := base64.StdEncoding.DecodeString(payload)
	if decErr != nil || len(data) == 0 || int64(len(data)) > maxWebUIFileSize {
		return "", "", fmt.Errorf("invalid base64 image")
	}
	tmp, err := os.CreateTemp("", "b64img_*"+ext)
	if err != nil {
		return "", "", err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return "", "", err
	}
	if err := tmp.Close(); err != nil {
		return "", "", err
	}
	return register(tmpName, "image", "")
}

// registerReplyAttachment 把媒体源登记进 data/webui_files（复制文件并写
// 元数据；元数据 filename 使用存储名，保证前端 by-name 检索命中），
// 返回 attachment_id 与存储名。远端 URL 会先下载到临时文件（对齐本体
// _send 把媒体落盘的行为）。失败返回错误。
func (s *Server) registerReplyAttachment(srcPath, attachType, displayName string) (attachmentID, storedName string, err error) {
	src := strings.TrimSpace(srcPath)
	if src == "" {
		return "", "", fmt.Errorf("empty media source")
	}
	dir := s.webuiFilesDir()
	if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
		return "", "", mkErr
	}
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		tmp, cErr := os.CreateTemp(dir, "dl_*")
		if cErr != nil {
			return "", "", cErr
		}
		tmpName := tmp.Name()
		defer func() { _ = os.Remove(tmpName) }()
		client := &http.Client{Timeout: 60 * time.Second}
		resp, gErr := client.Get(src)
		if gErr != nil {
			return "", "", gErr
		}
		_, cErr = io.Copy(tmp, io.LimitReader(resp.Body, maxWebUIFileSize+1))
		_ = resp.Body.Close()
		if cErr == nil {
			cErr = tmp.Close()
		} else {
			_ = tmp.Close()
		}
		if cErr != nil {
			return "", "", cErr
		}
		src = tmpName
	} else if abs, aErr := filepath.Abs(src); aErr == nil {
		src = abs
	}
	info, statErr := os.Stat(src)
	if statErr != nil {
		return "", "", statErr
	}
	if info.IsDir() || info.Size() == 0 || info.Size() > maxWebUIFileSize {
		return "", "", fmt.Errorf("media source unusable: %s", src)
	}

	// 存储名对齐本体 generate_timestamp_id 语义：唯一 id + 扩展名；
	// 显示名（displayName / 源文件名）仅用于扩展名推断与前端展示。
	display := strings.TrimSpace(displayName)
	if display == "" {
		display = filepath.Base(src)
	}
	ext := strings.ToLower(filepath.Ext(display))
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(src))
	}
	attachmentID = uuid.NewString()
	storedName = attachmentID + ext
	dst := filepath.Join(dir, storedName)
	if cErr := copyFileLimited(src, dst, maxWebUIFileSize); cErr != nil {
		return "", "", cErr
	}
	meta := webuiFileMeta{
		AttachmentID: attachmentID,
		Filename:     storedName,
		Type:         attachType,
		Size:         info.Size(),
		CreatedAt:    time.Now().Format(time.RFC3339Nano),
		FileToken:    uuid.NewString(),
	}
	data, _ := json.Marshal(meta)
	if wErr := writeFileAtomic(filepath.Join(dir, attachmentID+".json"), data, 0o644); wErr != nil {
		_ = os.Remove(dst)
		return "", "", wErr
	}
	return attachmentID, storedName, nil
}

// copyFileLimited 复制源文件到目标路径，超过 limit 报错（防异常大文件）。
func copyFileLimited(src, dst string, limit int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(in, limit+1))
	if err != nil {
		_ = out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return err
	}
	if n > limit {
		_ = os.Remove(dst)
		return fmt.Errorf("file exceeds size limit")
	}
	return nil
}

// chainPartsSink 按到达顺序累积一次 run 的回复组件，run 结束时把完整
// parts 列表（plain/image/record/file/video）持久化进会话历史——对齐本体
// message_parts_helper.message_chain_to_storage_message_parts 的全组件
// 持久化语义（此前 Go 仅持久化纯文本）。媒体 part 登记产物携带
// attachment_id / filename（strip path 字段，对齐本体 storage parts 结构，
// 兼容现有 chat_store 读取与前端渲染）。
type chainPartsSink struct {
	mu    sync.Mutex
	parts []map[string]interface{}
	// lastPlainIndex 指向最后一个 plain part（无则 -1），把分帧到达的
	// 流式文本合并进同一 part（对齐本体 flush 语义）。
	lastPlainIndex int
}

func newChainPartsSink() *chainPartsSink {
	return &chainPartsSink{lastPlainIndex: -1}
}

// absorbPlain 追加一段文本（与最后一个 plain part 合并）。
func (sink *chainPartsSink) absorbPlain(text string) {
	if text == "" {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sink.lastPlainIndex >= 0 && sink.lastPlainIndex < len(sink.parts) {
		if last, ok := sink.parts[sink.lastPlainIndex]["text"].(string); ok {
			sink.parts[sink.lastPlainIndex]["text"] = last + text
			return
		}
	}
	sink.parts = append(sink.parts, map[string]interface{}{"type": "plain", "text": text})
	sink.lastPlainIndex = len(sink.parts) - 1
}

// absorbAttachment 追加一个媒体附件 part（type/attachment_id/filename/
// stored_filename，对齐本体 create_attachment_part_from_existing_file 结构）。
func (sink *chainPartsSink) absorbAttachment(attachType, attachmentID, filename, storedFilename string) {
	if attachmentID == "" {
		return
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	part := map[string]interface{}{
		"type":          attachType,
		"attachment_id": attachmentID,
		"filename":      filename,
	}
	if storedFilename != "" && storedFilename != filename {
		part["stored_filename"] = storedFilename
	}
	sink.parts = append(sink.parts, part)
	sink.lastPlainIndex = -1
}

// storageParts 返回待持久化的全组件 parts 快照（无组件时返回 nil）。
func (sink *chainPartsSink) storageParts() []map[string]interface{} {
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.parts) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(sink.parts))
	for _, p := range sink.parts {
		out = append(out, p)
	}
	return out
}

// processChatEvent runs the message through the "default" pipeline scheduler
// as a webchat-platform event and returns a channel closed when processing
// completes (nil if the pipeline is unavailable).
//
// Preferred path: enqueue on the event bus so dashboard chat shares the same
// single-goroutine pipeline as platform messages (the bus never runs two
// ProcessStage invocations concurrently). Completion is observed via a
// core.PipelineDone signal that the bus closes once the event is dispatched.
// Fallback: when the bus is unavailable (queue full / no scheduler), run the
// event through the scheduler directly in a goroutine.
//
// flags 按本体 request_flags 语义解析（未传默认 true）注入 Metadata；
// action_type / llm_checkpoint_id / thread_selected_text 等请求 extras 一并
// 注入（对齐本体 webchat_adapter.create_event 注入 event.extra）；
// reply part 解析为 Reply 组件追加进链（引用内容进 LLM 上下文）。
func (s *Server) processChatEvent(ctx context.Context, sessionID, runID, text string, req chatStreamRequest) <-chan struct{} {
	bus, ok := s.eventBus.(*core.EventBus)
	if !ok || bus == nil {
		return nil
	}
	chain := message.NewMessageChain(&message.Plain{Text: text})
	// reply 组件解析：引用消息 id → 历史内容进上下文（selected_text 优先，
	// 否则查会话/线程历史 plain 文本，只查 1 层，对齐本体 reply 语义）。
	chain.Chain = append(chain.Chain, s.replyComponents(req)...)
	// 上传的文件转成真实组件追加到链上，让管线能消费本地文件
	// （LLM 视觉上下文、平台转发等），而不是只留占位文本。
	if comps := s.filesToChainComponents(req.Files); len(comps) > 0 {
		chain.Chain = append(chain.Chain, comps...)
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "dashboard_chat",
			SelfID:     "dashboard_chat",
			SenderID:   "dashboard",
			SenderName: "dashboard",
			ConvID:     sessionID,
			IsGroup:    false,
		},
		Message:    chain,
		MessageStr: text,
		PlainText:  text,
		Timestamp:  time.Now(),
		// 不要预置 IsAtOrWakeCommand=true：WakingCheckStage 对已置位的
		// 事件直接跳过前缀剥离，导致 "/am_status" 的 "/" 不被剥掉、命令
		// handler 匹配失败（走向 LLM 闲聊）。置 false 让 WakingCheck 正常
		// 处理："/" 前缀剥离后命中命令；普通文本经好友自动唤醒（wakeByFriend）
		// 触发 LLM（CallLLM=true 兜底）。
		IsAtOrWakeCommand: false,
		CallLLM:           true,
		Metadata:          map[string]interface{}{},
		Ctx:               ctx,
	}
	if runID != "" {
		event.Metadata["run_id"] = runID
	}
	if req.SelectedProvider != "" {
		event.Metadata["selected_provider"] = req.SelectedProvider
	}
	if req.SelectedModel != "" {
		event.Metadata["selected_model"] = req.SelectedModel
	}
	// flags 解析对齐本体 request_flags.resolve_webchat_request_flags：
	// flags.* > 遗留顶层 enable_* > 默认 true，总是注入完整 flag 集。
	event.Metadata["flags"] = resolveWebchatRequestFlags(req.Flags, req.LegacyFlags)
	// 对齐本体 webchat_adapter.create_event：action_type / llm_checkpoint_id /
	// thread_selected_text 注入事件 extra（Go 侧为 Metadata）。
	for _, key := range []string{"action_type", "llm_checkpoint_id", "thread_selected_text"} {
		if req.Extras == nil {
			break
		}
		if v, ok := req.Extras[key]; ok && v != nil {
			event.Metadata[key] = v
		}
	}

	done := core.NewPipelineDone()
	event.Metadata[core.MetadataPipelineDone] = done
	if err := bus.Publish(event); err == nil {
		return done.Done()
	}

	// Bus unavailable (queue full) or scheduler missing: fall back to running
	// the event through the scheduler synchronously in a goroutine.
	scheduler := bus.GetScheduler("default")
	if scheduler == nil {
		return nil
	}
	go func() {
		defer done.Signal()
		if _, err := scheduler.Process(ctx, event); err != nil {
			logger.Error("dashboard chat pipeline failed: %v", err)
		}
	}()
	return done.Done()
}

// sendSSE writes one SSE data frame.
func sendSSE(w http.ResponseWriter, flusher http.Flusher, payload map[string]interface{}) {
	data, _ := json.Marshal(payload)
	// #nosec no-fprintf-to-responsewriter -- SSE 流：data 帧内容为 JSON 序列化负载，
	// Content-Type 为 text/event-stream，客户端按 JSON 解析，非 HTML 渲染上下文，无 XSS。
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data) // nosemgrep: go.lang.security.audit.xss.no-fprintf-to-responsewriter.no-fprintf-to-responsewriter
	flusher.Flush()
}

// chainPlainText extracts the plain text of a message chain.
func chainPlainText(chain *message.MessageChain) string {
	if chain == nil {
		return ""
	}
	var b strings.Builder
	for _, comp := range chain.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			b.WriteString(plain.Text)
		}
	}
	return b.String()
}

// plainTextFromParts joins the plain text of message parts.
func plainTextFromParts(parts []map[string]interface{}) string {
	var b strings.Builder
	for _, p := range parts {
		switch t, _ := p["type"].(string); t {
		case "plain", "":
			text, _ := p["text"].(string)
			b.WriteString(text)
		}
	}
	return b.String()
}

// replyComponents 解析请求 parts 中的 reply 组件（对齐本体
// message_parts_helper.parse_webchat_message_parts 的 reply 分支与独立
// 适配器 _parse_message_parts 的历史递归）：selected_text 优先，否则按
// message_id 查会话/线程历史 plain 文本（仅 1 层，历史内容中的 reply 不再
// 递归）。生成 Reply 组件追加进事件链，引用内容随链进入 LLM 上下文
// （pipeline collectFileAttachments 会展开 Reply.Chain）。
func (s *Server) replyComponents(req chatStreamRequest) []message.Component {
	var out []message.Component
	for _, p := range req.Parts {
		if t, _ := p["type"].(string); t != "reply" {
			continue
		}
		messageID := partIDString(p["message_id"])
		if messageID == "" {
			continue
		}
		selectedText, _ := p["selected_text"].(string)
		if selectedText == "" {
			selectedText = s.replyHistoryText(messageID)
		}
		out = append(out, &message.Reply{
			MessageID:  messageID,
			MessageStr: selectedText,
			Chain:      []message.Component{&message.Plain{Text: selectedText}},
		})
	}
	return out
}

// replyHistoryText 按 id 查历史消息（会话历史优先，线程历史兜底），提取
// 其 plain 文本。对齐本体 get_reply_parts（递归深度 1 层）。
func (s *Server) replyHistoryText(messageID string) string {
	if msg := s.chat.messageByID(messageID); msg != nil {
		if text := messageContentText(msg); text != "" {
			return text
		}
	}
	if msg := s.threads.messageByID(messageID); msg != nil {
		return messageContentText(msg)
	}
	return ""
}

// partIDString 把 part 中的 message_id 归一为字符串（JSON 数字为 float64，
// 对齐本体 cast_reply_id_to_str）。
func partIDString(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case nil:
		return ""
	default:
		return fmt.Sprint(t)
	}
}

// partsHaveMediaContent reports whether parts carry real content (plain text
// or a media part with attachment_id/filename). 对齐 Python 原版
// webchat_message_parts_have_content：纯图片/文件消息（无文本）也应放行。
func partsHaveMediaContent(parts []map[string]interface{}) bool {
	for _, p := range parts {
		switch t, _ := p["type"].(string); t {
		case "plain", "":
			if text, _ := p["text"].(string); text != "" {
				return true
			}
		case "image", "record", "file", "video":
			if id, _ := p["attachment_id"].(string); id != "" {
				return true
			}
			if name, _ := p["filename"].(string); name != "" {
				return true
			}
		}
	}
	return false
}

// filePartsToMessage converts a files array (uploaded attachments or legacy
// path strings) into message parts. Each element may be a string (legacy
// path) or a map carrying attachment_id/filename/type; recognizable types
// become real parts the WebUI renders via contentUrl/byNameUrl, while
// unrecognized entries fall back to the "[FILE] path" placeholder.
func filePartsToMessage(files []interface{}) []map[string]interface{} {
	if len(files) == 0 {
		return nil
	}
	out := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		switch v := f.(type) {
		case string:
			out = append(out, map[string]interface{}{"type": "plain", "text": "[FILE] " + v})
		case map[string]interface{}:
			typ, _ := v["type"].(string)
			id, _ := v["attachment_id"].(string)
			name, _ := v["filename"].(string)
			if name == "" {
				if p, ok := v["path"].(string); ok {
					name = p
				}
			}
			part := map[string]interface{}{
				"type":          typ,
				"attachment_id": id,
				"filename":      name,
			}
			if u, ok := v["url"].(string); ok && u != "" {
				part["embedded_url"] = u
			}
			switch typ {
			case "image", "record", "video", "file":
				out = append(out, part)
			default:
				// 无法识别类型：回退占位文本。
				out = append(out, map[string]interface{}{"type": "plain", "text": "[FILE] " + name})
			}
		default:
			out = append(out, map[string]interface{}{"type": "plain", "text": "[FILE] " + fmt.Sprint(f)})
		}
	}
	return out
}

// filesToChainComponents converts a files array (uploaded attachments) into
// message chain components so the pipeline receives the actual local files
// (image vision / platform forwarding) instead of dropping them. attachment_id
// maps to the file under data/webui_files; entries without one fall back to
// their path/file/url fields. Unresolvable entries are skipped.
func (s *Server) filesToChainComponents(files []interface{}) []message.Component {
	var comps []message.Component
	for _, f := range files {
		m, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		id, _ := m["attachment_id"].(string)
		path := ""
		if id != "" && safeAttachmentName(id) {
			path = filepath.Join(s.webuiFilesDir(), id)
		}
		if path == "" {
			for _, k := range []string{"path", "file"} {
				if p, ok := m[k].(string); ok && p != "" {
					path = p
					break
				}
			}
		}
		if path == "" {
			if u, ok := m["url"].(string); ok && u != "" {
				path = u
			}
		}
		if path == "" {
			continue
		}
		switch typ {
		case "image":
			comps = append(comps, &message.Image{Path: path, File: path, FileID: id})
		case "record":
			comps = append(comps, &message.Record{Path: path, File: path, FileID: id})
		case "video":
			comps = append(comps, &message.Video{Path: path, FileID: id})
		case "file":
			name, _ := m["filename"].(string)
			comps = append(comps, &message.File{Path: path, FileID: id, Name: name})
		}
	}
	return comps
}

// compile-time interface check: chatStreamAdapter must satisfy
// platform.PlatformAdapter.
var _ platform.PlatformAdapter = (*chatStreamAdapter)(nil)

// wsUpgrader upgrades HTTP connections to WebSocket.
var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin 白名单：仅允许同源连接（浏览器 WebSocket 无法设置自定义
	// Authorization 头，token 只能走 query——一次性 ws-ticket 或遗留 JWT，
	// 故用 Origin 校验防跨站 CSWSH）。无 Origin 头的非浏览器客户端
	// （curl/脚本）放行。
	CheckOrigin: func(r *http.Request) bool {
		origin := r.Header.Get("Origin")
		if origin == "" {
			return true
		}
		o, err := url.Parse(origin)
		if err != nil {
			return false
		}
		return o.Host == r.Host || o.Host == "localhost:6185" || o.Host == "127.0.0.1:6185"
	},
}

// wsClient wraps a live websocket connection. gorilla websocket forbids
// concurrent writers, so every frame is serialized behind writeMu; ctx is
// cancelled when the connection closes (or fails to write) so in-flight
// pipeline runs stop instead of living until the 300s deadline.
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc

	// runs tracks the cancel function of each in-flight chat run keyed by
	// message_id, so an interrupt frame can cancel a specific (or all) run.
	runMu sync.Mutex
	runs  map[string]context.CancelFunc
}

// wsPingInterval is how often the server sends a WS Ping to keep idle
// connections within the 10-minute read deadline (browsers only reply pong to
// a received ping). Exposed as a var so tests can shorten it.
var wsPingInterval = 4 * time.Minute

// handleUnifiedChatWS serves the WebUI's websocket chat transport
// (GET /api/v1/unified-chat/ws?token=...). The client sends messages shaped
// like {ct:"chat", t:"send", session_id, message_id, message:[parts], flags,
// selected_provider, selected_model} and receives JSON frames with the same
// event types as the SSE transport (session_id / user_message_saved /
// run_started / plain / complete / end), each carrying the message_id.
func (s *Server) handleUnifiedChatWS(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	// 鉴权（复核开放项 10-2）：优先一次性 ws-ticket（30s、单次使用，避免
	// 长效 JWT 进 URL / 代理日志），回退 JWT ?token= 校验（兼容旧客户端）。
	if s.auth == nil || !(s.consumeWSTicket(token) || s.auth.IsAuthenticated(token)) {
		writeJSON(w, http.StatusUnauthorized, apiError("未认证"))
		return
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.I18nWarn("WebSocket 升级失败: %v", err)
		return
	}
	wsCtx, wsCancel := context.WithCancel(context.Background())
	client := &wsClient{conn: conn, ctx: wsCtx, cancel: wsCancel, runs: make(map[string]context.CancelFunc)}
	defer wsCancel()
	defer conn.Close()

	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
	conn.SetPongHandler(func(string) error {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		return nil
	})

	// 服务端每 4 分钟主动发一次 Ping，否则空闲连接收不到任何帧、读侧
	// 10 分钟 ReadDeadline 一到就被强制断开。
	stopPing := make(chan struct{})
	defer close(stopPing)
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopPing:
				return
			case <-ticker.C:
				client.writeMu.Lock()
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					client.cancel()
				}
				client.writeMu.Unlock()
			}
		}
	}()

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			CT                 string                   `json:"ct"`
			T                  string                   `json:"t"`
			SessionID          string                   `json:"session_id"`
			MessageID          string                   `json:"message_id"`
			Message            []map[string]interface{} `json:"message"`
			Files              []interface{}            `json:"files"`
			Flags              map[string]interface{}   `json:"flags"`
			SelectedProvider   string                   `json:"selected_provider"`
			SelectedModel      string                   `json:"selected_model"`
			ActionType         string                   `json:"action_type"`
			LLMCheckpointID    string                   `json:"llm_checkpoint_id"`
			ThreadSelectedText string                   `json:"thread_selected_text"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.CT != "chat" {
			continue
		}

		switch msg.T {
		case "bind":
			// Acknowledge the session bind (Python sends session_bound).
			if msg.SessionID == "" {
				s.wsSend(client, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "session_id is required",
					"code": "INVALID_MESSAGE_FORMAT",
				})
				continue
			}
			s.wsSend(client, map[string]interface{}{
				"ct": "chat", "type": "session_bound", "session_id": msg.SessionID,
				"message_id": fmt.Sprintf("ws_sub_%d", time.Now().UnixNano()),
			})

		case "interrupt":
			// Cancel the in-flight pipeline run(s) for this connection: the
			// matching message_id run if provided, otherwise every active run.
			// The run's context is derived from this connection, so cancelling
			// it propagates to the LLM provider call and stops it early.
			client.runMu.Lock()
			if msg.MessageID != "" {
				if fn, ok := client.runs[msg.MessageID]; ok {
					fn()
				}
			} else {
				for _, fn := range client.runs {
					fn()
				}
			}
			client.runMu.Unlock()
			s.wsSend(client, map[string]interface{}{
				"ct": "chat", "t": "error", "data": "INTERRUPTED",
				"code":       "INTERRUPTED",
				"message_id": msg.MessageID,
			})

		case "send":
			if len(msg.Message) == 0 {
				s.wsSend(client, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "Message content is empty",
					"code": "INVALID_MESSAGE_FORMAT", "message_id": msg.MessageID,
				})
				continue
			}
			if msg.SessionID == "" {
				s.wsSend(client, map[string]interface{}{
					"ct": "chat", "t": "error", "data": "session_id is required",
					"code": "INVALID_MESSAGE_FORMAT", "message_id": msg.MessageID,
				})
				continue
			}
			go s.handleWSMessageSend(client, msg)
		}
	}
}

// wsSend writes one JSON frame to the websocket. Writes are serialized per
// connection (gorilla forbids concurrent writers); a failed write cancels the
// connection lifecycle so pending pipeline runs are torn down.
func (s *Server) wsSend(c *wsClient, payload map[string]interface{}) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteJSON(payload); err != nil {
		c.cancel()
		logger.Debug("ws send failed: %v", err)
	}
}

// handleWSMessageSend runs a chat message through the pipeline and streams the
// reply events back over the websocket (per message_id).
func (s *Server) handleWSMessageSend(c *wsClient, msg struct {
	CT                 string                   `json:"ct"`
	T                  string                   `json:"t"`
	SessionID          string                   `json:"session_id"`
	MessageID          string                   `json:"message_id"`
	Message            []map[string]interface{} `json:"message"`
	Files              []interface{}            `json:"files"`
	Flags              map[string]interface{}   `json:"flags"`
	SelectedProvider   string                   `json:"selected_provider"`
	SelectedModel      string                   `json:"selected_model"`
	ActionType         string                   `json:"action_type"`
	LLMCheckpointID    string                   `json:"llm_checkpoint_id"`
	ThreadSelectedText string                   `json:"thread_selected_text"`
}) {
	// 落库依赖绑定（无订阅者兜底与媒体登记共用，幂等）。
	s.chatAdapter.bindPersistence(s.chat, s.threads, s.registerReplyAttachment)

	sessionID := msg.SessionID
	text := plainTextFromParts(msg.Message)
	messageID := msg.MessageID
	if messageID == "" {
		messageID = fmt.Sprintf("r_%d", time.Now().UnixNano())
	}

	// Persist the user message.
	userRecord := map[string]interface{}{
		"id":          fmt.Sprintf("u_%d", time.Now().UnixNano()),
		"session_id":  sessionID,
		"sender_id":   "dashboard",
		"sender_name": "dashboard",
		"role":        "user",
		"type":        "user",
		"content":     map[string]interface{}{"type": "user", "message": msg.Message},
		"created_at":  time.Now().Format(time.RFC3339Nano),
	}
	s.chat.appendMessage(sessionID, userRecord)

	llmCheckpointID := msg.LLMCheckpointID
	if llmCheckpointID == "" {
		llmCheckpointID = fmt.Sprintf("c_%d", time.Now().UnixNano())
	}
	s.wsSend(c, map[string]interface{}{
		"ct": "chat", "type": "user_message_saved",
		"data":       map[string]interface{}{"id": userRecord["id"], "created_at": userRecord["created_at"], "llm_checkpoint_id": llmCheckpointID},
		"message_id": messageID,
	})
	s.wsSend(c, map[string]interface{}{
		"ct": "chat", "type": "run_started",
		"data":       map[string]interface{}{"run_id": messageID},
		"message_id": messageID,
	})

	ch, subSeq := s.chatAdapter.subscribe(sessionID)
	defer s.chatAdapter.unsubscribe(sessionID, subSeq, ch)

	// Derive a per-run context from the connection so an interrupt can cancel
	// this specific run (registered by message_id) without tearing down the
	// whole connection. Also register in the session-level registry so the
	// HTTP stop endpoint can cancel it.
	runCtx, runCancel := context.WithCancel(c.ctx)
	c.runMu.Lock()
	c.runs[messageID] = runCancel
	c.runMu.Unlock()
	s.registerChatRun(sessionID, messageID, runCancel)
	defer func() {
		s.unregisterChatRun(sessionID, messageID)
		c.runMu.Lock()
		delete(c.runs, messageID)
		c.runMu.Unlock()
		runCancel()
	}()

	req := chatStreamRequest{
		SessionID:        sessionID,
		Parts:            msg.Message,
		Files:            msg.Files,
		SelectedProvider: msg.SelectedProvider,
		SelectedModel:    msg.SelectedModel,
		Flags:            msg.Flags,
		PersistUser:      true,
		LLMCheckpointID:  llmCheckpointID,
		Extras: map[string]interface{}{
			"action_type":          msg.ActionType,
			"llm_checkpoint_id":    llmCheckpointID,
			"thread_selected_text": msg.ThreadSelectedText,
		},
	}
	done := s.processChatEvent(runCtx, sessionID, messageID, text, req)
	if done == nil {
		s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "error", "data": "对话管道不可用", "message_id": messageID})
		s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
		return
	}

	botSink := newChainPartsSink()
	var full strings.Builder
	deadline := time.After(300 * time.Second)
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-deadline:
			s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
			return
		case <-done:
			s.chatAdapter.unsubscribe(sessionID, subSeq, ch)
			for {
				select {
				case chain := <-ch:
					s.emitChainWS(c, &full, chain, messageID, botSink)
				default:
					goto done
				}
			}
		done:
			// 全组件持久化（对齐 SSE 路径与本体语义）。
			if parts := botSink.storageParts(); len(parts) > 0 {
				botRecord := map[string]interface{}{
					"id":          fmt.Sprintf("b_%d", time.Now().UnixNano()),
					"session_id":  sessionID,
					"sender_id":   "bot",
					"sender_name": "bot",
					"role":        "assistant",
					"type":        "bot",
					"content": map[string]interface{}{
						"type":    "bot",
						"message": parts,
					},
					"created_at": time.Now().Format(time.RFC3339Nano),
				}
				s.chat.appendMessage(sessionID, botRecord)
				s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "message_saved", "data": map[string]interface{}{"id": botRecord["id"], "created_at": botRecord["created_at"]}, "message_id": messageID})
			}
			s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "complete", "data": full.String(), "message_id": messageID})
			s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "end", "data": nil, "message_id": messageID})
			return
		case chain := <-ch:
			s.emitChainWS(c, &full, chain, messageID, botSink)
		}
	}
}

// emitChainWS sends WebSocket frames for a reply chain. 与 emitChainSSE 相同
// 的分帧语义（全组件帧 + 附件登记 + sink 累积），仅输出通道与帧封装不同。
func (s *Server) emitChainWS(c *wsClient, full *strings.Builder, chain *message.MessageChain, messageID string, sink *chainPartsSink) {
	if chain == nil {
		return
	}
	for _, comp := range chain.Chain {
		switch cc := comp.(type) {
		case *message.Plain:
			if cc.Text == "" {
				continue
			}
			full.WriteString(cc.Text)
			sink.absorbPlain(cc.Text)
			s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "plain", "chain_type": "text", "data": cc.Text, "message_id": messageID})
		case *message.Json:
			if b, err := json.Marshal(cc.Data); err == nil && len(b) > 0 {
				full.WriteString(string(b))
				sink.absorbPlain(string(b))
				s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "plain", "chain_type": "text", "data": string(b), "message_id": messageID})
			}
		case *message.Image:
			if data := chainImageDataURL(cc); data != "" {
				s.wsSend(c, map[string]interface{}{"ct": "chat", "type": "image", "data": data, "message_id": messageID})
				s.persistImageForSink(sink, cc)
				continue
			}
			if frame := s.mediaReplyFrame(sink, cc.Path, cc.File, "image", ""); frame != nil {
				frame["ct"] = "chat"
				frame["message_id"] = messageID
				s.wsSend(c, frame)
			}
		case *message.Record:
			if frame := s.mediaReplyFrame(sink, cc.Path, cc.File, "record", ""); frame != nil {
				frame["ct"] = "chat"
				frame["message_id"] = messageID
				s.wsSend(c, frame)
			}
		case *message.Video:
			if frame := s.mediaReplyFrame(sink, cc.Path, cc.URL, "video", ""); frame != nil {
				frame["ct"] = "chat"
				frame["message_id"] = messageID
				s.wsSend(c, frame)
			}
		case *message.File:
			if frame := s.mediaReplyFrame(sink, cc.Path, cc.URL, "file", cc.Name); frame != nil {
				frame["ct"] = "chat"
				frame["message_id"] = messageID
				s.wsSend(c, frame)
			}
		}
	}
}
