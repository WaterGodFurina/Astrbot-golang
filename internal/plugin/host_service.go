package plugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
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
			img := &message.Image{URL: c.URL, Path: c.Path, File: c.File, Base64: c.Base64, FileID: c.FileID}
			// data:image/...;base64,xxx / base64://xxx 形式的图片内容归一化到
			// Base64 字段（Python 插件 text_to_image/html_render 返回 data URI，
			// 若不归一化会被当作本地文件路径发送 → OneBot stat ENAMETOOLONG）。
			if img.Base64 == "" {
				if b64, ok := mediaDataURIToBase64(c.File); ok {
					img.Base64 = b64
					img.File = ""
				}
			}
			// SDK Image.fromFileSystem 对 data URI 误做 Path.resolve 时，伪装
			// 成绝对路径的 data URI 会同时出现在 path 字段（file 为 file://
			// URI）：path 一律清除（含 base64 已提取的场景），防止其他适配器
			// 按路径读取失败。
			if b64, ok := mediaDataURIToBase64(c.Path); ok {
				if img.Base64 == "" {
					img.Base64 = b64
				}
				img.Path = ""
			}
			out = append(out, img)
		case pluginsdk.CompRecord:
			rec := &message.Record{URL: c.URL, Path: c.Path, File: c.File, Base64: c.Base64, FileID: c.FileID}
			if rec.Base64 == "" {
				if b64, ok := mediaDataURIToBase64(c.File); ok {
					rec.Base64 = b64
					rec.File = ""
				}
			}
			if b64, ok := mediaDataURIToBase64(c.Path); ok {
				if rec.Base64 == "" {
					rec.Base64 = b64
				}
				rec.Path = ""
			}
			out = append(out, rec)
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

// mediaDataURIToBase64 提取 media 内容为裸 base64：
//   - base64://xxx
//   - data:image/png;base64,xxx（裸 data URI）
//   - file://<插件CWD>/data:image/png;base64,xxx 或 <绝对路径>/data:image/...
//     （Python SDK Image.fromFileSystem 对 data URI 误做 Path.resolve+as_uri
//     后的形态——data URI 被当作相对路径拼到插件工作目录，宿主必须识别并
//     归一化，否则会被当本地文件路径发送 → OneBot stat ENAMETOOLONG）
//
// 非此类字符串返回 ok=false。
func mediaDataURIToBase64(v string) (string, bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", false
	}
	if rest, found := strings.CutPrefix(s, "base64://"); found {
		return rest, true
	}
	lower := strings.ToLower(s)
	idx := strings.Index(lower, ";base64,")
	if idx < 0 {
		return "", false
	}
	// 要求 ";base64," 之前是 data:media 形态（允许 file:// 或绝对路径前缀）。
	if dataIdx := strings.LastIndex(lower[:idx], "data:"); dataIdx >= 0 {
		if strings.HasPrefix(lower[dataIdx:], "data:") {
			return s[idx+len(";base64,"):], true
		}
	}
	return "", false
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

// pluginAdminListFromConfig 从主配置读取插件管理管理员名单（config 键
// plugin_admin_list，字符串数组）。缺省返回 nil（无管理插件）。
func pluginAdminListFromConfig(cfgMgr *config.ConfigManager) []string {
	if cfgMgr == nil {
		return nil
	}
	cfg := cfgMgr.Get("default")
	if cfg == nil {
		return nil
	}
	v := cfg.GetNested("plugin_admin_list")
	if v == nil {
		return nil
	}
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
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

// personaDataPath 是人格数据文件路径。SetHostService 时按 subMgr.dataDir
// 对齐 dashboard 的写入路径（filepath.Join(dataDir, "personas.json")），
// 避免宿主 CWD 非项目根/配置 dataDir 非 "data" 时插件人格 RPC 静默读空。
var personaDataPath = "data/personas.json"

// loadPersonas 返回 data/personas.json 解析后的 persona 列表。
// mtime 未变化时直接返回缓存内容，变化时才重读文件。
func loadPersonas() []map[string]any {
	info, err := os.Stat(personaDataPath)
	if err != nil {
		return nil
	}
	personaCache.mu.Lock()
	defer personaCache.mu.Unlock()
	if personaCache.content != nil && personaCache.modTime.Equal(info.ModTime()) {
		return parsePersonas(personaCache.content)
	}
	data, err := os.ReadFile(personaDataPath)
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
	// CommandDescriptors 返回全部插件的命令描述符（含子命令/组/别名/权限/
	// 描述，map 序列化）。供插件桥查询全局指令列表（子进程架构下 helps 类
	// 插件无法从自身进程的注册表枚举其他插件指令）。
	CommandDescriptors() []map[string]any
}

// StarManagerLikeFunc 是 StarManagerLike 的函数式适配器，供调用方用闭包
// 便捷构造（避免在 plugin 包内匿名 struct 样板）。CmdFn 为 nil 时
// CommandDescriptors 返回 nil。
type StarManagerLikeFunc struct {
	Fn    func() []any
	CmdFn func() []map[string]any
}

// StarMetadataList implements StarManagerLike.
func (f StarManagerLikeFunc) StarMetadataList() []any {
	if f.Fn == nil {
		return nil
	}
	return f.Fn()
}

// CommandDescriptors implements StarManagerLike.
func (f StarManagerLikeFunc) CommandDescriptors() []map[string]any {
	if f.CmdFn == nil {
		return nil
	}
	return f.CmdFn()
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

// ChatLLMCmd 承载插件 ChatLLM 反向调用的完整参数（含多模态音频/工具/
// 多轮上下文/指定 provider），由 Go SDK HostServiceHooks.ChatLLM hook
// 从 sdkv1.ChatLLMRequest 解出后传给宿主回调。
type ChatLLMCmd struct {
	Prompt       string
	SystemPrompt string
	ImageURLs    []string
	AudioURLs    []string
	Tools        []map[string]interface{}
	Contexts     []map[string]interface{}
	ProviderID   string
}

// HostServiceExtras carries optional host-side managers layered onto the
// plugin HostService hooks after the base set (platform/plugin/conversation/
// provider/persona). They back the skills + platform-message-history RPCs the
// SDK now exposes (ListSkills / SetSkillActive / DeleteSkill /
// GetPlatformMessageHistory / Insert/Update/DeletePlatformMessageHistory).
type HostServiceExtras struct {
	// SkillMgr backs ListSkills/SetSkillActive/DeleteSkill when non-nil.
	SkillMgr *skills.SkillManager
	// Database backs the platform-message-history RPCs when non-nil.
	Database *db.Database
}

// hostExtras is the active set of optional extras passed to SetHostService.
var hostExtras HostServiceExtras

// SetHostService installs the HostService hooks (reverse plugin -> host RPCs)
// backed by the platform manager, the subprocess plugin manager (for config
// reads/writes), a ChatLLM callback (for plugins calling the LLM directly),
// plus the conversation/provider managers and a star metadata source (closure
// over star.Registry().All() via StarManagerLike，规避 star→plugin 导入环).
// Call once at startup, after all managers exist and before plugins begin
// handling messages.
func SetHostService(pm *platform.PlatformManager, subMgr *SubprocessManager, chatLLM func(cmd ChatLLMCmd) (string, error), convMgr *conversation.Manager, providerMgr *provider.ProviderManager, starMgr StarManagerLike, cfgMgr *config.ConfigManager, extra ...HostServiceExtras) {
	if len(extra) > 0 {
		hostExtras = extra[0]
	}
	if subMgr != nil && subMgr.dataDir != "" {
		personaDataPath = filepath.Join(subMgr.dataDir, "personas.json")
		// 大文件 Blob 存储根目录 data/blobs（P0-2）。启动即初始化，TTL 10min。
		bs, err := NewBlobStore(filepath.Join(subMgr.dataDir, "blobs"), 10*time.Minute, 1<<20)
		if err != nil {
			logger.I18nWarn("初始化 Blob 存储失败: %v", err)
		} else {
			setBlobStore(bs)
			logger.Info("Blob 存储已就绪: %s", filepath.Join(subMgr.dataDir, "blobs"))
		}
	}
	// 插件管理管理员名单：默认无管理插件（插件仅能启停自身）。
	// config 的 plugin_admin_list 数组可授权指定插件（注册名，如
	// astrbot_plugin_xxx_python）执行安装/卸载/操作其他插件。
	pluginsdk.SetPluginAdminList(pluginAdminListFromConfig(cfgMgr))
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
				// __schema__ 注入**扁平**结构（对齐原版插件期望）：插件侧
				// self.config.schema 是"配置项名 → 元数据"的 dict，插件
				// 直接按顶层 key 访问（如 update_manager 的
				// schema.get("white_plugin_list")）。Register 上报的
				// ConfigSchemaJson 是 WebUI 用的 {"type","properties"}
				// 包装（SDK _load_config_schema），需展开 properties。
				if s := subMgr.ConfigSchema(id); len(s) > 0 {
					if props, ok := s["properties"].(map[string]any); ok {
						cfg["__schema__"] = props
					} else {
						cfg["__schema__"] = s
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
		RegisterBridgeHook: func(pluginName, hookName string) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			id := subMgr.pluginConfigID(pluginName)
			if inst := subMgr.Get(id); inst == nil {
				// 找不到运行实例：不破坏插件启动，告警后忽略。
				logger.I18nWarn("插件 %s 注册桥接钩子 %s 失败：实例不存在", pluginName, hookName)
				return nil
			}
			subMgr.RegisterBridgeHook(id, hookName)
			return nil
		},
		UnregisterBridgeHook: func(pluginName, hookName string) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			id := subMgr.pluginConfigID(pluginName)
			if inst := subMgr.Get(id); inst == nil {
				logger.I18nWarn("插件 %s 注销桥接钩子 %s 失败：实例不存在", pluginName, hookName)
				return nil
			}
			subMgr.UnregisterBridgeHook(id, hookName)
			return nil
		},

		// ── 大文件 Blob 存储（P0-2）──
		CreateBlob: func(data []byte, mimeType, filename string, ttlSeconds int32) (*sdkv1.FileReference, error) {
			bs := getBlobStore()
			if bs == nil {
				return nil, fmt.Errorf("blob store not initialized")
			}
			ref, err := bs.Create(data, mimeType, filename, ttlSeconds)
			if err != nil {
				return nil, err
			}
			return &ref, nil
		},
		ReadBlob: func(handleID string, offset int64, limit int32) ([]byte, bool, int64, error) {
			bs := getBlobStore()
			if bs == nil {
				return nil, false, 0, fmt.Errorf("blob store not initialized")
			}
			return bs.Read(handleID, offset, limit)
		},
		GetBlobInfo: func(handleID string) (*sdkv1.FileReference, error) {
			bs := getBlobStore()
			if bs == nil {
				return nil, fmt.Errorf("blob store not initialized")
			}
			ref, err := bs.Info(handleID)
			if err != nil {
				return nil, err
			}
			return &ref, nil
		},
		ReleaseBlob: func(handleID string) error {
			bs := getBlobStore()
			if bs == nil {
				return fmt.Errorf("blob store not initialized")
			}
			return bs.Release(handleID)
		},
		ChatLLM: func(req *sdkv1.ChatLLMRequest) (string, error) {
			if chatLLM == nil {
				return "", fmt.Errorf("ChatLLM not configured on this host")
			}
			cmd := ChatLLMCmd{
				Prompt:       req.Prompt,
				SystemPrompt: req.SystemPrompt,
				ImageURLs:    req.ImageUrls,
				AudioURLs:    req.AudioUrls,
				ProviderID:   req.ProviderId,
			}
			if len(req.ToolsJson) > 0 {
				if err := json.Unmarshal(req.ToolsJson, &cmd.Tools); err != nil {
					return "", fmt.Errorf("ChatLLM tools_json 解析失败: %w", err)
				}
			}
			if len(req.ContextsJson) > 0 {
				if err := json.Unmarshal(req.ContextsJson, &cmd.Contexts); err != nil {
					return "", fmt.Errorf("ChatLLM contexts_json 解析失败: %w", err)
				}
			}
			return chatLLM(cmd)
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
			// 远程 t2i（HTML 模板 → 图片）：优先用户配置的 t2i_endpoint，
			// 未配置时用官方默认端点（RenderCustomTemplate 内部解析，对齐
			// 原版 ASTRBOT_T2I_DEFAULT_ENDPOINT + 官方端点列表容灾）。
			// 远程失败直接返回错误，让插件降级到纯文本渲染（text_to_image）
			// ——绝不能把 HTML 模板当纯文本渲染，否则帮助图显示 HTML 源码。
			endpoint := ""
			if cfgMgr != nil {
				if cfg := cfgMgr.Get("default"); cfg != nil {
					endpoint = cfg.GetString("t2i_endpoint")
				}
			}
			img, err := t2i.RenderCustomTemplate(endpoint, template, data, options)
			if err != nil {
				return "", fmt.Errorf("html_render 远程渲染失败: %w", err)
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
				entry := map[string]any{
					"id":            meta.ID,
					"model":         meta.Model,
					"type":          meta.Type,
					"provider_type": string(meta.ProviderType),
				}
				applyProviderCredentials(entry, p)
				out = append(out, entry)
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
			if capability == "embedding" {
				p := providerMgr.GetEmbeddingProvider()
				if p != nil {
					meta := p.Meta()
					entry := map[string]any{"id": meta.ID, "model": meta.Model, "type": meta.Type, "provider_type": string(meta.ProviderType)}
					applyProviderCredentials(entry, p)
					return entry
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
			// 复用 provider 源现有的 GetModels(ctx)（openai 家族：Zhipu/GLM、
			// DashScope、Groq、XAI、Xiaomi、Kimi、OpenAI Responses 等），供
			// 插件查询模型列表（如群总结插件挑选总结用的模型）。未实现
			// GetModels 的源（Gemini/Anthropic/Ollama 等）返回空。
			if providerMgr == nil {
				return nil
			}
			p := providerMgr.Get(providerID)
			if gm, ok := p.(interface {
				GetModels(context.Context) ([]string, error)
			}); ok {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				if models, err := gm.GetModels(ctx); err == nil {
					return models
				} else {
					logger.Warn("GetProviderModels(%s) 拉取模型列表失败: %v", providerID, err)
				}
			}
			return nil
		},

		// ── 插件/Star 管理（对齐 Python star_manager）──
		GetPluginRegistry: func() []map[string]any {
			// 用子进程插件的静态清单（含已安装但休眠/禁用的插件），对齐
			// Python 原版 star_registry 语义（原版插件全常驻、注册表含全部
			// 插件）：update_manager 等依赖 get_all_stars 枚举全部插件以
			// 填充黑/白名单选项的管理类插件，不能只看到运行中插件。
			// 每插件附带 commands（宿主聚合的指令描述符）：helps 类插件经
			// GetPluginRegistry 通道跨进程枚举全部插件指令。
			//
			// commands 数据源用 RegisteredPlugins() 的 inst.Meta.Commands
			//（插件 Register RPC 完成后宿主持有，含休眠占位实例），不依赖
			// starMgr 的运行时注册时序——首次加载期间插件注册完成即可拿到
			// 全部指令，避免"插件注册时宿主 starMgr 尚未 Rebridge"导致
			// helps 类插件只看到自己指令的竞态。
			commandsByPlugin := map[string][]map[string]any{}
			if subMgr != nil {
				for _, inst := range subMgr.RegisteredPlugins() {
					if inst == nil || inst.Meta == nil {
						continue
					}
					var cmds []map[string]any
					for _, c := range inst.Meta.Commands {
						cmds = append(cmds, map[string]any{
							"plugin_name":    inst.ID,
							"command":        c.Name,
							"handler_name":   c.Name,
							"aliases":        c.Aliases,
							"description":    c.Description,
							"permission":     c.Permission,
							"enabled":        true,
							"command_type":   "command",
							"parent_group":   c.ParentGroup,
							"is_sub_command": c.IsSubCommand,
						})
					}
					if len(cmds) > 0 {
						commandsByPlugin[inst.ID] = cmds
					}
				}
			}
			// 内置指令（help/sid 等）来自 starMgr 的全局命令描述符：宿主
			// 启动即注册内置指令，不受插件加载时序影响。
			if starMgr != nil {
				for _, d := range starMgr.CommandDescriptors() {
					pid, _ := d["plugin_name"].(string)
					if pid == "" || pid == "astrbot" {
						if pid == "astrbot" {
							commandsByPlugin["astrbot"] = append(commandsByPlugin["astrbot"], d)
						}
						continue
					}
					// 宿主侧启停/命令权限调整时，以 starMgr 的描述符为准
					// 修正 Meta 快照的 enabled/permission（Meta 恒为全量）。
					existing := commandsByPlugin[pid]
					for i, e := range existing {
						en, _ := e["command"].(string)
						den, _ := d["command"].(string)
						if en == den {
							if v, ok := d["enabled"]; ok {
								existing[i]["enabled"] = v
							}
							if v, ok := d["permission"]; ok && v != "" {
								existing[i]["permission"] = v
							}
						}
					}
				}
			}
			withCommands := func(id string, base map[string]any) map[string]any {
				if cmds := commandsByPlugin[id]; len(cmds) > 0 {
					base["commands"] = cmds
				}
				return base
			}
			if subMgr != nil {
				// 直接透传 ListInfo 返回的整条 info map（含 author/
				// support_platforms/astrbot_version/i18n/pages/logo_path 等
				// 对齐字段），再附加 commands，避免手挑字段遗漏新增元数据。
				var out []map[string]any
				for _, info := range subMgr.ListInfo() {
					instID, _ := info["id"].(string)
					out = append(out, withCommands(instID, info))
				}
				// 内置指令以独立条目附带（id=astrbot，对齐 Python 内置
				// star_registry 的保留插件）。
				if cmds := commandsByPlugin["astrbot"]; len(cmds) > 0 {
					out = append(out, map[string]any{
						"name":         "astrbot",
						"id":           "astrbot",
						"display_name": "AstrBot",
						"reserved":     true,
						"activated":    true,
						"commands":     cmds,
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
				out = append(out, withCommands(m.PluginID, map[string]any{
					"name":        m.Name,
					"author":      m.Author,
					"desc":        m.Desc,
					"version":     m.Version,
					"module_path": m.StarModulePath,
					"activated":   m.Activated,
					"repo":        m.Repo,
				}))
			}
			return out
		},
		GetStar: func(name string) map[string]any {
			// 与 GetPluginRegistry 一致：从静态插件清单查找（含休眠/禁用插件）。
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
			// 安装可能下载/编译数分钟：带超时 context，避免无界后台拉取。
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			_, err := subMgr.InstallFromSource(ctx, "", repo, InstallOptions{})
			return err
		},
		UninstallPlugin: func(pluginName string) error {
			if subMgr == nil {
				return fmt.Errorf("plugin manager not available")
			}
			id := subMgr.pluginConfigID(pluginName)
			return subMgr.Uninstall(id, false, false)
		},
		ListPlatforms: func() []map[string]any {
			// 全部已加载平台实例元数据（id/type/name/display_name）：
			// 子进程架构下插件无法访问宿主 Go 平台对象，经此发现平台并
			// 构造跨进程 bot 代理（call_action 转发宿主），群分析类插件
			// 初始化时调用（非消息路径，无运行期开销）。
			if pm == nil {
				return nil
			}
			var out []map[string]any
			for _, a := range pm.All() {
				if a == nil {
					continue
				}
				t := a.Type()
				out = append(out, map[string]any{
					"id":           a.ID(),
					"type":         t,
					"name":         t,
					"display_name": a.ID(),
				})
			}
			return out
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

		// ── 技能（Skills）──
		ListSkills: func() []map[string]any {
			if hostExtras.SkillMgr == nil {
				return nil
			}
			return hostExtras.SkillMgr.ListSkillsInfo()
		},
		SetSkillActive: func(name string, active bool) error {
			if hostExtras.SkillMgr == nil {
				return nil
			}
			return hostExtras.SkillMgr.SetSkillActive(name, active)
		},
		DeleteSkill: func(name string) error {
			if hostExtras.SkillMgr == nil {
				return nil
			}
			return hostExtras.SkillMgr.DeleteSkill(name)
		},

		// ── 平台消息历史 ──
		GetPlatformMessageHistory: func(platformID, userID string, limit int32) []map[string]any {
			if hostExtras.Database == nil {
				return nil
			}
			if limit <= 0 {
				limit = 200
			}
			rows, err := hostExtras.Database.GetPlatformMessageHistory(platformID, userID, int(limit))
			if err != nil {
				return nil
			}
			out := make([]map[string]any, 0, len(rows))
			for _, r := range rows {
				out = append(out, map[string]any{
					"id":          r.ID,
					"platform_id": platformID,
					"user_id":     userID,
					"sender_id":   r.SenderID,
					"content":     decodePMHistoryContent(r.Content),
					"created_at":  r.CreatedAt,
				})
			}
			return out
		},
		InsertPlatformMessageHistory: func(platformID, userID, senderID string, content any, llmCheckpointID string, maxMessages int32) map[string]any {
			if hostExtras.Database == nil {
				return map[string]any{}
			}
			contentJSON := encodePMHistoryContent(content)
			if err := hostExtras.Database.RecordPlatformMessage(platformID, userID, senderID, contentJSON); err != nil {
				return map[string]any{}
			}
			if maxMessages > 0 {
				_ = hostExtras.Database.TrimPlatformMessageHistory(platformID, userID, int(maxMessages))
			}
			if rows, err := hostExtras.Database.GetPlatformMessageHistory(platformID, userID, 1); err == nil && len(rows) > 0 {
				return map[string]any{
					"id":                rows[0].ID,
					"platform_id":       platformID,
					"user_id":           userID,
					"sender_id":         rows[0].SenderID,
					"content":           decodePMHistoryContent(rows[0].Content),
					"llm_checkpoint_id": llmCheckpointID,
					"created_at":        rows[0].CreatedAt,
				}
			}
			return map[string]any{
				"platform_id":       platformID,
				"user_id":           userID,
				"sender_id":         senderID,
				"content":           content,
				"llm_checkpoint_id": llmCheckpointID,
			}
		},
		UpdatePlatformMessageHistory: func(id int64, content any, llmCheckpointID string) error {
			if hostExtras.Database == nil {
				return nil
			}
			var contentPtr *string
			if content != nil {
				encoded := encodePMHistoryContent(content)
				contentPtr = &encoded
			}
			var ckPtr *string
			if llmCheckpointID != "" {
				ck := llmCheckpointID
				ckPtr = &ck
			}
			_, err := hostExtras.Database.UpdatePlatformMessageHistory(id, contentPtr, ckPtr)
			return err
		},
		DeletePlatformMessageHistory: func(id int64) error {
			if hostExtras.Database == nil {
				return nil
			}
			_, err := hostExtras.Database.DeletePlatformMessageHistoryByID(id)
			return err
		},
	})
}

// applyProviderCredentials copies provider credentials (key/api_base) from the
// provider's own config into the payload returned to Python plugins, so that
// SDK-side providers (e.g. EmbeddingProvider) can call OpenAI-compatible
// endpoints directly. Keys stay on the local unix-socket gRPC channel and are
// never exposed beyond plugin subprocesses on the same machine.
func applyProviderCredentials(entry map[string]any, p provider.AbstractProvider) {
	ch, ok := p.(interface{ Config() map[string]interface{} })
	if !ok {
		return
	}
	cfg := ch.Config()
	if cfg == nil {
		return
	}
	// key may be a list (rotation pool) or a plain string.
	if keys, ok := cfg["key"].([]interface{}); ok && len(keys) > 0 {
		if k, ok := keys[0].(string); ok {
			entry["key"] = k
		}
	} else if k, ok := cfg["key"].(string); ok {
		entry["key"] = k
	}
	// Embedding sources prefer embedding-specific endpoint overrides.
	if ab, ok := cfg["embedding_api_base"].(string); ok && ab != "" {
		entry["api_base"] = ab
	} else if ab, ok := cfg["api_base"].(string); ok && ab != "" {
		entry["api_base"] = ab
	}
	if ek, ok := cfg["embedding_api_key"].(string); ok && ek != "" {
		entry["key"] = ek
	}
}

// decodePMHistoryContent 把 db 存储的 content 串转回任意值：优先 JSON，
// 失败时原样字符串（历史留档可能是非 JSON 的纯文本）。
func decodePMHistoryContent(s string) any {
	var out any
	if s != "" && json.Unmarshal([]byte(s), &out) == nil {
		return out
	}
	return s
}

// encodePMHistoryContent 把 content 编码为 db 存储串：可 JSON 化值转 JSON，
// 字符串原样保留（与 RecordPlatformMessage 的 content 语义一致）。
func encodePMHistoryContent(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	b, err := json.Marshal(content)
	if err != nil {
		return ""
	}
	return string(b)
}
