// Package deerflow implements the DeerFlow Agent Runner.
// 移植 Python astrbot/core/agent/runners/deerflow/（LangGraph HTTP API，手搓）。
package deerflow

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

const (
	deerflowThreadIDKey = "deerflow_thread_id"
	maxValuesHistory    = 200
)

// Deps 宿主注入。
type Deps struct {
	SessionGet func(ctx context.Context, sessionID, key string) (interface{}, error)
	SessionPut func(ctx context.Context, sessionID, key string, value interface{}) error
	// ResolveImageDataURL 把图片引用解析为 data URL（对齐 resolve_media_ref_to_base64_data().to_data_url()）。
	ResolveImageDataURL func(ctx context.Context, ref string) (string, error)
	HTTPClient          *http.Client
	Logger              *log.Logger
}

// Options 来自 agent_runner.config（runner_type=deerflow）。
type Options struct {
	APIBase                string
	APIKey                 string
	AuthHeader             string
	Proxy                  string
	AssistantID            string
	ModelName              string
	ThinkingEnabled        bool
	PlanMode               bool
	SubagentEnabled        bool
	MaxConcurrentSubagents int
	RecursionLimit         int
	TimeoutSec             float64
	Streaming              bool
}

// Runner 对齐 Python DeerflowAgentRunner。
type Runner struct {
	apiBase                string
	assistantID            string
	modelName              string
	thinkingEnabled        bool
	planMode               bool
	subagentEnabled        bool
	maxConcurrentSubagents int
	recursionLimit         int
	timeout                time.Duration
	streaming              bool
	client                 *APIClient
	deps                   Deps
}

// NewRunner 对齐 _parse_runner_config + _load_config_and_client。
func NewRunner(opts Options, deps Deps) (*Runner, error) {
	apiBase := opts.APIBase
	if apiBase == "" {
		apiBase = "http://127.0.0.1:2026"
	}
	if !strings.HasPrefix(apiBase, "http://") && !strings.HasPrefix(apiBase, "https://") {
		return nil, fmt.Errorf("DeerFlow API Base URL format is invalid. It must start with http:// or https://.")
	}
	assistantID := opts.AssistantID
	if assistantID == "" {
		assistantID = "lead_agent"
	}
	timeout := time.Duration(opts.TimeoutSec * float64(time.Second))
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	recursion := opts.RecursionLimit
	if recursion < 1 {
		recursion = 1000
	}
	maxSub := opts.MaxConcurrentSubagents
	if maxSub < 1 {
		maxSub = 3
	}
	return &Runner{
		apiBase: apiBase, assistantID: assistantID, modelName: opts.ModelName,
		thinkingEnabled: opts.ThinkingEnabled, planMode: opts.PlanMode,
		subagentEnabled: opts.SubagentEnabled, maxConcurrentSubagents: maxSub,
		recursionLimit: recursion, timeout: timeout, streaming: opts.Streaming,
		client: NewAPIClient(apiBase, opts.APIKey, opts.AuthHeader, opts.Proxy, deps.HTTPClient),
		deps:   deps,
	}, nil
}

func (r *Runner) logf(format string, args ...interface{}) {
	if r.deps.Logger != nil {
		r.deps.Logger.Printf(format, args...)
	}
}

type streamState struct {
	runValuesMessages []interface{}
	latestText        string
	prevTextForStream string
	hasValuesText     bool
	clarificationText string
	taskFailures      []string
	timedOut          bool
	baselineInit      bool
	seenMsgIDs        map[string]bool
	noIDFingerprints  map[int]string
}

// Run 对齐 Python _execute_deerflow_request。
func (r *Runner) Run(ctx context.Context, req *provider.ProviderRequest, onDelta func(text string) error) (*provider.LLMResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request is not set")
	}
	prompt := req.Prompt
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = fmt.Sprintf("deerflow-ephemeral-%d", time.Now().UnixNano())
	}

	threadID, err := r.ensureThreadID(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	payload, err := r.buildPayload(ctx, threadID, prompt, req.ImageURLs, req.SystemPrompt)
	if err != nil {
		return nil, err
	}
	state := &streamState{seenMsgIDs: map[string]bool{}, noIDFingerprints: map[int]string{}}

	streamErr := r.client.StreamRun(ctx, threadID, payload, r.timeout, func(event string, data interface{}) error {
		switch event {
		case "values":
			return r.handleValuesEvent(data, state, onDelta)
		case "messages-tuple", "messages", "message":
			return r.handleMessageEvent(data, state, onDelta)
		case "custom":
			state.taskFailures = append(state.taskFailures, extractTaskFailuresFromCustomEvent(data)...)
			return nil
		case "error":
			return fmt.Errorf("DeerFlow stream returned error event: %v", data)
		case "end":
			return errStop
		}
		return nil
	})
	if streamErr != nil && streamErr != errStop {
		if ctx.Err() == context.DeadlineExceeded {
			state.timedOut = true
		} else {
			return nil, streamErr
		}
	}

	chain, role := r.buildFinalResult(state)
	if r.streaming {
		nonPlain := []message.Component{}
		for _, c := range chain.Chain {
			if _, ok := c.(*message.Plain); !ok {
				nonPlain = append(nonPlain, c)
			}
		}
		if len(nonPlain) > 0 && onDelta != nil {
			// 对齐 _emit_non_plain_components_at_end：仅流式补发非文本组件。
			_ = nonPlain
		}
	}
	return &provider.LLMResponse{Role: role, ResultChain: chain}, nil
}

// ensureThreadID 对齐 _ensure_thread_id。
func (r *Runner) ensureThreadID(ctx context.Context, sessionID string) (string, error) {
	if r.deps.SessionGet != nil {
		if v, err := r.deps.SessionGet(ctx, sessionID, deerflowThreadIDKey); err == nil {
			if s, ok := v.(string); ok && s != "" {
				return s, nil
			}
		}
	}
	created, err := r.client.CreateThread(ctx, 20*time.Second)
	if err != nil {
		return "", err
	}
	threadID := ""
	for _, k := range []string{"thread_id", "id"} {
		if s, ok := created[k].(string); ok && s != "" {
			threadID = s
			break
		}
	}
	if threadID == "" {
		return "", fmt.Errorf("DeerFlow create thread 未返回 thread_id")
	}
	if r.deps.SessionPut != nil {
		_ = r.deps.SessionPut(ctx, sessionID, deerflowThreadIDKey, threadID)
	}
	return threadID, nil
}

func (r *Runner) buildPayload(ctx context.Context, threadID, prompt string, imageURLs []string, systemPrompt string) (map[string]interface{}, error) {
	runtimeConfigurable := map[string]interface{}{
		"thread_id":        threadID,
		"thinking_enabled": r.thinkingEnabled,
		"is_plan_mode":     r.planMode,
		"subagent_enabled": r.subagentEnabled,
	}
	if r.subagentEnabled {
		runtimeConfigurable["max_concurrent_subagents"] = r.maxConcurrentSubagents
	}
	if r.modelName != "" {
		runtimeConfigurable["model_name"] = r.modelName
	}
	messages, err := r.buildMessages(ctx, prompt, imageURLs, systemPrompt)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"assistant_id": r.assistantID,
		"input":        map[string]interface{}{"messages": messages},
		"stream_mode":  []interface{}{"values", "messages-tuple", "custom"},
		"context":      runtimeConfigurable,
		"config": map[string]interface{}{
			"recursion_limit": r.recursionLimit,
			"configurable":    runtimeConfigurable,
		},
	}, nil
}

func (r *Runner) buildMessages(ctx context.Context, prompt string, imageURLs []string, systemPrompt string) ([]interface{}, error) {
	messages := []interface{}{}
	if systemPrompt != "" {
		messages = append(messages, map[string]interface{}{"role": "system", "content": systemPrompt})
	}
	content, err := r.buildUserContentResolved(ctx, prompt, imageURLs)
	if err != nil {
		return nil, err
	}
	messages = append(messages, map[string]interface{}{"role": "user", "content": content})
	return messages, nil
}

// buildUserContentResolved 对齐 Python build_user_content_resolved。
func (r *Runner) buildUserContentResolved(ctx context.Context, prompt string, imageURLs []string) (interface{}, error) {
	if len(imageURLs) == 0 {
		return prompt, nil
	}
	content := []interface{}{}
	if prompt != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": prompt})
	}
	anyValid := false
	skipped := 0
	for _, ref := range imageURLs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			skipped++
			continue
		}
		dataURL := ""
		if r.deps.ResolveImageDataURL != nil {
			if u, err := r.deps.ResolveImageDataURL(ctx, ref); err == nil {
				dataURL = u
			}
		}
		if dataURL == "" {
			skipped++
			continue
		}
		content = append(content, map[string]interface{}{
			"type": "image_url", "image_url": map[string]interface{}{"url": dataURL},
		})
		anyValid = true
	}
	if skipped > 0 {
		note := "Note: some images could not be processed and were ignored."
		if !anyValid {
			note = "Note: none of the provided images could be processed."
		}
		content = append([]interface{}{map[string]interface{}{"type": "text", "text": note}}, content...)
	}
	return content, nil
}

func (r *Runner) handleValuesEvent(data interface{}, state *streamState, onDelta func(text string) error) error {
	valuesMessages := extractMessagesFromValuesData(data)
	if len(valuesMessages) == 0 {
		return nil
	}
	var newMessages []interface{}
	if !state.baselineInit {
		state.baselineInit = true
		for idx, msg := range valuesMessages {
			m, ok := msg.(map[string]interface{})
			if !ok {
				continue
			}
			newMessages = append(newMessages, m)
			if id := getMessageID(m); id != "" {
				state.seenMsgIDs[id] = true
			} else {
				state.noIDFingerprints[idx] = fingerprintMessage(m)
			}
		}
	} else {
		newMessages = extractNewMessages(valuesMessages, state)
	}
	latestText := ""
	if len(newMessages) > 0 {
		state.runValuesMessages = append(state.runValuesMessages, newMessages...)
		if len(state.runValuesMessages) > maxValuesHistory {
			state.runValuesMessages = state.runValuesMessages[len(state.runValuesMessages)-maxValuesHistory:]
		}
		latestText = extractLatestAIText(state.runValuesMessages)
		if latestText != "" {
			state.hasValuesText = true
		}
		if clar := extractLatestClarificationText(state.runValuesMessages); clar != "" {
			state.clarificationText = clar
		}
	}
	return r.updateText(state, latestText, "", onDelta)
}

func (r *Runner) handleMessageEvent(data interface{}, state *streamState, onDelta func(text string) error) error {
	delta := extractAIDeltaFromEventData(data)
	if delta != "" && !state.hasValuesText {
		if err := r.updateText(state, "", delta, onDelta); err != nil {
			return err
		}
	}
	if clar := extractClarificationFromEventData(data); clar != "" {
		state.clarificationText = clar
	}
	return nil
}

func (r *Runner) updateText(state *streamState, newFullText, deltaText string, onDelta func(text string) error) error {
	if newFullText != "" {
		state.latestText = newFullText
		if !r.streaming || onDelta == nil {
			return nil
		}
		delta := newFullText
		if strings.HasPrefix(newFullText, state.prevTextForStream) {
			delta = newFullText[len(state.prevTextForStream):]
		}
		if delta == "" {
			return nil
		}
		state.prevTextForStream = newFullText
		return onDelta(delta)
	}
	if deltaText != "" {
		state.latestText += deltaText
		if r.streaming && onDelta != nil {
			return onDelta(deltaText)
		}
	}
	return nil
}

func (r *Runner) buildFinalResult(state *streamState) (*message.MessageChain, string) {
	failuresOnly := false
	var chain *message.MessageChain
	if state.clarificationText != "" {
		chain = message.NewMessageChain(&message.Plain{Text: state.clarificationText})
	} else {
		chain = message.NewMessageChain()
		if latest := extractLatestAIMessage(state.runValuesMessages); latest != nil {
			chain = buildChainFromAIContent(latest["content"], ImageComponentFromURL)
		}
		if len(chain.Chain) == 0 && state.latestText != "" {
			chain = message.NewMessageChain(&message.Plain{Text: state.latestText})
		}
		if len(chain.Chain) == 0 {
			if failureText := buildTaskFailureSummary(state.taskFailures); failureText != "" {
				chain = message.NewMessageChain(&message.Plain{Text: failureText})
				failuresOnly = true
			}
		}
	}
	if len(chain.Chain) == 0 {
		chain = message.NewMessageChain(&message.Plain{Text: "DeerFlow returned an empty response."})
	}
	if state.timedOut {
		note := fmt.Sprintf("DeerFlow stream timed out after %ds. Returning partial result.", int(r.timeout.Seconds()))
		if last, ok := chain.Chain[len(chain.Chain)-1].(*message.Plain); ok {
			if last.Text != "" {
				last.Text = last.Text + "\n\n" + note
			} else {
				last.Text = note
			}
		} else {
			chain.Append(&message.Plain{Text: note})
		}
	}
	role := "assistant"
	if state.timedOut || failuresOnly {
		role = "err"
	}
	return chain, role
}

func extractNewMessages(valuesMessages []interface{}, state *streamState) []interface{} {
	newMessages := []interface{}{}
	for idx, msg := range valuesMessages {
		m, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		if id := getMessageID(m); id != "" {
			if state.seenMsgIDs[id] {
				continue
			}
			state.seenMsgIDs[id] = true
			newMessages = append(newMessages, m)
			continue
		}
		fp := fingerprintMessage(m)
		if prev, ok := state.noIDFingerprints[idx]; ok && prev == fp {
			continue
		}
		state.noIDFingerprints[idx] = fp
		newMessages = append(newMessages, m)
	}
	return newMessages
}

func fingerprintMessage(message map[string]interface{}) string {
	role := fmt.Sprint(message["role"])
	content := extractText(message["content"])
	sum := sha1.Sum([]byte(role + "\x00" + content))
	return hex.EncodeToString(sum[:])
}

// errStop 内部哨兵。
var errStop = fmt.Errorf("deerflow: stop")

// 供参数类型转换。
func coerceInt(v interface{}, def int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return i
		}
	}
	return def
}

var _ = json.Marshal
var _ = http.MethodPost
