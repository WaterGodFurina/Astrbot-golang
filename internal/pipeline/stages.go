// Package pipeline implements the message processing stages.
// Ported from astrbot/core/pipeline/
//
// The pipeline processes events through 9 ordered stages:
//  1. WakingCheckStage      - Check wake conditions
//  2. WhitelistCheckStage   - Check whitelist/blacklist
//  3. SessionStatusCheckStage - Check session enabled
//  4. RateLimitStage         - Check rate limit
//  5. ContentSafetyCheckStage - Check content safety
//  6. PreProcessStage        - Preprocess media, STT, path mapping
//  7. ProcessStage           - Plugin handler execution + LLM agent
//  8. ResultDecorateStage    - Decorate result (prefix, T2I, TTS, etc.)
//  9. RespondStage           - Send message chain to platform
package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/agent"
	"github.com/WaterGodFurina/Astrbot-golang/internal/contentsafety"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/cron"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/ratelimit"
	"github.com/WaterGodFurina/Astrbot-golang/internal/sandbox"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Pipeline")

// pluginRPCTimeout bounds every gRPC call into a subprocess plugin so a hung
// plugin handler (infinite loop, deadlock) cannot freeze the pipeline forever.
const pluginRPCTimeout = 30 * time.Second

var (
	// tagBlockRe matches <script>...</script> and <style>...</style> blocks.
	tagBlockRe = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	// tagRe matches any HTML tag.
	tagRe = regexp.MustCompile(`(?s)<[^>]*>`)
	// whitespaceRe collapses runs of whitespace.
	whitespaceRe = regexp.MustCompile(`\s+`)
)

// StageResult controls pipeline flow after a stage.
type StageResult = core.StageResult

// PipelineContext provides shared context for all stages.
type PipelineContext struct {
	AstrbotConfig  map[string]interface{}
	PluginManager  *star.Manager
	ConvManager    *conversation.Manager
	SessionService *conversation.SessionServiceManager
	PlatformMgr    *platform.PlatformManager
	// PersonaResolver resolves a persona's system prompt by conversation UMO
	// and persona id. Optional.
	PersonaResolver func(umo, personaID string) string
	// PersonaSkillsResolver returns the skill allow-list configured on a
	// persona. nil = unrestricted, empty slice = no skills allowed.
	// Optional.
	PersonaSkillsResolver func(personaID string) []string
	// UmoAliasResolver returns the display name a user set for a session UMO
	// (via the /name command). Optional.
	UmoAliasResolver func(umo string) string
	// SkillManager provides active skills for LLM system prompt injection.
	// Optional.
	SkillManager *skills.SkillManager
	// SandboxManager routes computer-use tools when the sandbox runtime is
	// active. Optional.
	SandboxManager *sandbox.Manager
	// CronManager schedules future tasks (future_task tool). Optional.
	CronManager *cron.CronJobManager
	// Database records platform messages / provider calls for statistics.
	// Optional.
	Database *db.Database
	// EventBus allows stages to re-publish events (e.g. rate-limit stall).
	// Optional.
	EventBus *core.EventBus
	// SubPlugins is the subprocess plugin runtime. When set, ProcessStage and
	// ResultDecorateStage use it to collect LLM tools, apply on_llm_request
	// hooks, and run result-decoration hooks. Optional.
	SubPlugins *plugin.SubprocessManager
}

// PipelineStage is the interface every stage must implement.
type PipelineStage interface {
	Name() string
	Initialize(ctx *PipelineContext) error
	Process(ctx context.Context, event *core.Event) (*StageResult, error)
}

// ---------------------------------------------------------------------------
// Stage 1: WakingCheckStage
// ---------------------------------------------------------------------------

// WakingCheckStage checks whether the bot should wake up for this message.
// Ported from astrbot/core/pipeline/waking_check/stage.py
type WakingCheckStage struct {
	wakePrefixes []string
	nickname     []string
	wakeByAt     bool
	wakeByPrefix bool
	wakeByFriend bool
	ignoreAtAll  bool
	cmdPrefix    string
	aiWakePrefix string
}

func NewWakingCheckStage() *WakingCheckStage {
	return &WakingCheckStage{
		wakeByAt:     true,
		wakeByPrefix: true,
		// Friend messages auto-wake unless friend_message_needs_wake_prefix is
		// set (mirrors Python's private-chat wake logic); overridden below.
		wakeByFriend: true,
		cmdPrefix:    "/",
	}
}

func (s *WakingCheckStage) Name() string { return "waking_check" }

func (s *WakingCheckStage) Initialize(ctx *PipelineContext) error {
	// wake_prefix lives at the top level of the config (astrbot/core/config/default.py:294),
	// not inside platform_settings.
	s.wakePrefixes = append(s.wakePrefixes, toStringList(ctx.AstrbotConfig["wake_prefix"])...)
	// Fall back to the "/" command prefix (Python DEFAULT_CONFIG ships wake_prefix=["/"])
	if len(s.wakePrefixes) == 0 && s.cmdPrefix != "" {
		s.wakePrefixes = append(s.wakePrefixes, s.cmdPrefix)
	}

	ps := bindPlatformSettings(ctx.AstrbotConfig)
	s.nickname = append(s.nickname, ps.Nickname...)
	if ps.WakeByAt != nil {
		s.wakeByAt = *ps.WakeByAt
	}
	if ps.WakeByPrefix != nil {
		s.wakeByPrefix = *ps.WakeByPrefix
	}
	if ps.WakeByFriend != nil {
		s.wakeByFriend = *ps.WakeByFriend
	}
	// friend_message_needs_wake_prefix=true means friend chat must use a prefix.
	if ps.FriendMessageNeedsWakePrefix {
		s.wakeByFriend = false
	}
	s.ignoreAtAll = ps.IgnoreAtAll
	if ps.CmdPrefix != "" {
		s.cmdPrefix = ps.CmdPrefix
	}

	// AI wake word: provider_settings.wake_prefix (e.g. "ai").
	// When set, LLM chat requires "<prefix><ai word> <text>".
	if psAI := bindProviderSettings(ctx.AstrbotConfig); psAI != nil {
		s.aiWakePrefix = strings.TrimSpace(psAI.WakePrefix)
	}

	logger.Debug("WakingCheck initialized: prefixes=%v, nicknames=%v, wakeByAt=%v, wakeByPrefix=%v, wakeByFriend=%v, aiWakePrefix=%q",
		s.wakePrefixes, s.nickname, s.wakeByAt, s.wakeByPrefix, s.wakeByFriend, s.aiWakePrefix)
	return nil
}

func (s *WakingCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// If the event already has is_at_or_wake_command set, skip
	if event.IsAtOrWakeCommand {
		return &StageResult{Continue: true}, nil
	}

	// Use the pure-text message string (Python's event.message_str), which
	// excludes At components, so "/help" is matched by the "/" wake prefix.
	text := event.MessageStr
	if text == "" {
		text = event.PlainText
	}
	if text == "" {
		text = extractPlainText(event.Message)
	}
	logger.Debug("WakingCheck: text=%q plaintext=%q prefixes=%v wakeByPrefix=%v wakeByFriend=%v isGroup=%v",
		text, event.PlainText, s.wakePrefixes, s.wakeByPrefix, s.wakeByFriend, event.Source.IsGroup)

	// Check wake prefixes
	if s.wakeByPrefix && len(s.wakePrefixes) > 0 {
		for _, prefix := range s.wakePrefixes {
			if strings.HasPrefix(text, prefix) {
				s.applyPrefixWake(event, text, prefix)
				return &StageResult{Continue: true}, nil
			}
		}
	}

	// Check nicknames
	if s.wakeByPrefix && len(s.nickname) > 0 {
		for _, nick := range s.nickname {
			if strings.Contains(text, nick) {
				event.IsAtOrWakeCommand = true
				event.SetExtra("llm_wake", true)
				logger.Debug("Woken by nickname '%s'", nick)
				return &StageResult{Continue: true}, nil
			}
		}
	}

	// Check @mention / @all / reply-quoting-bot (Python parity)
	if s.wakeByAt && event.Message != nil {
		for _, comp := range event.Message.Chain {
			if at, ok := comp.(*message.At); ok {
				if at.TargetID == event.Source.SelfID {
					event.IsAtOrWakeCommand = true
					// Group @-wake also supports prefix chat: "@bot /你好" triggers LLM.
					atText := strings.TrimSpace(text)
					matched := false
					if s.wakeByPrefix && len(s.wakePrefixes) > 0 {
						for _, prefix := range s.wakePrefixes {
							if strings.HasPrefix(atText, prefix) {
								s.applyPrefixWake(event, atText, prefix)
								matched = true
								break
							}
						}
					}
					if !matched {
						// @mention without a prefix still starts a chat
						// (Python: is_at_or_wake_command triggers the LLM agent).
						event.SetExtra("llm_wake", true)
						logger.Debug("Woken by @mention (chat enabled)")
					}
					return &StageResult{Continue: true}, nil
				}
			}
			if _, ok := comp.(*message.AtAll); ok {
				if s.ignoreAtAll {
					continue
				}
				event.IsAtOrWakeCommand = true
				event.SetExtra("llm_wake", true)
				logger.Debug("Woken by @all")
				return &StageResult{Continue: true}, nil
			}
			if r, ok := comp.(*message.Reply); ok && !event.Source.IsGroup {
				// quoting the bot in a private chat wakes it (Python parity)
				if r.SenderID == event.Source.SelfID {
					event.IsAtOrWakeCommand = true
					event.SetExtra("llm_wake", true)
					return &StageResult{Continue: true}, nil
				}
			}
		}
	}

	// Friend messages wake (configurable)
	if s.wakeByFriend && !event.Source.IsGroup {
		event.IsAtOrWakeCommand = true
		event.SetExtra("llm_wake", true)
		return &StageResult{Continue: true}, nil
	}

	// Not woken — allow plugin handlers to decide (they may have their own filters)
	// In Python, this sets is_wake=False and continues; if no handlers match, the event is stopped.
	event.IsAtOrWakeCommand = false
	return &StageResult{Continue: true}, nil
}

// applyPrefixWake strips the wake prefix (and optional AI wake word) from the
// event text and marks whether LLM chat should be triggered.
func (s *WakingCheckStage) applyPrefixWake(event *core.Event, text, prefix string) {
	event.IsAtOrWakeCommand = true
	trimmed := strings.TrimSpace(strings.TrimPrefix(text, prefix))
	event.WakeCommand = trimmed
	// AI wake word: when provider_settings.wake_prefix is configured
	// (e.g. "ai"), LLM chat requires "<prefix><ai wake word> <text>".
	// Without an AI wake word, the prefix alone triggers chat.
	if s.aiWakePrefix != "" {
		if strings.HasPrefix(trimmed, s.aiWakePrefix) {
			trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, s.aiWakePrefix))
			event.SetExtra("llm_wake", true)
		} else {
			// AI wake word configured but not present: do not trigger chat
			event.SetExtra("llm_wake", false)
		}
	} else {
		event.SetExtra("llm_wake", true)
	}
	// Strip the wake prefix so command filters match the bare command
	// (mirrors astrbot/core/pipeline/waking_check/stage.py).
	event.MessageStr = trimmed
	event.PlainText = trimmed
	logger.Debug("Woken by prefix '%s', stripped to %q (llm_wake=%v)",
		prefix, event.MessageStr, event.GetExtra("llm_wake"))
}

// ---------------------------------------------------------------------------
// Stage 2: WhitelistCheckStage
// ---------------------------------------------------------------------------

// WhitelistCheckStage filters events by whitelist/blacklist.
// Ported from astrbot/core/pipeline/whitelist_check/stage.py
type WhitelistCheckStage struct {
	enableWhitelist       bool
	whitelist             map[string]bool
	wlIgnoreAdminOnGroup  bool
	wlIgnoreAdminOnFriend bool
	wlLog                 bool
}

func NewWhitelistCheckStage() *WhitelistCheckStage {
	return &WhitelistCheckStage{
		whitelist: make(map[string]bool),
	}
}

func (s *WhitelistCheckStage) Name() string { return "whitelist_check" }

func (s *WhitelistCheckStage) Initialize(ctx *PipelineContext) error {
	ps := bindPlatformSettings(ctx.AstrbotConfig)
	s.enableWhitelist = ps.EnableIDWhiteList
	for _, id := range ps.IDWhitelist {
		if id = strings.TrimSpace(id); id != "" {
			s.whitelist[id] = true
		}
	}
	// Prefer the per-scope keys; fall back to the generic wl_ignore_admin.
	if ps.WLIgnoreAdminOnGroup != nil {
		s.wlIgnoreAdminOnGroup = *ps.WLIgnoreAdminOnGroup
	} else {
		s.wlIgnoreAdminOnGroup = ps.WLIgnoreAdmin
	}
	if ps.WLIgnoreAdminOnFriend != nil {
		s.wlIgnoreAdminOnFriend = *ps.WLIgnoreAdminOnFriend
	} else {
		s.wlIgnoreAdminOnFriend = ps.WLIgnoreAdmin
	}
	s.wlLog = ps.WLLog
	logger.Debug("WhitelistCheck initialized: enable=%v, whitelist_size=%d", s.enableWhitelist, len(s.whitelist))
	return nil
}

func (s *WhitelistCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if !s.enableWhitelist {
		return &StageResult{Continue: true}, nil
	}

	// An empty whitelist means the check is disabled (all sessions allowed),
	// mirroring astrbot/core/pipeline/whitelist_check/stage.py.
	if len(s.whitelist) == 0 {
		return &StageResult{Continue: true}, nil
	}

	// Admin bypass
	if event.Role == "admin" {
		if s.wlIgnoreAdminOnGroup && event.Source.IsGroup {
			return &StageResult{Continue: true}, nil
		}
		if s.wlIgnoreAdminOnFriend && !event.Source.IsGroup {
			return &StageResult{Continue: true}, nil
		}
	}

	unifiedOrigin := event.UnifiedMsgOrigin()
	groupID := event.Source.ConvID

	if !s.whitelist[unifiedOrigin] && !s.whitelist[strings.TrimSpace(groupID)] {
		if s.wlLog {
			logger.Debug("Session %s not in allowlist, stopping event", unifiedOrigin)
		}
		event.Stop()
		return &StageResult{Continue: false}, nil
	}

	return &StageResult{Continue: true}, nil
}

// ---------------------------------------------------------------------------
// Stage 3: SessionStatusCheckStage
// ---------------------------------------------------------------------------

// SessionStatusCheckStage checks if the session is overall enabled.
// Ported from astrbot/core/pipeline/session_status_check/stage.py
type SessionStatusCheckStage struct {
	sessionService *conversation.SessionServiceManager
	convMgr        *conversation.Manager
}

func NewSessionStatusCheckStage() *SessionStatusCheckStage {
	return &SessionStatusCheckStage{}
}

func (s *SessionStatusCheckStage) Name() string { return "session_status_check" }

func (s *SessionStatusCheckStage) Initialize(ctx *PipelineContext) error {
	s.sessionService = ctx.SessionService
	s.convMgr = ctx.ConvManager
	return nil
}

func (s *SessionStatusCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if s.sessionService == nil {
		return &StageResult{Continue: true}, nil
	}

	umo := event.UnifiedMsgOrigin()
	if !s.sessionService.IsSessionEnabled(umo) {
		logger.Debug("Session %s is disabled; stopping event", umo)

		// Workaround for #2309: create conversation if not exists
		if s.convMgr != nil {
			convID := s.convMgr.GetCurrConversationID(umo)
			if convID == "" {
				s.convMgr.NewConversation(umo, event.Source.Platform)
			}
		}

		event.Stop()
		return &StageResult{Continue: false}, nil
	}
	return &StageResult{Continue: true}, nil
}

// ---------------------------------------------------------------------------
// Stage 4: RateLimitStage
// ---------------------------------------------------------------------------

// RateLimitStage checks rate limits per session.
// Ported from astrbot/core/pipeline/rate_limit_check/stage.py
type RateLimitStage struct {
	limiter  *ratelimit.RateLimiter
	eventBus *core.EventBus
}

func NewRateLimitStage() *RateLimitStage {
	return &RateLimitStage{}
}

func (s *RateLimitStage) Name() string { return "rate_limit" }

func (s *RateLimitStage) Initialize(ctx *PipelineContext) error {
	s.eventBus = ctx.EventBus
	maxReq := 20
	windowSeconds := 60
	strategy := ratelimit.StrategyStall

	ps := bindPlatformSettings(ctx.AstrbotConfig)
	// Nested rate_limit {count,time,strategy} (current config format).
	if ps.RateLimit.Count > 0 {
		maxReq = ps.RateLimit.Count
	}
	if ps.RateLimit.Time > 0 {
		windowSeconds = ps.RateLimit.Time
	}
	if ps.RateLimit.Strategy == "discard" {
		strategy = ratelimit.StrategyDiscard
	}
	// Legacy flat keys override when present.
	if v := int(ps.RateLimitTime); v > 0 {
		windowSeconds = v
	}
	if ps.RateLimitStrategy == "discard" {
		strategy = ratelimit.StrategyDiscard
	}

	s.limiter = ratelimit.NewRateLimiter(maxReq, DurationFromSeconds(windowSeconds), strategy)
	logger.Debug("RateLimit initialized: max=%d, window=%ds, strategy=%v", maxReq, windowSeconds, strategy)
	return nil
}

func (s *RateLimitStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	sessionID := event.UnifiedMsgOrigin()
	allowed, stall := s.limiter.Allow(sessionID)
	if !allowed {
		if stall > 0 {
			logger.Debug("Session %s rate-limited, stalling for %.2fs", sessionID, stall.Seconds())
			// Stall strategy: re-publish the event once the window frees up,
			// matching Python's async sleep + resume. This must not block the
			// single-goroutine event bus, so we schedule a delayed re-queue.
			if s.eventBus != nil {
				s.eventBus.PublishDelayed(event, stall)
			} else {
				// No bus reference (tests): fall back to stopping the event.
				event.Stop()
			}
			return &StageResult{Continue: false}, nil
		}
		logger.Debug("Session %s rate-limited, discarded", sessionID)
		event.Stop()
		return &StageResult{Continue: false}, nil
	}
	return &StageResult{Continue: true}, nil
}

// ---------------------------------------------------------------------------
// Stage 5: ContentSafetyCheckStage
// ---------------------------------------------------------------------------

// ContentSafetyCheckStage checks message content against safety rules.
// Ported from astrbot/core/pipeline/content_safety_check/stage.py
type ContentSafetyCheckStage struct {
	selector *contentsafety.StrategySelector
}

func NewContentSafetyCheckStage() *ContentSafetyCheckStage {
	return &ContentSafetyCheckStage{}
}

func (s *ContentSafetyCheckStage) Name() string { return "content_safety_check" }

func (s *ContentSafetyCheckStage) Initialize(ctx *PipelineContext) error {
	config := map[string]interface{}{}
	if cs, ok := ctx.AstrbotConfig["content_safety"].(map[string]interface{}); ok {
		config = cs
	}
	s.selector = contentsafety.NewStrategySelector(config)
	logger.Debug("ContentSafetyCheck initialized: enabled=%v", s.selector.IsEnabled())
	return nil
}

func (s *ContentSafetyCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if !s.selector.IsEnabled() {
		return &StageResult{Continue: true}, nil
	}

	text := event.PlainText
	if text == "" {
		text = extractPlainText(event.Message)
	}

	// Also check quoted reply text
	texts := []string{text}
	if event.Message != nil {
		for _, comp := range event.Message.Chain {
			if reply, ok := comp.(*message.Reply); ok && reply.Chain != nil {
				for _, rc := range reply.Chain {
					if plain, ok := rc.(*message.Plain); ok && plain.Text != "" {
						texts = append(texts, plain.Text)
					}
				}
			}
		}
	}

	ok, info := s.selector.Check(strings.Join(texts, "\n"))
	if !ok {
		if event.IsAtOrWakeCommand {
			// Set a block result
			event.Result = &message.MessageEventResult{}
			event.Result.Chain = []message.Component{&message.Plain{Text: "Your message or the model response contains inappropriate content and has been blocked."}}
		}
		event.Stop()
		logger.Debug("Content safety check failed: %s", info)
		return &StageResult{Continue: false}, nil
	}
	return &StageResult{Continue: true}, nil
}

// ---------------------------------------------------------------------------
// Stage 6: PreProcessStage
// ---------------------------------------------------------------------------

// PreProcessStage normalizes media components, maps paths, and runs STT.
// Ported from astrbot/core/pipeline/preprocess_stage/stage.py
type PreProcessStage struct {
	config map[string]interface{}
}

func NewPreProcessStage() *PreProcessStage {
	return &PreProcessStage{}
}

func (s *PreProcessStage) Name() string { return "preprocess" }

func (s *PreProcessStage) Initialize(ctx *PipelineContext) error {
	s.config = ctx.AstrbotConfig
	return nil
}

func (s *PreProcessStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if event.Message == nil || len(event.Message.Chain) == 0 {
		return &StageResult{Continue: true}, nil
	}

	// Build the plain text representation of the message
	plainText := extractPlainText(event.Message)
	event.PlainText = plainText

	// Normalize image paths: file:// URIs → local paths
	for i, comp := range event.Message.Chain {
		if img, ok := comp.(*message.Image); ok {
			normalizeImagePath(img)
			event.Message.Chain[i] = img
		}
		// Convert Record components to plain text via STT would happen here
		// (requires actual STT service, skip for now)
	}

	logger.Debug("PreProcess: plain_text='%s', components=%d", plainText, len(event.Message.Chain))
	return &StageResult{Continue: true}, nil
}

// ---------------------------------------------------------------------------
// Stage 7: ProcessStage
// ---------------------------------------------------------------------------

// ProcessStage dispatches to plugin handlers (star) and/or LLM agent.
// Ported from astrbot/core/pipeline/process_stage/stage.py
type ProcessStage struct {
	pluginMgr     *star.Manager
	convMgr       *conversation.Manager
	config        map[string]interface{}
	personaPrompt func(umo, personaID string) string
	personaSkills func(personaID string) []string
	umoAlias      func(umo string) string
	skillMgr      *skills.SkillManager
	platformMgr   *platform.PlatformManager
	sandboxMgr    *sandbox.Manager
	cronMgr       *cron.CronJobManager
	database      *db.Database
	providerConf  *ProviderSettings
	// subPlugins is the subprocess plugin runtime: source of LLM tools and
	// on_llm_request hooks.
	subPlugins *plugin.SubprocessManager

	// MCP servers (data/mcp_server.json). Loaded lazily on the first tool
	// collection; full tool name = "<sanitized_server>.<tool_name>".
	mcpMu      sync.Mutex
	mcpLoaded  bool
	mcpSig     string
	mcpClients map[string]*agent.MCPClient       // sanitized server name -> client
	mcpSchemas map[string]map[string]interface{} // full tool name -> OpenAI tool schema
}

func NewProcessStage() *ProcessStage {
	return &ProcessStage{}
}

// SetPersonaResolver registers a callback that resolves a persona's system
// prompt for a conversation (persona id -> prompt text).
func (s *ProcessStage) SetPersonaResolver(fn func(umo, personaID string) string) {
	s.personaPrompt = fn
}

// SetPersonaSkillsResolver registers a callback that resolves a persona's
// skill allow-list (persona id -> allowed skill names).
func (s *ProcessStage) SetPersonaSkillsResolver(fn func(personaID string) []string) {
	s.personaSkills = fn
}

// personaID extracts the persona id from the provider settings.
func personaID(settings map[string]interface{}) string {
	if p, ok := settings["persona"].(string); ok {
		return p
	}
	return ""
}

func (s *ProcessStage) Name() string { return "process" }

func (s *ProcessStage) Initialize(ctx *PipelineContext) error {
	s.pluginMgr = ctx.PluginManager
	s.convMgr = ctx.ConvManager
	s.config = ctx.AstrbotConfig
	s.skillMgr = ctx.SkillManager
	s.platformMgr = ctx.PlatformMgr
	s.sandboxMgr = ctx.SandboxManager
	s.cronMgr = ctx.CronManager
	s.database = ctx.Database
	s.subPlugins = ctx.SubPlugins
	s.providerConf = bindProviderSettings(ctx.AstrbotConfig)
	if ctx.PersonaResolver != nil {
		s.personaPrompt = ctx.PersonaResolver
	}
	if ctx.PersonaSkillsResolver != nil {
		s.personaSkills = ctx.PersonaSkillsResolver
	}
	if ctx.UmoAliasResolver != nil {
		s.umoAlias = ctx.UmoAliasResolver
	}
	return nil
}

func (s *ProcessStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Run subprocess plugin on_message hooks (mirrors Python's
	// @filter.on_message): plugins observe every incoming message.
	dispatchSubprocessHooks(s.subPlugins, event, "on_message")

	// Record the incoming message for statistics (message_count).
	if s.database != nil {
		msg := event.MessageStr
		if msg == "" {
			msg = extractPlainText(event.Message)
		}
		if msg != "" {
			_ = s.database.RecordPlatformMessage(event.Source.Platform, event.UnifiedMsgOrigin(), event.Source.SenderID, msg)
		}
	}

	// Try plugin handlers first
	activated := s.findMatchingHandlers(event)
	if len(activated) > 0 {
		// Execute handlers in priority order
		for _, handler := range activated {
			if event.IsStopped() {
				break
			}
			if err := s.executeHandler(ctx, event, handler); err != nil {
				logger.Error("Handler %s failed: %v", handler.HandlerFullName, err)
			}
		}
		// If any handler produced a result, yield
		if event.Result != nil {
			return &StageResult{Continue: true}, nil
		}
	}

	// If not woken, stop
	if !event.IsAtOrWakeCommand {
		event.Stop()
		return &StageResult{Continue: false}, nil
	}

	// Call LLM agent
	if s.shouldCallLLM(event) {
		if err := s.callLLMAgent(ctx, event); err != nil {
			logger.Error("LLM agent call failed: %v", err)
			event.Result = &message.MessageEventResult{}
			event.Result.Chain = []message.Component{&message.Plain{Text: "LLM 调用失败: " + err.Error()}}
		}
	}

	return &StageResult{Continue: true}, nil
}

// findMatchingHandlers returns plugin handlers that match this event.
func (s *ProcessStage) findMatchingHandlers(event *core.Event) []*star.StarHandlerMetadata {
	if s.pluginMgr == nil {
		return nil
	}
	registry := s.pluginMgr.Handlers()
	if registry == nil {
		return nil
	}

	// Get all filter-type handlers, sorted by priority
	handlers := registry.GetFilterHandlers()
	result := []*star.StarHandlerMetadata{}

	for _, handler := range handlers {
		if !handler.Enabled {
			continue
		}
		// Build filter context from event
		fctx := &star.FilterContext{
			MessageStr:    event.MessageStr,
			IsAtOrWake:    event.IsAtOrWakeCommand,
			EventSenderID: event.Source.SenderID,
			EventPlatform: event.Source.Platform,
			EventRole:     event.Role,
		}
		// Check each filter on the handler
		matched := false
		for _, filter := range handler.EventFilters {
			if filter.Match(fctx) {
				matched = true
				break
			}
		}
		if matched {
			logger.Debug("ProcessStage: handler %s matched for message %q", handler.HandlerFullName, event.MessageStr)
			result = append(result, handler)
		}
	}

	return result
}

// executeHandler invokes a single handler.
func (s *ProcessStage) executeHandler(ctx context.Context, event *core.Event, handler *star.StarHandlerMetadata) error {
	if handler.Handler == nil {
		return nil
	}
	return handler.Handler(event)
}

// shouldCallLLM returns true if the LLM agent should be invoked.
func (s *ProcessStage) shouldCallLLM(event *core.Event) bool {
	// Check if provider is enabled (absent key -> allowed, matching the
	// original assertion logic).
	if s.providerConf != nil && s.providerConf.Enable != nil && !*s.providerConf.Enable {
		return false
	}

	// Don't call LLM if the event was already handled and stopped
	if event.IsStopped() {
		return false
	}

	// Explicitly requested
	if event.CallLLM {
		return true
	}

	// Chat is triggered when the event was woken and llm_wake is set by
	// WakingCheckStage. Prefix wake honors the optional AI wake word
	// (provider_settings.wake_prefix); @mention / nickname / @all / friend
	// auto-wake always enable chat (Python parity: is_at_or_wake_command runs
	// the LLM agent).
	if v, ok := event.GetExtra("llm_wake").(bool); ok {
		return v
	}

	return false
}

// callLLMAgent invokes the LLM provider and sets the result.
func (s *ProcessStage) callLLMAgent(ctx context.Context, event *core.Event) error {
	prompt := event.PlainText
	if prompt == "" {
		prompt = event.MessageStr
	}
	if prompt == "" {
		prompt = extractPlainText(event.Message)
	}
	logger.Debug("callLLMAgent: prompt=%q plaintext=%q messagestr=%q", prompt, event.PlainText, event.MessageStr)
	if prompt == "" {
		return nil
	}

	providerCfg, providerSettings, err := s.resolveProvider()
	if err != nil {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 " + err.Error()}}
		return nil
	}

	// Resolve the conversation. Mirrors Python's `_get_session_conv`: the
	// conversation is lazily created if it does not exist yet. The current
	// user message is appended to history only after the LLM round finishes
	// (Python appends the user+assistant pair post-completion), so the prompt
	// is not duplicated in req.Contexts.
	personaID := ""
	if s.convMgr != nil {
		conv := s.convMgr.GetOrCreateConversation(event.UnifiedMsgOrigin(), event.Source.Platform)
		if conv != nil {
			personaID = conv.Persona
			providerSettings["persona"] = conv.Persona
		}
	}
	// Fall back to the global default persona (provider_settings.default_personality)
	if personaID == "" && s.providerConf != nil {
		personaID = s.providerConf.DefaultPersonality
		if personaID != "" {
			providerSettings["persona"] = personaID
		}
	}

	// Resolve the persona system prompt (conv.Persona holds the persona id).
	systemPrompt := ""
	if s.personaPrompt != nil {
		systemPrompt = s.personaPrompt(event.UnifiedMsgOrigin(), personaID)
	}
	if systemPrompt == "" {
		systemPrompt = personaPrompt(providerSettings)
	}

	// Inject active skills into the system prompt (mirrors Python's
	// astr_main_agent._ensure_persona_and_skills).
	systemPrompt = s.applySkills(systemPrompt, providerSettings, personaID)

	// Apply on_llm_request hooks from subprocess plugins: they may modify the
	// system prompt (e.g. identity injection) or stop the LLM call entirely.
	if s.subPlugins != nil {
		sp, stop, err := s.applyLLMRequestHooks(event, systemPrompt, prompt)
		if err != nil {
			logger.I18nWarn("插件 on_llm_request 钩子执行失败: %v", err)
		} else {
			systemPrompt = sp
			if stop {
				logger.Debug("plugin on_llm_request hook stopped the LLM call")
				event.Stop()
				return nil
			}
		}
	}

	// on_waiting_llm_request fires right before the provider call (e.g. a
	// plugin may show a "processing" indicator).
	dispatchSubprocessHooks(s.subPlugins, event, "on_waiting_llm_request")

	// computer_use_runtime drives whether local/sandbox tools are exposed and
	// whether the local-mode hint is appended to the system prompt.
	computerUseRuntime := "local"
	if s.providerConf != nil && s.providerConf.ComputerUseRuntime != "" {
		computerUseRuntime = s.providerConf.ComputerUseRuntime
	}

	req := &provider.ProviderRequest{
		Prompt:       prompt,
		SessionID:    event.UnifiedMsgOrigin(),
		SystemPrompt: systemPrompt,
		Conversation: s.convMgr,
		Contexts:     s.conversationHistory(event.UnifiedMsgOrigin()),
	}

	providerType, _ := providerCfg["type"].(string)
	if providerType == "" {
		providerType, _ = providerCfg["provider"].(string)
	}
	if providerType == "" {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 模型提供商配置缺少 type 字段，请重新配置提供商"}}
		return nil
	}

	// Merge the provider source config (api_base/key live on the source,
	// mirroring astrbot/core/provider/manager.py get_merged_provider_config).
	mergedCfg := mergeProviderSource(providerCfg, s.config["provider_sources"])

	inst, err := provider.CreateProvider(providerType, mergedCfg, providerSettings)
	if err != nil {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 初始化模型提供商失败: " + err.Error()}}
		return nil
	}

	chatInst, ok := inst.(provider.ChatProvider)
	if !ok {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 提供商 " + providerType + " 不支持聊天能力"}}
		return nil
	}

	llmCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	// Inject active tools (built-in + MCP servers) so the model can call them.
	req.Tools = s.collectTools(computerUseRuntime)
	toolNames := make([]string, 0, len(req.Tools))
	for _, t := range req.Tools {
		if fn, ok := t["function"].(map[string]interface{}); ok {
			if name, ok := fn["name"].(string); ok {
				toolNames = append(toolNames, name)
			}
		}
	}
	logger.Debug("callLLMAgent: injecting %d tool(s): %v", len(toolNames), toolNames)

	// Computer Use "local" runtime: announce host access in the system prompt.
	switch computerUseRuntime {
	case "local":
		req.SystemPrompt += "\n" + localModePrompt(workspaceRoot(event.UnifiedMsgOrigin())) + "\n"
	case "sandbox":
		req.SystemPrompt += "\n" + sandboxModePrompt() + "\n"
	}

	// Streaming is only supported for providers that implement ChatProvider;
	// the OpenAI-compatible path covers most backends.
	streamingEnabled := false
	if s.providerConf != nil {
		streamingEnabled = s.providerConf.StreamingResponse
	}

	// System context reminder (identifier / group name / datetime), appended as
	// an extra user-content part like Python's astr_main_agent.
	if reminder := s.buildSystemReminder(event); reminder != "" {
		logger.Debug("callLLMAgent: system_reminder=%q", reminder)
		req.ExtraUserContentParts = append(req.ExtraUserContentParts, map[string]interface{}{
			"type": "text",
			"text": reminder,
		})
	}

	// Tool-call loop: up to 5 rounds of tool execution + follow-up.
	messages := append([]map[string]interface{}{}, req.Contexts...)
	messages = append(messages, req.ToUserMessage())

	streamer := newStreamSender(s, event)
	defer streamer.flush()

	resp, err := s.chatRound(llmCtx, chatInst, req, streamingEnabled, streamer)
	if err != nil {
		logger.Error("LLM call failed: %v", err)
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 LLM 调用失败: " + err.Error()}}
		return nil
	}
	s.recordProviderCall(providerCfg, event.UnifiedMsgOrigin(), resp)
	for round := 0; round < 5 && len(resp.ToolsCallName) > 0; round++ {
		// Append the assistant tool-call message
		assistantMsg := map[string]interface{}{
			"role":       "assistant",
			"content":    resp.CompletionText,
			"tool_calls": buildToolCallsMessage(resp),
		}
		messages = append(messages, assistantMsg)

		// Execute each requested tool and append tool results
		for i, name := range resp.ToolsCallName {
			args := map[string]interface{}{}
			if i < len(resp.ToolsCallArgs) {
				args = resp.ToolsCallArgs[i]
			}
			toolID := ""
			if i < len(resp.ToolsCallIDs) {
				toolID = resp.ToolsCallIDs[i]
			}
			result := s.executeTool(event, computerUseRuntime, name, args)
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolID,
				"content":      result,
			})
		}

		// Follow-up request with tool results. Each round gets its own timeout
		// so one slow round does not exhaust the whole tool-loop budget.
		req.Contexts = messages
		roundCtx, roundCancel := context.WithTimeout(llmCtx, 120*time.Second)
		resp, err = s.chatRound(roundCtx, chatInst, req, streamingEnabled, streamer)
		roundCancel()
		if err != nil {
			logger.Error("LLM tool-loop call failed: %v", err)
			event.Result = &message.MessageEventResult{}
			event.Result.Chain = []message.Component{&message.Plain{Text: "😕 LLM 调用失败: " + err.Error()}}
			return nil
		}
		s.recordProviderCall(providerCfg, event.UnifiedMsgOrigin(), resp)
	}

	if resp.Role == "err" {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 " + resp.CompletionText}}
		return nil
	}

	// Append user + assistant reply to history (Python appends the pair
	// post-completion; the user message is intentionally not in req.Contexts
	// since it is sent as the current prompt).
	if s.convMgr != nil {
		s.convMgr.AppendHistory(event.UnifiedMsgOrigin(), "user", prompt)
		s.convMgr.AppendHistory(event.UnifiedMsgOrigin(), "assistant", resp.CompletionText)
	}

	// on_llm_response fires after the LLM reply is produced (e.g. plugins that
	// capture conversation memory).
	dispatchSubprocessHooks(s.subPlugins, event, "on_llm_response")

	streamer.flush()
	if streamer.sentAny() {
		// Text was already streamed to the platform incrementally; mark the
		// event so RespondStage does not send a duplicate full message.
		event.SetExtra("streamed", true)
	}

	event.Result = &message.MessageEventResult{}
	event.Result.Chain = []message.Component{&message.Plain{Text: resp.CompletionText}}
	return nil
}

// chatRound issues a single LLM request. When streaming is enabled it consumes
// the stream channel, forwards content deltas to the platform incrementally,
// and consolidates content + tool calls into a single response.
func (s *ProcessStage) chatRound(ctx context.Context, inst provider.ChatProvider, req *provider.ProviderRequest, streaming bool, streamer *streamSender) (*provider.LLMResponse, error) {
	start := time.Now()
	if !streaming {
		resp, err := inst.TextChat(ctx, req)
		if err != nil {
			return nil, err
		}
		logger.Debug("LLM call completed in %v, text_len=%d", time.Since(start), len(resp.CompletionText))
		return resp, nil
	}
	streamCh, err := inst.TextChatStream(ctx, req)
	if err != nil {
		return nil, err
	}
	full := &provider.LLMResponse{Role: "assistant", CompletionText: "", ToolsCallName: []string{}, ToolsCallArgs: []map[string]interface{}{}, ToolsCallIDs: []string{}}
	var content, reasoning strings.Builder
	for chunk := range streamCh {
		if chunk.Role == "err" {
			// err chunk 提前返回后，生产者 goroutine 仍可能继续向缓冲为 100
			// 的 channel 写数据而被阻塞（泄漏 goroutine 与 resp.Body）。
			// 用后台 goroutine 排空剩余流，直到生产者关闭 channel；这样既能
			// 立刻返回错误，又能让生产者退出并执行其 defer（close(ch)、
			// resp.Body.Close()），避免重复 Close。
			go func() {
				for range streamCh {
				}
			}()
			return &provider.LLMResponse{Role: "err", CompletionText: chunk.CompletionText}, nil
		}
		if chunk.IsChunk {
			content.WriteString(chunk.CompletionText)
			reasoning.WriteString(chunk.ReasoningContent)
			streamer.push(chunk.CompletionText)
			continue
		}
		if len(chunk.ToolsCallName) > 0 {
			full.ToolsCallName = chunk.ToolsCallName
			full.ToolsCallArgs = chunk.ToolsCallArgs
			full.ToolsCallIDs = chunk.ToolsCallIDs
		}
		if chunk.Usage != nil {
			full.Usage = chunk.Usage
		}
		if chunk.CompletionText != "" && !chunk.IsChunk {
			// The final consolidated chunk carries the authoritative full text;
			// replace (not append to) the accumulated deltas to avoid doubling.
			content.Reset()
			content.WriteString(chunk.CompletionText)
		}
	}
	full.CompletionText = content.String()
	full.ReasoningContent = reasoning.String()
	logger.Debug("LLM call completed in %v, text_len=%d", time.Since(start), len(full.CompletionText))
	return full, nil
}

// streamSender emits streamed content.
//
// Priority:
//  1. Native stream-edit messaging (QQ C2C) — deltas are throttled into a
//     single progressively-updated message. Requires markdown permission on
//     QQ Open Platform; if the fragment call fails we fall back to #2.
//  2. Sentence segmentation — complete sentences (。！？!?；;\n) are sent as
//     separate natural messages as they form.
//
// Group chats and unsupported platforms get no incremental sends; the final
// response is delivered once by RespondStage (matches AstrBot).
type streamSender struct {
	stage     *ProcessStage
	event     *core.Event
	pending   strings.Builder
	lastFlush time.Time
	frag      platform.StreamFragmenter
	msgID     string
	sent      bool
	done      bool
	segMode   bool
	fragWarn  bool
}

func newStreamSender(stage *ProcessStage, event *core.Event) *streamSender {
	ss := &streamSender{stage: stage, event: event, lastFlush: time.Now()}
	if stage.platformMgr != nil && !event.Source.IsGroup {
		ss.frag = stage.platformMgr.GetFragmenter(event.Source.Platform)
		if ss.frag == nil {
			ss.segMode = true
		}
	}
	return ss
}

func (ss *streamSender) push(text string) {
	if text == "" {
		return
	}
	ss.pending.WriteString(text)
	if ss.frag != nil {
		// Throttle: refresh the streamed message at most ~2x/sec.
		if time.Since(ss.lastFlush) >= 500*time.Millisecond {
			ss.flushFragment(false)
		}
		return
	}
	if ss.segMode {
		ss.flushSentences()
	}
}

// flushFragment pushes the full accumulated text through the native
// stream-edit protocol (final=true also emits the state=10 end fragment).
func (ss *streamSender) flushFragment(final bool) {
	if ss.pending.Len() == 0 {
		return
	}
	text := ss.pending.String()
	ss.lastFlush = time.Now()
	if ss.msgID == "" {
		id, err := ss.frag.StreamStart(ss.event.Source.ConvID, text)
		if err != nil {
			ss.onFragFailure(err)
			return
		}
		ss.msgID = id
	} else if err := ss.frag.StreamUpdate(ss.event.Source.ConvID, ss.msgID, text); err != nil {
		ss.onFragFailure(err)
		return
	}
	ss.sent = true
	if final {
		if err := ss.frag.StreamEnd(ss.event.Source.ConvID, ss.msgID, text); err != nil {
			logger.I18nWarn("流式结束发送失败: %v", err)
		}
	}
}

// onFragFailure switches from native streaming to sentence segmentation so
// the user still gets progressive output when the platform rejects streaming.
func (ss *streamSender) onFragFailure(err error) {
	if !ss.fragWarn {
		logger.I18nWarn("原生流式不可用 (%v)，回退到按句子切分输出", err)
		ss.fragWarn = true
	}
	ss.frag = nil
	ss.segMode = true
	ss.msgID = ""
	ss.flushSentences()
}

// flushSentences sends every complete sentence that has accumulated so far.
func (ss *streamSender) flushSentences() {
	for {
		s := ss.pending.String()
		if len(s) > sentenceMaxLen {
			seg, rest := cutUTF8(s, sentenceMaxLen)
			if seg != "" {
				ss.sendSegment(seg)
				ss.pending.Reset()
				ss.pending.WriteString(rest)
				continue
			}
		}
		seg, rest := cutAtSentenceBoundary(s)
		if seg == "" {
			return
		}
		ss.sendSegment(seg)
		ss.pending.Reset()
		ss.pending.WriteString(rest)
	}
}

func (ss *streamSender) sendSegment(text string) {
	// Trim paragraph-break whitespace so segments don't render with stray
	// blank lines, and drop whitespace-only fragments entirely.
	text = strings.TrimSpace(text)
	if text == "" || ss.stage.platformMgr == nil {
		return
	}
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: text}}}
	if err := ss.stage.platformMgr.Send(ss.event.Source.Platform, ss.event.Source.ConvID, chain); err != nil {
		logger.I18nWarn("流式片段发送失败: %v", err)
		return
	}
	ss.sent = true
}

const sentenceMaxLen = 1500

// sentenceTerminators are the characters that end a natural sentence.
const sentenceTerminators = "。！？!?；;\n"

// cutAtSentenceBoundary splits s at the LAST sentence-terminating rune,
// returning the prefix (including the full terminator rune) and the remainder.
// Returns ("", s) when no boundary exists.
func cutAtSentenceBoundary(s string) (string, string) {
	idx := strings.LastIndexAny(s, sentenceTerminators)
	if idx < 0 {
		return "", s
	}
	_, size := utf8.DecodeRuneInString(s[idx:])
	if size <= 0 {
		size = 1
	}
	end := idx + size
	return s[:end], s[end:]
}

// cutUTF8 returns the longest prefix of s that is valid UTF-8 and at most max
// bytes, together with the remainder. It never splits a multi-byte rune.
func cutUTF8(s string, max int) (string, string) {
	if len(s) <= max {
		return "", s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	if cut == 0 {
		_, size := utf8.DecodeRuneInString(s)
		cut = size
	}
	return s[:cut], s[cut:]
}

func (ss *streamSender) flush() {
	if ss.done {
		return
	}
	ss.done = true
	switch {
	case ss.frag != nil:
		ss.flushFragment(true)
	case ss.segMode:
		if ss.pending.Len() > 0 {
			ss.sendSegment(ss.pending.String())
			ss.pending.Reset()
		}
	}
}

func (ss *streamSender) sentAny() bool {
	return ss.sent
}

// buildSystemReminder builds the <system_reminder> context block for the LLM
// request, mirroring Python's astr_main_agent._apply_system_context_reminder.
func (s *ProcessStage) buildSystemReminder(event *core.Event) string {
	if s.providerConf == nil {
		return ""
	}
	var parts []string
	if s.providerConf.Identifier {
		nickname := event.Source.SenderName
		if s.umoAlias != nil {
			if alias := s.umoAlias(event.UnifiedMsgOrigin()); alias != "" {
				nickname = alias
			}
		}
		parts = append(parts, fmt.Sprintf("User ID: %s, Nickname: %s", event.Source.SenderID, nickname))
	}
	if s.providerConf.GroupNameDisplay && event.Source.IsGroup {
		name := event.Source.GroupName
		if name == "" {
			name = event.Source.ConvID
		}
		parts = append(parts, fmt.Sprintf("Group name: %s", name))
	}
	if s.providerConf.DatetimeSystemPrompt {
		loc := time.Local
		if tz, ok := s.config["timezone"].(string); ok && tz != "" {
			if l, err := time.LoadLocation(tz); err == nil {
				loc = l
			}
		}
		now := time.Now().In(loc)
		parts = append(parts, fmt.Sprintf("Current datetime: %s, Weekday: %s",
			now.Format("2006-01-02 15:04 (MST)"), now.Weekday().String()))
	}
	if len(parts) == 0 {
		return ""
	}
	return "<system_reminder>\n" + strings.Join(parts, "\n") + "</system_reminder>"
}

// recordProviderCall persists an LLM call for statistics (provider_stats).
func (s *ProcessStage) recordProviderCall(providerCfg map[string]interface{}, umo string, resp *provider.LLMResponse) {
	if s.database == nil || resp == nil || resp.Role == "err" {
		return
	}
	providerID, _ := providerCfg["id"].(string)
	model, _ := providerCfg["model"].(string)
	input := 0
	output := 0
	if resp.Usage != nil {
		input = resp.Usage.InputOther + resp.Usage.InputCached
		output = resp.Usage.Output
	}
	now := float64(time.Now().UnixMilli()) / 1000
	_ = s.database.RecordProviderCall(umo, providerID, model, input, 0, output, now, now)
}

// applySkills appends the active-skills section to the system prompt.
// It mirrors Python's astr_main_agent._ensure_persona_and_skills: only active
// skills are listed, persona skill allow-lists are honored, and a runtime of
// "none" warns that Computer Use is disabled.
func (s *ProcessStage) applySkills(systemPrompt string, providerSettings map[string]interface{}, personaID string) string {
	if s.skillMgr == nil {
		return systemPrompt
	}
	runtime := "local"
	if s.providerConf != nil && s.providerConf.ComputerUseRuntime != "" {
		runtime = s.providerConf.ComputerUseRuntime
	}
	active := s.skillMgr.ListSkills(true, runtime)
	if len(active) == 0 {
		return systemPrompt
	}

	// Honor the persona's skill allow-list (nil = unrestricted).
	if s.personaSkills != nil && personaID != "" {
		allowed := s.personaSkills(personaID)
		if allowed != nil {
			if len(allowed) == 0 {
				active = nil
			} else {
				allowSet := make(map[string]bool, len(allowed))
				for _, name := range allowed {
					allowSet[name] = true
				}
				filtered := active[:0:0]
				for _, sk := range active {
					if allowSet[sk.Name] {
						filtered = append(filtered, sk)
					}
				}
				active = filtered
			}
		}
	}
	if len(active) == 0 {
		return systemPrompt
	}

	logger.Debug("callLLMAgent: injecting %d skill(s) into system prompt", len(active))
	systemPrompt += "\n" + skills.BuildSkillsPrompt(active) + "\n"
	if runtime == "none" {
		systemPrompt += "User has not enabled the Computer Use feature. " +
			"You cannot use shell or Python to perform skills. " +
			"If you need to use these capabilities, ask the user to enable Computer Use in the AstrBot WebUI -> Config.\n"
	}
	return systemPrompt
}

// buildToolCallsMessage converts LLMResponse tool calls into the OpenAI
// assistant message tool_calls structure.
func buildToolCallsMessage(resp *provider.LLMResponse) []map[string]interface{} {
	result := []map[string]interface{}{}
	for i, name := range resp.ToolsCallName {
		args := map[string]interface{}{}
		if i < len(resp.ToolsCallArgs) {
			args = resp.ToolsCallArgs[i]
		}
		argsJSON, _ := json.Marshal(args)
		id := ""
		if i < len(resp.ToolsCallIDs) {
			id = resp.ToolsCallIDs[i]
		}
		result = append(result, map[string]interface{}{
			"id":   id,
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": string(argsJSON),
			},
		})
	}
	return result
}

// dispatchSubprocessHooks runs every loaded subprocess plugin's hooks whose
// Event matches the given event name. These are pipeline-adjacent events
// (on_message, on_llm_response, on_after_message_sent, on_waiting_llm_request)
// that are not star filter handlers.
func dispatchSubprocessHooks(sub *plugin.SubprocessManager, event *core.Event, hookEvent string) {
	if sub == nil {
		return
	}
	sdkEvent := star.CoreEventToSDK(event)
	for _, inst := range sub.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != hookEvent {
				continue
			}
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			_, _, err := inst.Client.HandleHook(rpcCtx, h.Name, sdkEvent, nil)
			rpcCancel()
			if err != nil {
				logger.I18nWarn("插件 %s 钩子 %s (%s) 执行失败: %v", inst.Name, h.Name, hookEvent, err)
			}
		}
	}
}

// applyLLMRequestHooks runs every loaded subprocess plugin's on_llm_request
// hooks, letting them modify the system prompt or stop the LLM call.
func (s *ProcessStage) applyLLMRequestHooks(event *core.Event, systemPrompt, userPrompt string) (string, bool, error) {
	if s.subPlugins == nil {
		return systemPrompt, false, nil
	}
	sdkEvent := star.CoreEventToSDK(event)
	for _, inst := range s.subPlugins.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != "on_llm_request" {
				continue
			}
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			sp, stop, err := inst.Client.HandleLLMRequest(rpcCtx, h.Name, sdkEvent, systemPrompt, userPrompt)
			rpcCancel()
			if err != nil {
				logger.I18nWarn("插件 %s 的 on_llm_request 钩子 %s 执行失败: %v", inst.Name, h.Name, err)
				continue
			}
			systemPrompt = sp
			if stop {
				return systemPrompt, true, nil
			}
		}
	}
	return systemPrompt, false, nil
}

// collectPluginTools returns the OpenAI tool schemas contributed by all loaded
// subprocess plugins (each plugin's registered LLM function tools).
func (s *ProcessStage) collectPluginTools() []map[string]interface{} {
	if s.subPlugins == nil {
		return nil
	}
	var out []map[string]interface{}
	for _, inst := range s.subPlugins.List() {
		if inst.Meta == nil {
			continue
		}
		for _, t := range inst.Meta.Tools {
			params := map[string]interface{}{}
			if len(t.ParamsJson) > 0 {
				_ = json.Unmarshal(t.ParamsJson, &params)
			}
			out = append(out, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  params,
				},
			})
		}
	}
	return out
}

// executePluginTool dispatches a tool call to the subprocess plugin that
// registered a tool with the given name. Returns (result, handled).
func (s *ProcessStage) executePluginTool(event *core.Event, name string, args map[string]interface{}) (string, bool) {
	if s.subPlugins == nil {
		return "", false
	}
	sdkEvent := star.CoreEventToSDK(event)
	for _, inst := range s.subPlugins.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, t := range inst.Meta.Tools {
			if t.Name != name {
				continue
			}
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			text, isErr, err := inst.Client.HandleTool(rpcCtx, name, args, sdkEvent)
			rpcCancel()
			if err != nil {
				return fmt.Sprintf("插件工具 %s 执行失败: %v", name, err), true
			}
			if isErr {
				return "插件工具 " + name + " 返回错误: " + text, true
			}
			return text, true
		}
	}
	return "", false
}

// collectTools builds the OpenAI tool schema for all active tools
// (built-in tools + enabled MCP servers + Computer Use local tools).
func (s *ProcessStage) collectTools(computerUseRuntime string) []map[string]interface{} {
	tools := []map[string]interface{}{}

	// Built-in tools with real Go executors
	tools = append(tools, builtinTools()...)

	// Computer Use local runtime tools (shell/python/file/grep)
	if computerUseRuntime == "local" {
		tools = append(tools, collectLocalTools()...)
	} else if computerUseRuntime == "sandbox" {
		tools = append(tools, collectSandboxTools()...)
	}

	// Proactive capability: future_task tool.
	if addCronTools(s.config) {
		tools = append(tools, futureTaskToolSchema())
	}

	// MCP server tools (enabled servers from data/mcp_server.json). Loading is
	// async so a slow MCP server never blocks the LLM call.
	s.ensureMCPTools()
	for _, schema := range s.mcpSchemasSnapshot() {
		tools = append(tools, schema)
	}
	// Subprocess plugin LLM function tools.
	tools = append(tools, s.collectPluginTools()...)
	return tools
}

// loadMCPTools (re)loads enabled MCP servers from data/mcp_server.json and
// caches their tool schemas under "<sanitized_server>.<tool_name>".
// ensureMCPTools marks the schemas as loaded once (under the lock, avoiding a
// TOCTOU double load when dashboard chat and the event bus pipeline run
// concurrently) and kicks off the actual connection work in a goroutine so a
// slow MCP server never blocks the LLM call.
func (s *ProcessStage) ensureMCPTools() {
	s.mcpMu.Lock()
	if s.mcpLoaded {
		s.mcpMu.Unlock()
		return
	}
	s.mcpLoaded = true
	s.mcpMu.Unlock()
	go s.loadMCPTools()
}

// mcpSchemasSnapshot returns a shallow copy of the MCP tool schema map so the
// caller can iterate without holding the lock while loadMCPTools may be
// replacing the map.
func (s *ProcessStage) mcpSchemasSnapshot() []map[string]interface{} {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()
	schemas := make([]map[string]interface{}, 0, len(s.mcpSchemas))
	for _, schema := range s.mcpSchemas {
		schemas = append(schemas, schema)
	}
	return schemas
}

// loadMCPTools connects to enabled MCP servers and caches their tools. It runs
// in a goroutine (see ensureMCPTools) so connecting to slow servers does not
// stall the pipeline.
func (s *ProcessStage) loadMCPTools() {
	s.mcpMu.Lock()
	defer s.mcpMu.Unlock()

	data, err := os.ReadFile("data/mcp_server.json")
	if err != nil {
		s.mcpClients = nil
		s.mcpSchemas = nil
		return
	}
	var mcpCfg struct {
		McpServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &mcpCfg); err != nil {
		logger.I18nWarn("解析 data/mcp_server.json 失败: %v", err)
		s.mcpClients = nil
		s.mcpSchemas = nil
		return
	}

	// Clean up previously connected clients.
	for _, cl := range s.mcpClients {
		cl.Cleanup()
	}
	s.mcpClients = make(map[string]*agent.MCPClient)
	s.mcpSchemas = make(map[string]map[string]interface{})

	for name, cfg := range mcpCfg.McpServers {
		if active, _ := cfg["active"].(bool); !active {
			continue
		}
		safeName := sanitizeToolName(name)
		client := agent.NewMCPClient(name, cfg)
		// Use a fresh context for the connection; do NOT cancel it afterwards,
		// because the underlying SSE transport may share it for its read loop.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := client.Connect(ctx)
		cancel()
		if err != nil {
			logger.I18nWarn("MCP 服务器 %q 连接失败: %v", name, err)
			continue
		}
		s.mcpClients[safeName] = client
		for _, tool := range client.Tools() {
			fullName := safeName + "." + sanitizeToolName(tool.Name)
			schema := map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        fullName,
					"description": "MCP 服务器工具（" + name + "）: " + tool.Description,
					"parameters":  tool.InputSchema,
				},
			}
			s.mcpSchemas[fullName] = schema
		}
		logger.Debug("MCP server %q connected (%d tools)", name, len(client.Tools()))
	}
	s.mcpLoaded = true
}

// executeMCPTool dispatches a tool call to the matching MCP server. The tool
// name format is "<sanitized_server>.<tool_name>". Returns ("", false) when
// the name is not an MCP tool.
func (s *ProcessStage) executeMCPTool(ctx context.Context, name string, args map[string]interface{}) (string, bool) {
	dot := strings.IndexByte(name, '.')
	if dot <= 0 || dot == len(name)-1 {
		return "", false
	}
	serverName := name[:dot]
	toolName := name[dot+1:]

	s.mcpMu.Lock()
	client := s.mcpClients[serverName]
	s.mcpMu.Unlock()
	if client == nil {
		return fmt.Sprintf("MCP 工具 %s 执行失败: 服务器 %q 未连接", name, serverName), true
	}
	logger.Debug("executeMCPTool: server=%s tool=%s", serverName, toolName)
	// Bound the tool call: the SSE transport waits for a response event. A
	// short first-attempt timeout lets a stale connection fail fast so the
	// reconnect path (below) kicks in instead of hanging the pipeline.
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := client.CallTool(callCtx, toolName, args)
	if err != nil {
		// A transport failure (e.g. SSE connection lost) can leave the client
		// unable to receive responses; reconnect once and retry with a fresh
		// timeout.
		logger.I18nWarn("MCP 工具 %s 调用失败 (%v)，正在重连并重试…", name, err)
		reconnCtx, reconnCancel := context.WithTimeout(ctx, 30*time.Second)
		rc := client.Reconnect(reconnCtx)
		reconnCancel()
		if rc != nil {
			logger.I18nWarn("MCP 服务器 %q 重连失败: %v", serverName, rc)
		} else {
			retryCtx, retryCancel := context.WithTimeout(ctx, 60*time.Second)
			result, err = client.CallTool(retryCtx, toolName, args)
			retryCancel()
			if err != nil {
				return fmt.Sprintf("MCP 工具 %s 执行失败: %v", name, err), true
			}
		}
	}
	if err != nil {
		return fmt.Sprintf("MCP 工具 %s 执行失败: %v", name, err), true
	}
	if result.IsError {
		return fmt.Sprintf("MCP 工具 %s 返回错误: %s", name, mcpContentText(result.Content)), true
	}
	text := mcpContentText(result.Content)
	if text == "" {
		return fmt.Sprintf("MCP 工具 %s 执行成功（无文本内容）", name), true
	}
	logger.Debug("executeMCPTool: tool %s returned text_len=%d", name, len(text))
	return text, true
}

// mcpContentText extracts the textual content from an MCP tool call result,
// joining text-type blocks with newlines.
func mcpContentText(content []map[string]interface{}) string {
	var parts []string
	for _, block := range content {
		blockType, _ := block["type"].(string)
		switch blockType {
		case "text":
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		default:
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// sanitizeToolName replaces characters invalid for OpenAI tool names
// (^[a-zA-Z0-9_-]+$) with underscores.
func sanitizeToolName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}

// builtinTools returns the OpenAI tool schemas for built-in tools that have
// real Go executors in executeTool. These are always available regardless of
// the Computer Use runtime.
func builtinTools() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "get_current_time",
				"description": "获取当前日期和时间。可选指定 IANA 时区（如 Asia/Shanghai、UTC），缺省使用本地时区。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"timezone": map[string]interface{}{
							"type":        "string",
							"description": "IANA 时区名，如 Asia/Shanghai",
						},
					},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]interface{}{
				"name":        "web_fetch",
				"description": "抓取指定 URL 的网页内容并返回纯文本（自动去除 HTML 标签）。用于联网查询资料、查看网页。",
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"url": map[string]interface{}{
							"type":        "string",
							"description": "要抓取的完整 URL（含 http:// 或 https://）",
						},
						"max_length": map[string]interface{}{
							"type":        "integer",
							"description": "返回的最大字符数，默认 20000",
						},
					},
					"required": []interface{}{"url"},
				},
			},
		},
	}
}

// executeBuiltinTool runs a built-in tool by name and returns the result text.
// Returns (result, handled); handled=false means the name is not a built-in.
func executeBuiltinTool(name string, args map[string]interface{}) (string, bool) {
	switch name {
	case "get_current_time":
		return executeGetCurrentTime(argString(args, "timezone")), true
	case "web_fetch":
		return executeWebFetch(argString(args, "url"), argInt(args, "max_length", 20000)), true
	}
	return "", false
}

// executeGetCurrentTime formats the current time, optionally in a given IANA
// timezone.
func executeGetCurrentTime(timezone string) string {
	now := time.Now()
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.Local
	}
	return now.In(loc).Format("2006-01-02 15:04:05") + " " + loc.String()
}

// executeWebFetch fetches a URL and converts its content to plain text,
// truncated to maxLength characters.
func executeWebFetch(rawURL string, maxLength int) string {
	if rawURL == "" {
		return "web_fetch 错误: 缺少 url 参数"
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "web_fetch 错误: url 必须以 http:// 或 https:// 开头"
	}
	if maxLength <= 0 {
		maxLength = 20000
	}
	if maxLength > 200000 {
		maxLength = 200000
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(rawURL)
	if err != nil {
		return fmt.Sprintf("web_fetch 错误: 请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("web_fetch 错误: HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, int64(maxLength*2))
	body, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Sprintf("web_fetch 错误: 读取失败: %v", err)
	}

	text := string(body)
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		text = htmlToText(body)
	}
	text = strings.TrimSpace(text)
	if len(text) > maxLength {
		text = text[:maxLength]
	}
	if text == "" {
		return "web_fetch 结果: 页面无文本内容"
	}
	return text
}

// htmlToText strips HTML tags/entities, collapsing runs of whitespace into a
// single space.
func htmlToText(body []byte) string {
	// Drop script/style blocks first so their content is not surfaced.
	s := string(body)
	s = tagBlockRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = whitespaceRe.ReplaceAllString(s, " ")
	return s
}

// executeTool runs a tool call and returns the result text.
// Dispatches to built-in tools, MCP servers, and the Computer Use local or
// sandbox executors.
func (s *ProcessStage) executeTool(event *core.Event, runtime, name string, args map[string]interface{}) string {
	umo := event.UnifiedMsgOrigin()
	logger.Debug("executeTool: name=%s args=%v", name, args)

	result := ""
	handled := false
	if runtime == "sandbox" {
		if r, h := s.executeSandboxTool(name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		if r, h := executeBuiltinTool(name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		if r, h := s.executeMCPTool(context.Background(), name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		if r, h := s.executePluginTool(event, name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		switch name {
		case "astrbot_execute_shell":
			result = executeLocalShell(umo, argString(args, "command"), argBool(args, "background"), argInt(args, "timeout", 300))
		case "astrbot_shell_session":
			result = executeShellSession(umo, argString(args, "action"), argString(args, "session_id"), argString(args, "data"))
		case "astrbot_execute_python":
			result = executeLocalPython(umo, argString(args, "code"), argInt(args, "timeout", 30))
		case "astrbot_file_read_tool":
			result = executeFileRead(argString(args, "path"), umo, argInt(args, "offset", 0), argInt(args, "limit", 0))
		case "astrbot_file_write_tool":
			result = executeFileWrite(argString(args, "path"), argString(args, "content"), umo)
		case "astrbot_file_edit_tool":
			result = executeFileEdit(argString(args, "path"), argString(args, "old"), argString(args, "new"), argBool(args, "replace_all"), umo)
		case "astrbot_grep_tool":
			result = executeGrep(argString(args, "pattern"), argString(args, "path"), argString(args, "glob"), argInt(args, "result_limit", 100), umo)
		case "future_task":
			result = executeFutureTask(s.cronMgr, umo, event.GetSenderID(), args)
		default:
			result = fmt.Sprintf("工具 %s 执行失败: 该工具尚未实现 Go 端执行器", name)
		}
	}
	logger.Debug("tool %s result: %.200s", name, result)
	return result
}

// addCronTools reports whether the proactive future_task tool should be
// injected (provider_settings.proactive_capability.add_cron_tools; absent key
// defaults to true).
func addCronTools(config map[string]interface{}) bool {
	ps := bindProviderSettings(config)
	if ps == nil || ps.Proactive.AddCronTools == nil {
		return true
	}
	return *ps.Proactive.AddCronTools
}

// executeSandboxTool routes computer-use tools into the sandbox runtime.
func (s *ProcessStage) executeSandboxTool(name string, args map[string]interface{}) (string, bool) {
	if s.sandboxMgr == nil {
		return "Sandbox manager not configured.", true
	}
	ctx := context.Background()
	timeout := argInt(args, "timeout", 0)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
		defer cancel()
	}
	switch name {
	case "astrbot_execute_shell":
		if err := s.ensureSandboxStarted(ctx); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxShell(ctx, s.sandboxMgr, argString(args, "command")), true
	case "astrbot_execute_python":
		if err := s.ensureSandboxStarted(ctx); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxPython(ctx, s.sandboxMgr, argString(args, "code")), true
	case "astrbot_file_read_tool":
		if err := s.ensureSandboxStarted(ctx); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxFileRead(ctx, s.sandboxMgr, argString(args, "path")), true
	case "astrbot_file_write_tool":
		if err := s.ensureSandboxStarted(ctx); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxFileWrite(ctx, s.sandboxMgr, argString(args, "path"), argString(args, "content")), true
	case "astrbot_file_edit_tool":
		if err := s.ensureSandboxStarted(ctx); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxFileEdit(ctx, s.sandboxMgr, argString(args, "path"), argString(args, "old"), argString(args, "new"), argBool(args, "replace_all")), true
	case "astrbot_grep_tool":
		if err := s.ensureSandboxStarted(ctx); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxGrep(ctx, s.sandboxMgr, argString(args, "pattern"), argString(args, "path")), true
	}
	return "", false
}

// ensureSandboxStarted lazily starts the sandbox booter on first use.
func (s *ProcessStage) ensureSandboxStarted(ctx context.Context) error {
	if s.sandboxMgr.IsRunning() {
		return nil
	}
	if err := s.sandboxMgr.Start(ctx); err != nil {
		return err
	}
	if s.skillMgr != nil {
		_ = s.sandboxMgr.SyncSkills(ctx)
	}
	return nil
}

// resolveProvider picks the provider config to use for this chat.
func (s *ProcessStage) resolveProvider() (map[string]interface{}, map[string]interface{}, error) {
	providers, _ := s.config["provider"].([]interface{})
	providerSettings, _ := s.config["provider_settings"].(map[string]interface{})
	if providerSettings == nil {
		providerSettings = map[string]interface{}{}
	}

	selected := ""
	if s.providerConf != nil {
		selected = s.providerConf.DefaultProviderID
	}

	if selected != "" {
		for _, p := range providers {
			pc, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if id, _ := pc["id"].(string); id == selected {
				return pc, providerSettings, nil
			}
		}
	}
	// Fallback: first enabled provider
	for _, p := range providers {
		pc, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if enable, _ := pc["enable"].(bool); enable {
			return pc, providerSettings, nil
		}
	}
	return nil, nil, fmt.Errorf("未找到可用的模型提供商，请先配置")
}

// personaPrompt extracts the persona system prompt from settings.
func personaPrompt(settings map[string]interface{}) string {
	if p, ok := settings["persona"].(string); ok {
		return p
	}
	return ""
}

// conversationHistory converts conversation history to LLM context messages.
// The history slice is shallow-copied so a concurrent AppendHistory (from
// another message racing the same session) can never mutate the slice the LLM
// is reading. When provider_settings.max_context_length is set (>0), the
// history is truncated to the most recent 2*max_context_length entries,
// keeping user/assistant message pairs together, and a short system hint notes
// that older history was dropped.
func (s *ProcessStage) conversationHistory(umo string) []map[string]interface{} {
	if s.convMgr == nil {
		return nil
	}
	conv := s.convMgr.GetConversation(umo)
	if conv == nil {
		return nil
	}
	history := append([]map[string]interface{}{}, conv.History...)

	maxCtx := 0
	if ps, ok := s.config["provider_settings"].(map[string]interface{}); ok {
		if v, ok := ps["max_context_length"].(float64); ok && v > 0 {
			maxCtx = int(v)
		}
	}
	if maxCtx > 0 && len(history) > maxCtx*2 {
		// Keep only the most recent 2*max_context_length entries, aligned to an
		// even boundary so a user/assistant pair is never split.
		start := len(history) - maxCtx*2
		if start%2 != 0 {
			start++
		}
		history = append([]map[string]interface{}{}, history[start:]...)
		history = append(history, map[string]interface{}{
			"role":    "system",
			"content": "注意：由于对话上下文长度限制，更早的历史消息已被截断。",
		})
	}
	return history
}

// mergeProviderSource overlays the provider source config onto a provider config
// (source provides api_base/key etc.; provider values win; id stays the provider's).
// Ported from astrbot/core/provider/manager.py get_merged_provider_config.
func mergeProviderSource(pc map[string]interface{}, sourcesRaw interface{}) map[string]interface{} {
	sourceID, _ := pc["provider_source_id"].(string)
	if sourceID == "" {
		return pc
	}
	sources, _ := sourcesRaw.([]interface{})
	var source map[string]interface{}
	for _, s := range sources {
		if sm, ok := s.(map[string]interface{}); ok {
			if id, _ := sm["id"].(string); id == sourceID {
				source = sm
				break
			}
		}
	}
	if source == nil {
		return pc
	}
	merged := map[string]interface{}{}
	for k, v := range source {
		merged[k] = v
	}
	for k, v := range pc {
		merged[k] = v
	}
	return merged
}

// ---------------------------------------------------------------------------
// Stage 8: ResultDecorateStage
// ---------------------------------------------------------------------------

// ResultDecorateStage applies decorations to the result (prefix, split, at, quote).
// Ported from astrbot/core/pipeline/result_decorate/stage.py
type ResultDecorateStage struct {
	replyPrefix      string
	replyWithMention bool
	replyWithQuote   bool
	maxSegmentLength int
	subPlugins       *plugin.SubprocessManager
}

func NewResultDecorateStage() *ResultDecorateStage {
	return &ResultDecorateStage{}
}

func (s *ResultDecorateStage) Name() string { return "result_decorate" }

func (s *ResultDecorateStage) Initialize(ctx *PipelineContext) error {
	ps := bindPlatformSettings(ctx.AstrbotConfig)
	s.replyPrefix = ps.ReplyPrefix
	s.replyWithMention = ps.ReplyWithMention
	s.replyWithQuote = ps.ReplyWithQuote
	s.subPlugins = ctx.SubPlugins
	return nil
}

func (s *ResultDecorateStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if event.Result == nil || len(event.Result.Chain) == 0 {
		return &StageResult{Continue: true}, nil
	}

	// Apply reply prefix
	if s.replyPrefix != "" {
		for i, comp := range event.Result.Chain {
			if plain, ok := comp.(*message.Plain); ok {
				event.Result.Chain[i] = &message.Plain{Text: s.replyPrefix + plain.Text}
				break
			}
		}
	}

	// Apply @mention (only for group messages)
	if s.replyWithMention && event.Source.IsGroup {
		// Insert At component at the beginning
		at := &message.At{TargetID: event.Source.SenderID, Name: event.Source.SenderName}
		newChain := make([]message.Component, 0, len(event.Result.Chain)+1)
		newChain = append(newChain, at)
		// Add newline after @
		if len(event.Result.Chain) > 0 {
			if plain, ok := event.Result.Chain[0].(*message.Plain); ok {
				event.Result.Chain[0] = &message.Plain{Text: "\n" + plain.Text}
			}
		}
		newChain = append(newChain, event.Result.Chain...)
		event.Result.Chain = newChain
	}

	// Apply reply quote
	if s.replyWithQuote && event.MessageObj != nil && event.MessageObj.MessageID != "" {
		reply := &message.Reply{MessageID: event.MessageObj.MessageID}
		newChain := make([]message.Component, 0, len(event.Result.Chain)+1)
		newChain = append(newChain, reply)
		newChain = append(newChain, event.Result.Chain...)
		event.Result.Chain = newChain
	}

	// Run subprocess plugin on_decorating_result hooks (may rewrite the chain,
	// e.g. message transforms) and stop the pipeline if requested.
	if s.subPlugins != nil && len(event.Result.Chain) > 0 {
		sdkChain := chainToSDK(event.Result.Chain)
		stop, err := s.applyResultHooks(event, &sdkChain)
		if err != nil {
			logger.I18nWarn("插件 on_decorating_result 钩子执行失败: %v", err)
		}
		if len(sdkChain) > 0 {
			event.Result.Chain = plugin.ComponentsFromSDK(sdkChain)
		}
		if stop {
			return &StageResult{Continue: false}, nil
		}
	}

	return &StageResult{Continue: true}, nil
}

// applyResultHooks runs every loaded subprocess plugin's on_decorating_result
// hooks against the outgoing chain.
func (s *ResultDecorateStage) applyResultHooks(event *core.Event, chain *[]pluginsdk.Component) (bool, error) {
	sdkEvent := star.CoreEventToSDK(event)
	cur := *chain
	for _, inst := range s.subPlugins.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != "on_decorating_result" && h.Event != "on_result_handling" {
				continue
			}
			hookName := h.Name
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			newChain, stop, err := inst.Client.HandleHook(rpcCtx, hookName, sdkEvent, cur)
			rpcCancel()
			if err != nil {
				logger.I18nWarn("插件 %s 的结果钩子 %s 执行失败: %v", inst.Name, hookName, err)
				continue
			}
			cur = newChain
			if stop {
				return true, nil
			}
		}
	}
	*chain = cur
	return false, nil
}

// chainToSDK converts a host result chain into SDK components for hook RPCs.
func chainToSDK(chain []message.Component) []pluginsdk.Component {
	out := make([]pluginsdk.Component, 0, len(chain))
	for _, c := range chain {
		if c == nil {
			continue
		}
		comp := pluginsdk.Component{Type: pluginsdk.ComponentType(c.Type())}
		switch v := c.(type) {
		case *message.Plain:
			comp.Text = v.Text
		case *message.At:
			comp.TargetID = v.TargetID
			comp.Name = v.Name
		case *message.Image:
			comp.URL = v.URL
			comp.Path = v.Path
			comp.File = v.File
			comp.Base64 = v.Base64
			comp.FileID = v.FileID
		case *message.Record:
			comp.URL = v.URL
			comp.Path = v.Path
			comp.File = v.File
			comp.FileID = v.FileID
		case *message.File:
			comp.URL = v.URL
			comp.Path = v.Path
			comp.FileID = v.FileID
			comp.Name = v.Name
		case *message.Video:
			comp.URL = v.URL
			comp.Path = v.Path
			comp.FileID = v.FileID
		case *message.Face:
			comp.ID = v.ID
		case *message.Emoji:
			comp.ID = v.ID
			comp.URL = v.URL
		case *message.Json:
			comp.Data = v.Data
		case *message.Reply:
			comp.ID = v.MessageID
			comp.Text = v.MessageStr
		}
		out = append(out, comp)
	}
	return out
}

// ---------------------------------------------------------------------------
// Stage 9: RespondStage
// ---------------------------------------------------------------------------

// RespondStage sends the result message chain to the platform.
// Ported from astrbot/core/pipeline/respond/stage.py
type RespondStage struct {
	platformMgr *platform.PlatformManager
	subPlugins  *plugin.SubprocessManager
}

func NewRespondStage() *RespondStage {
	return &RespondStage{}
}

func (s *RespondStage) Name() string { return "respond" }

func (s *RespondStage) Initialize(ctx *PipelineContext) error {
	s.platformMgr = ctx.PlatformMgr
	s.subPlugins = ctx.SubPlugins
	return nil
}

func (s *RespondStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Content was already streamed to the platform incrementally by
	// ProcessStage; skip the duplicate final send.
	if streamed, _ := event.GetExtra("streamed").(bool); streamed {
		return &StageResult{Continue: false}, nil
	}

	if event.Result == nil || len(event.Result.Chain) == 0 {
		return &StageResult{Continue: false}, nil
	}

	// Validate components: skip empty Plain
	validChain := make([]message.Component, 0, len(event.Result.Chain))
	for _, comp := range event.Result.Chain {
		if plain, ok := comp.(*message.Plain); ok {
			if strings.TrimSpace(plain.Text) == "" {
				continue
			}
		}
		validChain = append(validChain, comp)
	}

	if len(validChain) == 0 {
		logger.Debug("Respond: no valid components to send")
		return &StageResult{Continue: false}, nil
	}

	// Send via platform manager
	if s.platformMgr != nil {
		chain := event.Result.ToMessageChain()
		chain.Chain = validChain
		err := s.platformMgr.Send(event.Source.Platform, event.Source.ConvID, chain)
		if err != nil {
			logger.Error("Failed to send message chain: %v", err)
		} else if s.subPlugins != nil {
			// on_after_message_sent fires after a reply is delivered (e.g.
			// plugins that clean up pending state or react to sent messages).
			dispatchSubprocessHooks(s.subPlugins, event, "on_after_message_sent")
		}
	}

	// Clear the result
	event.Result = nil
	return &StageResult{Continue: false}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// extractPlainText extracts all plain text from a message chain.
func extractPlainText(mc *message.MessageChain) string {
	if mc == nil {
		return ""
	}
	var sb strings.Builder
	for _, comp := range mc.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			sb.WriteString(c.Text)
		case *message.At:
			if c.Name != "" {
				sb.WriteString("@" + c.Name)
			} else {
				sb.WriteString("@" + c.TargetID)
			}
		case *message.Reply:
			if c.Chain != nil {
				// Reply.Chain is []Component, not *MessageChain
				for _, rc := range c.Chain {
					if plain, ok := rc.(*message.Plain); ok {
						sb.WriteString(plain.Text)
					}
				}
			}
		}
	}
	return sb.String()
}

// normalizeImagePath converts file:// URIs to local paths.
func normalizeImagePath(img *message.Image) {
	if img.File == "" {
		return
	}
	if strings.HasPrefix(img.File, "file://") {
		img.File = strings.TrimPrefix(img.File, "file://")
	}
}

// DurationFromSeconds creates a duration from seconds.
func DurationFromSeconds(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}

// BuildDefaultPipeline creates the default 9-stage pipeline matching Python.
func BuildDefaultPipeline(pctx *PipelineContext) ([]PipelineStage, error) {
	stages := []PipelineStage{
		NewWakingCheckStage(),
		NewWhitelistCheckStage(),
		NewSessionStatusCheckStage(),
		NewRateLimitStage(),
		NewContentSafetyCheckStage(),
		NewPreProcessStage(),
		NewProcessStage(),
		NewResultDecorateStage(),
		NewRespondStage(),
	}
	for _, s := range stages {
		if err := s.Initialize(pctx); err != nil {
			return nil, err
		}
	}
	return stages, nil
}

// Pipeline orchestrates stages.
type Pipeline struct {
	stages []PipelineStage
	mu     sync.RWMutex
}

// NewPipeline creates a pipeline.
func NewPipeline() *Pipeline {
	return &Pipeline{}
}

// SetStages replaces all stages.
func (p *Pipeline) SetStages(stages []PipelineStage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stages = stages
}

// Process runs the event through all stages.
func (p *Pipeline) Process(ctx context.Context, event *core.Event) error {
	p.mu.RLock()
	defer p.mu.RUnlock()

	for _, stage := range p.stages {
		result, err := stage.Process(ctx, event)
		if err != nil {
			return err
		}
		if result != nil && !result.Continue {
			return nil
		}
		if event.IsStopped() {
			logger.Debug("Stage %s stopped event propagation", stage.Name())
			return nil
		}
	}
	return nil
}
