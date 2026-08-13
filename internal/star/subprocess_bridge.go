// Package star - bridge that registers subprocess plugin handlers (RPC-backed,
// via the plugin SDK) into the star handler system so the pipeline can execute
// them. Commands/filters/hooks run inside the plugin's child process; the
// Handler closures forward events over gRPC.
package star

import (
	"context"
	"strings"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// logger 供 star 包记录日志。
var logger = log.GetDefault().WithComponent("Star")

// pluginRPCTimeout bounds every gRPC call into a plugin's subprocess so a hung
// plugin handler (infinite loop, deadlock) cannot freeze the pipeline forever.
const pluginRPCTimeout = 30 * time.Second

// AlwaysMatchFilter matches every event (used for hooks that observe all
// pipeline events regardless of type).
type AlwaysMatchFilter struct{}

// Match implements HandlerFilter.
func (f *AlwaysMatchFilter) Match(ctx *FilterContext) bool { return true }

// FilterType implements HandlerFilter.
func (f *AlwaysMatchFilter) FilterType() string { return "always" }

// hookEventType maps a hook event name to a star event type.
func hookEventType(name string) (EventType, bool) {
	switch name {
	case "on_message", "message":
		return EventTypeOnAstrMessageEvent, true
	case "on_message_received":
		return EventTypeOnMessageReceivedEvent, true
	case "on_pre_process":
		return EventTypeOnPreProcessEvent, true
	case "on_result_handling":
		return EventTypeOnResultHandlingEvent, true
	case "on_decorating_result":
		return EventTypeOnDecoratingResultEvent, true
	case "on_after_message_sent":
		return EventTypeOnAfterMessageSentEvent, true
	case "on_llm_request":
		return EventTypeOnLLMRequestEvent, true
	case "on_llm_response":
		return EventTypeOnLLMResponseEvent, true
	case "on_tool_call":
		return EventTypeOnToolCallEvent, true
	default:
		// lifecycle hooks (startup/shutdown) are not pipeline events
		return 0, false
	}
}

// RegisterSubprocessPlugins bridges a batch of running subprocess plugins.
func RegisterSubprocessPlugins(starMgr *Manager, insts []*plugin.PluginInstance) {
	for _, inst := range insts {
		RegisterSubprocessPlugin(starMgr, inst)
	}
}

// RegisterSubprocessPlugin bridges one subprocess plugin's commands, filters
// and hooks into the star registry. Uses the same `plugin_` prefixes as the
// legacy .so bridge, so RemovePluginCommands/Filters/Hooks clean them up.
func RegisterSubprocessPlugin(starMgr *Manager, inst *plugin.PluginInstance) {
	if starMgr == nil || inst == nil || inst.Client == nil || inst.Meta == nil {
		return
	}
	client := inst.Client
	meta := inst.Meta

	for _, cmd := range meta.Commands {
		cmd := cmd
		handler := &StarHandlerMetadata{
			// 用插件 ID 限定 full name，避免不同插件注册同名指令时互相覆盖
			// （否则 WebUI 重命名一个指令会连带影响另一个同名指令）。
			HandlerFullName:   "plugin_" + inst.ID + "_" + cmd.Name,
			HandlerName:       cmd.Name,
			HandlerModulePath: "data.plugins",
			PluginName:        inst.ID,
			Handler: func(event interface{}) error {
				e, ok := event.(*core.Event)
				if !ok {
					return nil
				}
				parts := strings.Fields(e.MessageStr)
				var args []string
				if len(parts) > 1 {
					args = parts[1:]
				}
				logger.Debug("plugin RPC HandleCommand: name=%s args=%v", cmd.Name, args)
				rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
				text, chain, err := client.HandleCommand(rpcCtx, cmd.Name, args, CoreEventToSDK(e))
				rpcCancel()
				if err != nil {
					text = "插件执行失败: " + err.Error()
					chain = nil
				}
				if len(chain) > 0 {
					e.Result = message.NewMessageEventResult()
					e.Result.Chain = plugin.ComponentsFromSDK(chain)
					return nil
				}
				if text == "" {
					return nil
				}
				e.Result = message.NewMessageEventResult()
				e.Result.Chain = []message.Component{&message.Plain{Text: text}}
				return nil
			},
			EventType:    EventTypeFilter,
			EventFilters: []HandlerFilter{NewCommandFilter(cmd.Name, cmd.Aliases, nil)},
			Desc:         cmd.Description,
			Enabled:      true,
		}
		if cmd.Permission == "admin" {
			handler.EventFilters = append(handler.EventFilters, NewPermissionFilter(PermissionAdmin))
		}
		starMgr.Handlers().Append(handler)
	}

	for _, f := range meta.Filters {
		f := f
		handler := &StarHandlerMetadata{
			HandlerFullName:   "plugin_filter_" + inst.ID + "_" + f.Name,
			HandlerName:       f.Name,
			HandlerModulePath: "data.plugins",
			PluginName:        inst.ID,
			Handler: func(event interface{}) error {
				e, ok := event.(*core.Event)
				if !ok {
					return nil
				}
				rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
				allow, err := client.HandleFilter(rpcCtx, f.Name, CoreEventToSDK(e))
				rpcCancel()
				if err != nil {
					return nil
				}
				if !allow {
					e.Stop()
				}
				return nil
			},
			EventType:    EventTypeFilter,
			EventFilters: []HandlerFilter{&AlwaysMatchFilter{}},
			Desc:         "plugin filter: " + f.Name,
			Enabled:      true,
		}
		starMgr.Handlers().Append(handler)
	}

	for _, h := range meta.Hooks {
		// on_llm_request / on_decorating_result hooks are invoked directly by
		// the pipeline stages (ProcessStage / ResultDecorateStage) because they
		// carry plugin-specific payloads (ProviderRequest / result chain); they
		// are not star pipeline handlers.
		switch h.Event {
		case "on_llm_request", "on_decorating_result", "on_result_handling":
			continue
		}
		et, ok := hookEventType(h.Event)
		if !ok {
			// lifecycle hooks (startup/shutdown) are fired by the lifecycle
			// via SubprocessManager.TriggerHook, not bridged into the pipeline.
			continue
		}
		h := h
		handler := &StarHandlerMetadata{
			HandlerFullName:   "plugin_hook_" + inst.ID + "_" + h.Name,
			HandlerName:       h.Name,
			HandlerModulePath: "data.plugins",
			PluginName:        inst.ID,
			Handler: func(event interface{}) error {
				e, ok := event.(*core.Event)
				if !ok {
					return nil
				}
				rpcCtx, rpcCancel := context.WithTimeout(context.Background(), pluginRPCTimeout)
				_, _, err := client.HandleHook(rpcCtx, h.Name, CoreEventToSDK(e), nil)
				rpcCancel()
				return err
			},
			EventType:    et,
			EventFilters: []HandlerFilter{&AlwaysMatchFilter{}},
			Desc:         "plugin hook: " + h.Name,
			Enabled:      true,
		}
		starMgr.Handlers().Append(handler)
	}

	if len(meta.Commands)+len(meta.Filters)+len(meta.Hooks) > 0 {
		logger.I18nInfo("子进程插件 %s 已注册到流水线（%d 个命令，%d 个过滤器，%d 个钩子）",
			inst.Name, len(meta.Commands), len(meta.Filters), len(meta.Hooks))
	}
}

// CoreEventToSDK converts a host core.Event into the SDK's serializable Event
// view that crosses the gRPC boundary.
func CoreEventToSDK(e *core.Event) *pluginsdk.Event {
	if e == nil {
		return &pluginsdk.Event{}
	}
	out := &pluginsdk.Event{
		Type:       coreEventTypeName(e.Type),
		Platform:   e.Source.Platform,
		SelfID:     e.Source.SelfID,
		SenderID:   e.Source.SenderID,
		SenderName: e.Source.SenderName,
		ConvID:     e.Source.ConvID,
		GroupName:  e.Source.GroupName,
		IsGroup:    e.Source.IsGroup,
		IsAtBot:    e.Source.IsAtBot,
		IsAdmin:    e.Source.IsAdmin,
		MessageStr: e.MessageStr,
		PlainText:  e.PlainText,
		RawMessage: e.RawMessage,
		Timestamp:  e.Timestamp.Unix(),
		Metadata:   e.Metadata,
	}
	if e.MessageObj != nil {
		out.MessageID = e.MessageObj.MessageID
	}
	if e.Message != nil {
		for _, c := range e.Message.Chain {
			out.Chain = append(out.Chain, componentToSDK(c))
		}
	}
	return out
}

var coreEventTypeNames = map[core.EventType]string{
	core.EventMessage: "message",
	core.EventNotice:  "notice",
	core.EventRequest: "request",
	core.EventMeta:    "meta",
}

func coreEventTypeName(t core.EventType) string {
	if s, ok := coreEventTypeNames[t]; ok {
		return s
	}
	return "message"
}

// componentToSDK flattens a host message component into the SDK's serializable
// Component.
func componentToSDK(c message.Component) pluginsdk.Component {
	if c == nil {
		return pluginsdk.Component{Type: pluginsdk.CompUnknown}
	}
	out := pluginsdk.Component{Type: pluginsdk.ComponentType(c.Type())}
	switch v := c.(type) {
	case *message.Plain:
		out.Text = v.Text
	case *message.At:
		out.TargetID = v.TargetID
		out.Name = v.Name
	case *message.Image:
		out.URL = v.URL
		out.Path = v.Path
		out.File = v.File
		out.Base64 = v.Base64
		out.FileID = v.FileID
	case *message.Record:
		out.URL = v.URL
		out.Path = v.Path
		out.File = v.File
		out.Base64 = v.Base64
		out.FileID = v.FileID
	case *message.File:
		out.URL = v.URL
		out.Path = v.Path
		out.FileID = v.FileID
		out.Name = v.Name
	case *message.Video:
		out.URL = v.URL
		out.Path = v.Path
		out.FileID = v.FileID
	case *message.Face:
		out.ID = v.ID
	case *message.Emoji:
		out.ID = v.ID
		out.URL = v.URL
	case *message.Json:
		out.Data = v.Data
	case *message.Reply:
		out.ID = v.MessageID
		out.Text = v.MessageStr
	}
	return out
}

// RemovePluginCommands removes all subprocess-plugin command handlers from the
// star registry (prefix "plugin_"). Used when re-bridging after install,
// unload, reload or crash-restart.
func RemovePluginCommands(starMgr *Manager) {
	if starMgr == nil || starMgr.Handlers() == nil {
		return
	}
	for _, h := range starMgr.Handlers().All() {
		if strings.HasPrefix(h.HandlerFullName, "plugin_") {
			starMgr.Handlers().Remove(h.HandlerFullName)
		}
	}
}

// RemovePluginFilters removes all subprocess-plugin filter handlers from the
// star registry (prefix "plugin_filter_").
func RemovePluginFilters(starMgr *Manager) {
	if starMgr == nil || starMgr.Handlers() == nil {
		return
	}
	for _, h := range starMgr.Handlers().All() {
		if strings.HasPrefix(h.HandlerFullName, "plugin_filter_") {
			starMgr.Handlers().Remove(h.HandlerFullName)
		}
	}
}

// RemovePluginHooks removes all subprocess-plugin hook handlers from the star
// registry (prefix "plugin_hook_").
func RemovePluginHooks(starMgr *Manager) {
	if starMgr == nil || starMgr.Handlers() == nil {
		return
	}
	for _, h := range starMgr.Handlers().All() {
		if strings.HasPrefix(h.HandlerFullName, "plugin_hook_") {
			starMgr.Handlers().Remove(h.HandlerFullName)
		}
	}
}
