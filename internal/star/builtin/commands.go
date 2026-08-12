// Package builtin implements AstrBot's built-in commands.
// Ported from astrbot/builtin_stars/builtin_commands/
package builtin

import (
	"fmt"
	"strings"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

var logger = log.GetDefault().WithComponent("BuiltinCommands")

const version = "4.27.2-go"

// Deps carries the dependencies needed by built-in commands.
type Deps struct {
	StarMgr         *star.Manager
	ConfigMgr       *config.ConfigManager
	ConversationMgr *conversation.Manager
}

// builtinState holds per-session mutable state (provider selection, variables, umo aliases).
type builtinState struct {
	mu          sync.Mutex
	selectedLLM map[string]string            // umo -> provider id
	selectedTTS map[string]string            // umo -> provider id
	selectedSTT map[string]string            // umo -> provider id
	sessionVars map[string]map[string]string // umo -> key -> value
	umoAliases  map[string]string            // umo -> alias
}

func newBuiltinState() *builtinState {
	return &builtinState{
		selectedLLM: make(map[string]string),
		selectedTTS: make(map[string]string),
		selectedSTT: make(map[string]string),
		sessionVars: make(map[string]map[string]string),
		umoAliases:  make(map[string]string),
	}
}

var state = newBuiltinState()

// RegisterBuiltin registers all built-in command handlers into the star manager.
func RegisterBuiltin(deps Deps) {
	reg := func(name string, permission star.PermissionType, desc string, fn func(event *core.Event)) {
		handler := &star.StarHandlerMetadata{
			HandlerFullName:   "builtin_" + name,
			HandlerName:       name,
			HandlerModulePath: "astrbot.builtin_stars.builtin_commands.main",
			Handler: func(event interface{}) error {
				if e, ok := event.(*core.Event); ok {
					fn(e)
				}
				return nil
			},
			EventType:    star.EventTypeFilter,
			EventFilters: []star.HandlerFilter{star.NewCommandFilter(name, nil, nil)},
			Desc:         desc,
			Enabled:      true,
		}
		if permission != star.PermissionEveryone {
			handler.EventFilters = append(handler.EventFilters, star.NewPermissionFilter(permission))
		}
		deps.StarMgr.Handlers().Append(handler)
	}

	reg("help", star.PermissionEveryone, "显示帮助", func(e *core.Event) { helpCmd(deps, e) })
	reg("sid", star.PermissionEveryone, "获取会话 ID 信息", func(e *core.Event) { sidCmd(e) })
	reg("name", star.PermissionAdmin, "设置当前 UMO 的显示名称", func(e *core.Event) { nameCmd(deps, e) })
	reg("reset", star.PermissionEveryone, "重置当前会话的 LLM 上下文", func(e *core.Event) { resetCmd(deps, e) })
	reg("new", star.PermissionEveryone, "创建新对话", func(e *core.Event) { newCmd(deps, e) })
	reg("stop", star.PermissionEveryone, "停止当前会话正在运行的任务", func(e *core.Event) { stopCmd(e) })
	reg("stats", star.PermissionEveryone, "查看当前对话 Token 用量统计", func(e *core.Event) { statsCmd(e) })
	reg("provider", star.PermissionAdmin, "查看或切换 LLM Provider", func(e *core.Event) { providerCmd(deps, e) })
	reg("set", star.PermissionEveryone, "设置会话变量", func(e *core.Event) { setCmd(e) })
	reg("unset", star.PermissionEveryone, "移除会话变量", func(e *core.Event) { unsetCmd(e) })
	reg("dashboard_update", star.PermissionAdmin, "更新 AstrBot WebUI", func(e *core.Event) {
		reply(e, "❌ Go 版暂不支持在线更新 WebUI。")
	})

	logger.Info("Built-in commands registered (help, sid, name, reset, new, stop, stats, provider, set, unset)")
}

// args returns the command arguments (message with command name stripped).
func args(e *core.Event) []string {
	parts := strings.Fields(e.MessageStr)
	if len(parts) <= 1 {
		return nil
	}
	return parts[1:]
}

func reply(e *core.Event, text string) {
	e.Result = message.NewMessageEventResult()
	e.Result.Chain = []message.Component{&message.Plain{Text: text}}
}

// ---------------------------------------------------------------------------
// /help
// ---------------------------------------------------------------------------

func helpCmd(deps Deps, e *core.Event) {
	lines := []string{
		fmt.Sprintf("AstrBot v%s(WebUI: 0.1.0-go)", version),
		"/help - 显示帮助",
		"/sid - 获取会话 ID 信息",
		"/reset - 重置 LLM 会话",
		"/stop - 停止当前会话正在运行的任务",
		"/new - 创建新对话",
		"/stats - 查看当前对话 Token 用量",
		"/provider [idx] - 查看或切换 LLM Provider",
		"/name <name> - 设置当前 UMO 的显示名称",
		"/set <key> <value> - 设置会话变量",
		"/unset <key> - 移除会话变量",
	}
	reply(e, strings.Join(lines, "\n"))
}

// ---------------------------------------------------------------------------
// /sid
// ---------------------------------------------------------------------------

func sidCmd(e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	uid := e.Source.SenderID
	ret := fmt.Sprintf(
		"UMO: 「%s」\nUID: 「%s」\n*Use UMO to set whitelist and configure routing, use UID to set admin list\n\nYour session information:\nBot ID: 「%s」\nMessage Type: 「%s」\nSession ID: 「%s」",
		umo, uid, e.Source.Platform, e.Source.ConvID, e.Source.ConvID,
	)
	if e.Source.IsGroup {
		ret += fmt.Sprintf("\n\nThe group's ID: 「%s」. Set this ID to whitelist to allow the entire group.", e.Source.ConvID)
	}
	reply(e, ret)
}

// ---------------------------------------------------------------------------
// /name
// ---------------------------------------------------------------------------

func nameCmd(deps Deps, e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	al := args(e)
	autoName := e.Source.SenderName
	manualNeeded := platformNeedsManualName(e.Source.Platform)

	if len(al) == 0 || strings.TrimSpace(al[0]) == "" {
		cur := state.umoAliases[umo]
		if manualNeeded {
			if cur == "" {
				reply(e, fmt.Sprintf(
					"当前平台「%s」无法自动获取昵称（QQ 官方私聊消息不携带用户名）。\n请使用 /name <你的名字> 设置，之后 LLM 才能识别你。\nUMO: %s",
					e.Source.Platform, umo,
				))
			} else {
				reply(e, fmt.Sprintf("当前昵称: %s\nUMO: %s", cur, umo))
			}
			return
		}
		if autoName != "" {
			reply(e, fmt.Sprintf(
				"当前平台「%s」会自动获取昵称：\n昵称: %s\nUMO: %s\n（无需手动设置；如需自定义可用 /name <名字> 覆盖）",
				e.Source.Platform, autoName, umo,
			))
		} else {
			reply(e, "Usage: /name <name>\nUMO: "+umo+"\nAlias: "+cur)
		}
		return
	}
	alias := strings.Join(al, " ")
	state.mu.Lock()
	state.umoAliases[umo] = alias
	state.mu.Unlock()
	persistUmoAlias(deps, umo, alias)
	reply(e, fmt.Sprintf("UMO name set to: %s\nUMO: %s", alias, umo))
}

// platformNeedsManualName reports whether the platform's messages carry a
// sender nickname. QQ 官方 C2C messages do not include the username, so users
// there must set it manually via /name; OneBot (aiocqhttp) and most other
// platforms provide it automatically.
func platformNeedsManualName(platform string) bool {
	switch platform {
	case "qq_official", "qq_official_webhook":
		return true
	}
	return false
}

func persistUmoAlias(deps Deps, umo, alias string) {
	if deps.ConfigMgr == nil {
		return
	}
	cfg := deps.ConfigMgr.Get("default")
	if cfg == nil {
		return
	}
	all := cfg.All()
	aliases, _ := all["umo_alias"].(map[string]interface{})
	if aliases == nil {
		aliases = map[string]interface{}{}
	}
	aliases[umo] = alias
	_ = cfg.Set("umo_alias", aliases)
	_ = cfg.Save()
}

// ---------------------------------------------------------------------------
// /reset
// ---------------------------------------------------------------------------

func resetCmd(deps Deps, e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	// Admin requirement for group reset (mirrors Python's RstScene default)
	if e.Source.IsGroup && e.Role != "admin" {
		reply(e, fmt.Sprintf(
			"Reset command requires admin permission in group scenario, you (ID %s) are not admin, cannot perform this action.",
			e.Source.SenderID,
		))
		return
	}
	if deps.ConversationMgr == nil {
		reply(e, "😕 会话管理器不可用。")
		return
	}
	cid := deps.ConversationMgr.GetCurrConversationID(umo)
	if cid == "" {
		reply(e, "😕 You are not in a conversation. Use /new to create one.")
		return
	}
	deps.ConversationMgr.ClearHistory(umo)
	reply(e, "✅ Conversation reset successfully.")
}

// ---------------------------------------------------------------------------
// /new
// ---------------------------------------------------------------------------

func newCmd(deps Deps, e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	if deps.ConversationMgr == nil {
		reply(e, "😕 会话管理器不可用。")
		return
	}
	conv := deps.ConversationMgr.NewConversation(umo, e.Source.Platform)
	cid := conv.CID
	if len(cid) > 4 {
		cid = cid[:4]
	}
	reply(e, fmt.Sprintf("✅ Switched to new conversation: %s。", cid))
}

// ---------------------------------------------------------------------------
// /stop
// ---------------------------------------------------------------------------

func stopCmd(e *core.Event) {
	// Go pipeline has no long-running agent registry yet; report idle.
	reply(e, "✅ No running tasks in the current session.")
}

// ---------------------------------------------------------------------------
// /stats
// ---------------------------------------------------------------------------

func statsCmd(e *core.Event) {
	reply(e, "📊 No stats available for this conversation yet.")
}

// ---------------------------------------------------------------------------
// /provider
// ---------------------------------------------------------------------------

func providerCmd(deps Deps, e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	al := args(e)

	providers := listProviders(deps)
	if len(al) == 0 {
		lines := []string{"## LLM Providers\n"}
		for i, p := range providers {
			line := fmt.Sprintf("%d. %s (%s)", i+1, p.ID, p.Model)
			if state.selectedLLM[umo] == p.ID {
				line += " 👈"
			}
			lines = append(lines, line)
		}
		lines = append(lines, "\nUse /provider <idx> to switch LLM providers.")
		reply(e, strings.Join(lines, "\n"))
		return
	}

	if al[0] == "tts" || al[0] == "stt" {
		reply(e, "❌ Go 版暂不支持 TTS/STT Provider 切换。")
		return
	}
	idx := 0
	fmt.Sscanf(al[0], "%d", &idx)
	if idx < 1 || idx > len(providers) {
		reply(e, "❌ Invalid provider index.")
		return
	}
	p := providers[idx-1]
	state.mu.Lock()
	state.selectedLLM[umo] = p.ID
	state.mu.Unlock()
	persistSelectedProvider(deps, umo, p.ID)
	reply(e, fmt.Sprintf("✅ Successfully switched to %s.", p.ID))
}

type providerInfo struct {
	ID    string
	Model string
	Type  string
}

func listProviders(deps Deps) []providerInfo {
	if deps.ConfigMgr == nil {
		return nil
	}
	cfg := deps.ConfigMgr.Get("default")
	if cfg == nil {
		return nil
	}
	all := cfg.All()
	providers, _ := all["provider"].([]interface{})
	result := []providerInfo{}
	for _, p := range providers {
		pc, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := pc["id"].(string)
		if id == "" {
			continue
		}
		model, _ := pc["model"].(string)
		ptype, _ := pc["type"].(string)
		result = append(result, providerInfo{ID: id, Model: model, Type: ptype})
	}
	return result
}

func persistSelectedProvider(deps Deps, umo, providerID string) {
	if deps.ConfigMgr == nil {
		return
	}
	cfg := deps.ConfigMgr.Get("default")
	if cfg == nil {
		return
	}
	all := cfg.All()
	sel, _ := all["provider_selection"].(map[string]interface{})
	if sel == nil {
		sel = map[string]interface{}{}
	}
	sel[umo] = providerID
	_ = cfg.Set("provider_selection", sel)
	_ = cfg.Save()
}

// ---------------------------------------------------------------------------
// /set /unset
// ---------------------------------------------------------------------------

func setCmd(e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	al := args(e)
	if len(al) < 2 {
		reply(e, "格式: /set 变量名 值")
		return
	}
	state.mu.Lock()
	if state.sessionVars[umo] == nil {
		state.sessionVars[umo] = map[string]string{}
	}
	state.sessionVars[umo][al[0]] = strings.Join(al[1:], " ")
	state.mu.Unlock()
	reply(e, fmt.Sprintf("会话 %s 变量 %s 存储成功。使用 /unset 移除。", umo, al[0]))
}

func unsetCmd(e *core.Event) {
	umo := e.UnifiedMsgOrigin()
	al := args(e)
	if len(al) == 0 {
		reply(e, "没有那个变量名。格式 /unset 变量名。")
		return
	}
	state.mu.Lock()
	if vars, ok := state.sessionVars[umo]; ok {
		if _, exists := vars[al[0]]; !exists {
			state.mu.Unlock()
			reply(e, "没有那个变量名。格式 /unset 变量名。")
			return
		}
		delete(vars, al[0])
	}
	state.mu.Unlock()
	reply(e, fmt.Sprintf("会话 %s 变量 %s 移除成功。", umo, al[0]))
}
