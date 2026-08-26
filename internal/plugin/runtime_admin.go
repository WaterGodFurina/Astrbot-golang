// Package plugin - subprocess runtime management extensions: dashboard-facing
// list/enable/disable/uninstall and config storage, mirroring the legacy .so
// manager's extensions.go semantics but backed by the install manifest and
// compiled binaries instead of .so files.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
)

// ListInfo returns dashboard-compatible plugin info: running subprocess
// instances plus installed-but-disabled plugins from the persisted manifest,
// so the WebUI keeps showing plugins that were switched off.
func (m *SubprocessManager) ListInfo() []map[string]interface{} {
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		man = &Manifest{Version: 1}
	}
	entryByID := make(map[string]*ManifestEntry, len(man.Plugins))
	for i := range man.Plugins {
		entryByID[man.Plugins[i].ID] = &man.Plugins[i]
	}

	// 容量预分配：在锁内读 len(m.instances)，避免无锁并发读写实例表。
	m.mu.RLock()
	instCap := len(m.instances)
	m.mu.RUnlock()
	result := make([]map[string]interface{}, 0, instCap+len(man.Plugins))
	for _, inst := range m.List() {
		desc := ""
		if inst.Meta != nil {
			desc = inst.Meta.Description
		}
		e := entryByID[inst.ID]
		info := map[string]interface{}{
			"name":                    inst.Name,
			"display_name":            m.pluginDisplayName(inst, e),
			"short_desc":              m.pluginShortDesc(inst, e),
			"marketplace_name":        strings.ReplaceAll(inst.Name, "_", "-"),
			"version":                 inst.Version,
			"description":             desc,
			"path":                    inst.Binary,
			"id":                      inst.ID,
			"language":                inst.Language,
			"logo":                    m.pluginLogoURL(inst.ID),
			"loaded":                  true,
			"enabled":                 true,
			"activated":               true,
			"reserved":                false,
			"author":                  "",
			"repo":                    "",
			"idle_unload_blocked":     e != nil && e.IdleUnloadBlocked,
			"install_source":          nil,
			"updates_enabled":         true,
			"update_disabled_reason":  "",
			"logo_path":               "",
			"support_platforms":       []string{},
			"astrbot_version":         "",
			"i18n":                    nil,
			"pages":                   []interface{}{},
			"root_dir_name":           "",
			"star_handler_full_names": []string{},
		}
		if e != nil {
			info["repo"] = e.Repo
			if info["repo"] == "" {
				info["repo"] = e.Source
			}
			info["install_source"] = e.installSourceMap()
			info["updates_enabled"] = updatesEnabled(e.InstallMethod)
			if !updatesEnabled(e.InstallMethod) {
				info["update_disabled_reason"] = "该插件缺少可用的安装源或仓库地址，无法更新或重新安装"
			}
			// 以下字段从持久化 manifest 条目读取（对齐 Python StarMetadata），
			// entry 为 nil 时保持上方的空值。
			info["author"] = e.Author
			info["logo_path"] = e.LogoPath
			info["support_platforms"] = e.SupportPlatforms
			info["astrbot_version"] = e.AstrbotVersion
			info["i18n"] = e.I18n
			info["pages"] = e.Pages
			info["root_dir_name"] = e.ID
		}
		result = append(result, info)
	}
	// manifest 中已启用但当前未加载的插件：可能是闲置自动卸载（idle sweep）
	// 后处于休眠状态——插件仍启用，触发时会自动唤醒。
	for _, e := range man.Plugins {
		if m.Get(e.ID) != nil {
			continue
		}
		repo := e.Repo
		if repo == "" {
			repo = e.Source
		}
		desc := "插件已禁用"
		enabled := false
		if e.Enabled {
			// 已启用但进程不在：闲置自动卸载后的休眠状态（懒加载会自动唤醒）。
			desc = "插件已休眠（闲置自动卸载，使用时自动唤醒）"
			enabled = true
		}
		result = append(result, map[string]interface{}{
			"name":                    e.Name,
			"display_name":            m.pluginDisplayName(nil, &e),
			"short_desc":              m.pluginShortDesc(nil, &e),
			"marketplace_name":        strings.ReplaceAll(e.Name, "_", "-"),
			"version":                 e.Version,
			"description":             desc,
			"path":                    e.Binary,
			"id":                      e.ID,
			"language":                e.Language,
			"logo":                    m.pluginLogoURL(e.ID),
			"loaded":                  false,
			"enabled":                 enabled,
			"activated":               false,
			"reserved":                false,
			"author":                  e.Author,
			"repo":                    repo,
			"idle_unload_blocked":     e.IdleUnloadBlocked,
			"install_source":          e.installSourceMap(),
			"updates_enabled":         updatesEnabled(e.InstallMethod),
			"update_disabled_reason":  "",
			"logo_path":               e.LogoPath,
			"support_platforms":       e.SupportPlatforms,
			"astrbot_version":         e.AstrbotVersion,
			"i18n":                    e.I18n,
			"pages":                   e.Pages,
			"root_dir_name":           e.ID,
			"star_handler_full_names": []string{},
		})
	}
	return result
}

// pluginDisplayName resolves the display name shown in the WebUI: manifest
// record > runtime instance > the standalone metadata file (legacy entries) >
// plugin name.
func (m *SubprocessManager) pluginDisplayName(inst *PluginInstance, e *ManifestEntry) string {
	if e != nil && e.DisplayName != "" {
		return e.DisplayName
	}
	if inst != nil && inst.DisplayName != "" {
		return inst.DisplayName
	}
	name := ""
	id := ""
	if inst != nil {
		name = inst.Name
		id = inst.ID
	} else if e != nil {
		name = e.Name
		id = e.ID
	}
	if name != "" && id != "" {
		if meta := m.readPluginMetadataFile(id); meta != nil {
			if v, _ := meta["display_name"].(string); v != "" {
				return v
			}
		}
	}
	return name
}

// pluginShortDesc resolves the one-line short description (manifest > instance
// > standalone metadata file > "").
func (m *SubprocessManager) pluginShortDesc(inst *PluginInstance, e *ManifestEntry) string {
	if e != nil && e.ShortDesc != "" {
		return e.ShortDesc
	}
	if inst != nil && inst.ShortDesc != "" {
		return inst.ShortDesc
	}
	name := ""
	id := ""
	if inst != nil {
		name = inst.Name
		id = inst.ID
	} else if e != nil {
		name = e.Name
		id = e.ID
	}
	if name != "" && id != "" {
		if meta := m.readPluginMetadataFile(id); meta != nil {
			if v, _ := meta["short_desc"].(string); v != "" {
				return v
			}
		}
	}
	return ""
}

// pluginLogoURL returns the dashboard-relative URL of the plugin's cached
// logo ("" when absent), consumed by the WebUI as <img src>.
func (m *SubprocessManager) pluginLogoURL(id string) string {
	if m.PluginLogoFile(id) == "" {
		return ""
	}
	return "/api/v1/plugins/logo?plugin_id=" + url.QueryEscape(id)
}

// installSourceMap serializes the manifest entry into the dashboard's
// install_source contract consumed by the WebUI (install method, registry,
// market plugin id, repo, download URL).
func (e *ManifestEntry) installSourceMap() map[string]interface{} {
	if e == nil {
		return nil
	}
	method := e.InstallMethod
	if method == "" {
		// Legacy entries installed before install-source tracking: a git/archive
		// source is still reinstallable, so present it as a repository source.
		if isRepoURL(e.Source) {
			method = "repository"
		} else {
			method = "url"
		}
	}
	return map[string]interface{}{
		"schema_version":   1,
		"root_dir_name":    e.ID,
		"install_method":   method,
		"registry_url":     e.RegistryURL,
		"registry_name":    e.RegistryName,
		"market_plugin_id": e.MarketPluginID,
		"repo":             e.Repo,
		"download_url":     e.DownloadURL,
		"implicit":         false,
	}
}

// isRepoURL reports whether a source looks like a git clone / archive URL that
// can be re-fetched on update.
func isRepoURL(source string) bool {
	s := strings.ToLower(strings.TrimSpace(source))
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "git@") || strings.HasPrefix(s, "ssh://")
}

// updatesEnabled reports whether the plugin's install method allows updates
// (market and repository installs can be refreshed).
func updatesEnabled(method string) bool {
	m := strings.ToLower(method)
	if m == "" {
		return true // legacy: reinstallable from its stored source
	}
	return m == "market" || m == "repository"
}

// ListFailedPlugins wraps Failed() into the dashboard's /plugins/failed
// contract (name -> {name, path, reason, enabled}).
func (m *SubprocessManager) ListFailedPlugins() map[string]interface{} {
	result := map[string]interface{}{}
	for id, err := range m.Failed() {
		name, binary := id, ""
		if man, merr := LoadManifest(m.manifestPath()); merr == nil {
			if e := man.Get(id); e != nil {
				name, binary = e.Name, e.Binary
			}
		}
		result[id] = map[string]interface{}{
			"name":    name,
			"path":    binary,
			"reason":  err.Error(),
			"enabled": false,
		}
	}
	return result
}

// SetEnabled enables/disables an installed plugin: enable loads the cached
// binary from the manifest (idempotent when already running), disable unloads
// it; the manifest Enabled flag is persisted either way. manifestMu is released
// around Load/Unload because both trigger lifecycle-hook RPCs (on_plugin_loaded
// / on_plugin_unloaded) that must not run while holding the manifest write lock
// (a hung plugin would otherwise freeze every manifest operation).
func (m *SubprocessManager) SetEnabled(id string, enabled bool) error {
	// 串行化 manifest 读→改→写，防止并发 SetEnabled/BindSource 丢条目。
	m.manifestMu.Lock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		m.manifestMu.Unlock()
		return err
	}
	entry := man.Get(id)
	if entry == nil {
		m.manifestMu.Unlock()
		return fmt.Errorf("plugin %s not in install manifest", id)
	}
	if enabled {
		if m.Get(id) == nil {
			// 用带超时的 context 调用 Load：插件二进制握手/注册卡死时避免
			// WebUI 启用请求被阻塞 15-30s。
			loadCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			m.manifestMu.Unlock() // Load 会触发 on_plugin_loaded RPC，不能持锁
			_, loadErr := m.LoadLang(loadCtx, id, entry.Binary, entry.Language)
			cancel()
			m.manifestMu.Lock()
			if loadErr != nil {
				m.manifestMu.Unlock()
				return loadErr
			}
			// 重新读取，避免持锁期间磁盘 manifest 已被并发更新而旧快照被覆盖。
			if man, err = LoadManifest(m.manifestPath()); err != nil {
				m.manifestMu.Unlock()
				return err
			}
			entry = man.Get(id)
			if entry == nil {
				m.manifestMu.Unlock()
				return fmt.Errorf("plugin %s not in install manifest", id)
			}
		}
		entry.Enabled = true
	} else {
		if m.Get(id) != nil {
			m.manifestMu.Unlock() // Unload 会触发 on_plugin_unloaded RPC，不能持锁
			unloadErr := m.Unload(id)
			m.manifestMu.Lock()
			if unloadErr != nil {
				m.manifestMu.Unlock()
				return unloadErr
			}
			if man, err = LoadManifest(m.manifestPath()); err != nil {
				m.manifestMu.Unlock()
				return err
			}
			entry = man.Get(id)
			if entry == nil {
				m.manifestMu.Unlock()
				return fmt.Errorf("plugin %s not in install manifest", id)
			}
		}
		entry.Enabled = false
	}
	err = man.Save(m.manifestPath())
	m.manifestMu.Unlock()
	return err
}

// BindSource updates the persisted install source of an installed plugin so
// future update/reinstall requests resolve from the new registry/repository.
// It mirrors the dashboard's install_source record (market or repository).
func (m *SubprocessManager) BindSource(id string, method, registryURL, registryName, marketPluginID, repo, downloadURL string) error {
	// 串行化 manifest 读→改→写，防止并发修改互相覆盖。
	m.manifestMu.Lock()
	defer m.manifestMu.Unlock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		return err
	}
	entry := man.Get(id)
	if entry == nil {
		return fmt.Errorf("plugin %s not in install manifest", id)
	}
	if method != "" {
		entry.InstallMethod = method
	}
	if registryURL != "" {
		entry.RegistryURL = registryURL
	}
	if registryName != "" {
		entry.RegistryName = registryName
	}
	if marketPluginID != "" {
		entry.MarketPluginID = marketPluginID
	}
	if repo != "" {
		entry.Repo = repo
	}
	if downloadURL != "" {
		entry.DownloadURL = downloadURL
	}
	return man.Save(m.manifestPath())
}

// ReinstallSource reinstalls a plugin from its persisted source: it unloads
// the running instance (if any), re-fetches, re-scans, re-builds and reloads
// the plugin, updating the manifest. Source metadata is carried over.
func (m *SubprocessManager) ReinstallSource(ctx context.Context, id string, opts InstallOptions) (*PluginInstance, error) {
	// 串行化 manifest 读取，防止与并发写互相覆盖；快照出所需字段后立即释放
	// 锁（InstallFromSource 内部 recordInstall 会再次持 manifestMu，避免死锁）。
	m.manifestMu.Lock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		m.manifestMu.Unlock()
		return nil, err
	}
	entry := man.Get(id)
	if entry == nil {
		m.manifestMu.Unlock()
		return nil, fmt.Errorf("plugin %s not in install manifest", id)
	}
	// 更新时外部（dashboard，对齐 Python resolve_market_update_info）可能已从
	// 插件原 registry 重新解析出该插件的最新下载源：显式传入的
	// download_url/repo 优先于 manifest 快照，避免更新拉取固定旧版本 zip
	//（manifest 的 download_url 指向具体版本，如 …/2.6.0/xxx-2.6.0-<sha>.zip，
	// 直接用会永远装回旧版本）。
	dlURL := entry.DownloadURL
	if v := strings.TrimSpace(opts.DownloadURL); v != "" {
		dlURL = v
	}
	repo := entry.Repo
	if v := strings.TrimSpace(opts.Repo); v != "" {
		repo = v
	}
	source := dlURL
	if source == "" {
		source = entry.Source
	}
	if source == "" {
		source = repo
	}
	if source == "" {
		m.manifestMu.Unlock()
		return nil, fmt.Errorf("plugin %s has no install source", id)
	}
	carry := InstallOptions{
		IgnoreRisk:     opts.IgnoreRisk,
		CCChoice:       opts.CCChoice,
		GoChoice:       opts.GoChoice,
		PythonChoice:   opts.PythonChoice,
		GoMirror:       opts.GoMirror,
		PythonMirror:   opts.PythonMirror,
		Progress:       opts.Progress,
		InstallMethod:  entry.InstallMethod,
		RegistryURL:    entry.RegistryURL,
		RegistryName:   entry.RegistryName,
		MarketPluginID: entry.MarketPluginID,
		Repo:           repo,
		DownloadURL:    dlURL,
	}
	m.manifestMu.Unlock()

	if m.Get(id) != nil {
		if err := m.Unload(id); err != nil {
			return nil, fmt.Errorf("unload plugin %s: %w", id, err)
		}
	}

	return m.InstallFromSource(ctx, id, source, carry)
}

// ManifestEntry returns the persisted install-manifest entry for id (nil when
// absent), consumed by the dashboard to re-resolve the plugin's original
// registry when refreshing update sources (对齐 Python resolve_market_update_info).
func (m *SubprocessManager) ManifestEntry(id string) *ManifestEntry {
	man := m.cachedManifest()
	if man == nil {
		return nil
	}
	return man.Get(id)
}

// Uninstall removes an installed plugin: unloads it, drops the manifest entry,
// deletes its compiled binary directory and optionally its config directory.
// Uninstall removes a plugin. deleteConfig 是否删除插件配置
// (data/plugins_config/<id>)，deleteData 是否删除插件运行时数据
// (data/plugins_data/<id>)。二进制与文档缓存始终清理。
func (m *SubprocessManager) Uninstall(id string, deleteConfig, deleteData bool) error {
	// 持 per-plugin 生命周期锁：与 Load/InstallFromSource 尾段互斥，防止并发
	// 安装/卸载交错导致"卸载后条目被重建"或"删除已重新安装插件的二进制"。
	unlock := m.lockOp(id)
	defer unlock()

	var entry *ManifestEntry
	// 串行化 manifest 读→改→写：先读取条目，Unload 之后在锁内 Remove+Save。
	m.manifestMu.Lock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		m.manifestMu.Unlock()
		return err
	}
	if e := man.Get(id); e != nil {
		entry = e
	}
	if m.Get(id) != nil {
		m.manifestMu.Unlock()
		if err := m.unloadLocked(id, true); err != nil {
			return err
		}
		m.manifestMu.Lock()
		// 卸载窗口内插件可能被并发安装重新加载：复查实例表，命中则中止卸载，
		// 避免 Remove+Save 清掉新安装的条目。
		if m.Get(id) != nil {
			m.manifestMu.Unlock()
			return fmt.Errorf("plugin %s 在卸载期间被重新加载，已中止卸载", id)
		}
	}
	man.Remove(id)
	if err := man.Save(m.manifestPath()); err != nil {
		m.manifestMu.Unlock()
		return err
	}
	m.manifestMu.Unlock()

	// 二进制目录（始终删除）。
	_ = os.RemoveAll(filepath.Join(m.dataDir, "plugins-bin", sanitizeID(id)))
	// 插件本体源码目录（Go/Python 统一在 plugins/<id>，始终删除）。
	_ = os.RemoveAll(filepath.Join(m.dataDir, "plugins", sanitizeID(id)))

	// 目录足迹：优先用安装时记录的 manifest 条目，旧版本安装则按 id 推导。
	// manifest 子路径都先 sanitize/校验，防止路径穿越导致误删 dataDir 之外
	// 目录。
	cfgDir := filepath.Join(m.dataDir, "plugins_config", sanitizeID(id))
	docsDir := filepath.Join(m.dataDir, "plugins", sanitizeID(id))
	dataRoot := filepath.Join(m.dataDir, "plugins_data", sanitizeID(id))
	if entry != nil {
		if p, err := m.safeDataDirPath(entry.ConfigDir); err == nil {
			cfgDir = p
		}
		if p, err := m.safeDataDirPath(entry.DocsDir); err == nil {
			docsDir = p
		}
		if p, err := m.safeDataDirPath(entry.DataDir); err == nil {
			dataRoot = p
		}
	}

	if deleteData {
		_ = os.RemoveAll(dataRoot)
		// 含 data/ 与文档缓存。
		_ = os.RemoveAll(docsDir)
	} else {
		// 只清文档缓存文件，保留运行时数据目录(plugins_data/ 与 data/)。
		for _, f := range []string{"README.md", "readme.md", "CHANGELOG.md", "changelog.md", "config_schema.json"} {
			_ = os.Remove(filepath.Join(docsDir, f))
		}
	}
	if deleteConfig {
		_ = os.RemoveAll(cfgDir)
	}

	logger.I18nInfo("插件 %s 已卸载 (deleteConfig=%v deleteData=%v)", id, deleteConfig, deleteData)
	return nil
}

// safeDataDirPath joins a manifest-recorded relative subpath under dataDir,
// rejecting any path that escapes dataDir or resolves back to the dataDir
// root itself (protects Uninstall's os.RemoveAll from traversal in a tampered
// manifest, and from wiping the whole dataDir for legacy entries with empty
// footprint fields).
func (m *SubprocessManager) safeDataDirPath(sub string) (string, error) {
	if sub == "" {
		return "", fmt.Errorf("empty data subpath")
	}
	abs := filepath.Join(m.dataDir, filepath.Clean(filepath.FromSlash(sub)))
	if abs == m.dataDir {
		return "", fmt.Errorf("unsafe data subpath: %s", sub)
	}
	rel, err := filepath.Rel(m.dataDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe data subpath: %s", sub)
	}
	return abs, nil
}

// instanceByName returns the running instance for a plugin identifier (id or
// name), or nil. 同名 Go/Python 多变体并存时（实例按 id = name_language 分键）
// 取 id 字典序最小者，保证解析确定性（map 遍历顺序随机会导致 GetConfig/
// SetConfig/会话等待喂入漂移到任意一个变体）。
func (m *SubprocessManager) instanceByName(name string) *PluginInstance {
	if inst := m.Get(name); inst != nil {
		return inst
	}
	var hit *PluginInstance
	for _, inst := range m.List() {
		if inst.Name == name && (hit == nil || inst.ID < hit.ID) {
			hit = inst
		}
	}
	return hit
}

// ConfigSchema returns the plugin's config schema exported via Register().
// id 为插件实例 id（name_language）：同名 Go/Python 插件的 schema 完全隔离。
func (m *SubprocessManager) ConfigSchema(id string) map[string]interface{} {
	inst := m.instanceByName(id)
	// 按需加载：插件运行中时优先实时调插件 GetConfigSchema RPC——插件可在
	// 运行期更新 config.schema（如 update_manager 动态填充插件列表的
	// options/labels），这些值不在 Register 静态快照里，必须实时取。
	// RPC 失败/空响应（插件未实现或实例不可用）回退 Register 快照/磁盘缓存。
	if inst != nil && inst.Client != nil {
		rpcCtx, cancel := context.WithTimeout(context.Background(), pluginHookRPCTimeout)
		data, err := inst.Client.GetConfigSchema(rpcCtx)
		cancel()
		if err == nil && len(data) > 0 {
			var schema map[string]interface{}
			if err := json.Unmarshal(data, &schema); err == nil {
				return schema
			}
		}
	}
	if inst != nil && inst.Meta != nil && len(inst.Meta.ConfigSchemaJson) > 0 {
		var schema map[string]interface{}
		if err := json.Unmarshal(inst.Meta.ConfigSchemaJson, &schema); err != nil {
			logger.I18nWarn("ConfigSchema(%s): %v", id, err)
		} else {
			return schema
		}
	}
	// 插件已禁用/未加载时回退到落盘的 schema 缓存，保证配置对话框仍可渲染。
	if data, err := os.ReadFile(m.schemaCachePath(id)); err == nil {
		var schema map[string]interface{}
		if json.Unmarshal(data, &schema) == nil {
			return schema
		}
	}
	return map[string]interface{}{}
}

// schemaCachePath returns the persisted config-schema cache for a plugin
// (under plugins_config/<id>/ alongside config.json).
func (m *SubprocessManager) schemaCachePath(id string) string {
	return filepath.Join(m.dataDir, "plugins_config", sanitizeID(id), "config_schema.json")
}

// cacheConfigSchema persists a loaded plugin's config schema so the WebUI can
// render its config dialog even while the plugin is disabled (unloaded).
func (m *SubprocessManager) cacheConfigSchema(id string, meta *sdkv1.RegisterResponse) {
	if meta == nil || len(meta.ConfigSchemaJson) == 0 {
		return
	}
	path := m.schemaCachePath(id)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)           // #nosec G301 -- 配置 schema 缓存目录（WebUI 需读取）
	_ = os.WriteFile(path, meta.ConfigSchemaJson, 0o644) // #nosec G306 -- schema 缓存非常规敏感信息
}

// Components returns the plugin's behavior components (commands / llm tools /
// hooks) consumed by the WebUI "行为" detail page. Empty map when the plugin
// is not loaded. id 为插件实例 id（name_language）。
func (m *SubprocessManager) Components(id string) map[string]interface{} {
	inst := m.instanceByName(id)
	if inst == nil || inst.Meta == nil {
		return nil
	}
	out := map[string]interface{}{}
	cmds := make([]interface{}, 0, len(inst.Meta.Commands))
	for _, c := range inst.Meta.Commands {
		cmds = append(cmds, map[string]interface{}{
			"cmd": c.Name, "name": c.Name, "handler_name": c.Name,
			"desc": c.Description, "aliases": c.Aliases,
			"permission": c.Permission, "type": "指令",
		})
	}
	tools := make([]interface{}, 0, len(inst.Meta.Tools))
	for _, t := range inst.Meta.Tools {
		tools = append(tools, map[string]interface{}{
			"name": t.Name, "handler_name": t.Name, "desc": t.Description, "type": "函数工具",
		})
	}
	hooks := make([]interface{}, 0, len(inst.Meta.Hooks))
	for _, h := range inst.Meta.Hooks {
		hooks = append(hooks, map[string]interface{}{
			"name": h.Name, "handler_name": h.Name,
			"event_type": h.Event, "event_type_h": h.Event, "type": "事件监听器",
		})
	}
	if len(cmds) > 0 {
		out["command"] = cmds
	}
	if len(tools) > 0 {
		out["llm_tool"] = tools
	}
	if len(hooks) > 0 {
		out["hook"] = hooks
	}
	// 休眠策略：与指令/函数工具同列的行为配置项。global_* 反映全局闲置
	// 自动休眠开关；blocked 表示本插件是否被排除在休眠之外（常驻）。
	out["sleep"] = []interface{}{map[string]interface{}{
		"name":           "idle_sleep",
		"handler_name":   "idle_sleep",
		"desc":           "插件闲置自动休眠（回收空闲插件进程内存，触发时自动唤醒）",
		"type":           "休眠策略",
		"global_enabled": m.IdleUnloadEnabled(),
		"global_minutes": m.IdleUnloadMinutes(),
		"blocked":        m.IdleUnloadBlocked(inst.ID),
	}}
	if len(out) == 0 {
		return nil
	}
	return out
}

// configPath returns plugins_config/<id>/config.json. 插件配置项统一存放在
// data/plugins_config/ 下、按插件实例 id（name_language）分文件夹——同名
// Go/Python 插件的配置完全隔离；与源码/运行时数据(data/plugins/)分开，方便
// 用户直接编辑配置文件。config 文件只保存真实配置项，插件元数据
// （name/desc/version 等）存独立的 metadata.json（见 metadataPath）。
func (m *SubprocessManager) configPath(id string) string {
	return filepath.Join(m.dataDir, "plugins_config", sanitizeID(id), "config.json")
}

// metadataPath returns plugins_config/<id>/metadata.json —— 插件打包元数据
// 的独立存储文件，与 config.json 分离（不进入 WebUI 配置对话框）。
func (m *SubprocessManager) metadataPath(id string) string {
	return filepath.Join(m.dataDir, "plugins_config", sanitizeID(id), "metadata.json")
}

// metadataConfigKeys 是历史上被 writeMetadataConfig 混入 config.json 的插件
// 身份键。它们属于元数据而非可配置项，读取配置时必须剥离（并做一次性迁移）。
var metadataConfigKeys = []string{
	"name", "desc", "author", "version", "repo", "homepage",
	"display_name", "short_desc", "cgo",
}

// writeMetadataConfig writes the plugin's metadata.json content to the
// standalone plugins_config/<id>/metadata.json file. 元数据与配置彻底分离：
// config.json 只保存用户可编辑的配置项，插件信息（name/desc/author/version/
// repo/cgo）全部落在独立的 metadata 文件，供详情展示，不再污染配置对话框。
func (m *SubprocessManager) writeMetadataConfig(id string, meta *PluginMetadata) {
	if meta == nil {
		return
	}
	path := m.metadataPath(id)
	_ = os.MkdirAll(filepath.Dir(path), 0o755) // #nosec G301 -- 插件元数据目录（用户态）

	od := config.NewOrderedJSON()
	cgo := "no"
	if meta.RequiresCgo() {
		cgo = "yes"
	}
	od.Set("name", meta.Name)
	od.Set("desc", meta.Description)
	od.Set("author", meta.Author)
	od.Set("version", meta.Version)
	od.Set("repo", meta.Repo)
	if meta.Homepage != "" {
		od.Set("homepage", meta.Homepage)
	}
	if meta.DisplayName != "" {
		od.Set("display_name", meta.DisplayName)
	}
	if meta.ShortDesc != "" {
		od.Set("short_desc", meta.ShortDesc)
	}
	od.Set("cgo", cgo)

	out, err := json.MarshalIndent(od, "", "  ")
	if err != nil {
		logger.I18nWarn("writeMetadataConfig(%s): %v", id, err)
		return
	}
	// #nosec G306 -- 插件元数据非常规敏感信息
	if err := os.WriteFile(path, out, 0o644); err != nil {
		logger.I18nWarn("writeMetadataConfig(%s): %v", id, err)
	}
}

// readPluginMetadataFile reads the standalone metadata file
// (plugins_config/<id>/metadata.json), returning nil when absent.
func (m *SubprocessManager) readPluginMetadataFile(id string) map[string]interface{} {
	data, err := os.ReadFile(m.metadataPath(id))
	if err != nil {
		return nil
	}
	var meta map[string]interface{}
	if json.Unmarshal(data, &meta) != nil {
		return nil
	}
	return meta
}

// stripMetadataKeys removes packaged-metadata keys from a loaded config map.
// 旧版本安装会把元数据键混入 config.json；发现残留时一次性迁移：元数据写入
// 独立 metadata 文件、config.json 重写为纯配置，让磁盘立即收敛。
func (m *SubprocessManager) stripMetadataKeys(id string, cfg map[string]interface{}) map[string]interface{} {
	if cfg == nil {
		return cfg
	}
	removed := map[string]interface{}{}
	for _, k := range metadataConfigKeys {
		if v, ok := cfg[k]; ok {
			removed[k] = v
			delete(cfg, k)
		}
	}
	if len(removed) == 0 {
		return cfg
	}
	// 元数据迁移到独立文件（与既有内容合并，键值以本次为准）。
	m.mergeMetadataFile(id, removed)
	// 重写 config.json（不含元数据键）。
	if data, err := json.MarshalIndent(cfg, "", "  "); err == nil {
		_ = os.WriteFile(m.configPath(id), data, 0o600) // #nosec G306 -- 插件配置（用户态）非常规敏感信息
	}
	return cfg
}

// mergeMetadataFile merges key-values into the standalone metadata file.
func (m *SubprocessManager) mergeMetadataFile(id string, kv map[string]interface{}) {
	if len(kv) == 0 {
		return
	}
	path := m.metadataPath(id)
	od := config.NewOrderedJSON()
	// #nosec G304 -- 读取插件元数据（安装时写入的固定路径）
	if data, err := os.ReadFile(path); err == nil {
		if existing, err := config.ParseOrderedJSON(data); err == nil {
			od = existing
		}
	}
	for k, v := range kv {
		od.Set(k, v)
	}
	out, err := json.MarshalIndent(od, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755) // #nosec G301 -- 插件元数据目录（用户态）
	_ = os.WriteFile(path, out, 0o644)         // #nosec G306 -- 插件元数据非常规敏感信息
}

// LoadConfig reads the plugin config from plugins_config/<id>/config.json.
// 打包元数据键（name/desc/version 等）会被剥离——config 文件只承载真实配置项。
func (m *SubprocessManager) LoadConfig(id string) map[string]interface{} {
	data, err := os.ReadFile(m.configPath(id))
	if err != nil {
		return map[string]interface{}{}
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return map[string]interface{}{}
	}
	return m.stripMetadataKeys(id, cfg)
}

// FlatSchema returns the plugin's config schema as a flat {key: {type,...}}
// map (the "items" the WebUI renders). It normalizes the
// {"type":"object","properties":{...}} form (used by the Go SDK's
// ConfigSchema) to the properties map, and recursively converts nested
// "properties" into "items" (the shape the WebUI AstrBotConfig renders for
// object groups), so both flat and grouped layouts work.
func (m *SubprocessManager) FlatSchema(id string) map[string]interface{} {
	schema := m.ConfigSchema(id)
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		schema = props
	}
	return normalizeSchema(schema)
}

// FlatSchemaByID returns the FlatSchema for a specific plugin instance,
// reading the Register metadata of that exact instance (by id). Same-name
// plugins must not shadow each other: e.g. a freshly installed Python plugin
// with a versioned id (astrbot-plugin-xxx-4.11.2-<commit>) sharing the same
// name as an older Go plugin — the WebUI config dialog for the Python one
// would otherwise show the Go plugin's schema (fewer/no hints).
//
// It first tries to pull the plugin's CURRENT config schema via the
// GetConfigSchema RPC (plugins like update_manager refresh options/labels
// at runtime). Falls back to the Register snapshot when the RPC is
// unimplemented, times out, or returns empty.
func (m *SubprocessManager) FlatSchemaByID(id string) map[string]interface{} {
	inst := m.Get(id)
	if inst == nil {
		return map[string]interface{}{}
	}

	// Try live schema from the plugin via GetConfigSchema RPC.
	// Use a short timeout so a hung plugin does not block the UI.
	if inst.Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		raw, err := inst.Client.GetConfigSchema(ctx)
		cancel()
		if err == nil && len(raw) > 0 {
			var schema map[string]interface{}
			if json.Unmarshal(raw, &schema) == nil && schema != nil {
				if props, ok := schema["properties"].(map[string]interface{}); ok {
					schema = props
				}
				return normalizeSchema(schema)
			}
		}
	}

	// Fallback to the Register snapshot.
	if inst.Meta == nil || len(inst.Meta.ConfigSchemaJson) == 0 {
		return map[string]interface{}{}
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(inst.Meta.ConfigSchemaJson, &schema); err != nil {
		return map[string]interface{}{}
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		schema = props
	}
	return normalizeSchema(schema)
}

// normalizeSchema converts every nested {"properties":{...}} into
// {"items":{...}} recursively, matching the WebUI config component's
// expectations. Existing "items" (array element schemas) are preserved. It also
// maps JSON-Schema style types to the AstrBot/WebUI config types
// (boolean→bool, integer→int, number→float, array→list) so inputs render.
func normalizeSchema(schema map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(schema))
	for key, metaAny := range schema {
		meta, ok := metaAny.(map[string]interface{})
		if !ok {
			out[key] = metaAny
			continue
		}
		nm := make(map[string]interface{}, len(meta))
		for k, v := range meta {
			nm[k] = v
		}
		if t, ok := nm["type"].(string); ok {
			if mapped, ok := jsonSchemaToAstrBotType[t]; ok {
				nm["type"] = mapped
			}
		}
		if props, ok := nm["properties"].(map[string]interface{}); ok {
			if _, hasItems := nm["items"]; !hasItems {
				nm["items"] = normalizeSchema(props)
			}
		}
		out[key] = nm
	}
	return out
}

// jsonSchemaToAstrBotType maps JSON-Schema config types to the types the WebUI
// ConfigItemRenderer understands.
var jsonSchemaToAstrBotType = map[string]string{
	"boolean": "bool",
	"integer": "int",
	"number":  "float",
	"array":   "list",
}

// SaveConfig persists the plugin config to plugins_config/<id>/config.json.
// 防御性剥离元数据键：config 文件只允许真实配置项（防止旧路径把元数据写回）。
func (m *SubprocessManager) SaveConfig(id string, cfg map[string]interface{}) error {
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	cfg = m.stripMetadataKeys(id, cfg)
	path := m.configPath(id)
	_ = os.MkdirAll(filepath.Dir(path), 0755) // #nosec G301 -- 插件配置目录（用户态）
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600) // #nosec G306 -- 插件配置（用户态）非常规敏感信息
}

// pluginDataRoot returns the unified per-plugin data root directory
// (data/plugins_data/<id>). Plugins are launched with this as their working
// directory, so their relative-path data files land here.
func (m *SubprocessManager) pluginDataRoot(id string) string {
	return filepath.Join(m.dataDir, "plugins_data", sanitizeID(id))
}

// migratePluginData moves a plugin's data directory from oldID to newID when
// the latter does not exist yet. Used at install time: the source-derived id
// (astrbot-plugin-xxx-4.11.2-<commit>) is replaced by the stable id
// (<name>_<language>); migrating the data directory preserves the plugin's
// runtime data across reinstalls (uninstall without clearing data).
func (m *SubprocessManager) migratePluginData(oldID, newID string) {
	if oldID == "" || oldID == newID {
		return
	}
	oldDir := m.pluginDataRoot(oldID)
	newDir := m.pluginDataRoot(newID)
	if info, err := os.Stat(oldDir); err != nil || !info.IsDir() {
		return
	}
	if _, err := os.Stat(newDir); err == nil {
		return // 新目录已存在（可能是残留），不覆盖
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		logger.I18nWarn("插件数据目录迁移失败 %s → %s: %v", oldDir, newDir, err)
		return
	}
	logger.I18nInfo("插件数据目录已迁移 %s → %s", oldDir, newDir)
}

// PluginDataDir returns the per-plugin data directory (data/plugins_data/<id>),
// creating it if needed.
func (m *SubprocessManager) PluginDataDir(id string) string {
	dir := m.pluginDataRoot(id)
	_ = os.MkdirAll(dir, 0o755) // #nosec G301 -- 插件数据目录（用户态）
	return dir
}

// docsPath returns the per-plugin docs directory (plugins/<id>) where
// README.md/CHANGELOG.md are cached at install time（与源码本体同目录）。
func (m *SubprocessManager) docsPath(id string) string {
	return filepath.Join(m.dataDir, "plugins", sanitizeID(id))
}

// Readme returns the plugin's README content, reading from the locally cached
// copy captured at install time. When the cache is missing (plugin installed
// before caching was added) it falls back to fetching from the plugin's repo
// URL. Returns an empty string when no README is available.
func (m *SubprocessManager) Readme(id string) string {
	for _, file := range []string{"README.md", "readme.md"} {
		if content := m.readCachedDoc(id, file); content != "" {
			return content
		}
	}
	return m.fetchRepoDoc(id, []string{"README.md", "readme.md"})
}

// Changelog returns the plugin's CHANGELOG content with the same cache-first,
// repo-fallback semantics as Readme.
func (m *SubprocessManager) Changelog(id string) string {
	for _, file := range []string{"CHANGELOG.md", "changelog.md"} {
		if content := m.readCachedDoc(id, file); content != "" {
			return content
		}
	}
	return m.fetchRepoDoc(id, []string{"CHANGELOG.md", "changelog.md"})
}

// readCachedDoc reads a cached doc file from the plugin docs directory
// (plugins/<id>，与源码本体同目录). id 再次经 sanitizeID 归一化（拒绝 /、
// \、.、.. 等穿越字符），即使上游传入异常值也不会逃逸 data/plugins 目录。
func (m *SubprocessManager) readCachedDoc(id, file string) string {
	content, err := os.ReadFile(filepath.Join(m.docsPath(id), file)) // #nosec G304 -- id 经 sanitizeID 归一化防穿越
	if err != nil {
		return ""
	}
	return string(content)
}

// fetchRepoDoc fetches a doc file (README.md/CHANGELOG.md) from the plugin's
// GitHub repository when it was not cached locally. repoDocFile returns the
// first successful fetch, or "" when unavailable.
func (m *SubprocessManager) fetchRepoDoc(name string, candidates []string) string {
	repo := m.repoURLFor(name)
	if repo == "" {
		return ""
	}
	rawURLs := rawRepoDocURLs(repo, candidates)
	// GitHub 加速前缀（config github_proxy，与市场拉取/插件 git clone 一致）；
	// 未配置加速且网络不通时直连会拖住详情页 README 数十秒，因此：
	// ① 每 URL 只给 5s；② 失败结果负面缓存（docFetchCache）避免每次打开
	// 详情都重试。
	if proxy := m.githubProxy; proxy != "" {
		for i, u := range rawURLs {
			if strings.HasPrefix(u, "https://raw.githubusercontent.com/") {
				rawURLs[i] = strings.TrimRight(proxy, "/") + "/" + u
			}
		}
	}
	cacheKey := name + "|" + strings.Join(candidates, ",")
	m.docMu.Lock()
	if hit, ok := m.docFetchCache[cacheKey]; ok {
		// 5 分钟 TTL 内的结果（成功或负面）直接复用；过期删除（重新拉取）。
		if time.Since(hit.ts) < docFetchCacheTTL {
			m.docMu.Unlock()
			return hit.content
		}
		delete(m.docFetchCache, cacheKey)
	}
	m.docMu.Unlock()

	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: safeRedirect}
	const maxDocSize = 2 << 20
	for _, rawURL := range rawURLs {
		resp, err := client.Get(rawURL)
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			continue
		}
		content, err := io.ReadAll(io.LimitReader(resp.Body, maxDocSize+1))
		_ = resp.Body.Close()
		if err != nil || int64(len(content)) > maxDocSize || len(content) == 0 {
			continue
		}
		m.docMu.Lock()
		m.docFetchCache[cacheKey] = docCacheEntry{content: string(content), ts: time.Now()}
		m.docMu.Unlock()
		return string(content)
	}
	// 负面缓存：本次未取到（网络不通/无 README）也记录（TTL 内不再重试），
	// 前端立即得到"没有 README"而非长时间转圈。
	m.docMu.Lock()
	m.docFetchCache[cacheKey] = docCacheEntry{content: "", ts: time.Now()}
	m.docMu.Unlock()
	return ""
}

// repoURLFor returns the plugin's repository URL (manifest Repo, else Source).
func (m *SubprocessManager) repoURLFor(name string) string {
	man := m.cachedManifest()
	for i := range man.Plugins {
		e := &man.Plugins[i]
		if e.Name == name || e.ID == name {
			if e.Repo != "" {
				return e.Repo
			}
			return e.Source
		}
	}
	return ""
}

// rawRepoDocURLs builds raw.githubusercontent.com URLs for candidate doc files
// across the default branch heads (HEAD, main, master).
func rawRepoDocURLs(repo string, candidates []string) []string {
	host := ""
	path := ""
	if u, err := url.Parse(repo); err == nil {
		host = u.Host
		path = strings.Trim(u.Path, "/")
	} else {
		// git@host:owner/repo.git shorthand. 逐段安全解析：先定位 '@' 后第一个
		// ':' 作为 host 边界，再解析 owner/repo 路径，任何一段缺失都不越界。
		if at := strings.Index(repo, "@"); at >= 0 {
			hostPart := repo[at+1:]
			if colon := strings.Index(hostPart, ":"); colon >= 0 {
				host = hostPart[:colon]
				rest := strings.TrimPrefix(hostPart[colon+1:], ":")
				if i := strings.Index(rest, "/"); i > 0 {
					path = rest
				}
			}
		}
	}
	if host == "" || !strings.EqualFold(host, "github.com") || strings.Count(path, "/") < 1 {
		return nil
	}
	path = strings.TrimSuffix(path, ".git")
	var out []string
	for _, branch := range []string{"HEAD", "main", "master"} {
		for _, file := range candidates {
			out = append(out, "https://raw.githubusercontent.com/"+path+"/"+branch+"/"+file)
		}
	}
	return out
}
