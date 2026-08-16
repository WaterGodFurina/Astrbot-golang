package plugin

import (
	"encoding/base64"
	"fmt"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/t2i"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ComponentsFromSDK converts SDK components (received from a plugin over RPC)
// back into host message components for sending.
func ComponentsFromSDK(chain []pluginsdk.Component) []message.Component {
	out := make([]message.Component, 0, len(chain))
	for _, c := range chain {
		switch c.Type {
		case pluginsdk.CompPlain:
			out = append(out, &message.Plain{Text: c.Text})
		case pluginsdk.CompAt:
			out = append(out, &message.At{TargetID: c.TargetID, Name: c.Name})
		case pluginsdk.CompImage:
			out = append(out, &message.Image{URL: c.URL, Path: c.Path, File: c.File, Base64: c.Base64, FileID: c.FileID})
		case pluginsdk.CompRecord:
			out = append(out, &message.Record{URL: c.URL, Path: c.Path, File: c.File, Base64: c.Base64, FileID: c.FileID})
		case pluginsdk.CompFile:
			out = append(out, &message.File{URL: c.URL, Path: c.Path, FileID: c.FileID, Name: c.Name})
		case pluginsdk.CompVideo:
			out = append(out, &message.Video{URL: c.URL, Path: c.Path, FileID: c.FileID})
		case pluginsdk.CompFace:
			out = append(out, &message.Face{ID: c.ID})
		case pluginsdk.CompEmoji:
			out = append(out, &message.Emoji{ID: c.ID, URL: c.URL})
		case pluginsdk.CompJson:
			out = append(out, &message.Json{Data: c.Data})
		case pluginsdk.CompReply:
			out = append(out, &message.Reply{MessageID: c.ID, MessageStr: c.Text})
		}
	}
	return out
}

// CallActionAdapter is implemented by platform adapters that can serve generic
// API calls (e.g. the aiocqhttp OneBot adapter).
type CallActionAdapter interface {
	CallAction(api string, params map[string]any) (map[string]any, error)
}

// RecallAdapter is implemented by platform adapters that can recall messages.
type RecallAdapter interface {
	RecallMessage(messageID string) error
}

// pluginConfigID 把 HostService 反调用携带的插件注册名解析为实例 id
//（配置目录按 id = name_language 分键）。先查运行中实例（RPC 调用者必然
// 是运行中的），未命中回退 manifest 首条同名条目；都没有则返回原名作为
// 目录键兜底（兼容无 manifest 的测试/旧布局）。
func (m *SubprocessManager) pluginConfigID(name string) string {
	if m == nil {
		return name
	}
	if inst := m.instanceByName(name); inst != nil {
		if inst.ID != "" {
			return inst.ID
		}
		// 实例缺 ID（构造型测试/边缘状态）→ 用 name 作为目录键兜底。
		return name
	}
	if man, err := LoadManifest(m.manifestPath()); err == nil {
		for _, e := range man.Plugins {
			if e.Name == name {
				return e.ID
			}
		}
	}
	return name
}

// resolvePluginConfig returns the merged plugin config for the HostService
// GetConfig hook (nil manager → empty config). Testable seam: the hook body is
// kept as a plain function so paths can be verified without an RPC broker.
func resolvePluginConfig(m *SubprocessManager, name string) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m.ConfigResolver().ResolvePluginConfig(m.pluginConfigID(name))
}

// SetHostService installs the HostService hooks (reverse plugin -> host RPCs)
// backed by the platform manager, the subprocess plugin manager (for config
// reads/writes) and a ChatLLM callback (for plugins calling the LLM directly).
// Call once at startup, after both managers exist and before plugins begin
// handling messages.
func SetHostService(pm *platform.PlatformManager, subMgr *SubprocessManager, chatLLM func(prompt, systemPrompt string, imageURLs []string) (string, error)) {
	pluginsdk.SetHostHooks(pluginsdk.HostServiceHooks{
		CallAction: func(platformID, api string, params map[string]any) (map[string]any, error) {
			adapter := pm.Get(platformID)
			if adapter == nil {
				// platformID may be empty (any adapter) - pick the first that
				// supports CallAction.
				for _, a := range pm.All() {
					if ca, ok := a.(CallActionAdapter); ok {
						return ca.CallAction(api, params)
					}
				}
				return nil, fmt.Errorf("platform %q has no adapter supporting CallAction", platformID)
			}
			ca, ok := adapter.(CallActionAdapter)
			if !ok {
				return nil, fmt.Errorf("platform adapter %q does not support CallAction", platformID)
			}
			return ca.CallAction(api, params)
		},
		SendMessage: func(platformID, sessionID string, chain []pluginsdk.Component) error {
			comps := ComponentsFromSDK(chain)
			if len(comps) == 0 {
				return nil
			}
			if pm == nil {
				return fmt.Errorf("platform manager not available")
			}
			return pm.Send(platformID, sessionID, message.NewMessageChain(comps...))
		},
		RecallMessage: func(platformID, messageID string) error {
			adapter := pm.Get(platformID)
			if adapter == nil {
				return fmt.Errorf("platform %q not found", platformID)
			}
			if r, ok := adapter.(RecallAdapter); ok {
				return r.RecallMessage(messageID)
			}
			if ca, ok := adapter.(CallActionAdapter); ok {
				_, err := ca.CallAction("delete_msg", map[string]any{"message_id": messageID})
				return err
			}
			return fmt.Errorf("platform adapter %q does not support RecallMessage", platformID)
		},
		GetConfig: func(pluginName string) (map[string]any, error) {
			// 合并 schema 默认值：Python 插件的 __init__/get_config 依赖配置
			// 带全量默认键（Python AstrBot 语义），裸配置缺键会让插件
			// KeyError（如 box 的 config["protect_ids"]）。
			cfg := resolvePluginConfig(subMgr, pluginName)
			if cfg == nil {
				return map[string]any{}, nil
			}
			return cfg, nil
		},
		SetConfig: func(pluginName string, cfg map[string]any) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			return subMgr.SaveConfig(subMgr.pluginConfigID(pluginName), cfg)
		},
		ChatLLM: func(prompt, systemPrompt string, imageURLs []string) (string, error) {
			if chatLLM == nil {
				return "", fmt.Errorf("ChatLLM not configured on this host")
			}
			return chatLLM(prompt, systemPrompt, imageURLs)
		},
		React: func(platformID, sessionID, messageID, emoji string) error {
			if pm == nil {
				return fmt.Errorf("platform manager not available")
			}
			return pm.React(platformID, sessionID, messageID, emoji)
		},
		TextToImage: func(text, templateName string) (string, error) {
			if text == "" {
				return "", fmt.Errorf("空文本无法渲染")
			}
			data, err := t2i.RenderTextToPNG(text, t2i.ImageOptions{})
			if err != nil {
				return "", fmt.Errorf("t2i 渲染失败: %w", err)
			}
			return base64.StdEncoding.EncodeToString(data), nil
		},
	})
}
