// Package pipeline implements the message processing stages.
// Ported from astrbot/core/pipeline/
//
// The pipeline processes events through 10 ordered stages:
//  1. SessionWaitStage      - Feed events to session-waiting plugins (SessionWaiter)
//  2. WakingCheckStage      - Check wake conditions
//  3. WhitelistCheckStage   - Check whitelist/blacklist
//  4. SessionStatusCheckStage - Check session enabled
//  5. RateLimitStage         - Check rate limit
//  6. ContentSafetyCheckStage - Check content safety
//  7. PreProcessStage        - Preprocess media, STT, path mapping
//  8. ProcessStage           - Plugin handler execution + LLM agent
//  9. ResultDecorateStage    - Decorate result (prefix, T2I, TTS, etc.)
//  10. RespondStage           - Send message chain to platform
package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"math"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- #nosec G404: 仅用于 emoji 随机选择、TTS 触发概率与延时抖动（非安全场景）
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/agent"
	"github.com/WaterGodFurina/Astrbot-golang/internal/contentsafety"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/cron"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/knowledgebase"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/ratelimit"
	"github.com/WaterGodFurina/Astrbot-golang/internal/sandbox"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"github.com/WaterGodFurina/Astrbot-golang/internal/t2i"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("Pipeline")

// pluginRPCTimeout bounds every gRPC call into a subprocess plugin so a hung
// plugin handler (infinite loop, deadlock) cannot freeze the pipeline forever.
const pluginRPCTimeout = 30 * time.Second

// errNoAvailableProvider 表示配置中没有可用的模型提供商。平台侧静默处理
// （不回复用户），宿主启动时会打印一条 warn 提示（见 lifecycle）。
var errNoAvailableProvider = errors.New("未找到可用的模型提供商，请先配置")

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
	// KBManager provides knowledge-base retrieval for context injection.
	// Optional.
	KBManager *knowledgebase.Manager
	// KBRetriever resolves KB context text for a prompt (umo, query) -> formatted
	// reference text. When set it overrides KBManager for injection.
	// Optional.
	KBRetriever func(umo, query string) (string, error)
	// ProviderManager resolves chat/STT/TTS/embedding providers.
	// Optional.
	ProviderManager *provider.ProviderManager
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

	// Ported from astrbot/core/pipeline/waking_check/stage.py
	ignoreBotSelfMessage         bool
	uniqueSession                bool
	emptyMentionWaiting          bool
	emptyMentionWaitingNeedReply bool

	// adminsID lists the configured admin sender ids (config "admins_id").
	// Senders in this list get event.Role="admin" (mirrors Python's waking_check
	// admin detection); everyone else is "member".
	adminsID []string

	// umoAutoNameRecorder 对齐 Python v4.28.0 (#9909)：唤醒成功时异步记录
	// UMO 可读名称（群名/用户名）到数据库，供 WebUI 展示。
	umoAutoNameRecorder *UmoAutoNameRecorder
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
	s.ignoreBotSelfMessage = ps.IgnoreBotSelfMessage
	s.uniqueSession = ps.UniqueSession
	s.emptyMentionWaiting = ps.EmptyMentionWaiting
	s.emptyMentionWaitingNeedReply = ps.EmptyMentionWaitingNeedReply
	if ps.CmdPrefix != "" {
		s.cmdPrefix = ps.CmdPrefix
	}

	// AI wake word: provider_settings.wake_prefix (e.g. "ai").
	// When set, LLM chat requires "<prefix><ai word> <text>".
	if psAI := bindProviderSettings(ctx.AstrbotConfig); psAI != nil {
		s.aiWakePrefix = strings.TrimSpace(psAI.WakePrefix)
	}

	// Admin ids (top-level "admins_id", e.g. ["astrbot"]). Used to mark the
	// sender as admin so admin-gated commands (/provider /name) respond.
	s.adminsID = toStringList(ctx.AstrbotConfig["admins_id"])

	// 对齐 Python v4.28.0 (#9909)：唤醒成功时异步记录 UMO 可读名称到数据库。
	if ctx.Database != nil {
		s.umoAutoNameRecorder = NewUmoAutoNameRecorder(ctx.Database, "")
	}

	logger.Debug("WakingCheck initialized: prefixes=%v, nicknames=%v, wakeByAt=%v, wakeByPrefix=%v, wakeByFriend=%v, aiWakePrefix=%q",
		s.wakePrefixes, s.nickname, s.wakeByAt, s.wakeByPrefix, s.wakeByFriend, s.aiWakePrefix)
	return nil
}

func (s *WakingCheckStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Ported from waking_check/stage.py: 设置 sender 身份。配置在
	// admins_id 中的发送者标记为 admin，其余为 member；webchat 的 API key
	// 可通过 _api_key_allow_admin_role 显式关闭管理员身份。
	event.Role = "member"
	apiKeyAllowAdminRole := true
	if v := event.GetExtra("_api_key_allow_admin_role"); v != nil {
		if b, isBool := v.(bool); isBool {
			apiKeyAllowAdminRole = b
		}
	}
	// 平台已标记管理员（如 QQ 群 owner/admin member_role）时直接保留，
	// 不依赖 admins_id 配置。
	if apiKeyAllowAdminRole && event.Source.IsAdmin {
		event.Role = "admin"
	}
	if apiKeyAllowAdminRole {
		for _, adminID := range s.adminsID {
			if event.Source.SenderID == adminID {
				event.Role = "admin"
				break
			}
		}
	}
	event.Source.IsAdmin = event.Role == "admin"

	// If the event already has is_at_or_wake_command set, skip
	if event.IsAtOrWakeCommand {
		return &StageResult{Continue: true}, nil
	}

	// Apply unique session: in group chats each member gets an isolated
	// conversation id (ported from build_unique_session_id).
	if s.uniqueSession && event.Source.IsGroup {
		if sid := buildUniqueSessionID(event.Source.Platform, event.Source.SenderID, event.Source.ConvID); sid != "" {
			event.Source.ConvID = sid
			logger.Debug("WakingCheck: unique session applied, conv=%s", sid)
		}
	}

	// Ignore bot self messages (ported from waking_check/stage.py).
	if s.ignoreBotSelfMessage && event.Source.SelfID != "" &&
		event.Source.SelfID == event.Source.SenderID {
		event.Stop()
		logger.Debug("WakingCheck: ignored bot self message")
		return &StageResult{Continue: false}, nil
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
				// A message that is only the wake prefix (e.g. "/") triggers
				// the empty-mention flow (ported from builtin_stars/astrbot/main.py).
				if event.WakeCommand == "" && s.emptyMentionWaiting {
					return s.applyEmptyMention(event)
				}
				// 对齐 Python v4.28.0 (#9909)：唤醒成功，异步记录 UMO 名称。
				s.scheduleUmoAutoName(event)
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
				// 对齐 Python v4.28.0 (#9909)：唤醒成功，异步记录 UMO 名称。
				s.scheduleUmoAutoName(event)
				return &StageResult{Continue: true}, nil
			}
		}
	}

	// Check @mention / @all / reply-quoting-bot (Python parity)
	if s.wakeByAt && event.Message != nil {
		for _, comp := range event.Message.Chain {
			if at, ok := comp.(*message.At); ok {
				if at.TargetID != event.Source.SelfID {
					logger.Debug("WakingCheck: @ mention target=%q != selfID=%q platform=%s conv=%s",
						at.TargetID, event.Source.SelfID, event.Source.Platform, event.Source.ConvID)
				}
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
						if s.emptyMentionWaiting && isSingleEmptyMention(event) {
							return s.applyEmptyMention(event)
						}
						event.SetExtra("llm_wake", true)
						logger.Debug("Woken by @mention (chat enabled)")
					}
					// 对齐 Python v4.28.0 (#9909)：唤醒成功，异步记录 UMO 名称。
					s.scheduleUmoAutoName(event)
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
				// 对齐 Python v4.28.0 (#9909)：唤醒成功，异步记录 UMO 名称。
				s.scheduleUmoAutoName(event)
				return &StageResult{Continue: true}, nil
			}
			if r, ok := comp.(*message.Reply); ok && !event.Source.IsGroup {
				// quoting the bot in a private chat wakes it (Python parity)
				if r.SenderID == event.Source.SelfID {
					event.IsAtOrWakeCommand = true
					event.SetExtra("llm_wake", true)
					// 对齐 Python v4.28.0 (#9909)：唤醒成功，异步记录 UMO 名称。
					s.scheduleUmoAutoName(event)
					return &StageResult{Continue: true}, nil
				}
			}
		}
	}

	// Friend messages wake (configurable)
	if s.wakeByFriend && !event.Source.IsGroup {
		event.IsAtOrWakeCommand = true
		event.SetExtra("llm_wake", true)
		// 对齐 Python v4.28.0 (#9909)：唤醒成功，异步记录 UMO 名称。
		s.scheduleUmoAutoName(event)
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

// emptyMentionPrompt instructs the LLM to greet the user when they only
// mentioned the bot without content (mirrors builtin_stars/astrbot/main.py).
const emptyMentionPrompt = "注意，你正在社交媒体上中与用户进行聊天，用户只是通过@来唤醒你，但并未在这条消息中输入内容，他可能会在接下来一条发送他想发送的内容。" +
	"你友好地询问用户想要聊些什么或者需要什么帮助，回复要符合人设，不要太过机械化。" +
	"请注意，你仅需要输出要回复用户的内容，不要输出其他任何东西"

// isSingleEmptyMention reports whether the message chain contains only a
// single @-mention of the bot (Python: len(messages)==1 and At self).
func isSingleEmptyMention(event *core.Event) bool {
	if event.Message == nil || len(event.Message.Chain) != 1 {
		return false
	}
	at, ok := event.Message.Chain[0].(*message.At)
	return ok && at.TargetID != "" && at.TargetID == event.Source.SelfID
}

// applyEmptyMention handles a message that only mentions the bot (or only
// carries a wake prefix) without any content. When empty_mention_waiting_need_reply
// is enabled the LLM is asked to greet the user; otherwise the event is stopped.
// scheduleUmoAutoName 对齐 Python v4.28.0 (#9909)：唤醒成功时异步记录 UMO
// 可读名称到数据库。nil-safe。
func (s *WakingCheckStage) scheduleUmoAutoName(event *core.Event) {
	if s.umoAutoNameRecorder != nil {
		s.umoAutoNameRecorder.Schedule(event)
	}
}

func (s *WakingCheckStage) applyEmptyMention(event *core.Event) (*StageResult, error) {
	if !s.emptyMentionWaitingNeedReply {
		event.Stop()
		logger.Debug("WakingCheck: empty mention ignored (empty_mention_waiting_need_reply=false)")
		return &StageResult{Continue: false}, nil
	}
	event.PlainText = emptyMentionPrompt
	event.MessageStr = emptyMentionPrompt
	event.SetExtra("llm_wake", true)
	// 对齐 Python v4.28.0 (#9909)：空提及唤醒成功，异步记录 UMO 名称。
	s.scheduleUmoAutoName(event)
	logger.Debug("WakingCheck: empty mention -> LLM greeting")
	return &StageResult{Continue: true}, nil
}

// buildUniqueSessionID constructs the per-member session id for group chats
// when unique_session is enabled (ported from waking_check/stage.py
// UNIQUE_SESSION_ID_BUILDERS).
func buildUniqueSessionID(platform, senderID, groupID string) string {
	switch platform {
	case "aiocqhttp", "slack":
		return senderID + "_" + groupID
	case "dingtalk":
		return senderID
	// qq_official / qq_official_webhook: 对齐 Python v4.28.0 (#9814)，
	// 群聊场景拼接 senderID_groupID 实现按成员隔离。
	case "qq_official", "qq_official_webhook":
		return senderID + "_" + groupID
	case "lark":
		return senderID + "%" + groupID
	case "misskey":
		return groupID + "_" + senderID
	case "matrix":
		if groupID != "" {
			return senderID + "_" + groupID
		}
		return senderID
	default:
		// telegram / discord / webchat have no builder (None in Python).
		return ""
	}
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
				delays := 0
				if v, ok := event.GetExtra(rateLimitDelaysKey).(int); ok {
					delays = v
				}
				// Bound the re-queues: under sustained traffic an event must
				// not be re-delayed forever (queue starvation/growth).
				if delays >= rateLimitMaxDelays {
					logger.I18nWarn("会话 %s 限流: 事件 %q 已延迟重排队 %d 次，超过上限 %d，丢弃",
						sessionID, event.MessageStr, delays, rateLimitMaxDelays)
					event.Stop()
					return &StageResult{Continue: false}, nil
				}
				event.SetExtra(rateLimitDelaysKey, delays+1)
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

// rateLimitDelaysKey is the Event.Metadata key counting how many times an
// event has been re-queued by the rate-limit stall strategy.
const rateLimitDelaysKey = "rate_limit_delays"

// rateLimitMaxDelays bounds how many times a rate-limited event may be
// re-queued before it is dropped with a notice, so sustained traffic cannot
// keep a single event (and the queue) alive forever.
const rateLimitMaxDelays = 5

// ---------------------------------------------------------------------------
// Stage 5: ContentSafetyCheckStage
// ---------------------------------------------------------------------------

// ContentSafetyCheckStage checks message content against safety rules.
// Ported from astrbot/core/pipeline/content_safety_check/stage.py
type ContentSafetyCheckStage struct {
	selector    *contentsafety.StrategySelector
	platformMgr *platform.PlatformManager
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
	s.platformMgr = ctx.PlatformMgr
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
		// 与 ProcessStage 的 no_permission_reply 相同：调度器对
		// Continue:false 直接短路，写 event.Result 的提示不会被
		// RespondStage 送达，必须直接经平台发送。
		if event.IsAtOrWakeCommand && s.platformMgr != nil {
			chain := message.NewMessageChain(&message.Plain{Text: "Your message or the model response contains inappropriate content and has been blocked."})
			_ = s.platformMgr.Send(event.Source.Platform, event.Source.ConvID, chain)
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
	config      map[string]interface{}
	providerMgr *provider.ProviderManager
	convMgr     *conversation.Manager
	platformMgr *platform.PlatformManager
}

func NewPreProcessStage() *PreProcessStage {
	return &PreProcessStage{}
}

func (s *PreProcessStage) Name() string { return "preprocess" }

func (s *PreProcessStage) Initialize(ctx *PipelineContext) error {
	s.config = ctx.AstrbotConfig
	s.providerMgr = ctx.ProviderManager
	s.convMgr = ctx.ConvManager
	s.platformMgr = ctx.PlatformMgr
	return nil
}

// applyPreAckEmoji sends a pre-response emoji reaction when the platform
// config enables platform_specific.<platform>.pre_ack_emoji for a woken
// message (ported from preprocess_stage/stage.py). Runs async so a slow
// platform API never blocks the pipeline.
func (s *PreProcessStage) applyPreAckEmoji(event *core.Event) {
	if !event.IsAtOrWakeCommand || s.platformMgr == nil ||
		event.MessageObj == nil || event.MessageObj.MessageID == "" {
		return
	}
	platform := event.Source.Platform
	switch platform {
	case "telegram", "lark", "discord":
	default:
		return
	}
	ps, ok := s.config["platform_specific"].(map[string]interface{})
	if !ok {
		return
	}
	p, ok := ps[platform].(map[string]interface{})
	if !ok {
		return
	}
	cfg, ok := p["pre_ack_emoji"].(map[string]interface{})
	if !ok {
		return
	}
	enabled, _ := cfg["enable"].(bool)
	if !enabled {
		return
	}
	emojis := toStringList(cfg["emojis"])
	if len(emojis) == 0 {
		return
	}
	emoji := emojis[rand.Intn(len(emojis))]
	go func() {
		if err := s.platformMgr.React(platform, event.Source.ConvID, event.MessageObj.MessageID, emoji); err != nil {
			logger.I18nWarn("pre_ack_emoji 发送失败 (%s): %v", platform, err)
		}
	}()
}

func (s *PreProcessStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Pre-response emoji reaction for platforms that enable pre_ack_emoji.
	s.applyPreAckEmoji(event)

	// Quoted-message parser limits (provider_settings.quoted_message_parser):
	// cap nested quote/forward depth and quoted image count.
	applyQuotedMessageParser(s.config, event)

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
	}

	// Speech-to-text: convert Record (voice) components to plain text when a
	// STT provider is enabled (mirrors Python preprocess_stage).
	if s.sttEnabled(event.UnifiedMsgOrigin()) {
		for i, comp := range event.Message.Chain {
			rec, ok := comp.(*message.Record)
			if !ok {
				continue
			}
			text, err := s.sttRecord(event, rec)
			if err != nil {
				logger.I18nWarn("STT 转写失败: %v", err)
				continue
			}
			if text == "" {
				continue
			}
			event.Message.Chain[i] = &message.Plain{Text: text}
			event.PlainText += text
			event.MessageStr += text
		}
	}

	logger.Debug("PreProcess: plain_text='%s', components=%d", plainText, len(event.Message.Chain))
	return &StageResult{Continue: true}, nil
}

// sttEnabled reports whether STT should run (global provider_stt_settings.enable).
func (s *PreProcessStage) sttEnabled(umo string) bool {
	if s.providerMgr == nil {
		return false
	}
	cfg, _ := s.config["provider_stt_settings"].(map[string]interface{})
	if cfg == nil {
		return false
	}
	enabled, _ := cfg["enable"].(bool)
	return enabled
}

// sttRecord transcribes a voice component using the session/global STT provider
// (provider_perf_speech_to_text rule wins).
func (s *PreProcessStage) sttRecord(event *core.Event, rec *message.Record) (string, error) {
	var stt provider.STTProvider
	providerID := ""
	if rules := sessionRulesMemo(event, s.convMgr); rules != nil {
		providerID, _ = rules[conversation.RuleProviderSpeechToText].(string)
	}
	if providerID != "" {
		if p := s.providerMgr.Get(providerID); p != nil {
			if sp, ok := p.(provider.STTProvider); ok {
				stt = sp
			}
		}
	}
	if stt == nil {
		stt = s.providerMgr.GetSTTProvider()
	}
	if stt == nil {
		return "", fmt.Errorf("未配置 STT 提供商")
	}
	audioURL := rec.URL
	if audioURL == "" {
		audioURL = rec.Path
	}
	if audioURL == "" {
		audioURL = rec.File
	}
	if audioURL == "" {
		return "", fmt.Errorf("语音消息缺少音频路径/URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return stt.GetText(ctx, audioURL)
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
	mcpClients map[string]*agent.MCPClient       // sanitized server name -> client
	mcpSchemas map[string]map[string]interface{} // full tool name -> OpenAI tool schema

	// toolPermsMu guards toolPerms (tool name -> configured permission level),
	// parsed lazily once from the stage's immutable config snapshot.
	toolPermsMu sync.Mutex
	toolPerms   map[string]string

	// Subagents (subagent_orchestrator): handoff tools injected into the main
	// LLM and executed as a fresh persona round.
	subAgentEnabled bool
	subAgents       []*SubAgent

	// Platform config toggles consumed by handler matching (ported from
	// waking_check/stage.py).
	disableBuiltinCommands bool
	noPermissionReply      bool

	// groupCtx tracks group-chat context awareness (provider_ltm_settings:
	// group_icl_enable records + active_reply probability).
	groupCtx *GroupChatContext

	// Knowledge-base context retrieval for prompt injection. Provided by the
	// lifecycle (reuses the dashboard retrieval pipeline); nil = no KB.
	kbRetriever func(umo, query string) (string, error)

	// toolSchemaMode: "full" (default) sends complete tool schemas; "skills_like"
	// sends light schemas and re-queries the LLM for arguments when a tool is
	// chosen (saves tokens on large tool sets).
	toolSchemaMode string

	// doom loop protection: track consecutive same-tool calls per session and
	// pause the tool after a threshold, asking the session owner to confirm.
	doomMu       sync.Mutex
	doomTrackers map[string]*doomTracker // key: unified msg origin

	// umoAutoNameRecorder 对齐 Python v4.28.0 (#9909)：handler 命中时异步记录
	// UMO 可读名称到数据库（handler 命中路径的 schedule 在 Python 的
	// WakingCheckStage 中，Go 侧移至 ProcessStage，因 handler 匹配在此执行）。
	umoAutoNameRecorder *UmoAutoNameRecorder
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
	// Wire the plugin-activation snapshot for filterSkillsForCurrentConfig
	// (subprocess plugin ids are the data/plugins root dir names, matching
	// the plugin skill source_label).
	if ctx.SubPlugins != nil {
		sub := ctx.SubPlugins
		SetActivePluginIDsProvider(func() map[string]bool {
			out := map[string]bool{}
			for _, inst := range sub.RegisteredPlugins() {
				if inst == nil || inst.ID == "" {
					continue
				}
				out[inst.ID] = true
			}
			return out
		})
	}
	s.providerConf = bindProviderSettings(ctx.AstrbotConfig)
	// Wire dequeue_context_length into the conversation manager so AppendHistory
	// truncates stored history instead of growing it without bound (M-33).
	// max_context_length <= 0 (the default -1) keeps the historical no-truncate
	// behavior.
	if s.convMgr != nil && s.providerConf != nil {
		dequeue := 0
		if s.providerConf.MaxContextLength > 0 {
			dequeue = s.providerConf.DequeueContextLength
		}
		s.convMgr.SetDequeueContextLength(dequeue)
	}
	s.subAgents, s.subAgentEnabled = loadSubAgents(ctx.AstrbotConfig)
	s.kbRetriever = ctx.KBRetriever
	s.toolSchemaMode = "full"
	if ps, ok := ctx.AstrbotConfig["provider_settings"].(map[string]interface{}); ok {
		if m, ok := ps["tool_schema_mode"].(string); ok && m != "" {
			s.toolSchemaMode = m
		}
	}
	s.doomTrackers = make(map[string]*doomTracker)
	if ctx.PersonaResolver != nil {
		s.personaPrompt = ctx.PersonaResolver
	}
	if ctx.PersonaSkillsResolver != nil {
		s.personaSkills = ctx.PersonaSkillsResolver
	}
	if ctx.UmoAliasResolver != nil {
		s.umoAlias = ctx.UmoAliasResolver
	}
	ps := bindPlatformSettings(ctx.AstrbotConfig)
	s.noPermissionReply = ps.NoPermissionReply
	if db, ok := ctx.AstrbotConfig["disable_builtin_commands"].(bool); ok {
		s.disableBuiltinCommands = db
	}
	s.groupCtx = NewGroupChatContext(ctx.AstrbotConfig)

	// 对齐 Python v4.28.0 (#9909)：handler 命中路径的 UMO 自动名称记录。
	if ctx.Database != nil {
		s.umoAutoNameRecorder = NewUmoAutoNameRecorder(ctx.Database, "")
	}
	return nil
}

func (s *ProcessStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Prefer the event's own execution context (e.g. a WebSocket session that
	// can be cancelled by an interrupt) over the dispatch loop's process-level
	// context, so cancelling one run does not affect the whole bus.
	if event.Ctx != nil {
		ctx = event.Ctx
	}
	// Doom-loop confirmation: if a tool was paused for this session, only the
	// original asker may confirm. A confirmation resumes the original request
	// (message is rewritten to it); any other reply clears the paused state
	// and the message flows through the normal pipeline.
	switch s.maybeHandleDoomConfirm(event) {
	case doomResumed:
		// fall through: re-run the original request through the pipeline
	case doomNotConsumed:
		// normal message
	}
	// Run subprocess plugin on_message hooks (mirrors Python's
	// @filter.on_message): plugins observe every incoming message.
	dispatchSubprocessHooks(s.subPlugins, event, "on_message")
	// Push serialized events to plugins that registered a bridge hook
	// (botpy/telegram compat layers); no-op unless a plugin opted in.
	dispatchBridgeHooks(s.subPlugins, event)

	// Group chat context awareness (mirrors main.py on_message): record the
	// message when group_icl_enable (or active_reply) is enabled.
	if s.groupCtx != nil && s.groupChatContextEnabled(event) && event.Message != nil {
		hasImageOrPlain := false
		for _, comp := range event.Message.Chain {
			if _, ok := comp.(*message.Plain); ok {
				hasImageOrPlain = true
				break
			}
			if _, ok := comp.(*message.Image); ok {
				hasImageOrPlain = true
				break
			}
		}
		if hasImageOrPlain {
			needActive := s.groupCtx.NeedActiveReply(event)
			if s.groupLTMSetting(event, "group_icl_enable") {
				s.groupCtx.HandleMessage(event)
			}
			// Active reply: a non-woken group message may trigger an LLM
			// reply with a configured probability (mirrors main.py).
			if needActive && !event.IsAtOrWakeCommand {
				event.SetExtra("active_reply", true)
			}
		}
	}

	// Record the incoming message for statistics (message_count).
	if s.database != nil {
		msg := event.MessageStr
		if msg == "" {
			msg = extractPlainText(event.Message)
		}
		if msg != "" {
			_ = s.database.RecordPlatformMessage(event.Source.Platform, event.UnifiedMsgOrigin(), event.Source.SenderID, msg)
		}
		// Group message history retention (provider_ltm_settings).
		if event.Source.IsGroup && event.Source.Platform != "webchat" {
			if s.groupLTMSetting(event, "group_message_history_enable") {
				keep := providerLTMInt(s.config, "group_message_history_max_cnt", 700)
				_ = s.database.TrimPlatformMessageHistory(event.Source.Platform, event.UnifiedMsgOrigin(), keep)
			}
		}
	}

	// Try plugin handlers first
	activated, permissionDenied := s.findMatchingHandlers(event)
	// Session custom rules (session_plugin_config) can disable/enable plugins
	// for this session.
	activated = s.filterHandlersBySession(event, activated)
	// Ported from waking_check/stage.py: a permission filter failure only skips
	// that handler (raise_error=False path); the other matched handlers still
	// run. Only when NO handler is left does the event stop before the LLM.
	if permissionDenied && len(activated) == 0 {
		// 对齐 Python v4.28.0 (#9909)：权限拒绝且无其他 handler 命中时，
		// 若此前已被前缀/@提及唤醒（IsAtOrWakeCommand=true），异步记录 UMO 名称。
		// 对应 Python waking_check 中 permission_not_pass + raise_error 路径。
		if event.IsAtOrWakeCommand {
			s.scheduleUmoAutoName(event)
		}
		// When no_permission_reply is enabled, send the reply directly —
		// Continue=false short-circuits the pipeline before
		// ResultDecorateStage/RespondStage, so an event.Result chain would
		// never be delivered; either way the event is stopped before any
		// handler or the LLM runs.
		if s.noPermissionReply && !event.IsStopped() {
			s.replyText(event, fmt.Sprintf("您(ID: %s)的权限不足以使用此指令。通过 /sid 获取 ID 并请管理员添加。", event.Source.SenderID))
		}
		event.Stop()
		return &StageResult{Continue: false}, nil
	}
	if len(activated) > 0 {
		// 对齐 Python v4.28.0 (#9909)：handler 命中（is_wake=True），异步记录 UMO 名称。
		// Python 在 WakingCheckStage 中 handler 匹配时设置 is_wake=True，
		// Go 侧 handler 匹配在 ProcessStage，故在此 schedule。
		s.scheduleUmoAutoName(event)
		// Execute handlers in priority order
		for _, handler := range activated {
			if event.IsStopped() {
				break
			}
			if err := s.executeHandler(ctx, event, handler); err != nil {
				logger.Error("Handler %s failed: %v", handler.HandlerFullName, err)
				// on_plugin_error: notify other plugins of the failure.
				dispatchSubprocessHooksPayload(s.subPlugins, event, "on_plugin_error", &pluginsdk.PluginError{
					PluginName:  handler.PluginName,
					HandlerName: handler.HandlerName,
					Error:       err.Error(),
				})
			}
		}
		// If any handler produced a result, yield
		if event.Result != nil {
			return &StageResult{Continue: true}, nil
		}
		// 插件 handler 主动发送过回复（对齐 Python _has_send_oper）：事件已
		// 处理，不得再走 LLM（box 等"主动发图回复"的插件命令）。
		if event.HasSendOper {
			logger.Debug("ProcessStage: plugin 已发送回复，跳过 LLM")
			return &StageResult{Continue: true}, nil
		}
		// 插件 handler 调用了 event.stop_event()（stop_propagation，无
		// Result/send 的主动回复路径，如 box recall_task）：事件已处理，
		// 停止管线，不得走 LLM 兜底。
		if event.IsStopped() {
			logger.Debug("ProcessStage: plugin 已停止事件，跳过 LLM")
			return &StageResult{Continue: false}, nil
		}
	}

	// If not woken, stop — unless an active reply was requested by the group
	// chat context (provider_ltm_settings.active_reply probability hit) or the
	// caller explicitly requested an LLM call (event.CallLLM, e.g. the WebUI
	// dashboard chat which bypasses platform wake words).
	if !event.IsAtOrWakeCommand {
		if event.CallLLM {
			logger.Debug("ProcessStage: explicit CallLLM for %s", event.UnifiedMsgOrigin())
			event.SetExtra("llm_wake", true)
		} else if active, _ := event.GetExtra("active_reply").(bool); active {
			logger.Debug("ProcessStage: active reply triggered for %s", event.UnifiedMsgOrigin())
			event.SetExtra("llm_wake", true)
		} else {
			event.Stop()
			return &StageResult{Continue: false}, nil
		}
	}

	// Call LLM agent
	if s.shouldCallLLM(event) {
		if err := s.callLLMAgent(ctx, event); err != nil {
			if errors.Is(err, errNoAvailableProvider) {
				// 未配置可用模型提供商：平台侧静默（不回复用户），
				// 仅在启动时打印 warn（见 lifecycle）。
				logger.Debug("skip LLM call: %v", err)
				event.Result = nil
			} else {
				logger.Error("LLM agent call failed: %v", err)
				event.Result = &message.MessageEventResult{}
				event.Result.Chain = []message.Component{&message.Plain{Text: "LLM 调用失败: " + err.Error()}}
			}
		}
	}

	return &StageResult{Continue: true}, nil
}

// findMatchingHandlers returns plugin handlers that match this event.
// Ported from waking_check/stage.py: non-permission filters are AND-ed
// together; a failing PermissionFilter marks permissionDenied (the caller
// decides whether to notify the user). Built-in command handlers are skipped
// when disable_builtin_commands is enabled.
func (s *ProcessStage) findMatchingHandlers(event *core.Event) (handlers []*star.StarHandlerMetadata, permissionDenied bool) {
	if s.pluginMgr == nil {
		return nil, false
	}
	registry := s.pluginMgr.Handlers()
	if registry == nil {
		return nil, false
	}

	// Get all filter-type handlers, sorted by priority
	all := registry.GetFilterHandlers()
	result := []*star.StarHandlerMetadata{}
	denied := false

	logger.Debug("[dbg] findMatchingHandlers: %d 个 filter handler, msg=%q wake=%v", len(all), event.MessageStr, event.IsAtOrWakeCommand)
	for _, handler := range all {
		// 经快照读取 filters/Enabled：dashboard 可能在运行期经写锁修改
		// EventFilters/Enabled（SetHandlerPermission/Enable/Disable），
		// 直接遍历共享切片会与写者并发构成数据竞争。
		filters, enabled, ok := registry.SnapshotFilters(handler.HandlerFullName)
		if !ok || !enabled {
			continue
		}
		if s.disableBuiltinCommands && handler.HandlerModulePath == "astrbot.builtin_stars.builtin_commands.main" {
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
		// All non-permission filters must pass (AND); a failing permission
		// filter marks the event as permission-denied.
		passed := true
		filterDenied := false
		for _, filter := range filters {
			if _, isPerm := filter.(*star.PermissionFilter); isPerm {
				if !filter.Match(fctx) {
					filterDenied = true
				}
				continue
			}
			if !filter.Match(fctx) {
				passed = false
				break
			}
		}
		if !passed {
			continue
		}
		if filterDenied {
			denied = true
			continue
		}
		logger.Debug("[dbg] 命中 handler: %s", handler.HandlerFullName)
		result = append(result, handler)
	}

	return result, denied
}

// executeHandler invokes a single handler.
func (s *ProcessStage) executeHandler(ctx context.Context, event *core.Event, handler *star.StarHandlerMetadata) error {
	if handler.Handler == nil {
		return nil
	}
	return handler.Handler(event)
}

// sessionRulesMemo returns the event-level memoized session rules.
// GetSessionRules runs 2 SQL queries + JSON decode per call; the pipeline
// queries it up to 6 times per event (plugin filter, provider override,
// persona override, TTS ×2, STT). Rules cannot change mid-event, so caching
// on the event is safe. Returns nil when the conversation manager is absent.
func sessionRulesMemo(event *core.Event, convMgr *conversation.Manager) map[string]interface{} {
	if convMgr == nil {
		return nil
	}
	if r, ok := event.GetExtra("_session_rules").(map[string]interface{}); ok {
		return r
	}
	r := convMgr.GetSessionRules(event.UnifiedMsgOrigin())
	event.SetExtra("_session_rules", r)
	return r
}

// filterHandlersBySession applies the session_plugin_config rule
// (disabled_plugins / enabled_plugins) to the matched plugin handlers,
// mirroring Python SessionPluginManager.filter_handlers_by_session.
// scheduleUmoAutoName 对齐 Python v4.28.0 (#9909)：handler 命中或权限拒绝路径
// 触发时异步记录 UMO 名称。nil-safe。
func (s *ProcessStage) scheduleUmoAutoName(event *core.Event) {
	if s.umoAutoNameRecorder != nil {
		s.umoAutoNameRecorder.Schedule(event)
	}
}

func (s *ProcessStage) filterHandlersBySession(event *core.Event, handlers []*star.StarHandlerMetadata) []*star.StarHandlerMetadata {
	if s.convMgr == nil || len(handlers) == 0 {
		return handlers
	}
	rules := sessionRulesMemo(event, s.convMgr)
	pc, ok := rules[conversation.RulePluginConfig].(map[string]interface{})
	if !ok {
		return handlers
	}
	disabledRaw, _ := pc["disabled_plugins"].([]interface{})
	enabledRaw, _ := pc["enabled_plugins"].([]interface{})
	disabled := make(map[string]bool, len(disabledRaw))
	for _, d := range disabledRaw {
		if s, ok := d.(string); ok {
			disabled[s] = true
		}
	}
	enabled := make(map[string]bool, len(enabledRaw))
	for _, e := range enabledRaw {
		if s, ok := e.(string); ok {
			enabled[s] = true
		}
	}
	if len(disabled) == 0 && len(enabled) == 0 {
		return handlers
	}
	out := make([]*star.StarHandlerMetadata, 0, len(handlers))
	for _, h := range handlers {
		name := h.PluginName
		if name == "" {
			// Built-in/system handlers are never filtered.
			out = append(out, h)
			continue
		}
		if disabled[name] {
			continue
		}
		if len(enabled) > 0 && !enabled[name] {
			continue
		}
		out = append(out, h)
	}
	return out
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

// agentRequest carries the state prepared for one LLM agent invocation
// through the tool loop and the reply finalization (the callLLMAgent split).
type agentRequest struct {
	event              *core.Event
	prompt             string // final user prompt (after knowledge-base injection)
	systemPrompt       string // persona prompt after skills / safety / hooks
	providerCfg        map[string]interface{}
	providerSettings   map[string]interface{}
	chatInst           provider.ChatProvider
	req                *provider.ProviderRequest
	computerUseRuntime string
	streaming          bool
	// cleanup removes image-compress temp files once the whole request
	// completes; nil when there is nothing to clean up.
	cleanup func()
}

// callLLMAgent invokes the LLM provider and sets the result. It is a thin
// orchestrator: prepareAgentRequest resolves the persona/provider and
// assembles the request, runAgentToolLoop drives the chat + tool-call rounds,
// and finalizeAgentReply persists the reply and flushes the stream.
func (s *ProcessStage) callLLMAgent(ctx context.Context, event *core.Event) error {
	// Pre-boot the sandbox BEFORE the first LLM request when Computer Use runs
	// in sandbox mode. 沙盒此前在首次工具调用时才惰性启动，导致首轮系统提示词
	// 构建（applySkills）时沙盒技能缓存仍为空，python-sandbox 等沙盒内置技能
	// 从未注入 agent 上下文（agent 因此不了解沙盒环境与文件处理流程）。此处
	// 提前启动并同步技能，使首轮请求即可带上沙盒技能。ensureSandboxStarted 幂等，
	// 已运行则直接返回，仅首个请求付出 ~10s 启动成本。
	if s.providerConf != nil && s.providerConf.ComputerUseRuntime == "sandbox" && s.sandboxMgr != nil {
		if err := s.ensureSandboxStarted(ctx, event.UnifiedMsgOrigin()); err != nil {
			logger.I18nWarn("沙盒预启动失败（继续处理）: %v", err)
		}
	}

	ar, err := s.prepareAgentRequest(event)
	if err != nil || ar == nil {
		// A failure reply was already written to event.Result, a plugin hook
		// stopped the call, or the prompt was empty.
		return nil
	}
	if ar.cleanup != nil {
		defer ar.cleanup()
	}

	streamer := newStreamSender(s, event)
	defer streamer.flush()

	resp, ok := s.runAgentToolLoop(ctx, ar, streamer)
	if !ok {
		return nil
	}
	s.finalizeAgentReply(ar, resp, streamer)
	return nil
}

// resolveAgentContext extracts the prompt, resolves the effective provider
// config and the persona system prompt, and applies the prompt-shaping steps
// (skills, safety mode, on_llm_request hooks, knowledge base). It returns
// (nil, nil) when the call is already finished (empty prompt or a plugin hook
// stopped it) and (nil, err) when a failure reply was written to event.Result.
func (s *ProcessStage) resolveAgentContext(event *core.Event) (*agentRequest, error) {
	// Prefer the adapter's clean message_str (mirrors Python's use of
	// event.message_str). PlainText is the chain-rendered text and may carry a
	// self-mention (e.g. the qq_official adapter prepends At{qq_official} to
	// C2C messages), which would otherwise pollute the prompt and history.
	prompt := event.MessageStr
	if prompt == "" {
		prompt = event.PlainText
	}
	if prompt == "" {
		prompt = extractPlainText(event.Message)
	}
	logger.Debug("callLLMAgent: prompt=%q plaintext=%q messagestr=%q", prompt, event.PlainText, event.MessageStr)
	// 纯媒体消息（图片/语音）没有可提取的文本，但同样需要调用 LLM（对齐
	// Python：req.prompt 为空但 image_urls/audio_urls 非空时继续）。
	if prompt == "" {
		imgs, auds := collectMediaURLs(event)
		if len(imgs) == 0 && len(auds) == 0 {
			return nil, nil
		}
	}

	// Reset the doom-loop counters for this session at each request entry so
	// repetition is never measured across request boundaries. The paused tool
	// state is preserved (L-18).
	s.resetDoomLoopCount(event.UnifiedMsgOrigin())

	// Trace span for this agent invocation (TracePage).
	if event.Trace == nil {
		traceEnabled := func() bool {
			if v, ok := s.config["trace_enable"].(bool); ok {
				return v
			}
			return false
		}
		event.Trace = log.NewTraceSpan("astr_main_agent", event.UnifiedMsgOrigin(), event.Source.SenderName, truncateRunes(prompt, 60), traceEnabled)
		event.Trace.Record("astr_agent_prepare", map[string]interface{}{
			"persona_id": personaIDOrDefault(s.config),
		})
	}

	providerCfg, providerSettings, err := s.resolveProvider()
	if err != nil {
		// 无可用模型提供商：平台侧静默（不回复），宿主启动时已打印 warn。
		event.Result = &message.MessageEventResult{}
		return nil, err
	}

	// Session-level provider override (custom rules / /provider command):
	// when the session has a provider_perf_chat_completion rule, use that
	// provider instead of the global default.
	if rules := sessionRulesMemo(event, s.convMgr); rules != nil {
		if pid, _ := rules[conversation.RuleProviderChatCompletion].(string); pid != "" {
			if pc := findProviderByID(s.config, pid); pc != nil {
				providerCfg = pc
			}
		}
	}

	// Dashboard-selected provider/model override (WebUI chat writes
	// selected_provider/selected_model into event.Metadata, chat_stream.go).
	// Applied on a copy so the shared provider config is never mutated (L-23).
	providerCfg = s.applySelectedProviderModel(event, providerCfg)

	// Resolve the conversation. Mirrors Python's `_get_session_conv`: the
	// conversation is lazily created if it does not exist yet. The current
	// user message is appended to history only after the LLM round finishes
	// (Python appends the user+assistant pair post-completion), so the prompt
	// is not duplicated in req.Contexts.
	personaID := ""
	if s.convMgr != nil {
		conv := s.convMgr.GetOrCreateConversation(event.UnifiedMsgOrigin(), event.Source.PlatformID)
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

	// Session custom-rule persona overrides everything (highest priority):
	// session_service_config.persona_id from the WebUI rules editor.
	if rules := sessionRulesMemo(event, s.convMgr); rules != nil {
		if sc, ok := rules[conversation.RuleServiceConfig].(map[string]interface{}); ok {
			if pid, ok := sc["persona_id"].(string); ok && pid != "" {
				personaID = pid
				providerSettings["persona"] = pid
			}
		}
	}

	// Trace: persona selection (sel_persona).
	if event.Trace != nil {
		event.Trace.Record("sel_persona", map[string]interface{}{
			"persona_id": personaID,
		})
	}

	// Resolve the persona system prompt (conv.Persona holds the persona id).
	systemPrompt := ""
	if s.personaPrompt != nil {
		systemPrompt = s.personaPrompt(event.UnifiedMsgOrigin(), personaID)
	}
	if systemPrompt == "" {
		// providerSettings["persona"] 在上方被写成 persona ID（非提示词文本），
		// 不能再把它当系统提示词回退（persona 被删除/改名时会把 ID 直接发给
		// 模型）。仅兼容历史遗留的"纯文本 persona"形态配置。
		if p, ok := providerSettings["persona_prompt_text"].(string); ok && p != "" {
			systemPrompt = p
		}
	}

	// Inject active skills into the system prompt (mirrors Python's
	// astr_main_agent._ensure_persona_and_skills).
	systemPrompt = s.applySkills(systemPrompt, providerSettings, personaID, event.UnifiedMsgOrigin())

	// LLM safety mode: prefix the safety prompt when enabled (mirrors
	// astr_main_agent._apply_llm_safety_mode).
	systemPrompt = s.applyLLMSafetyMode(systemPrompt)

	// Apply on_llm_request hooks from subprocess plugins: they may modify the
	// system prompt and/or user prompt, or stop the LLM call entirely.
	if s.subPlugins != nil {
		sp, up, stop, err := s.applyLLMRequestHooks(event, systemPrompt, prompt)
		if err != nil {
			logger.I18nWarn("插件 on_llm_request 钩子执行失败: %v", err)
		} else {
			systemPrompt = sp
			prompt = up
			if stop {
				logger.Debug("plugin on_llm_request hook stopped the LLM call")
				event.Stop()
				return nil, nil
			}
		}
	}

	// on_waiting_llm_request fires right before the provider call (e.g. a
	// plugin may show a "processing" indicator).
	dispatchSubprocessHooks(s.subPlugins, event, "on_waiting_llm_request")

	// Knowledge-base retrieval: inject related KB content into the prompt
	// (non-agentic mode), using the session kb_config rule when set.
	prompt = s.applyKnowledgeBase(event, prompt)

	// computer_use_runtime drives whether local/sandbox tools are exposed and
	// whether the local-mode hint is appended to the system prompt.
	computerUseRuntime := "local"
	if s.providerConf != nil && s.providerConf.ComputerUseRuntime != "" {
		computerUseRuntime = s.providerConf.ComputerUseRuntime
	}

	return &agentRequest{
		event:              event,
		prompt:             prompt,
		systemPrompt:       systemPrompt,
		providerCfg:        providerCfg,
		providerSettings:   providerSettings,
		computerUseRuntime: computerUseRuntime,
	}, nil
}

// prepareAgentRequest resolves the persona/provider for the event (via
// resolveAgentContext), assembles the ProviderRequest, creates the chat
// provider instance, and injects the tool schemas / streaming flags.
// Return conventions match resolveAgentContext: (nil, nil) = already
// finished, (nil, err) = failure reply written to event.Result.
func (s *ProcessStage) prepareAgentRequest(event *core.Event) (*agentRequest, error) {
	ar, err := s.resolveAgentContext(event)
	if err != nil || ar == nil {
		return nil, err
	}
	providerCfg, providerSettings := ar.providerCfg, ar.providerSettings
	prompt, systemPrompt := ar.prompt, ar.systemPrompt

	imageURLs, audioURLs := collectMediaURLs(event)
	req := &provider.ProviderRequest{
		Prompt:       prompt,
		SessionID:    event.UnifiedMsgOrigin(),
		SystemPrompt: systemPrompt,
		ImageURLs:    imageURLs,
		AudioURLs:    audioURLs,
		Conversation: s.convMgr,
		Contexts:     s.conversationHistory(event.UnifiedMsgOrigin()),
	}

	// File attachments (当前消息与引用回复中的 File 组件) 注入 LLM 上下文，
	// 使 agent 能看到下载 URL 并 astrbot_download_file（对齐 Python
	// astr_main_agent 的 [File Attachment ...]）。
	for _, part := range collectFileAttachments(event) {
		req.ExtraUserContentParts = append(req.ExtraUserContentParts, map[string]interface{}{
			"type": "text",
			"text": part,
		})
	}

	// Sanitize context by the provider's supported modalities
	// (provider_settings.sanitize_context_by_modalities).
	if s.providerConf != nil && s.providerConf.SanitizeContextByModalities {
		if mods := providerModalities(providerCfg); len(mods) > 0 {
			req.Contexts = sanitizeContextByModalities(req.Contexts, mods)
		}
	}

	// Image compression for the provider (provider_settings.image_compress_*).
	if len(req.ImageURLs) > 0 {
		compressed := make([]string, 0, len(req.ImageURLs))
		for _, u := range req.ImageURLs {
			compressed = append(compressed, s.compressImageForProvider(u))
		}
		req.ImageURLs = compressed
		// Temp files created by compressImageForProvider are consumed by the
		// provider during chatRound; the orchestrator removes them once the
		// whole request completes.
		tempFiles := []string{}
		for _, p := range compressed {
			if isCompressTempFile(p) {
				tempFiles = append(tempFiles, p)
			}
		}
		if len(tempFiles) > 0 {
			ar.cleanup = func() {
				for _, p := range tempFiles {
					os.Remove(p)
				}
			}
		}
	}

	// Group chat context injection: records received before this message are
	// appended to the request (mirrors GroupChatContext.on_req_llm).
	if s.groupCtx != nil && s.groupChatContextEnabled(event) && s.groupLTMSetting(event, "group_icl_enable") {
		s.groupCtx.OnReqLLM(event, req)
	}

	providerType, _ := providerCfg["type"].(string)
	if providerType == "" {
		providerType, _ = providerCfg["provider"].(string)
	}
	if providerType == "" {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 模型提供商配置缺少 type 字段，请重新配置提供商"}}
		return nil, fmt.Errorf("provider config missing type field")
	}

	// Merge the provider source config (api_base/key live on the source,
	// mirroring astrbot/core/provider/manager.py get_merged_provider_config).
	mergedCfg := mergeProviderSource(providerCfg, s.config["provider_sources"])

	inst, err := provider.CreateProvider(providerType, mergedCfg, providerSettings)
	if err != nil {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 初始化模型提供商失败: " + err.Error()}}
		return nil, err
	}

	chatInst, ok := inst.(provider.ChatProvider)
	if !ok {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 提供商 " + providerType + " 不支持聊天能力"}}
		return nil, fmt.Errorf("provider %s has no chat capability", providerType)
	}
	ar.chatInst = chatInst

	// Inject active tools (built-in + MCP servers) so the model can call them.
	// skills_like mode sends light schemas (name/description only) to save
	// tokens; arguments are re-queried once a tool is selected.
	if s.toolSchemaMode == "skills_like" {
		req.Tools = s.collectLightTools(ar.computerUseRuntime)
	} else {
		req.Tools = s.collectTools(ar.computerUseRuntime)
	}
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
	switch ar.computerUseRuntime {
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
	// unsupported_streaming_strategy=turn_off disables streaming entirely.
	if streamingEnabled && s.unsupportedStreamingStrategyIsTurnOff() {
		streamingEnabled = false
	}
	ar.streaming = streamingEnabled

	// System context reminder (identifier / group name / datetime), appended as
	// an extra user-content part like Python's astr_main_agent.
	if reminder := s.buildSystemReminder(event); reminder != "" {
		logger.Debug("callLLMAgent: system_reminder=%q", reminder)
		req.ExtraUserContentParts = append(req.ExtraUserContentParts, map[string]interface{}{
			"type": "text",
			"text": reminder,
		})
	}

	ar.req = req
	return ar, nil
}

// collectMediaURLs gathers image/audio URLs from the event message chain for
// the multimodal provider request (mirrors Python's astr_main_agent media
// attachment collection). Image components prefer URL, then local path
// (file://), then base64 data; Record components prefer URL, then path.
func collectMediaURLs(event *core.Event) (imageURLs, audioURLs []string) {
	if event.Message == nil {
		return nil, nil
	}
	for _, comp := range event.Message.Chain {
		switch c := comp.(type) {
		case *message.Image:
			switch {
			case c.URL != "":
				imageURLs = append(imageURLs, c.URL)
			case c.Path != "":
				imageURLs = append(imageURLs, "file://"+c.Path)
			case c.Base64 != "":
				imageURLs = append(imageURLs, "data:image/png;base64,"+c.Base64)
			}
		case *message.Record:
			switch {
			case c.URL != "":
				audioURLs = append(audioURLs, c.URL)
			case c.Path != "":
				audioURLs = append(audioURLs, "file://"+c.Path)
			}
		}
	}
	return imageURLs, audioURLs
}

// collectFileAttachments gathers File components from the event's message chain
// and any quoted-reply / forward-node chains. For each file with a resolvable
// download URL it downloads the file to a host temp path (mirroring Python's
// astr_main_agent File.get_file()) and returns "[File Attachment ...]" text
// parts for the LLM context, so the agent can astrbot_upload_file the host
// path into the sandbox.
func collectFileAttachments(event *core.Event) []string {
	if event.Message == nil {
		return nil
	}
	var parts []string
	var walk func(comps []message.Component, quoted bool)
	walk = func(comps []message.Component, quoted bool) {
		for _, comp := range comps {
			switch c := comp.(type) {
			case *message.File:
				name := c.Name
				if name == "" {
					name = c.FileID
				}
				if name == "" {
					name = "unknown"
				}
				prefix := "[File Attachment: "
				if quoted {
					prefix = "[File Attachment in quoted message: "
				}
				if path := downloadFileAttachment(c); path != "" {
					c.Path = path
					parts = append(parts, fmt.Sprintf("%sname %s, path %s]", prefix, name, path))
				} else if validHTTPURL(c.URL) {
					parts = append(parts, fmt.Sprintf("%sname %s, url %s (download failed)]", prefix, name, c.URL))
				} else {
					parts = append(parts, fmt.Sprintf("%sname %s (no download URL resolved)]", prefix, name))
				}
			case *message.Reply:
				walk(c.Chain, true)
			case *message.Nodes:
				for _, n := range c.Nodes {
					if n != nil {
						walk(n.Content, quoted)
					}
				}
			}
		}
	}
	walk(event.Message.Chain, false)
	return parts
}

// downloadFileAttachment downloads a File component's URL to a host temp
// directory (data/temp) and returns the local path, or "" when there is no
// usable URL or the download fails. Mirrors Python's File.get_file()/
// _download_file which caches the remote file locally before the LLM round.
func downloadFileAttachment(c *message.File) string {
	if c == nil || !validHTTPURL(c.URL) {
		return ""
	}
	dir := filepath.Join("data", "temp")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	name := strings.TrimSpace(c.Name)
	if name == "" {
		name = c.FileID
	}
	safe := filepath.Base(strings.ReplaceAll(name, " ", "_"))
	if safe == "" || safe == "." || safe == "/" {
		safe = "attachment"
	}
	dst := filepath.Join(dir, fmt.Sprintf("fileseg_%s_%d", safe, time.Now().UnixNano()))
	dlCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := utils.DownloadFile(dlCtx, c.URL, dst); err != nil {
		logger.I18nWarn("文件附件下载失败 (%s): %v", c.URL, err)
		os.Remove(dst)
		return ""
	}
	return dst
}

// runAgentToolLoop issues the initial chat round, executes the requested
// tools (up to provider_settings.max_agent_step rounds) and runs the
// follow-up rounds with the tool results. It returns ok=false when a failure
// reply was already written to event.Result (or the provider reported an
// error role), in which case the caller must not finalize the reply.
func (s *ProcessStage) runAgentToolLoop(ctx context.Context, ar *agentRequest, streamer *streamSender) (*provider.LLMResponse, bool) {
	event := ar.event
	req := ar.req

	llmCtx, cancel := context.WithTimeout(ctx, 300*time.Second)
	defer cancel()

	// Context-limit handling: llm_compress / truncate_by_turns based on
	// provider_settings.max_context_length (token estimate).
	req.Contexts = s.maybeCompressContext(llmCtx, ar.chatInst, ar.systemPrompt, req.Contexts)

	// Tool-call loop: up to 5 rounds of tool execution + follow-up.
	messages := append([]map[string]interface{}{}, req.Contexts...)
	messages = append(messages, req.ToUserMessage())

	// 对齐 Python tool_loop_agent_runner：messages 为空（且无新 prompt）时跳过 LLM
	// 请求，返回 err 响应而非发空请求。
	if len(messages) == 0 && req.Prompt == "" {
		logger.Warn("Skipping LLM request because no messages remain after agent/request hooks and context processing.")
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "No messages remain for the LLM request."}}
		return nil, false
	}

	resp, err := s.chatRound(llmCtx, ar.chatInst, req, ar.streaming, streamer)
	if err != nil {
		logger.Error("LLM call failed: %v", err)
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 LLM 调用失败: " + err.Error()}}
		return nil, false
	}
	s.recordProviderCall(ar.providerCfg, event.UnifiedMsgOrigin(), resp)
	// skills_like: the main request carried no tool parameters. When the model
	// chose tools, re-query once with the chosen tools' full parameter schemas
	// (minimal context) so the LLM produces proper arguments.
	if s.toolSchemaMode == "skills_like" && len(resp.ToolsCallName) > 0 {
		if requery, ok := s.requeryToolArgs(llmCtx, ar.chatInst, req, resp, ar.computerUseRuntime); ok {
			resp = requery
		}
	}
	// Max agent steps: 优先 agent_runner.config.misc.max_steps，回退 provider_settings.max_agent_step，
	// 默认 5（对齐 Python #9801/#9818）。
	maxSteps := 5
	if v := agentRunnerMaxSteps(s.config); v > 0 {
		maxSteps = v
	} else if s.providerConf != nil && s.providerConf.MaxAgentStep > 0 {
		maxSteps = s.providerConf.MaxAgentStep
	}
	for round := 0; round < maxSteps && len(resp.ToolsCallName) > 0; round++ {
		// Append the assistant tool-call message
		assistantMsg := map[string]interface{}{
			"role":       "assistant",
			"content":    resp.CompletionText,
			"tool_calls": buildToolCallsMessage(resp),
		}
		messages = append(messages, assistantMsg)

		// Execute each requested tool and append tool results
		doomed := false
		for i, name := range resp.ToolsCallName {
			args := map[string]interface{}{}
			if i < len(resp.ToolsCallArgs) {
				args = resp.ToolsCallArgs[i]
			}
			toolID := ""
			if i < len(resp.ToolsCallIDs) {
				toolID = resp.ToolsCallIDs[i]
			}
			if !s.checkDoomLoop(event, doomLoopKey(name, args)) {
				// Tool paused by doom-loop detection; stop the whole tool loop.
				doomed = true
				break
			}
			// provider_settings.show_tool_use_status: notify the user a tool is
			// being called (mirrors astr_agent_run_util.py). When
			// show_tool_call_result is also enabled the status is folded into
			// the result notice sent after the tool returns.
			if s.providerConf != nil && s.providerConf.ShowToolUseStatus && !s.providerConf.ShowToolCallResult {
				s.sendToolStatus(event, toolStatusCall(name))
			}
			// Trace: tool call + result (agent_tool_call / agent_tool_result).
			if event.Trace != nil {
				event.Trace.Record("agent_tool_call", map[string]interface{}{
					"tool_name": name,
					"tool_args": truncateRunes(fmt.Sprintf("%v", args), 200),
				})
			}
			result := s.executeToolWithTimeout(event, ar.computerUseRuntime, name, args)
			// on_llm_tool_respond fires after the tool executed, carrying the
			// tool name/args plus its result.
			dispatchSubprocessHooksPayload(s.subPlugins, event, "on_llm_tool_respond", &pluginsdk.ToolCall{
				Name:   name,
				Args:   args,
				Result: result,
			})
			// Oversized tool output is spilled to a file with a read hint so the
			// model does not re-run the tool just to see the full result.
			result = materializeToolResult(result, toolID)
			// provider_settings.show_tool_use_status + show_tool_call_result:
			// send a combined "tool called → result" notice.
			if s.providerConf != nil && s.providerConf.ShowToolUseStatus && s.providerConf.ShowToolCallResult {
				s.sendToolStatus(event, fmt.Sprintf("%s\n%s", toolStatusCall(name), toolStatusResult(result)))
			}
			if event.Trace != nil {
				event.Trace.Record("agent_tool_result", map[string]interface{}{
					"tool_name":   name,
					"tool_result": truncateRunes(result, 200),
				})
			}
			messages = append(messages, map[string]interface{}{
				"role":         "tool",
				"tool_call_id": toolID,
				"content":      result,
			})
		}
		if doomed {
			break
		}

		// Follow-up request with tool results. Each round gets its own timeout
		// so one slow round does not exhaust the whole tool-loop budget.
		req.Contexts = messages
		roundCtx, roundCancel := context.WithTimeout(llmCtx, 120*time.Second)
		resp, err = s.chatRound(roundCtx, ar.chatInst, req, ar.streaming, streamer)
		roundCancel()
		if err != nil {
			logger.Error("LLM tool-loop call failed: %v", err)
			event.Result = &message.MessageEventResult{}
			event.Result.Chain = []message.Component{&message.Plain{Text: "😕 LLM 调用失败: " + err.Error()}}
			return nil, false
		}
		s.recordProviderCall(ar.providerCfg, event.UnifiedMsgOrigin(), resp)
	}

	if resp.Role == "err" {
		event.Result = &message.MessageEventResult{}
		event.Result.Chain = []message.Component{&message.Plain{Text: "😕 " + resp.CompletionText}}
		return nil, false
	}
	return resp, true
}

// finalizeAgentReply appends the user/assistant pair to the conversation
// history, fires the on_llm_response hooks, persists the reply for enabled
// group sessions (group LTM), flushes the stream and sets event.Result.
func (s *ProcessStage) finalizeAgentReply(ar *agentRequest, resp *provider.LLMResponse, streamer *streamSender) {
	event := ar.event

	// The tool loop ends early when the model hit maxSteps or a tool was paused
	// by doom-loop detection: the last response carries tool calls and usually
	// no text. Record a visible notice instead of polluting history with an
	// empty assistant turn (L-24).
	if strings.TrimSpace(resp.CompletionText) == "" && len(resp.ToolsCallName) > 0 {
		resp.CompletionText = "（已达最大工具调用步数/工具被暂停，未能生成最终回复。）"
	}

	// Append user + assistant reply to history (Python appends the pair
	// post-completion; the user message is intentionally not in req.Contexts
	// since it is sent as the current prompt).
	if s.convMgr != nil {
		s.convMgr.AppendHistory(event.UnifiedMsgOrigin(), "user", ar.prompt)
		s.convMgr.AppendHistory(event.UnifiedMsgOrigin(), "assistant", resp.CompletionText)
	}

	// on_llm_response fires after the LLM reply is produced (e.g. plugins that
	// capture conversation memory). Payload carries the reply text.
	dispatchSubprocessHooksPayload(s.subPlugins, event, "on_llm_response", &pluginsdk.LLMResponse{
		Text: resp.CompletionText,
	})

	// Persist the bot reply for enabled group sessions (mirrors
	// builtin_stars/astrbot/main.py persist_llm_response).
	if s.database != nil && event.Source.IsGroup && event.Source.Platform != "webchat" &&
		s.groupLTMSetting(event, "group_message_history_enable") && resp.CompletionText != "" {
		_ = s.database.RecordPlatformMessage(event.Source.Platform, event.UnifiedMsgOrigin(), event.Source.SelfID, resp.CompletionText)
		keep := providerLTMInt(s.config, "group_message_history_max_cnt", 700)
		_ = s.database.TrimPlatformMessageHistory(event.Source.Platform, event.UnifiedMsgOrigin(), keep)
	}

	streamer.flush()
	if streamer.sentAny() {
		// Text was already streamed to the platform incrementally; mark the
		// event so RespondStage does not send a duplicate full message.
		event.SetExtra("streamed", true)
	}

	event.Result = &message.MessageEventResult{}
	event.Result.Chain = []message.Component{&message.Plain{Text: resp.CompletionText}}
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
		// Anthropic-style XML tool calls (<function_calls>) are parsed into
		// real tool calls (same as the streaming path) so they execute instead
		// of leaking into the reply.
		if calls, ok := parseXMLToolCalls(resp.CompletionText); ok {
			for i, c := range calls {
				resp.ToolsCallName = append(resp.ToolsCallName, c.name)
				resp.ToolsCallArgs = append(resp.ToolsCallArgs, c.args)
				resp.ToolsCallIDs = append(resp.ToolsCallIDs, fmt.Sprintf("xml_%d", i))
			}
			logger.I18nInfo("解析到 %d 个 XML 工具调用并转为标准工具调用", len(calls))
		}
		resp.CompletionText = stripToolCallXML(resp.CompletionText)
		logger.Debug("LLM call completed in %v, text_len=%d", time.Since(start), len(resp.CompletionText))
		return resp, nil
	}
	streamCh, err := inst.TextChatStream(ctx, req)
	if err != nil {
		return nil, err
	}
	full := &provider.LLMResponse{Role: "assistant", CompletionText: "", ToolsCallName: []string{}, ToolsCallArgs: []map[string]interface{}{}, ToolsCallIDs: []string{}}
	var content, reasoning strings.Builder
	// ctrlPending accumulates the tail of the stream that might be a control
	// marker split across chunks; only the confirmed-safe prefix is released.
	var ctrlPending string
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
			reasoning.WriteString(chunk.GetReasoningContent())
			// Suppress model control markup (XML tool calls, advisor/reasoning
			// tags) from the user-facing stream; parsed/handled at completion.
			// A marker split across chunks is caught by holding back the suffix
			// that could begin a marker until the next chunk resolves it (L-21).
			ctrlPending += chunk.CompletionText
			if containsControlText(ctrlPending) {
				// A (possibly split) control marker is present; drop it all so
				// none of it leaks to the user.
				ctrlPending = ""
			} else {
				safe := len(ctrlPending) - controlTextPendingLen(ctrlPending)
				if safe > 0 {
					streamer.push(ctrlPending[:safe])
					ctrlPending = ctrlPending[safe:]
				}
			}
			// Display the reasoning content when provider_settings.
			// display_reasoning_text is enabled (mirrors the Python
			// `chain.type == "reasoning" and not show_reasoning: continue`).
			// 通过回退取值接口 GetReasoningContent 读取，空值返回空串，不显示。
			if s.providerConf != nil && s.providerConf.DisplayReasoningText &&
				chunk.GetReasoningContent() != "" {
				streamer.push(chunk.GetReasoningContent())
			}
			continue
		}
		if len(chunk.ToolsCallName) > 0 {
			full.ToolsCallName = chunk.ToolsCallName
			full.ToolsCallArgs = chunk.ToolsCallArgs
			full.ToolsCallIDs = chunk.ToolsCallIDs
		}
		if u := chunk.GetUsage(); u != nil {
			full.Usage = u
		}
		if chunk.CompletionText != "" && !chunk.IsChunk {
			// The final consolidated chunk carries the authoritative full text;
			// replace (not append to) the accumulated deltas to avoid doubling.
			content.Reset()
			content.WriteString(chunk.CompletionText)
		}
	}
	// Release any buffered tail that never resolved into a control marker
	// (the stream ended without completing one).
	if ctrlPending != "" {
		streamer.push(ctrlPending)
		ctrlPending = ""
	}
	full.CompletionText = content.String()
	full.ReasoningContent = reasoning.String()
	// Anthropic-style XML tool calls (<function_calls>) are parsed into real
	// tool calls so they execute instead of leaking into the reply.
	if calls, ok := parseXMLToolCalls(full.CompletionText); ok {
		for i, c := range calls {
			full.ToolsCallName = append(full.ToolsCallName, c.name)
			full.ToolsCallArgs = append(full.ToolsCallArgs, c.args)
			full.ToolsCallIDs = append(full.ToolsCallIDs, fmt.Sprintf("xml_%d", i))
		}
		full.CompletionText = stripToolCallXML(full.CompletionText)
		logger.I18nInfo("解析到 %d 个 XML 工具调用并转为标准工具调用", len(calls))
	}
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
	logger.Debug("stream segment send: %.300s", text)
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
	// 通过回退取值接口读取 usage，缺失时返回 0，避免 nil 解引用。
	input := resp.UsageInput()
	output := resp.UsageOutput()
	now := float64(time.Now().UnixMilli()) / 1000
	_ = s.database.RecordProviderCall(umo, providerID, model, input, 0, output, now, now)
}

// applySkills appends the active-skills section to the system prompt.
// It mirrors Python's astr_main_agent._ensure_persona_and_skills:
//   - list active skills for the configured computer_use_runtime;
//   - drop plugin skills whose plugin is deactivated or excluded by the
//     plugin_set allow-list (mirrors _filter_skills_for_current_config);
//   - for the "local" runtime, merge request-scoped workspace skills
//     (workspace overrides same-name skills, sorted by name);
//   - honor the persona skill allow-list (nil = unrestricted, empty =
//     disabled; workspace skills are skipped only when disabled);
//   - a runtime of "none" warns that Computer Use is disabled.
func (s *ProcessStage) applySkills(systemPrompt string, providerSettings map[string]interface{}, personaID string, umo string) string {
	if s.skillMgr == nil {
		return systemPrompt
	}
	runtime := "local"
	if s.providerConf != nil && s.providerConf.ComputerUseRuntime != "" {
		runtime = s.providerConf.ComputerUseRuntime
	}
	active := s.skillMgr.ListSkills(true, runtime)
	active = filterSkillsForCurrentConfig(active, s.config)
	var workspaceSkills []*skills.SkillInfo
	if runtime == "local" {
		workspaceSkills = s.skillMgr.ListWorkspaceSkills(workspaceRoot(umo))
	}

	if len(active) > 0 || len(workspaceSkills) > 0 {
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
		// Workspace skills merge AFTER persona filtering: a disabled persona
		// (persona.skills == []) excludes workspace skills too; otherwise
		// workspace skills override same-name entries and the merged map is
		// sorted by name (mirrors astr_main_agent.py:586-590).
		if len(workspaceSkills) > 0 && personaWorkspaceUnrestricted(s.personaSkills, personaID) {
			byName := make(map[string]*skills.SkillInfo, len(active)+len(workspaceSkills))
			for _, sk := range active {
				byName[sk.Name] = sk
			}
			for _, sk := range workspaceSkills {
				byName[sk.Name] = sk
			}
			names := make([]string, 0, len(byName))
			for name := range byName {
				names = append(names, name)
			}
			sort.Strings(names)
			active = make([]*skills.SkillInfo, 0, len(byName))
			for _, name := range names {
				active = append(active, byName[name])
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

// personaWorkspaceUnrestricted reports whether workspace skills may merge:
// Python gates the merge on `not persona or persona.get("skills") != []`,
// i.e. only a persona explicitly configured with an empty skills list blocks
// workspace skills.
func personaWorkspaceUnrestricted(personaSkills func(personaID string) []string, personaID string) bool {
	if personaSkills == nil || personaID == "" {
		return true
	}
	return personaSkills(personaID) == nil
}

// filterSkillsForCurrentConfig mirrors Python's
// astr_main_agent._filter_skills_for_current_config: plugin-sourced skills
// require their plugin to be registered+activated and (unless no allow-list
// is configured) be included in the plugin_set config. Non-plugin skills
// pass through. In this Go host every subprocess plugin registered in the
// star registry is activated (subprocess_bridge registers Activated: true),
// so activation == registry presence.
func filterSkillsForCurrentConfig(list []*skills.SkillInfo, config map[string]interface{}) []*skills.SkillInfo {
	if len(list) == 0 {
		return list
	}
	var allowedPlugins map[string]bool
	if raw, ok := config["plugin_set"].([]interface{}); ok {
		star := false
		names := map[string]bool{}
		for _, v := range raw {
			if name, ok := v.(string); ok {
				if name == "*" {
					star = true
				}
				names[name] = true
			}
		}
		if !star {
			allowedPlugins = names
		}
	}
	registered := activePluginIDs()
	filtered := make([]*skills.SkillInfo, 0, len(list))
	for _, sk := range list {
		if sk.SourceType != skills.SourcePlugin {
			filtered = append(filtered, sk)
			continue
		}
		if !registered[sk.PluginName] {
			continue
		}
		if allowedPlugins == nil || allowedPlugins[sk.PluginName] {
			filtered = append(filtered, sk)
		}
	}
	return filtered
}

// activePluginIDs snapshots the activated plugin ids (subprocess plugin
// registry; mirrors iterating star_registry for plugin.activated). The
// package-level indirection keeps filterSkillsForCurrentConfig testable
// without a full ProcessStage.
var activePluginIDs = func() map[string]bool {
	// Default no-op: without a wired snapshot no plugin id is known, so
	// plugin-sourced skills would be dropped. Pipeline initialization always
	// wires this (see SetActivePluginIDsProvider).
	return nil
}

// SetActivePluginIDsProvider wires the plugin-activation snapshot used by
// filterSkillsForCurrentConfig. Called by ProcessStage.Initialize.
func SetActivePluginIDsProvider(fn func() map[string]bool) {
	if fn == nil {
		return
	}
	activePluginIDs = fn
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
// Event matches the given event name (payload-less). These are
// pipeline-adjacent events (on_message, on_llm_response, on_after_message_sent,
// on_waiting_llm_request, on_tool_call) that are not star filter handlers.
func dispatchSubprocessHooks(sub *plugin.SubprocessManager, event *core.Event, hookEvent string) {
	dispatchSubprocessHooksPayload(sub, event, hookEvent, nil)
}

// dispatchBridgeHooks pushes serialized events to plugins that registered a
// bridge hook (botpy/telegram compat layers). The registry is empty unless a
// plugin opted in, so the common path is a single nil-check return.
func dispatchBridgeHooks(sub *plugin.SubprocessManager, event *core.Event) {
	if sub == nil {
		return
	}
	hooks := sub.BridgeHookSnapshot()
	if len(hooks) == 0 {
		return
	}
	sdkEvent := star.CoreEventToSDKEvent(event)
	for instID, names := range hooks {
		inst := sub.Get(instID)
		if inst == nil || inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, name := range names {
			rpcCtx, cancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			_, _, _, err := inst.Client.HandleHookWithPayload(rpcCtx, name, sdkEvent, nil, nil)
			cancel()
			if err != nil {
				logger.I18nWarn("插件 %s 桥接钩子 %s 执行失败: %v", inst.Name, name, err)
			}
		}
	}
}

// dispatchSubprocessHooksPayload is dispatchSubprocessHooks with a JSON payload
// for payload-carrying events (on_llm_response → sdk.LLMResponse,
// on_using_llm_tool/on_llm_tool_respond → sdk.ToolCall, on_plugin_error →
// sdk.PluginError).
func dispatchSubprocessHooksPayload(sub *plugin.SubprocessManager, event *core.Event, hookEvent string, payload any) {
	if sub == nil {
		return
	}
	sdkEvent := star.CoreEventToSDKEvent(event)
	for _, inst := range sub.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != hookEvent {
				continue
			}
			// 钩子为被动广播，不计入活动时间（否则带钩子插件永不休眠）。
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			_, _, res, err := inst.Client.HandleHookWithPayload(rpcCtx, h.Name, sdkEvent, nil, payload)
			rpcCancel()
			if err != nil {
				logger.I18nWarn("插件 %s 钩子 %s (%s) 执行失败: %v", inst.Name, h.Name, hookEvent, err)
			}
			if res.Sent && event != nil {
				// 插件在钩子中主动发送过（对齐 Python _has_send_oper）。
				event.HasSendOper = true
			}
		}
	}
}

// applyLLMRequestHooks runs every loaded subprocess plugin's on_llm_request
// hooks, letting them modify the system prompt, the user prompt, or stop the
// LLM call.
func (s *ProcessStage) applyLLMRequestHooks(event *core.Event, systemPrompt, userPrompt string) (string, string, bool, error) {
	if s.subPlugins == nil {
		return systemPrompt, userPrompt, false, nil
	}
	sdkEvent := star.CoreEventToSDKEvent(event)
	for _, inst := range s.subPlugins.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, h := range inst.Meta.Hooks {
			if h.Event != "on_llm_request" {
				continue
			}
			// on_llm_request 是被动广播，不计入活动时间（但计入进行中 RPC，
			// 防止执行中被闲置清扫回收）。
			defer inst.RPCGuardPassive()()
			rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
			sp, up, stop, res, err := inst.Client.HandleLLMRequest(rpcCtx, h.Name, sdkEvent, systemPrompt, userPrompt)
			rpcCancel()
			if err != nil {
				logger.I18nWarn("插件 %s 的 on_llm_request 钩子 %s 执行失败: %v", inst.Name, h.Name, err)
				continue
			}
			if res.Sent {
				event.HasSendOper = true
			}
			systemPrompt = sp
			userPrompt = up
			if stop {
				return systemPrompt, userPrompt, true, nil
			}
		}
	}
	return systemPrompt, userPrompt, false, nil
}

// toolsRefreshTTL 是插件工具列表 ListTools RPC 的缓存时长：TTL 内跳过重复
// 刷新，避免每次 LLM 调用都对每个插件同步发起一次 RPC。
const toolsRefreshTTL = 5 * time.Minute

// collectPluginTools returns the OpenAI tool schemas contributed by all loaded
// subprocess plugins (each plugin's registered LLM function tools).
func (s *ProcessStage) collectPluginTools() []map[string]interface{} {
	if s.subPlugins == nil {
		return nil
	}
	// 先刷新运行中插件的实时工具列表（插件工具在实例化阶段注册，晚于
	// Register 快照；RefreshTools 成功后回写管理器工具注册表）。TTL 内已
	// 刷新过的插件跳过，避免每次 LLM 调用都同步发起 ListTools RPC。TTL
	// 过期时先用旧快照（AllPluginTools 兜底），后台异步刷新，下次请求生效
	// ——同步刷新会阻塞 LLM 热路径最长 30s/插件（插件假死/被调试器暂停时
	// 用户消息长时间无响应）。
	for _, inst := range s.subPlugins.List() {
		if inst.Meta == nil {
			continue
		}
		if inst.ToolsFreshWithin(toolsRefreshTTL) {
			continue
		}
		go inst.RefreshTools(context.Background())
	}
	// 注入全部已注册工具（含闲置休眠插件：其工具保留在注册表中，LLM 调用
	// 时按名唤醒——避免休眠导致 LLM 工具集收缩）。
	seen := make(map[string]bool, len(s.subPlugins.List()))
	var out []map[string]interface{}
	for _, e := range s.subPlugins.AllPluginTools() {
		t := e.Desc
		if t == nil || seen[t.Name] {
			continue
		}
		seen[t.Name] = true
		params := map[string]interface{}{}
		if len(t.ParamsJson) > 0 {
			_ = json.Unmarshal(t.ParamsJson, &params)
		}
		safeName := pluginToolSafeName(t.Name)
		if safeName == "" {
			continue
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        safeName,
				"description": t.Description,
				"parameters":  params,
			},
		})
	}
	return out
}

// executePluginTool dispatches a tool call to the subprocess plugin that
// registered a tool with the given name. Returns (result, handled).
func (s *ProcessStage) executePluginTool(event *core.Event, name string, args map[string]interface{}) (string, bool) {
	if s.subPlugins == nil {
		return "", false
	}
	sdkEvent := star.CoreEventToSDKEvent(event)
	// 1) 运行中实例直接命中。
	for _, inst := range s.subPlugins.List() {
		if inst.Client == nil || inst.Meta == nil {
			continue
		}
		for _, t := range inst.ToolsSnapshot() {
			// LLM calls use the sanitized name; the plugin RPC uses the original.
			if pluginToolSafeName(t.Name) != name {
				continue
			}
			return s.dispatchPluginTool(inst, t, event, name, args, sdkEvent)
		}
	}
	// 2) 注册表兜底：工具属于闲置休眠的插件 → EnsureLoaded 唤醒后再分发。
	if id, ok := s.subPlugins.ToolOwner(name); ok {
		ctx, cancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
		inst, err := s.subPlugins.EnsureLoaded(ctx, id)
		cancel()
		if err != nil {
			return fmt.Sprintf("插件工具 %s 所属插件 %s 唤醒失败: %v", name, id, err), true
		}
		if inst == nil || inst.Client == nil {
			return fmt.Sprintf("插件工具 %s 所属插件 %s 未就绪", name, id), true
		}
		inst.Touch()
		inst.RefreshTools(context.Background())
		for _, t := range inst.ToolsSnapshot() {
			if pluginToolSafeName(t.Name) != name {
				continue
			}
			return s.dispatchPluginTool(inst, t, event, name, args, sdkEvent)
		}
	}
	return "", false
}

// dispatchPluginTool invokes one plugin tool RPC and formats the result.
func (s *ProcessStage) dispatchPluginTool(inst *plugin.PluginInstance, t *sdkv1.ToolDesc, event *core.Event, name string, args map[string]interface{}, sdkEvent *sdkv1.SDKEvent) (string, bool) {
	inst.Touch()            // 活动标记：参与闲置卸载判定
	defer inst.RPCGuard()() // 进行中 RPC 计数：防止执行中的工具被闲置清扫回收
	rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
	text, isErr, res, err := inst.Client.HandleTool(rpcCtx, t.Name, args, sdkEvent)
	rpcCancel()
	if err != nil {
		return fmt.Sprintf("插件工具 %s 执行失败: %v", name, err), true
	}
	if res.Sent {
		event.HasSendOper = true
	}
	if isErr {
		return "插件工具 " + name + " 返回错误: " + text, true
	}
	return text, true
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
	tools = append(tools, s.mcpSchemasSnapshot()...)
	// Subprocess plugin LLM function tools.
	tools = append(tools, s.collectPluginTools()...)

	// Subagent handoff tools (transfer_to_<name>).
	if s.subAgentEnabled {
		tools = append(tools, subAgentToolSchemas(s.subAgents)...)
	}

	// Web search + extract tools — injected only when the matching provider is
	// enabled AND its API key is configured (per provider_settings).
	provider, webOn := webSearchProviderInfo(s.config)
	if webOn {
		switch provider {
		case "tavily":
			if len(tavilyKeys(s.config)) > 0 {
				tools = append(tools, tavilySearchToolSchema(), tavilyExtractToolSchema())
			}
		case "bocha":
			if len(bochaKeys(s.config)) > 0 {
				tools = append(tools, bochaSearchToolSchema())
			}
		case "brave":
			if len(braveKeys(s.config)) > 0 {
				tools = append(tools, braveSearchToolSchema())
			}
		case "firecrawl":
			if len(firecrawlKeys(s.config)) > 0 {
				tools = append(tools, firecrawlSearchToolSchema(), firecrawlExtractToolSchema())
			}
		case "baidu_ai_search":
			if providerString(s.config, "websearch_baidu_app_builder_key") != "" {
				tools = append(tools, baiduSearchToolSchema())
			}
		case "exa":
			if len(exaKeys(s.config)) > 0 {
				tools = append(tools, exaSearchToolSchema(), exaContentsToolSchema())
			}
		case "anysearch":
			// 对齐 Python v4.28.0 #9767 AnySearch：支持匿名调用，key 列表
			// 为空时也注入工具（执行时会发一次无 Authorization 的请求）。
			tools = append(tools, anySearchToolSchema())
		}
	}

	// send_message_to_user: proactive messaging (always available when a
	// platform manager exists).
	if s.platformMgr != nil {
		tools = append(tools, sendMessageToolSchema())
	}

	// get_group_message_history: enabled via provider_ltm_settings.
	if providerLTMBool(s.config, "group_message_history_enable") {
		tools = append(tools, groupHistoryToolSchema())
	}

	// astr_kb_search: agentic knowledge-base mode.
	if kbAgenticMode(s.config) {
		tools = append(tools, kbSearchToolSchema())
	}
	return tools
}

// collectLightTools returns the tool schemas with empty parameters (only name +
// description). Used by skills_like mode to reduce token usage; the arguments
// are filled in by a follow-up re-query when the LLM chooses a tool.
func (s *ProcessStage) collectLightTools(computerUseRuntime string) []map[string]interface{} {
	all := s.collectTools(computerUseRuntime)
	for i, tool := range all {
		if _, ok := tool["function"].(map[string]interface{}); !ok {
			continue
		}
		// Deep-copy the schema before rewriting parameters so MCP schemas
		// shared with the cached s.mcpSchemas map are not polluted.
		cloned := deepCopyInterface(tool).(map[string]interface{})
		cfn, _ := cloned["function"].(map[string]interface{})
		cfn["parameters"] = map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
		all[i] = cloned
	}
	return all
}

// deepCopyInterface returns a deep copy of a JSON-like value (map, slice or
// scalar) so callers can mutate a schema without affecting the original.
func deepCopyInterface(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		cp := make(map[string]interface{}, len(t))
		for k, val := range t {
			cp[k] = deepCopyInterface(val)
		}
		return cp
	case []interface{}:
		cp := make([]interface{}, len(t))
		for i, val := range t {
			cp[i] = deepCopyInterface(val)
		}
		return cp
	case []map[string]interface{}:
		cp := make([]map[string]interface{}, len(t))
		for i, val := range t {
			cp[i] = deepCopyInterface(val).(map[string]interface{})
		}
		return cp
	default:
		return v
	}
}

// collectParamToolsFor returns the full-parameters schemas of the named tools
// (description kept minimal). Used by the skills_like re-query.
func (s *ProcessStage) collectParamToolsFor(computerUseRuntime string, names []string) []map[string]interface{} {
	all := s.collectTools(computerUseRuntime)
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []map[string]interface{}
	for _, tool := range all {
		fn, ok := tool["function"].(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := fn["name"].(string)
		if !want[name] {
			continue
		}
		out = append(out, tool)
	}
	return out
}

// requeryToolArgs re-queries the LLM with the chosen tools' full parameter
// schemas so it produces concrete arguments (skills_like mode). Unlike the
// Python reference, the re-query context is minimal (original prompt + an
// explicit instruction) instead of the full conversation history, which avoids
// the model re-deciding the tool selection and saves tokens. Returns ok=false
// when the re-query fails or returns no tool call, in which case the caller
// keeps the original response.
func (s *ProcessStage) requeryToolArgs(ctx context.Context, chatInst provider.ChatProvider, req *provider.ProviderRequest, resp *provider.LLMResponse, computerUseRuntime string) (*provider.LLMResponse, bool) {
	paramTools := s.collectParamToolsFor(computerUseRuntime, resp.ToolsCallName)
	if len(paramTools) == 0 {
		return resp, false
	}
	instruction := "这是工具调用参数补全阶段。用户请求要求调用以下工具：" +
		strings.Join(resp.ToolsCallName, "、") +
		"。请根据原始用户请求，选择一个最合适的工具并调用它，提供完整、准确的参数。不要忽略工具调用，不要返回无关文本。"
	req2 := &provider.ProviderRequest{
		Prompt:                req.Prompt,
		SessionID:             req.SessionID,
		SystemPrompt:          req.SystemPrompt + "\n\n" + instruction,
		Tools:                 paramTools,
		Conversation:          req.Conversation,
		ExtraUserContentParts: req.ExtraUserContentParts,
	}
	rctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	requery, err := chatInst.TextChat(rctx, req2)
	if err != nil {
		logger.I18nWarn("skills_like 工具参数补全失败: %v", err)
		return resp, false
	}
	if len(requery.ToolsCallName) == 0 {
		logger.I18nWarn("skills_like 工具参数补全未返回工具调用，使用原响应")
		return resp, false
	}
	return requery, true
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
// stall the pipeline. 连接过程在锁外进行（无锁构建局部 clients/schemas），
// 最后一次性持锁替换，慢服务器不会阻塞 mcpMu 的并发读。
func (s *ProcessStage) loadMCPTools() {
	data, err := os.ReadFile("data/mcp_server.json")
	if err != nil {
		s.mcpMu.Lock()
		s.mcpClients = nil
		s.mcpSchemas = nil
		s.mcpMu.Unlock()
		return
	}
	var mcpCfg struct {
		McpServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &mcpCfg); err != nil {
		logger.I18nWarn("解析 data/mcp_server.json 失败: %v", err)
		s.mcpMu.Lock()
		s.mcpClients = nil
		s.mcpSchemas = nil
		s.mcpMu.Unlock()
		return
	}

	// 无锁构建局部 clients/schemas。
	clients := make(map[string]*agent.MCPClient)
	schemas := make(map[string]map[string]interface{})

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
		clients[safeName] = client
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
			schemas[fullName] = schema
		}
		logger.Debug("MCP server %q connected (%d tools)", name, len(client.Tools()))
	}

	// 持锁清理旧客户端并一次性替换为新状态。
	s.mcpMu.Lock()
	for _, cl := range s.mcpClients {
		cl.Cleanup()
	}
	s.mcpClients = clients
	s.mcpSchemas = schemas
	s.mcpLoaded = true
	s.mcpMu.Unlock()
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
	conn := client.Conn()
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := client.CallTool(callCtx, toolName, args)
	if err != nil {
		// A transport failure (e.g. SSE connection lost) can leave the client
		// unable to receive responses; reconnect once and retry with a fresh
		// timeout. Pass the connection the failed call used so a concurrent
		// reconnect that already rebuilt the connection is not torn down again.
		logger.I18nWarn("MCP 工具 %s 调用失败 (%v)，正在重连并重试…", name, err)
		reconnCtx, reconnCancel := context.WithTimeout(ctx, 30*time.Second)
		rc := client.Reconnect(reconnCtx, conn)
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

// webFetchMaxBytes caps how much of a fetched response body is read so a
// malicious server cannot dump an unbounded payload into the model context.
const webFetchMaxBytes = 4 << 20

// maxRedirects bounds how many redirects web_fetch follows before giving up.
const maxRedirects = 10

var (
	// cloudMetadataAddr is the well-known AWS/GCP/Azure metadata endpoint that
	// must never be reachable from the fetcher.
	cloudMetadataAddr = netip.MustParseAddr("169.254.169.254")
	// blockedNetPrefixes are extra reserved ranges (e.g. CGNAT space used by
	// some cloud metadata endpoints) rejected on top of the built-in netip
	// classifications.
	blockedNetPrefixes = []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),
	}
)

// validateWebFetchHost resolves host and rejects loopback, private, link-local,
// multicast, CGNAT and cloud-metadata addresses. It fails closed when the host
// cannot be resolved at all.
func validateWebFetchHost(host string) error {
	if host == "" {
		return fmt.Errorf("url 缺少主机名")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("域名解析失败: %v", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("域名 %q 无解析结果", host)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		switch {
		case addr == cloudMetadataAddr:
			return fmt.Errorf("拒绝访问云元数据地址 %s", addr)
		case addr.IsLoopback():
			return fmt.Errorf("拒绝访问环回地址 %s", addr)
		case addr.IsPrivate():
			return fmt.Errorf("拒绝访问私网地址 %s", addr)
		case addr.IsLinkLocalUnicast():
			return fmt.Errorf("拒绝访问链路本地地址 %s", addr)
		case addr.IsLinkLocalMulticast():
			return fmt.Errorf("拒绝访问链路本地组播地址 %s", addr)
		case addr.IsMulticast():
			return fmt.Errorf("拒绝访问组播地址 %s", addr)
		case addr.IsUnspecified():
			return fmt.Errorf("拒绝访问未指定地址 %s", addr)
		}
		for _, p := range blockedNetPrefixes {
			if p.Contains(addr) {
				return fmt.Errorf("拒绝访问保留地址段 %s 内的地址 %s", p, addr)
			}
		}
	}
	return nil
}

// validateWebFetchURL parses rawURL and verifies it is an http(s) URL whose
// host is safe to fetch. It returns a normalized URL (fragment stripped) for
// the actual request.
func validateWebFetchURL(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("无法解析 url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("url 必须以 http:// 或 https:// 开头")
	}
	if err := validateWebFetchHost(u.Hostname()); err != nil {
		return "", err
	}
	u.Fragment = ""
	return u.String(), nil
}

// webFetchRedirectGuard re-validates the destination of every redirect hop so
// a chain cannot bounce into a blocked address after the initial check.
func webFetchRedirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("重定向次数过多")
	}
	if req.URL == nil {
		return fmt.Errorf("重定向目标缺少 URL")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("重定向目标必须以 http:// 或 https:// 开头")
	}
	return validateWebFetchHost(req.URL.Hostname())
}

// allowedWebFetchContentType reports whether a response Content-Type is safe to
// surface to the model. Only textual payloads are allowed; binary media
// (images, archives, PDFs, ...) is rejected.
func allowedWebFetchContentType(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case ct == "":
		return true
	case strings.HasPrefix(ct, "text/"):
		return true
	case ct == "application/json":
		return true
	case ct == "application/xml":
		return true
	case ct == "application/javascript" || ct == "application/x-javascript":
		return true
	default:
		return false
	}
}

// executeWebFetch fetches a URL and converts its content to plain text,
// truncated to maxLength characters.
func executeWebFetch(rawURL string, maxLength int) string {
	if strings.TrimSpace(rawURL) == "" {
		return "web_fetch 错误: 缺少 url 参数"
	}
	if maxLength <= 0 {
		maxLength = 20000
	}
	if maxLength > 200000 {
		maxLength = 200000
	}

	reqURL, err := validateWebFetchURL(rawURL)
	if err != nil {
		return "web_fetch 错误: " + err.Error()
	}

	// Dial 时校验解析后的真实 IP：validateWebFetchHost 的 LookupIP 与真正
	// 建连之间可能发生 DNS rebinding，连接时刻的校验封死该 TOCTOU 窗口。
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			addr, err := netip.ParseAddr(host)
			if err != nil {
				return err
			}
			addr = addr.Unmap()
			if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
				addr.IsMulticast() || addr.IsUnspecified() || addr == cloudMetadataAddr {
				return fmt.Errorf("拒绝连接到受限地址 %s", addr)
			}
			for _, p := range blockedNetPrefixes {
				if p.Contains(addr) {
					return fmt.Errorf("拒绝连接到保留地址段 %s 内的地址 %s", p, addr)
				}
			}
			return nil
		},
	}
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: webFetchRedirectGuard,
		Transport:     &http.Transport{DialContext: dialer.DialContext},
	}
	resp, err := client.Get(reqURL)
	if err != nil {
		return fmt.Sprintf("web_fetch 错误: 请求失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("web_fetch 错误: HTTP %d", resp.StatusCode)
	}
	if !allowedWebFetchContentType(resp.Header.Get("Content-Type")) {
		return fmt.Sprintf("web_fetch 错误: 不支持的内容类型 %q", resp.Header.Get("Content-Type"))
	}

	limited := io.LimitReader(resp.Body, int64(webFetchMaxBytes))
	body, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Sprintf("web_fetch 错误: 读取失败: %v", err)
	}

	text := string(body)
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") {
		text = htmlToText(body)
	}
	text = strings.TrimSpace(text)
	// 按 rune 截断，避免切在多字节 UTF-8 字符中间产生乱码。
	if r := []rune(text); len(r) > maxLength {
		text = string(r[:maxLength])
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
// coreBuiltinToolSet lists the LLM tools implemented by the host itself
// (built-ins, Computer Use host tools, web-search/KB/message tools and the
// proactive future_task). tool_permissions only governs non-builtin tools —
// the dashboard exposes built-ins as readonly — mirroring Python, where the
// permission guard is applied to function tools registered by MCP servers and
// plugins, never to the core's own executors.
var coreBuiltinToolSet = map[string]bool{
	"get_current_time":           true,
	"web_fetch":                  true,
	"astrbot_execute_shell":      true,
	"astrbot_shell_session":      true,
	"astrbot_execute_python":     true,
	"astrbot_file_read_tool":     true,
	"astrbot_file_write_tool":    true,
	"astrbot_file_edit_tool":     true,
	"astrbot_grep_tool":          true,
	"astrbot_upload_file":        true,
	"astrbot_download_file":      true,
	"web_search_tavily":          true,
	"web_search_bocha":           true,
	"web_search_brave":           true,
	"web_search_firecrawl":       true,
	"web_search_baidu":           true,
	"web_search_exa":             true,
	"web_search_anysearch":       true,
	"tavily_extract_web_page":    true,
	"firecrawl_extract_web_page": true,
	"exa_get_contents":           true,
	"send_message_to_user":       true,
	"get_group_message_history":  true,
	"astr_kb_search":             true,
	"future_task":                true,
}

// parseToolPermissions reads config["tool_permissions"] into a tool name ->
// permission level map. Both the dashboard shape
// {"<tool>": {"permission": "admin"|"member"}} and a bare "<tool>": "admin"
// value are accepted (the python-sdk guard parses both shapes too); unknown
// levels are ignored so a malformed entry never locks a tool out.
func parseToolPermissions(config map[string]interface{}) map[string]string {
	raw, _ := config["tool_permissions"].(map[string]interface{})
	if len(raw) == 0 {
		return nil
	}
	perms := make(map[string]string, len(raw))
	for name, v := range raw {
		level := ""
		switch lv := v.(type) {
		case string:
			level = lv
		case map[string]interface{}:
			level, _ = lv["permission"].(string)
		}
		if level == "admin" || level == "member" {
			perms[name] = level
		}
	}
	if len(perms) == 0 {
		return nil
	}
	return perms
}

// adminOnlyTools returns the admin-only tool names configured in
// tool_permissions. The map is parsed once from the stage's config snapshot
// and cached: s.config is the deep copy taken at Initialize (config.Get/All
// already return copies, so the read is race-free) and the whole pipeline is
// rebuilt whenever the dashboard saves the config, so the cache never goes
// stale within a stage lifetime — repeated tool calls in one request (or
// across requests served by this stage) do not re-parse the config.
func (s *ProcessStage) adminOnlyTools() map[string]string {
	s.toolPermsMu.Lock()
	defer s.toolPermsMu.Unlock()
	if s.toolPerms == nil {
		s.toolPerms = parseToolPermissions(s.config)
	}
	return s.toolPerms
}

// toolPermissionDenied reports whether the event's role is not allowed to run
// the named tool under config tool_permissions (the dashboard tools panel). A
// tool configured as "admin" is refused for member events; builtin tools and
// the host-generated transfer_to_* subagent handoffs always pass. This is the
// host-side closure of the python-sdk permission guard, which has no host RPC
// to consult (review 1.2-6): MCP and plugin tools are both governed here.
func (s *ProcessStage) toolPermissionDenied(name string, event *core.Event) bool {
	if event.Role == "admin" {
		return false
	}
	if coreBuiltinToolSet[name] || strings.HasPrefix(name, "transfer_to_") {
		return false
	}
	return s.adminOnlyTools()[name] == "admin"
}

func (s *ProcessStage) executeTool(ctx context.Context, event *core.Event, runtime, name string, args map[string]interface{}) string {
	umo := event.UnifiedMsgOrigin()
	logger.Debug("executeTool: name=%s args=%v", name, args)

	// Host-side tool permission guard: a tool marked admin-only in
	// tool_permissions refuses to run for member events. Checked before any
	// dispatch so neither the executors nor the on_tool_call hooks observe a
	// denied invocation.
	if s.toolPermissionDenied(name, event) {
		logger.I18nWarn("工具 %s 需要管理员权限，用户 %s 无权调用", name, event.GetSenderID())
		return fmt.Sprintf("工具 %s 需要管理员权限，当前用户无权调用", name)
	}

	// Dispatch registered plugins' on_tool_call / on_using_llm_tool hooks before
	// executing the tool, stashing the tool name/args on the event metadata for
	// them to read and carrying a sdk.ToolCall payload.
	if s.subPlugins != nil {
		if event.Metadata == nil {
			event.Metadata = make(map[string]interface{})
		}
		event.Metadata["tool_name"] = name
		event.Metadata["tool_args"] = args
		call := &pluginsdk.ToolCall{Name: name, Args: args}
		dispatchSubprocessHooksPayload(s.subPlugins, event, "on_tool_call", call)
		dispatchSubprocessHooksPayload(s.subPlugins, event, "on_using_llm_tool", call)
	}

	result := ""
	handled := false
	if strings.HasPrefix(name, "transfer_to_") {
		// Subagent handoff: run the subagent's persona round and return its
		// reply as the tool result.
		if r, h := s.executeSubAgent(event, name, args); h {
			result, handled = r, true
		}
	}
	if !handled && runtime == "sandbox" {
		if r, h := s.executeSandboxTool(ctx, event.UnifiedMsgOrigin(), name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		if r, h := executeBuiltinTool(name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		if r, h := s.executeMCPTool(ctx, name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		if r, h := s.executePluginTool(event, name, args); h {
			result, handled = r, true
		}
	}
	if !handled {
		// Computer Use host tools (shell/python/file/grep) run only on the
		// "local" runtime. collectTools injects them solely for that runtime,
		// but OpenAI-compatible providers do not validate tool names, so an
		// unregistered name could otherwise reach the host executors while
		// Computer Use is disabled (M-19). The sandbox branch above is gated
		// the same way.
		switch name {
		case "astrbot_execute_shell", "astrbot_shell_session", "astrbot_execute_python",
			"astrbot_file_read_tool", "astrbot_file_write_tool", "astrbot_file_edit_tool",
			"astrbot_grep_tool", "astrbot_upload_file", "astrbot_download_file":
			if runtime != "local" {
				result = fmt.Sprintf("工具 %s 未启用（Computer Use 运行时为 %s，需要 local）", name, runtime)
			} else if !computerUseAllowed(s.config, event) {
				// 用户 ACL：本地运行时直接操作宿主机，仅白名单用户可调用。
				result = fmt.Sprintf("工具 %s 执行失败: computer_use 未授权该用户", name)
			} else {
				switch name {
				case "astrbot_execute_shell":
					result = executeLocalShell(umo, event.GetSenderID(), argString(args, "command"), argBool(args, "background"), argInt(args, "timeout", 300))
				case "astrbot_shell_session":
					result = executeShellSession(umo, event.GetSenderID(), argString(args, "action"), argString(args, "session_id"), argString(args, "data"))
				case "astrbot_execute_python":
					result = executeLocalPython(umo, argString(args, "code"), argInt(args, "timeout", 30))
				case "astrbot_file_read_tool":
					result = executeFileRead(argString(args, "path"), umo, argInt(args, "offset", 0), argInt(args, "limit", 0))
				case "astrbot_file_write_tool":
					ws := workspaceRoot(umo)
					before := gitTreeHash(ws)
					r := executeFileWrite(argString(args, "path"), argString(args, "content"), umo)
					result = snapshotFileMutation(ws, before, name, r)
				case "astrbot_file_edit_tool":
					ws := workspaceRoot(umo)
					before := gitTreeHash(ws)
					r := executeFileEdit(argString(args, "path"), argString(args, "old"), argString(args, "new"), argBool(args, "replace_all"), umo)
					result = snapshotFileMutation(ws, before, name, r)
				case "astrbot_grep_tool":
					result = executeGrep(argString(args, "pattern"), argString(args, "path"), argString(args, "glob"), argInt(args, "result_limit", 100), umo)
				case "astrbot_upload_file":
					result = executeLocalUpload(argString(args, "local_path"), umo)
				case "astrbot_download_file":
					result = executeLocalDownload(argString(args, "remote_path"), umo)
				}
			}
		case "future_task":
			// 存三段式 unified_msg_origin（platform_id:MessageType:session_id），
			// 保证 WebUI/Python future_task 解析时 message_type 与 session_id 对位。
			result = executeFutureTask(s.cronMgr, event.PythonUMO(), event.GetSenderID(), args)
		case "web_search_tavily":
			result = executeWebSearchTavily(s.config, args)
		case "web_search_bocha":
			result = executeWebSearchBocha(s.config, args)
		case "web_search_brave":
			result = executeWebSearchBrave(s.config, args)
		case "web_search_firecrawl":
			result = executeWebSearchFirecrawl(s.config, args)
		case "web_search_baidu":
			result = executeWebSearchBaidu(s.config, args)
		case "web_search_exa":
			result = executeWebSearchExa(s.config, args)
		case "web_search_anysearch":
			result = executeWebSearchAnySearch(s.config, args)
		case "tavily_extract_web_page":
			result = executeTavilyExtract(s.config, args)
		case "firecrawl_extract_web_page":
			result = executeFirecrawlExtract(s.config, args)
		case "exa_get_contents":
			result = executeExaGetContents(s.config, args)
		case "send_message_to_user":
			result = s.executeSendMessage(event, args)
		case "get_group_message_history":
			result = s.executeGroupHistory(event, args)
		case "astr_kb_search":
			result = s.executeKBSearch(event, args)
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

// executeSandboxTool routes computer-use tools into the per-session sandbox
// runtime. sessionID 是事件的 unified_msg_origin（群/私聊），每个会话独立
// 沙盒（对齐 Python session_booter 模型），同一会话的沙盒任务天然串行。
func (s *ProcessStage) executeSandboxTool(ctx context.Context, sessionID, name string, args map[string]interface{}) (string, bool) {
	if s.sandboxMgr == nil {
		// Only sandbox-only tools are reported as unavailable here; any other
		// name must fall through to the remaining executors so it is not
		// swallowed by the missing sandbox.
		switch name {
		case "astrbot_execute_shell", "astrbot_execute_python",
			"astrbot_file_read_tool", "astrbot_file_write_tool",
			"astrbot_file_edit_tool", "astrbot_grep_tool",
			"astrbot_upload_file", "astrbot_download_file":
			return "Sandbox manager not configured.", true
		}
		return "", false
	}
	// Default 300s (aligned with the local shell runtime); the model-supplied
	// timeout is respected but capped so a single call cannot hang forever.
	timeout := argInt(args, "timeout", 0)
	if timeout <= 0 {
		timeout = 300
	}
	if timeout > 600 {
		timeout = 600
	}
	tctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	switch name {
	case "astrbot_execute_shell":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxShell(tctx, s.sandboxMgr, sessionID, argString(args, "command")), true
	case "astrbot_execute_python":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxPython(tctx, s.sandboxMgr, sessionID, argString(args, "code")), true
	case "astrbot_file_read_tool":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxFileRead(tctx, s.sandboxMgr, sessionID, argString(args, "path")), true
	case "astrbot_file_write_tool":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxFileWrite(tctx, s.sandboxMgr, sessionID, argString(args, "path"), argString(args, "content")), true
	case "astrbot_file_edit_tool":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxFileEdit(tctx, s.sandboxMgr, sessionID, argString(args, "path"), argString(args, "old"), argString(args, "new"), argBool(args, "replace_all")), true
	case "astrbot_grep_tool":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxGrep(tctx, s.sandboxMgr, sessionID, argString(args, "pattern"), argString(args, "path")), true
	case "astrbot_upload_file":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxUploadFile(tctx, s.sandboxMgr, sessionID, argString(args, "local_path")), true
	case "astrbot_download_file":
		if err := s.ensureSandboxStarted(tctx, sessionID); err != nil {
			return "Sandbox error: " + err.Error(), true
		}
		return sandboxDownloadFile(tctx, s.sandboxMgr, sessionID, argString(args, "remote_path")), true
	}
	return "", false
}

// ensureSandboxStarted lazily ensures the session's sandbox is booted on first
// use (per-session, mirroring Python get_booter). 内部会做健康检查：会话沙盒
// 已失效（404/TTL 到期）时自动重建。
func (s *ProcessStage) ensureSandboxStarted(ctx context.Context, sessionID string) error {
	if _, err := s.sandboxMgr.EnsureSession(ctx, sessionID); err != nil {
		return err
	}
	if s.skillMgr != nil {
		// 先推送宿主 active 技能进 /workspace/skills（对齐 Python
		// computer_client._sync_skills_to_sandbox），再回扫沙盒技能刷新
		// 缓存（含沙盒内置技能——推送后 SyncSkills 才能看到全部条目）。
		if err := s.sandboxMgr.PushHostSkills(ctx, sessionID); err != nil {
			logger.Warn("推送宿主技能到沙盒失败: %v", err)
		}
		s.syncSandboxSkills(ctx, sessionID)
	}
	return nil
}

// syncSandboxSkills syncs sandbox skill metadata with a short retry window.
// /workspace/skills 由 cargo volume 挂载，容器刚就绪时可能尚未挂载完成，首次
// 扫描返回 0 个技能（观察为 "Synced 0 skills from sandbox"），导致 python-
// sandbox 内置技能丢失。重试直到扫到技能或窗口耗尽。
func (s *ProcessStage) syncSandboxSkills(ctx context.Context, sessionID string) {
	for attempt := 0; attempt < 5; attempt++ {
		if err := s.sandboxMgr.SyncSkills(ctx, sessionID); err == nil {
			if st := s.skillMgr.GetSandboxSkillsCacheStatus(); st != nil {
				if ready, _ := st["ready"].(bool); ready {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// resolveProvider picks the provider config to use for this chat.
func (s *ProcessStage) resolveProvider() (map[string]interface{}, map[string]interface{}, error) {
	providers, _ := s.config["provider"].([]interface{})
	// Copy the shared provider_settings map so per-request writes (e.g.
	// persona below) never mutate the shared config that other concurrent
	// requests read (M-21). Only the top level is written by callers, so a
	// shallow copy is sufficient.
	providerSettings := map[string]interface{}{}
	if ps, ok := s.config["provider_settings"].(map[string]interface{}); ok {
		for k, v := range ps {
			providerSettings[k] = v
		}
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
	return nil, nil, errNoAvailableProvider
}

// applySelectedProviderModel applies the dashboard-selected provider/model
// metadata (event.Metadata["selected_provider"] / ["selected_model"], written
// by chat_stream.go) on top of providerCfg. The result is a fresh map so the
// shared provider config is never mutated. With no selection the input map is
// returned unchanged.
func (s *ProcessStage) applySelectedProviderModel(event *core.Event, providerCfg map[string]interface{}) map[string]interface{} {
	if event.Metadata == nil {
		return providerCfg
	}
	selProvider, _ := event.Metadata["selected_provider"].(string)
	selModel, _ := event.Metadata["selected_model"].(string)
	if selProvider == "" && selModel == "" {
		return providerCfg
	}
	pc := make(map[string]interface{}, len(providerCfg)+1)
	for k, v := range providerCfg {
		pc[k] = v
	}
	if selProvider != "" {
		if found := findProviderByID(s.config, selProvider); found != nil {
			pc = make(map[string]interface{}, len(found)+1)
			for k, v := range found {
				pc[k] = v
			}
		}
	}
	if selModel != "" {
		pc["model"] = selModel
	}
	return pc
}

// conversationHistory converts conversation history to LLM context messages.
// The history slice is shallow-copied so a concurrent AppendHistory (from
// another message racing the same session) can never mutate the slice the LLM
// is reading. When provider_settings.max_context_length is set (>0), the
// history is truncated to the most recent 2*max_context_length entries,
// keeping user/assistant message pairs together, and a short system hint notes
// that older history was dropped.
// maybeCompressContext applies context-limit handling: token-based detection
// with either llm_compress (LLM summary + keep-recent) or truncate_by_turns.
// Mirrors Python's context manager (astr_main_agent + agent/context/compressor).
func (s *ProcessStage) maybeCompressContext(ctx context.Context, chatInst provider.ChatProvider, systemPrompt string, contexts []map[string]interface{}) []map[string]interface{} {
	if s.providerConf == nil || len(contexts) == 0 {
		return contexts
	}
	maxCtx := s.providerConf.MaxContextLength
	if maxCtx <= 0 {
		return contexts // unlimited
	}
	curTokens := estimateContextTokens(contexts)
	if curTokens <= 0 {
		return contexts
	}
	// Compression threshold 0.82 (Python default).
	if curTokens <= int(float64(maxCtx)*0.82) {
		return contexts
	}
	if s.providerConf.ContextLimitStrategy == "llm_compress" {
		if compressed, ok := s.llmCompressContext(ctx, chatInst, systemPrompt, contexts); ok {
			return compressed
		}
	}
	// Fallback: keep the most recent 2*max_context_length entries on even
	// boundaries (user/assistant pair intact).
	return truncateContextEntries(contexts, maxCtx)
}

// estimateContextTokens is a rough token estimate (chars/2 for CJK-heavy text,
// ~4 chars per token for others) used for overflow detection.
func estimateContextTokens(contexts []map[string]interface{}) int {
	total := 0
	for _, m := range contexts {
		content, _ := m["content"].(string)
		total += len([]rune(content))
	}
	// ~1 token per 2 runes (approximation).
	return total / 2
}

// truncateContextEntries keeps the recent 2*maxCtx entries aligned to a pair
// boundary, appending a truncation notice.
func truncateContextEntries(contexts []map[string]interface{}, maxCtx int) []map[string]interface{} {
	history := append([]map[string]interface{}{}, contexts...)
	if maxCtx > 0 && len(history) > maxCtx*2 {
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

// llmCompressContext summarizes the older rounds via the LLM and keeps the
// recent rounds exact (mirrors Python LLMSummaryCompressor).
func (s *ProcessStage) llmCompressContext(ctx context.Context, chatInst provider.ChatProvider, systemPrompt string, contexts []map[string]interface{}) ([]map[string]interface{}, bool) {
	keepRatio := s.providerConf.LLMCompressKeepRecentRatio
	if keepRatio < 0 {
		keepRatio = 0
	}
	if keepRatio > 0.3 {
		keepRatio = 0.3
	}
	rounds := splitContextRounds(contexts)
	totalTokens := estimateContextTokens(contexts)
	oldRounds, recentRounds := splitRoundsByRatio(rounds, totalTokens, keepRatio)
	if len(oldRounds) == 0 {
		return contexts, false
	}
	var summaryContexts []map[string]interface{}
	for _, rnd := range oldRounds {
		summaryContexts = append(summaryContexts, rnd...)
	}
	if len(summaryContexts) == 0 || summaryContexts[len(summaryContexts)-1]["role"] != "assistant" {
		summaryContexts = append(summaryContexts, map[string]interface{}{
			"role": "assistant", "content": "Acknowledged.",
		})
	}
	instruction := s.providerConf.LLMCompressInstruction
	if instruction == "" {
		instruction = "Based on our full conversation history, produce a concise summary of key takeaways and/or project progress. The primary goal of this summary is to enable seamless continuation of the work that follows."
	}
	summaryPrompt := "Generate a summary of our previous conversation history.\n" +
		"<extra_instruction>\n" + instruction + "\n\n" +
		"If a task appears to be in progress, end the summary with the latest known result and the concrete next step to continue the task.</extra_instruction>\n" +
		"Respond ONLY with the summary content, without any additional text or formatting."
	summaryContexts = append(summaryContexts, map[string]interface{}{"role": "user", "content": summaryPrompt})

	req := &provider.ProviderRequest{
		Prompt:   summaryPrompt,
		Contexts: summaryContexts[:len(summaryContexts)-1],
	}
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	resp, err := chatInst.TextChat(cctx, req)
	if err != nil || strings.TrimSpace(resp.CompletionText) == "" {
		return contexts, false
	}
	summary := strings.TrimSpace(resp.CompletionText)

	// Rebuild: leading system messages + summary pair + recent rounds.
	var result []map[string]interface{}
	for _, m := range contexts {
		if role, _ := m["role"].(string); role == "system" {
			result = append(result, m)
		} else {
			break
		}
	}
	result = append(result, map[string]interface{}{
		"role": "user", "content": "Our previous history conversation summary: " + summary,
	})
	result = append(result, map[string]interface{}{
		"role": "assistant", "content": "Acknowledged the summary of our previous conversation history.",
	})
	for _, rnd := range recentRounds {
		result = append(result, rnd...)
	}
	return result, true
}

// splitContextRounds splits a message list into user/assistant rounds.
func splitContextRounds(contexts []map[string]interface{}) [][]map[string]interface{} {
	var rounds [][]map[string]interface{}
	var cur []map[string]interface{}
	for _, m := range contexts {
		role, _ := m["role"].(string)
		if role == "system" {
			continue
		}
		if role == "user" && len(cur) > 0 {
			rounds = append(rounds, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		rounds = append(rounds, cur)
	}
	return rounds
}

// splitRoundsByRatio keeps the latest rounds within a token budget.
func splitRoundsByRatio(rounds [][]map[string]interface{}, totalTokens int, keepRatio float64) (oldRounds, recentRounds [][]map[string]interface{}) {
	if len(rounds) == 0 || keepRatio <= 0 || totalTokens <= 0 {
		return rounds, nil
	}
	budget := int(float64(totalTokens) * keepRatio)
	if budget < 1 {
		budget = 1
	}
	used := 0
	recentStart := len(rounds)
	for i := len(rounds) - 1; i >= 0; i-- {
		rndTokens := estimateContextTokens(rounds[i])
		if used > 0 && used+rndTokens > budget {
			break
		}
		used += rndTokens
		recentStart = i
	}
	return rounds[:recentStart], rounds[recentStart:]
}

func (s *ProcessStage) conversationHistory(umo string) []map[string]interface{} {
	if s.convMgr == nil {
		return nil
	}
	convID := s.convMgr.GetCurrConversationID(umo)
	if convID == "" {
		return nil
	}
	// Read through the manager so the History slice is deep-copied under its
	// lock — AppendHistory (another message racing this one) mutates the live
	// History, so reading conv.History directly here would be a data race.
	history := s.convMgr.GetConversationHistory(convID)
	if history == nil {
		return nil
	}

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
	subPlugins       *plugin.SubprocessManager

	// t2i (text-to-image) settings from the top-level config.
	t2iEnabled        bool
	t2iWordThreshold  int
	t2iStrategy       string // "local" | "remote"
	t2iEndpoint       string
	t2iTemplate       string
	t2iUseFileService bool

	// TTS settings (provider_tts_settings) + provider manager.
	providerMgr    *provider.ProviderManager
	convMgr        *conversation.Manager
	ttsEnabled     bool
	ttsTriggerProb float64
	ttsDualOutput  bool

	// forward_threshold: replies longer than this are sent as a forward
	// message (OneBot node) on the aiocqhttp platform.
	forwardThreshold int

	// segmented reply (platform_settings.segmented_reply) settings.
	segEnabled            bool
	segOnlyLLMResult      bool
	segSplitMode          string
	segRegex              string
	segRegexCompiled      *regexp.Regexp
	segSplitWords         []string
	segWordsPattern       *regexp.Regexp
	segContentCleanupRule *regexp.Regexp
	segWordsThreshold     int
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
	s.forwardThreshold = 1500
	if ps.ForwardThreshold > 0 {
		s.forwardThreshold = ps.ForwardThreshold
	}
	s.subPlugins = ctx.SubPlugins

	// Segmented reply settings (mirrors result_decorate/stage.py initialize).
	if sr, ok := ctx.AstrbotConfig["platform_settings"].(map[string]interface{}); ok {
		if cfg, ok := sr["segmented_reply"].(map[string]interface{}); ok {
			s.segEnabled, _ = cfg["enable"].(bool)
			s.segOnlyLLMResult, _ = cfg["only_llm_result"].(bool)
			s.segSplitMode, _ = cfg["split_mode"].(string)
			if s.segSplitMode == "" {
				s.segSplitMode = "regex"
			}
			s.segRegex, _ = cfg["regex"].(string)
			// 配置正则只编译一次（Initialize），避免每条消息热路径重新编译。
			if s.segRegex != "" {
				if re, err := regexp.Compile(s.segRegex); err == nil {
					s.segRegexCompiled = re
				} else {
					logger.Error("Invalid segmented-reply regular expression; using the default segmentation method: %v", err)
				}
			}
			s.segWordsThreshold = 150
			if v, ok := cfg["words_count_threshold"].(int); ok && v > 0 {
				s.segWordsThreshold = v
			}
			if v, ok := cfg["words_count_threshold"].(float64); ok && v > 0 {
				s.segWordsThreshold = int(v)
			}
			s.segSplitWords = []string{"。", "？", "！", "~", "…"}
			if ws := toStringList(cfg["split_words"]); len(ws) > 0 {
				s.segSplitWords = ws
			}
			// Build the words split pattern (Python: (.*?(w1|w2)|.+$) DOTALL).
			if len(s.segSplitWords) > 0 {
				escaped := make([]string, 0, len(s.segSplitWords))
				for _, w := range s.segSplitWords {
					escaped = append(escaped, regexp.QuoteMeta(w))
				}
				sort.SliceStable(escaped, func(i, j int) bool { return len(escaped[i]) > len(escaped[j]) })
				if re, err := regexp.Compile("(.*?(" + strings.Join(escaped, "|") + ")|.+$)"); err == nil {
					s.segWordsPattern = re
				}
			}
			if rule, _ := cfg["content_cleanup_rule"].(string); rule != "" {
				if re, err := regexp.Compile(rule); err == nil {
					s.segContentCleanupRule = re
				}
			}
		}
	}

	// t2i top-level keys: t2i (bool), t2i_word_threshold, t2i_strategy
	// (local/remote), t2i_endpoint, t2i_active_template, t2i_use_file_service.
	s.t2iEnabled, _ = ctx.AstrbotConfig["t2i"].(bool)
	s.t2iWordThreshold = 150
	switch v := ctx.AstrbotConfig["t2i_word_threshold"].(type) {
	case float64:
		if v > 0 {
			s.t2iWordThreshold = int(v)
		}
	case int:
		if v > 0 {
			s.t2iWordThreshold = v
		}
	case int64:
		if v > 0 {
			s.t2iWordThreshold = int(v)
		}
	}
	s.t2iStrategy, _ = ctx.AstrbotConfig["t2i_strategy"].(string)
	s.t2iEndpoint, _ = ctx.AstrbotConfig["t2i_endpoint"].(string)
	s.t2iTemplate, _ = ctx.AstrbotConfig["t2i_active_template"].(string)
	s.t2iUseFileService, _ = ctx.AstrbotConfig["t2i_use_file_service"].(bool)

	// TTS settings from provider_tts_settings.
	s.providerMgr = ctx.ProviderManager
	s.convMgr = ctx.ConvManager
	if ttsCfg, ok := ctx.AstrbotConfig["provider_tts_settings"].(map[string]interface{}); ok {
		s.ttsEnabled, _ = ttsCfg["enable"].(bool)
		if v, ok := ttsCfg["trigger_probability"].(float64); ok {
			s.ttsTriggerProb = v
		}
		s.ttsDualOutput, _ = ttsCfg["dual_output"].(bool)
	}
	if s.ttsTriggerProb <= 0 {
		s.ttsTriggerProb = 1.0
	}
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

	// Segmented reply: split long Plain components into multiple segments
	// (mirrors result_decorate/stage.py). The actual per-segment delivery
	// with intervals happens in RespondStage.
	if s.segEnabled && s.isSegmentedReplyPlatform(event.Source.Platform) {
		if !s.segOnlyLLMResult || event.Result.IsModelResult() {
			s.applySegmentedReply(event)
		}
	}

	// Text-to-image: when enabled and the reply is long enough, replace the
	// plain text with a rendered image (local gg engine or remote t2i service).
	if s.t2iEnabled {
		if err := s.applyT2I(event); err != nil {
			// On failure keep the original text reply.
			logger.I18nWarn("t2i 转换失败，回退文本回复: %v", err)
		}
	}

	// Forward message: on the aiocqhttp (OneBot) platform a reply whose plain
	// text is longer than forward_threshold is sent as a forward node
	// (ported from result_decorate/stage.py). When triggered the chain becomes
	// a single Node and @/quote decorations are skipped (Python can_decorate).
	forwarded := false
	if event.Source.Platform == "aiocqhttp" && s.forwardThreshold > 0 {
		wordCnt := 0
		for _, comp := range event.Result.Chain {
			if plain, ok := comp.(*message.Plain); ok {
				wordCnt += len([]rune(plain.Text))
			}
		}
		if wordCnt > s.forwardThreshold {
			node := &message.Node{
				UIN:     event.Source.SelfID,
				Name:    "AstrBot",
				Content: event.Result.Chain,
			}
			event.Result.Chain = []message.Component{node}
			forwarded = true
			logger.Debug("ResultDecorate: reply > %d chars, sent as forward node", s.forwardThreshold)
		}
	}

	// Apply @mention (only for group messages)
	if !forwarded && s.replyWithMention && event.Source.IsGroup {
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
	if !forwarded && s.replyWithQuote && event.MessageObj != nil && event.MessageObj.MessageID != "" {
		reply := &message.Reply{MessageID: event.MessageObj.MessageID}
		newChain := make([]message.Component, 0, len(event.Result.Chain)+1)
		newChain = append(newChain, reply)
		newChain = append(newChain, event.Result.Chain...)
		event.Result.Chain = newChain
	}

	// TTS: convert the reply to voice when enabled (global switch + session
	// tts_enabled + trigger probability + a usable TTS provider).
	if err := s.applyTTS(event); err != nil {
		logger.I18nWarn("TTS 转换失败，回退文本回复: %v", err)
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

// applyT2I converts a long plain-text reply into an image when the t2i option
// is enabled and the text length reaches t2i_word_threshold. Non-text
// components (at/quote/reply) are preserved; the plain text is replaced by an
// Image component carrying the rendered bytes as base64.
func (s *ResultDecorateStage) applyT2I(event *core.Event) error {
	// Streaming replies already delivered the text incrementally (sentence by
	// sentence); converting to an image would duplicate the content. Only
	// convert when the reply was produced non-streamed.
	if streamed, _ := event.GetExtra("streamed").(bool); streamed {
		return nil
	}
	chain := event.Result.Chain
	if len(chain) == 0 {
		return nil
	}
	var text strings.Builder
	for _, comp := range chain {
		if p, ok := comp.(*message.Plain); ok {
			text.WriteString(p.Text)
		}
	}
	trimmed := strings.TrimSpace(text.String())
	if trimmed == "" {
		return nil
	}
	if len([]rune(trimmed)) < s.t2iWordThreshold {
		return nil
	}

	var imgData []byte
	var err error
	switch s.t2iStrategy {
	case "local":
		imgData, err = renderLocalT2I(trimmed, s.t2iTemplate)
	case "remote", "":
		// 回退优先级：配置端点 → 官方默认远程端点 → 本地渲染（对齐用户
		// 期望的"配置t2i > 回退默认远程t2i > 本地gg"）。
		if s.t2iEndpoint != "" {
			imgData, err = t2i.RenderRemote(s.t2iEndpoint, trimmed, s.t2iTemplate)
			if err == nil {
				break
			}
			logger.I18nWarn("t2i 配置端点失败(%v)，回退默认远程端点", err)
		}
		// 默认远程端点（官方列表 + 兜底端点）；未配置端点时也先尝试远程。
		imgData, err = t2i.RenderRemote("", trimmed, s.t2iTemplate)
		if err == nil {
			break
		}
		logger.I18nWarn("t2i 远程渲染失败(%v)，回退本地渲染", err)
		imgData, err = renderLocalT2I(trimmed, s.t2iTemplate)
	default:
		return nil
	}
	if err != nil {
		return err
	}

	img := message.ImageFromBase64(base64.StdEncoding.EncodeToString(imgData))
	newChain := make([]message.Component, 0, len(chain)+1)
	for _, comp := range chain {
		if _, ok := comp.(*message.Plain); ok {
			continue // drop the text that has been rendered into the image
		}
		newChain = append(newChain, comp)
	}
	newChain = append(newChain, img)
	event.Result.Chain = newChain
	return nil
}

// renderLocalT2I renders text locally with the gg engine. A system CJK font is
// used when no font path is configured (the renderer falls back automatically).
func renderLocalT2I(text, templateName string) ([]byte, error) {
	opts := t2i.ImageOptions{}
	if templateName != "" && templateName != "base" {
		// templateName only affects the (optional) title in future templates;
		// it is accepted for forward compatibility.
	}
	return t2i.RenderTextToPNG(text, opts)
}

// applyTTS converts the reply plain text to a voice Record component when TTS
// is enabled (mirrors Python result_decorate stage TTS block).
func (s *ResultDecorateStage) applyTTS(event *core.Event) error {
	if !s.ttsEnabled || s.providerMgr == nil {
		return nil
	}
	if event.Result == nil || len(event.Result.Chain) == 0 {
		return nil
	}

	// Session tts_enabled rule (session_service_config), default true.
	if rules := sessionRulesMemo(event, s.convMgr); rules != nil {
		if sc, ok := rules[conversation.RuleServiceConfig].(map[string]interface{}); ok {
			if enabled, ok := sc["tts_enabled"].(bool); ok && !enabled {
				return nil
			}
		}
	}

	// Trigger probability.
	if s.ttsTriggerProb < 1.0 && rand.Float64() > s.ttsTriggerProb {
		return nil
	}

	// Resolve TTS provider: session rule provider_perf_text_to_speech wins.
	var tts provider.TTSProvider
	providerID := ""
	if rules := sessionRulesMemo(event, s.convMgr); rules != nil {
		providerID, _ = rules[conversation.RuleProviderTextToSpeech].(string)
	}
	if providerID != "" {
		if p := s.providerMgr.Get(providerID); p != nil {
			if tp, ok := p.(provider.TTSProvider); ok {
				tts = tp
			}
		}
	}
	if tts == nil {
		tts = s.providerMgr.GetTTSProvider()
	}
	if tts == nil {
		return fmt.Errorf("未配置 TTS 提供商")
	}

	newChain := make([]message.Component, 0, len(event.Result.Chain))
	for _, comp := range event.Result.Chain {
		plain, ok := comp.(*message.Plain)
		if !ok {
			newChain = append(newChain, comp)
			continue
		}
		if len([]rune(plain.Text)) <= 1 {
			newChain = append(newChain, comp)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		audio, err := tts.GetAudio(ctx, plain.Text)
		cancel()
		if err != nil {
			return fmt.Errorf("TTS 合成失败: %w", err)
		}
		if audio == "" {
			newChain = append(newChain, comp)
			continue
		}
		rec := &message.Record{URL: audio, File: audio, Text: plain.Text}
		if s.ttsDualOutput {
			newChain = append(newChain, comp)
		}
		newChain = append(newChain, rec)
	}
	event.Result.Chain = newChain
	return nil
}

// applyResultHooks runs every loaded subprocess plugin's on_decorating_result
// hooks against the outgoing chain.
func (s *ResultDecorateStage) applyResultHooks(event *core.Event, chain *[]pluginsdk.Component) (bool, error) {
	sdkEvent := star.CoreEventToSDKEvent(event)
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
			newChain, stop, res, err := inst.Client.HandleHook(rpcCtx, hookName, sdkEvent, cur)
			rpcCancel()
			if err != nil {
				logger.I18nWarn("插件 %s 的结果钩子 %s 执行失败: %v", inst.Name, hookName, err)
				continue
			}
			cur = newChain
			if res.Sent {
				event.HasSendOper = true
			}
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

	// Segmented-reply delivery settings (respond/stage.py initialize).
	segEnabled       *bool
	segOnlyLLMResult *bool
	intervalMethod   string
	logBase          float64
	segIntervalLo    float64
	segIntervalHi    float64
}

func NewRespondStage() *RespondStage {
	return &RespondStage{}
}

func (s *RespondStage) Name() string { return "respond" }

func (s *RespondStage) Initialize(ctx *PipelineContext) error {
	s.platformMgr = ctx.PlatformMgr
	s.subPlugins = ctx.SubPlugins

	// Segmented-reply interval settings (mirrors respond/stage.py initialize).
	if ps, ok := ctx.AstrbotConfig["platform_settings"].(map[string]interface{}); ok {
		if cfg, ok := ps["segmented_reply"].(map[string]interface{}); ok {
			if v, ok := cfg["enable"].(bool); ok {
				s.segEnabled = &v
			}
			if v, ok := cfg["only_llm_result"].(bool); ok {
				s.segOnlyLLMResult = &v
			}
			s.intervalMethod, _ = cfg["interval_method"].(string)
			s.logBase = 2.6
			if v, ok := cfg["log_base"].(float64); ok && v > 0 {
				s.logBase = v
			}
			if v, ok := cfg["log_base"].(int); ok && v > 0 {
				s.logBase = float64(v)
			}
			s.segIntervalLo, s.segIntervalHi = 1.5, 3.5
			if iv, ok := cfg["interval"].(string); ok && s.segEnabled != nil && *s.segEnabled {
				parts := strings.Split(strings.ReplaceAll(iv, " ", ""), ",")
				if len(parts) == 2 {
					if lo, err := strconv.ParseFloat(parts[0], 64); err == nil {
						s.segIntervalLo = lo
					}
					if hi, err := strconv.ParseFloat(parts[1], 64); err == nil {
						s.segIntervalHi = hi
					}
				}
			}
			logger.Info("Segmented-reply interval: [%v, %v]", s.segIntervalLo, s.segIntervalHi)
		}
	}
	return nil
}

func (s *RespondStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	// Content was already streamed to the platform incrementally by
	// ProcessStage; skip the duplicate final send.
	if streamed, _ := event.GetExtra("streamed").(bool); streamed {
		// The plain text was already streamed out. However, the result chain
		// may still carry media components produced after streaming (e.g. a
		// t2i-rendered image that replaced the text). Those must still be
		// delivered; only a pure-text chain is skipped to avoid duplication.
		if s.platformMgr != nil {
			media := mediaOnlyChain(event.Result)
			if len(media) > 0 {
				chain := event.Result.ToMessageChain()
				chain.Chain = media
				if err := s.platformMgr.Send(event.Source.Platform, event.Source.ConvID, chain); err != nil {
					logger.Error("Failed to send streamed media chain: %v", err)
				}
			}
		}
		return &StageResult{Continue: false}, nil
	}

	// 对齐 Python v4.28.0 (respond/stage.py.diff)：空消息链且非流式中间态时
	// 直接返回，不发送、不触发 after_message_sent 钩子。
	if event.Result != nil && len(event.Result.Chain) == 0 &&
		event.Result.ResultContentType != message.ResultStreamingResult {
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
		// Segmented reply: deliver each component with a computed interval,
		// keeping Reply/At as the header of the first segment and sending
		// Record components separately (mirrors respond/stage.py).
		if s.segReplyRequired(event) && len(validChain) > 1 {
			s.sendSegmented(ctx, event, validChain)
		} else {
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
	}

	// Clear the result
	event.Result = nil
	return &StageResult{Continue: false}, nil
}

// segReplyRequired mirrors respond/stage.py is_seg_reply_required: enabled +
// (not only_llm_result or the result is a model result) + platform allowed.
func (s *RespondStage) segReplyRequired(event *core.Event) bool {
	if s.segEnabled == nil || !*s.segEnabled {
		return false
	}
	if event.Result == nil {
		return false
	}
	if *s.segOnlyLLMResult && !event.Result.IsModelResult() {
		return false
	}
	return !segPlatformBlacklist[event.Source.Platform]
}

// sendSegmented delivers the chain component by component with an interval
// between sends. Reply/At header components are attached to the first
// segment only; Record components are always sent alone.
func (s *RespondStage) sendSegmented(ctx context.Context, event *core.Event, chain []message.Component) {
	header := []message.Component{}
	body := []message.Component{}
	for _, comp := range chain {
		switch comp.(type) {
		case *message.Reply, *message.At:
			header = append(header, comp)
		default:
			body = append(body, comp)
		}
	}
	if len(body) == 0 {
		// Reply/At only: nothing meaningful to send (Python may fix #2670).
		return
	}

	sendOne := func(comps []message.Component) {
		chain := event.Result.ToMessageChain()
		chain.Chain = comps
		if err := s.platformMgr.Send(event.Source.Platform, event.Source.ConvID, chain); err != nil {
			logger.Error("Failed to send segmented message chain: %v", err)
		}
	}

	headerFirst := len(header) > 0
	for i, comp := range body {
		// Compute the interval before sending this segment.
		interval := s.calcSegmentInterval(ctx, comp)
		if interval > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
		if _, isRecord := comp.(*message.Record); isRecord {
			// Record must be sent alone.
			sendOne([]message.Component{comp})
			continue
		}
		if headerFirst {
			seg := make([]message.Component, 0, len(header)+1)
			seg = append(seg, header...)
			seg = append(seg, comp)
			sendOne(seg)
			headerFirst = false
		} else {
			sendOne([]message.Component{comp})
		}
		_ = i
	}

	// Fire on_after_message_sent after the segmented delivery completes.
	if s.subPlugins != nil {
		dispatchSubprocessHooks(s.subPlugins, event, "on_after_message_sent")
	}
}

// calcSegmentInterval mirrors respond/stage.py _calc_comp_interval: log
// method uses log(word_count+1, log_base) + [0, 0.5); random uses the
// configured interval range.
func (s *RespondStage) calcSegmentInterval(ctx context.Context, comp message.Component) time.Duration {
	if s.intervalMethod == "log" {
		var base float64 = 2.6
		if s.logBase > 0 {
			base = s.logBase
		}
		seconds := 1.0 + rand.Float64()*0.5
		if plain, ok := comp.(*message.Plain); ok {
			wc := wordCount(plain.Text)
			seconds = math.Log(float64(wc)+1) / math.Log(base)
			seconds += rand.Float64() * 0.5
		}
		return time.Duration(seconds * float64(time.Second))
	}
	// random
	lo, hi := s.segIntervalLo, s.segIntervalHi
	if hi <= lo {
		lo, hi = 1.5, 3.5
	}
	seconds := lo + rand.Float64()*(hi-lo)
	return time.Duration(seconds * float64(time.Second))
}

// wordCount mirrors _word_cnt: words for ASCII text, alphanumeric runes
// otherwise.
func wordCount(text string) int {
	allASCII := true
	for _, r := range text {
		if r >= 128 {
			allASCII = false
			break
		}
	}
	if allASCII {
		return len(strings.Fields(text))
	}
	count := 0
	for _, r := range text {
		if isAlnum(r) {
			count++
		}
	}
	return count
}

func isAlnum(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// mediaOnlyChain keeps only non-plain-text components (Image/File/Video/
// Record) from a result, dropping At/Reply/Plain. Used to deliver media after
// the text was already streamed out.
func mediaOnlyChain(result *message.MessageEventResult) []message.Component {
	if result == nil {
		return nil
	}
	var media []message.Component
	for _, comp := range result.Chain {
		switch comp.(type) {
		case *message.Plain, *message.At, *message.Reply:
			continue
		default:
			media = append(media, comp)
		}
	}
	return media
}

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
	img.File = strings.TrimPrefix(img.File, "file://")
}

// DurationFromSeconds creates a duration from seconds.
func DurationFromSeconds(sec int) time.Duration {
	return time.Duration(sec) * time.Second
}

// BuildDefaultPipeline creates the default 10-stage pipeline matching Python.
func BuildDefaultPipeline(pctx *PipelineContext) ([]PipelineStage, error) {
	stages := []PipelineStage{
		NewSessionWaitStage(),
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

// personaIDOrDefault returns the configured default persona id (used by the
// trace span record).
func personaIDOrDefault(cfg map[string]interface{}) string {
	if ps, ok := cfg["provider_settings"].(map[string]interface{}); ok {
		if v, ok := ps["default_personality"].(string); ok {
			return v
		}
	}
	return ""
}
