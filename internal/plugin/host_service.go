package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sync"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
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
// （配置目录按 id = name_language 分键）。先查运行中实例（RPC 调用者必然
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

// serializeConversation 把会话对象序列化为 SDK 约定的 map（对齐 Python
// conversation_manager 返回的 JSON 结构），供 GetConversation/GetConversations
// hooks 使用。
func serializeConversation(conv *conversation.Conversation) map[string]any {
	if conv == nil {
		return nil
	}
	return map[string]any{
		"cid":                conv.CID,
		"user_id":            conv.UserID,
		"platform_id":        conv.PlatformID,
		"unified_msg_origin": conv.UnifiedMsgOrigin,
		"persona_id":         conv.Persona,
		"title":              conv.Title,
		"created_at":         conv.CreatedAt,
		"updated_at":         conv.UpdatedAt,
		"is_deleted":         conv.IsDeleted,
	}
}

// ── 人格数据源（data/personas.json）──
//
// lifecycle 包内的 loadPersonas 首字母小写不可跨包访问，此处按同一实现
// 维护一份本地副本（参照 internal/lifecycle/lifecycle.go），供 HostService
// 人格管理 hooks 使用。字段约定：persona_id/name/system_prompt/folder_id/
// is_default，由 dashboard 人格存储写入。

// personaFileCache 缓存 data/personas.json 的解析结果，避免每次调用都读盘。
// 通过文件 mtime 判断内容是否变化，仅在有变更时重新读取。
type personaFileCache struct {
	mu      sync.Mutex
	content []byte
	modTime time.Time
}

var personaCache personaFileCache

// loadPersonas 返回 data/personas.json 解析后的 persona 列表。
// mtime 未变化时直接返回缓存内容，变化时才重读文件。
func loadPersonas() []map[string]any {
	info, err := os.Stat("data/personas.json")
	if err != nil {
		return nil
	}
	personaCache.mu.Lock()
	defer personaCache.mu.Unlock()
	if personaCache.content != nil && personaCache.modTime.Equal(info.ModTime()) {
		return parsePersonas(personaCache.content)
	}
	data, err := os.ReadFile("data/personas.json")
	if err != nil {
		return nil
	}
	personaCache.content = data
	personaCache.modTime = info.ModTime()
	return parsePersonas(data)
}

// parsePersonas 解析 personas.json 的 personas 数组。
func parsePersonas(data []byte) []map[string]any {
	var store struct {
		Personas []map[string]any `json:"personas"`
	}
	if json.Unmarshal(data, &store) != nil {
		return nil
	}
	return store.Personas
}

// personaByID 按 persona_id 在人格列表里查找；未找到返回 nil。
func personaByID(id string) map[string]any {
	for _, p := range loadPersonas() {
		if pid, _ := p["persona_id"].(string); pid == id {
			return p
		}
	}
	return nil
}

// defaultPersona 返回默认人格：优先 is_default 标记为真者，其次取列表
// 第一条兜底；未配置任何人格时返回 nil。
func defaultPersona() map[string]any {
	ps := loadPersonas()
	for _, p := range ps {
		if d, _ := p["is_default"].(bool); d {
			return p
		}
	}
	if len(ps) > 0 {
		return ps[0]
	}
	return nil
}

// StarManagerLike 抽象 Star 管理器对 HostService 需要的最小能力（遍历插件
// 元数据）。用函数值而非 *star.Manager 是为了避免导入环：internal/star 经
// subprocess_bridge.go 已依赖 internal/plugin，plugin 再依赖 star 会构成
// import cycle。由调用方（lifecycle）构造闭包传入：闭包返回的元数据条目
// 是 *star.StarMetadata（字段 Name/Author/Desc/Version/StarModulePath/
// Activated/Repo），hooks 内按字段名反射读取。
type StarManagerLike interface {
	// StarMetadataList 返回全部插件元数据（*star.StarMetadata 的 any 包装）。
	StarMetadataList() []any
}

// StarManagerLikeFunc 是 StarManagerLike 的函数式适配器，供调用方用闭包
// 便捷构造（避免在 plugin 包内匿名 struct 样板）。
type StarManagerLikeFunc func() []any

// StarMetadataList implements StarManagerLike.
func (f StarManagerLikeFunc) StarMetadataList() []any {
	if f == nil {
		return nil
	}
	return f()
}

// StarMetadata 是插件元数据的只读视图（镜像 internal/star.StarMetadata 字段，
// 避免 plugin 包直接依赖 star 包造成导入环）。
type StarMetadata struct {
	Name           string
	Author         string
	Desc           string
	Version        string
	Repo           string
	StarModulePath string
	Activated      bool
	// PluginID 是 internal/star.StarMetadata 的指针接收者方法（Name/Author
	// 拼接的插件 ID），反射调用取得，供 GetStar 按插件 ID 匹配。
	PluginID string
}

// starMetadataFromAny 把 star 注册表返回的元数据条目（*star.StarMetadata）
// 转换为本包只读视图；类型不符时返回 nil（调用方自行跳过）。
func starMetadataFromAny(v any) *StarMetadata {
	s, ok := v.(*StarMetadata)
	if ok {
		return s
	}
	// 反射读取字段（镜像 internal/star.StarMetadata），避免硬编码依赖。
	get := func(name string) string {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer {
			rv = rv.Elem()
		}
		if !rv.IsValid() || rv.Kind() != reflect.Struct {
			return ""
		}
		// #nosec unsafe-reflect-by-name -- name 均为本函数内硬编码的编译期常量（"Name"/"Author"/…），
		// 用于规避 star→plugin 导入环，非用户可控的字段名。
		f := rv.FieldByName(name) // nosemgrep: go.lang.security.audit.unsafe-reflect-by-name.unsafe-reflect-by-name
		if !f.IsValid() {
			return ""
		}
		return f.String()
	}
	getBool := func(name string) bool {
		rv := reflect.ValueOf(v)
		if rv.Kind() == reflect.Pointer {
			rv = rv.Elem()
		}
		if !rv.IsValid() || rv.Kind() != reflect.Struct {
			return false
		}
		f := rv.FieldByName(name) // nosemgrep: go.lang.security.audit.unsafe-reflect-by-name.unsafe-reflect-by-name -- #nosec unsafe-reflect-by-name: name 为本函数硬编码的编译期常量，非用户输入
		if !f.IsValid() {
			return false
		}
		return f.Bool()
	}
	// PluginID 是指针接收者方法（值上取不到），须在指针值上反射调用。
	pluginID := ""
	if rv := reflect.ValueOf(v); rv.IsValid() && rv.Kind() == reflect.Pointer {
		if m := rv.MethodByName("PluginID"); m.IsValid() {
			if out := m.Call(nil); len(out) > 0 && out[0].Kind() == reflect.String {
				pluginID = out[0].String()
			}
		}
	}
	return &StarMetadata{
		Name:           get("Name"),
		Author:         get("Author"),
		Desc:           get("Desc"),
		Version:        get("Version"),
		Repo:           get("Repo"),
		StarModulePath: get("StarModulePath"),
		Activated:      getBool("Activated"),
		PluginID:       pluginID,
	}
}

// SetHostService installs the HostService hooks (reverse plugin -> host RPCs)
// backed by the platform manager, the subprocess plugin manager (for config
// reads/writes), a ChatLLM callback (for plugins calling the LLM directly),
// plus the conversation/provider managers and a star metadata source (closure
// over star.Registry().All() via StarManagerLike，规避 star→plugin 导入环).
// Call once at startup, after all managers exist and before plugins begin
// handling messages.
func SetHostService(pm *platform.PlatformManager, subMgr *SubprocessManager, chatLLM func(prompt, systemPrompt string, imageURLs []string) (string, error), convMgr *conversation.Manager, providerMgr *provider.ProviderManager, starMgr StarManagerLike, cfgMgr *config.ConfigManager) {
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
			// 附带配置 schema（if available），SDK 侧 AstrBotConfig 提取
			// __schema__ 挂到自身 schema 属性，使插件 __init__ 里
			// self.config.schema 可访问（update_manager 等依赖此属性
			// 动态填充 options/labels）。
			if subMgr != nil {
				id := subMgr.pluginConfigID(pluginName)
				inst := subMgr.Get(id)
				if inst != nil && inst.Meta != nil && len(inst.Meta.ConfigSchemaJson) > 0 {
					var schema map[string]any
					if err := json.Unmarshal(inst.Meta.ConfigSchemaJson, &schema); err == nil && schema != nil {
						cfg["__schema__"] = schema
					}
				}
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
		HtmlRender: func(template, data, options string) (string, error) {
			// 优先远程 t2i：配置了 t2i_endpoint 时调用 RenderCustomTemplate
			// （HTML 模板 + 数据 → 图片），失败回退本地 gg 渲染。
			endpoint := ""
			if cfgMgr != nil {
				if cfg := cfgMgr.Get("default"); cfg != nil {
					endpoint = cfg.GetString("t2i_endpoint")
				}
			}
			if endpoint != "" {
				img, err := t2i.RenderCustomTemplate(endpoint, template, data, options)
				if err == nil {
					return base64.StdEncoding.EncodeToString(img), nil
				}
				// 远程失败 → 回退本地渲染。
				logger.Warn("html_render 远程 t2i 渲染失败（%v），回退本地渲染", err)
			}
			// 本地 gg 兜底：模板为空时用 data 作为渲染文本。
			text := template
			if text == "" {
				text = data
			}
			if text == "" {
				return "", fmt.Errorf("html_render: 模板与数据均为空，无法渲染")
			}
			img, err := t2i.RenderTextToPNG(text, t2i.ImageOptions{})
			if err != nil {
				return "", fmt.Errorf("html_render 本地渲染失败: %w", err)
			}
			return base64.StdEncoding.EncodeToString(img), nil
		},

		// ── 会话管理（对齐 Python conversation_manager）──
		GetCurrConversationID: func(umo string) string {
			if convMgr == nil {
				return ""
			}
			return convMgr.GetCurrConversationID(umo)
		},
		NewConversation: func(umo, platformID, personaID string) string {
			if convMgr == nil {
				return ""
			}
			conv := convMgr.NewConversation(umo, platformID)
			if personaID != "" {
				convMgr.SetPersonaByCID(conv.CID, personaID)
			}
			return conv.CID
		},
		GetConversation: func(umo, cid string, createIfNotExists bool) map[string]any {
			if convMgr == nil {
				return nil
			}
			var conv *conversation.Conversation
			if cid != "" {
				conv = convMgr.GetConversationSnapshot(cid)
			} else {
				conv = convMgr.GetConversation(umo)
			}
			if conv == nil && createIfNotExists {
				conv = convMgr.GetOrCreateConversation(umo, "")
			}
			return serializeConversation(conv)
		},
		GetConversations: func(umo string) []map[string]any {
			if convMgr == nil {
				return nil
			}
			var out []map[string]any
			for _, c := range convMgr.AllConversations() {
				if umo != "" && c.UnifiedMsgOrigin != umo {
					continue
				}
				out = append(out, serializeConversation(c))
			}
			return out
		},
		DeleteConversation: func(umo, cid string) error {
			if convMgr == nil {
				return fmt.Errorf("conversation manager not available")
			}
			if cid != "" {
				if !convMgr.DeleteConversationByCID(cid) {
					return fmt.Errorf("conversation %q not found", cid)
				}
				return nil
			}
			convMgr.DeleteConversation(umo)
			return nil
		},
		SwitchConversation: func(umo, cid string) error {
			if convMgr == nil {
				return fmt.Errorf("conversation manager not available")
			}
			// 校验会话存在（GetConversationSnapshot 非 nil）后切换为当前
			// 会话；具体校验与切换逻辑在 Manager 内实现。
			return convMgr.SwitchConversation(umo, cid)
		},
		UpdateConversationTitle: func(umo, cid, title string) error {
			if convMgr == nil {
				return fmt.Errorf("conversation manager not available")
			}
			if cid == "" {
				cid = convMgr.GetCurrConversationID(umo)
			}
			if !convMgr.SetTitleByCID(cid, title) {
				return fmt.Errorf("conversation %q not found", cid)
			}
			return nil
		},
		UpdateConversationPersonaID: func(umo, cid, personaID string) error {
			if convMgr == nil {
				return fmt.Errorf("conversation manager not available")
			}
			if cid == "" {
				cid = convMgr.GetCurrConversationID(umo)
			}
			if !convMgr.SetPersonaByCID(cid, personaID) {
				return fmt.Errorf("conversation %q not found", cid)
			}
			return nil
		},

		// ── 人格管理（对齐 Python persona_manager，数据源 data/personas.json）──
		GetPersonas: func() []map[string]any {
			return loadPersonas()
		},
		GetDefaultPersona: func(umo string) map[string]any {
			// umo 维度不区分默认人格（全局默认），按 is_default 标记或
			// 首条人格兜底返回。
			return defaultPersona()
		},
		GetPersonaTree: func() (folders []map[string]any, personas []map[string]any) {
			ps := loadPersonas()
			// 按 folder_id 聚合出文件夹结构；无 folder_id 的人格只出现在
			// personas 全量列表（对齐 Python persona_manager）。
			byFolder := map[string][]map[string]any{}
			for _, p := range ps {
				fid, _ := p["folder_id"].(string)
				if fid == "" {
					continue
				}
				byFolder[fid] = append(byFolder[fid], p)
			}
			for fid, items := range byFolder {
				folders = append(folders, map[string]any{
					"folder_id": fid,
					"name":      fid,
					"children":  items,
				})
			}
			return folders, ps
		},
		ResolveSelectedPersona: func(umo, conversationPersonaID, platformName string, providerSettings map[string]any) (string, string, string, string, bool) {
			// 优先级：会话绑定人格 → Provider 默认人格 → 全局默认人格 → 无。
			if conversationPersonaID != "" && conversationPersonaID != "[%None]" {
				if p := personaByID(conversationPersonaID); p != nil {
					id, _ := p["persona_id"].(string)
					name, _ := p["name"].(string)
					prompt, _ := p["system_prompt"].(string)
					return id, name, prompt, "", false
				}
			}
			if ps, ok := providerSettings["default_personality"].(string); ok && ps != "" && ps != "[%None]" {
				if p := personaByID(ps); p != nil {
					id, _ := p["persona_id"].(string)
					name, _ := p["name"].(string)
					prompt, _ := p["system_prompt"].(string)
					return id, name, prompt, "", true
				}
			}
			if def := defaultPersona(); def != nil {
				id, _ := def["persona_id"].(string)
				name, _ := def["name"].(string)
				prompt, _ := def["system_prompt"].(string)
				return id, name, prompt, "", true
			}
			return "[%None]", "", "", "", false
		},

		// ── Provider 管理（对齐 Python provider_manager）──
		ListProviders: func(capability string) []map[string]any {
			if providerMgr == nil {
				return nil
			}
			var out []map[string]any
			for _, id := range providerMgr.All() {
				p := providerMgr.Get(id)
				if p == nil {
					continue
				}
				meta := p.Meta()
				if capability != "" && string(meta.ProviderType) != capability {
					continue
				}
				out = append(out, map[string]any{
					"id":            meta.ID,
					"model":         meta.Model,
					"type":          meta.Type,
					"provider_type": string(meta.ProviderType),
				})
			}
			return out
		},
		GetUsingProvider: func(umo, capability string) map[string]any {
			if providerMgr == nil {
				return nil
			}
			if capability == "" || capability == "chat_completion" {
				p := providerMgr.GetChatProvider()
				if p != nil {
					meta := p.Meta()
					return map[string]any{
						"id":            meta.ID,
						"model":         meta.Model,
						"type":          meta.Type,
						"provider_type": string(meta.ProviderType),
					}
				}
			}
			if capability == "text_to_speech" {
				p := providerMgr.GetTTSProvider()
				if p != nil {
					meta := p.Meta()
					return map[string]any{"id": meta.ID, "model": meta.Model, "type": meta.Type, "provider_type": string(meta.ProviderType)}
				}
			}
			if capability == "speech_to_text" {
				p := providerMgr.GetSTTProvider()
				if p != nil {
					meta := p.Meta()
					return map[string]any{"id": meta.ID, "model": meta.Model, "type": meta.Type, "provider_type": string(meta.ProviderType)}
				}
			}
			return nil
		},
		SetProvider: func(umo, providerID, capability string) error {
			if providerMgr == nil {
				return fmt.Errorf("provider manager not available")
			}
			if providerMgr.Get(providerID) == nil {
				return fmt.Errorf("provider %q not found", providerID)
			}
			if capability == "" || capability == "chat_completion" {
				providerMgr.SetDefaultChatProvider(providerID)
			} else if capability == "text_to_speech" {
				providerMgr.SetDefaultTTSProvider(providerID)
			} else if capability == "speech_to_text" {
				providerMgr.SetDefaultSTTProvider(providerID)
			} else {
				return fmt.Errorf("unsupported capability %q", capability)
			}
			return nil
		},
		GetProviderModels: func(providerID string) []string {
			// 模型列表需要 provider 具体实现，宿主暂不实现，返回 nil。
			return nil
		},

		// ── 插件/Star 管理（对齐 Python star_manager）──
		ListStars: func() []map[string]any {
			// 用子进程插件的静态清单（含已安装但休眠/禁用的插件），对齐
			// Python 原版 star_registry 语义（原版插件全常驻、注册表含全部
			// 插件）：update_manager 等依赖 get_all_stars 枚举全部插件以
			// 填充黑/白名单选项的管理类插件，不能只看到运行中插件。
			if subMgr != nil {
				var out []map[string]any
				for _, info := range subMgr.ListInfo() {
					name, _ := info["name"].(string)
					displayName, _ := info["display_name"].(string)
					if displayName == "" {
						displayName = name
					}
					desc, _ := info["description"].(string)
					if desc == "" {
						desc, _ = info["short_desc"].(string)
					}
					author, _ := info["author"].(string)
					version, _ := info["version"].(string)
					repo, _ := info["repo"].(string)
					activated, _ := info["activated"].(bool)
					reserved, _ := info["reserved"].(bool)
					out = append(out, map[string]any{
						"name":         name,
						"display_name": displayName,
						"author":       author,
						"desc":         desc,
						"version":      version,
						"module_path":  "data.plugins." + name,
						"activated":    activated,
						"repo":         repo,
						"reserved":     reserved,
					})
				}
				return out
			}
			if starMgr == nil {
				return nil
			}
			var out []map[string]any
			for _, raw := range starMgr.StarMetadataList() {
				m := starMetadataFromAny(raw)
				if m == nil {
					continue
				}
				out = append(out, map[string]any{
					"name":        m.Name,
					"author":      m.Author,
					"desc":        m.Desc,
					"version":     m.Version,
					"module_path": m.StarModulePath,
					"activated":   m.Activated,
					"repo":        m.Repo,
				})
			}
			return out
		},
		GetStar: func(name string) map[string]any {
			// 与 ListStars 一致：从静态插件清单查找（含休眠/禁用插件）。
			if subMgr != nil {
				for _, info := range subMgr.ListInfo() {
					instName, _ := info["name"].(string)
					if instName != name {
						continue
					}
					displayName, _ := info["display_name"].(string)
					if displayName == "" {
						displayName = instName
					}
					desc, _ := info["description"].(string)
					if desc == "" {
						desc, _ = info["short_desc"].(string)
					}
					author, _ := info["author"].(string)
					version, _ := info["version"].(string)
					repo, _ := info["repo"].(string)
					activated, _ := info["activated"].(bool)
					reserved, _ := info["reserved"].(bool)
					return map[string]any{
						"name":         instName,
						"display_name": displayName,
						"author":       author,
						"desc":         desc,
						"version":      version,
						"module_path":  "data.plugins." + instName,
						"activated":    activated,
						"repo":         repo,
						"reserved":     reserved,
					}
				}
				return nil
			}
			if starMgr == nil {
				return nil
			}
			for _, raw := range starMgr.StarMetadataList() {
				m := starMetadataFromAny(raw)
				// 按插件名或插件 ID（Name/Author 拼接）匹配。
				if m == nil || (m.Name != name && m.PluginID != name) {
					continue
				}
				return map[string]any{
					"name":        m.Name,
					"author":      m.Author,
					"desc":        m.Desc,
					"version":     m.Version,
					"module_path": m.StarModulePath,
					"activated":   m.Activated,
					"repo":        m.Repo,
				}
			}
			return nil
		},
		SetPluginEnabled: func(pluginName string, enabled bool) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			id := subMgr.pluginConfigID(pluginName)
			return subMgr.SetEnabled(id, enabled)
		},
		InstallPlugin: func(repo string) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			_, err := subMgr.InstallFromSource(context.Background(), "", repo, InstallOptions{})
			return err
		},
		UninstallPlugin: func(pluginName string) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			id := subMgr.pluginConfigID(pluginName)
			return subMgr.Uninstall(id, false, false)
		},

		// ── 会话等待（SessionWaiter 跨进程喂入）──
		RegisterSessionWait: func(pluginName, umo string, timeoutSeconds int32) string {
			// 插件注册"等待 umo 的下一条消息"：宿主记录到等待注册表并返回
			// wait_id；管线 SessionWaitStage 收到该 umo 消息时经
			// FeedSessionWait RPC 推送事件（subMgr 未就绪视为宿主不支持）。
			if subMgr == nil {
				return ""
			}
			return subMgr.RegisterSessionWait(pluginName, umo, timeoutSeconds)
		},
		UnregisterSessionWait: func(waitID string) {
			if subMgr == nil {
				return
			}
			subMgr.UnregisterSessionWait(waitID)
		},
	})
}
