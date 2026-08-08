// Package star - bridge that registers .so plugin commands into the star
// handler system so the pipeline can execute them.
package star

import (
	"context"
	"strings"

	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

var logger = log.GetDefault().WithComponent("Star")

// RemovePluginCommands removes all previously bridged .so plugin commands
// (HandlerFullName prefixed with "plugin_") from the star registry.
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

// RegisterPluginFilters registers .so plugin event filters as star handlers.
// A filter returning false stops the event (mirrors Python's filter semantics).
func RegisterPluginFilters(starMgr *Manager, filters []plugin.FilterHandler) {
	for _, f := range filters {
		f := f
		handler := &StarHandlerMetadata{
			HandlerFullName:   "plugin_filter_" + f.Name,
			HandlerName:       f.Name,
			HandlerModulePath: "data.plugins",
			Handler: func(event interface{}) error {
				e, ok := event.(*core.Event)
				if !ok {
					return nil
				}
				if !f.Filter(context.Background(), e) {
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
	if len(filters) > 0 {
		logger.Info("Registered %d .so plugin filter(s) into the pipeline", len(filters))
	}
}

// RemovePluginFilters removes previously bridged plugin filters.
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

// AlwaysMatchFilter matches every event.
type AlwaysMatchFilter struct{}

func (f *AlwaysMatchFilter) Match(ctx *FilterContext) bool { return true }
func (f *AlwaysMatchFilter) FilterType() string            { return "always" }

// RegisterPluginHooks registers .so plugin event hooks as star handlers
// (only hooks with a known event type are bridged; lifecycle hooks such as
// "startup" are fired directly by the lifecycle).
func RegisterPluginHooks(starMgr *Manager, hooks []plugin.HookHandler) {
	for _, h := range hooks {
		et, ok := hookEventType(h.Event)
		if !ok {
			continue
		}
		h := h
		handler := &StarHandlerMetadata{
			HandlerFullName:   "plugin_hook_" + h.Name,
			HandlerName:       h.Name,
			HandlerModulePath: "data.plugins",
			Handler: func(event interface{}) error {
				return h.Handler(context.Background())
			},
			EventType:    et,
			EventFilters: []HandlerFilter{&AlwaysMatchFilter{}},
			Desc:         "plugin hook: " + h.Name,
			Enabled:      true,
		}
		starMgr.Handlers().Append(handler)
	}
	if len(hooks) > 0 {
		logger.Info("Registered %d .so plugin hook(s) into the pipeline", len(hooks))
	}
}

// RemovePluginHooks removes previously bridged plugin hooks.
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

// RegisterPluginCommands registers all commands from loaded .so plugins as
// star filter handlers (command filters + permission filters).
func RegisterPluginCommands(starMgr *Manager, pluginCtx *plugin.Context, cmds []plugin.CommandHandler) {
	for _, cmd := range cmds {
		cmd := cmd
		handler := &StarHandlerMetadata{
			HandlerFullName:   "plugin_" + cmd.Name,
			HandlerName:       cmd.Name,
			HandlerModulePath: "data.plugins",
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
				var text string
				var err error
				if cmd.HandlerEx != nil {
					text, err = cmd.HandlerEx(pluginCtx, context.Background(), args)
				} else if cmd.Handler != nil {
					text, err = cmd.Handler(context.Background(), args)
				}
				if err != nil {
					text = "插件执行失败: " + err.Error()
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
	if len(cmds) > 0 {
		logger.Info("Registered %d .so plugin command(s) into the pipeline", len(cmds))
	}
}
