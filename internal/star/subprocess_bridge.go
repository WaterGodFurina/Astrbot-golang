// Package star - bridge that registers subprocess plugin handlers (RPC-backed,
// via the plugin SDK) into the star handler system so the pipeline can execute
// them. Commands/filters/hooks run inside the plugin's child process; the
// Handler closures forward events over gRPC.
package star

import (
	"context"
	"strings"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	"github.com/AstrBotDevs/AstrBot/internal/core"
	"github.com/AstrBotDevs/AstrBot/internal/plugin"
	"github.com/AstrBotDevs/AstrBot/pkg/message"
)

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
				text, err := client.HandleCommand(context.Background(), cmd.Name, args, coreEventToSDK(e))
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

	for _, f := range meta.Filters {
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
				allow, err := client.HandleFilter(context.Background(), f.Name, coreEventToSDK(e))
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
		et, ok := hookEventType(h.Event)
		if !ok {
			// lifecycle hooks (startup/shutdown) are fired by the lifecycle
			// via SubprocessManager.TriggerHook, not bridged into the pipeline.
			continue
		}
		h := h
		handler := &StarHandlerMetadata{
			HandlerFullName:   "plugin_hook_" + h.Name,
			HandlerName:       h.Name,
			HandlerModulePath: "data.plugins",
			Handler: func(event interface{}) error {
				e, ok := event.(*core.Event)
				if !ok {
					return nil
				}
				return client.HandleHook(context.Background(), h.Name, coreEventToSDK(e))
			},
			EventType:    et,
			EventFilters: []HandlerFilter{&AlwaysMatchFilter{}},
			Desc:         "plugin hook: " + h.Name,
			Enabled:      true,
		}
		starMgr.Handlers().Append(handler)
	}

	if len(meta.Commands)+len(meta.Filters)+len(meta.Hooks) > 0 {
		logger.Info("Registered subprocess plugin %s into the pipeline (%d commands, %d filters, %d hooks)",
			inst.Name, len(meta.Commands), len(meta.Filters), len(meta.Hooks))
	}
}

// coreEventToSDK converts a host core.Event into the SDK's serializable Event
// view that crosses the gRPC boundary.
func coreEventToSDK(e *core.Event) *pluginsdk.Event {
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
