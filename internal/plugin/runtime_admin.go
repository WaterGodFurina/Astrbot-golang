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
		info := map[string]interface{}{
			"name":                   inst.Name,
			"marketplace_name":       strings.ReplaceAll(inst.Name, "_", "-"),
			"version":                inst.Version,
			"description":            desc,
			"path":                   inst.Binary,
			"id":                     inst.ID,
			"loaded":                 true,
			"enabled":                true,
			"activated":              true,
			"reserved":               false,
			"author":                 "",
			"repo":                   "",
			"install_source":         nil,
			"updates_enabled":        true,
			"update_disabled_reason": "",
		}
		if e := entryByID[inst.ID]; e != nil {
			info["repo"] = e.Repo
			if info["repo"] == "" {
				info["repo"] = e.Source
			}
			info["install_source"] = e.installSourceMap()
			info["updates_enabled"] = updatesEnabled(e.InstallMethod)
			if !updatesEnabled(e.InstallMethod) {
				info["update_disabled_reason"] = "该插件缺少可用的安装源或仓库地址，无法更新或重新安装"
			}
		}
		result = append(result, info)
	}
	for _, e := range man.Plugins {
		if e.Enabled {
			continue
		}
		if m.Get(e.ID) != nil {
			continue
		}
		repo := e.Repo
		if repo == "" {
			repo = e.Source
		}
		result = append(result, map[string]interface{}{
			"name":                   e.Name,
			"marketplace_name":       strings.ReplaceAll(e.Name, "_", "-"),
			"version":                e.Version,
			"description":            "插件已禁用",
			"path":                   e.Binary,
			"id":                     e.ID,
			"loaded":                 false,
			"enabled":                false,
			"activated":              false,
			"reserved":               false,
			"author":                 "",
			"repo":                   repo,
			"install_source":         e.installSourceMap(),
			"updates_enabled":        updatesEnabled(e.InstallMethod),
			"update_disabled_reason": "",
		})
	}
	return result
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
			_, loadErr := m.Load(loadCtx, id, entry.Binary)
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
	source := entry.DownloadURL
	if source == "" {
		source = entry.Source
	}
	if source == "" {
		source = entry.Repo
	}
	if source == "" {
		m.manifestMu.Unlock()
		return nil, fmt.Errorf("plugin %s has no install source", id)
	}
	carry := InstallOptions{
		IgnoreRisk:     opts.IgnoreRisk,
		CCChoice:       opts.CCChoice,
		Progress:       opts.Progress,
		InstallMethod:  entry.InstallMethod,
		RegistryURL:    entry.RegistryURL,
		RegistryName:   entry.RegistryName,
		MarketPluginID: entry.MarketPluginID,
		Repo:           entry.Repo,
		DownloadURL:    entry.DownloadURL,
	}
	m.manifestMu.Unlock()

	if m.Get(id) != nil {
		if err := m.Unload(id); err != nil {
			return nil, fmt.Errorf("unload plugin %s: %w", id, err)
		}
	}

	return m.InstallFromSource(ctx, id, source, carry)
}

// Uninstall removes an installed plugin: unloads it, drops the manifest entry,
// deletes its compiled binary directory and optionally its config directory.
// Uninstall removes a plugin. deleteConfig 是否删除插件配置
// (data/plugins_config/<name>)，deleteData 是否删除插件运行时数据
// (data/plugins/<name>/data)。二进制与文档缓存始终清理。
func (m *SubprocessManager) Uninstall(id string, deleteConfig, deleteData bool) error {
	// 持 per-plugin 生命周期锁：与 Load/InstallFromSource 尾段互斥，防止并发
	// 安装/卸载交错导致"卸载后条目被重建"或"删除已重新安装插件的二进制"。
	unlock := m.lockOp(id)
	defer unlock()

	name := id
	var entry *ManifestEntry
	// 串行化 manifest 读→改→写：先读取条目，Unload 之后在锁内 Remove+Save。
	m.manifestMu.Lock()
	man, err := LoadManifest(m.manifestPath())
	if err != nil {
		m.manifestMu.Unlock()
		return err
	}
	if e := man.Get(id); e != nil {
		name = e.Name
		entry = e
	}
	if m.Get(id) != nil {
		m.manifestMu.Unlock()
		if err := m.unloadLocked(id); err != nil {
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

	// 目录足迹：优先用安装时记录的 manifest 条目，旧版本安装则按 name 推导。
	// name 与 manifest 子路径都先 sanitize/校验，防止路径穿越导致误删
	// dataDir 之外目录。
	cfgDir := filepath.Join(m.dataDir, "plugins_config", sanitizePluginName(name))
	docsDir := filepath.Join(m.dataDir, "plugins", sanitizePluginName(name))
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
// name), or nil.
func (m *SubprocessManager) instanceByName(name string) *PluginInstance {
	if inst := m.Get(name); inst != nil {
		return inst
	}
	for _, inst := range m.List() {
		if inst.Name == name {
			return inst
		}
	}
	return nil
}

// ConfigSchema returns the plugin's config schema exported via Register().
func (m *SubprocessManager) ConfigSchema(name string) map[string]interface{} {
	inst := m.instanceByName(name)
	if inst != nil && inst.Meta != nil && len(inst.Meta.ConfigSchemaJson) > 0 {
		var schema map[string]interface{}
		if err := json.Unmarshal(inst.Meta.ConfigSchemaJson, &schema); err != nil {
			logger.I18nWarn("ConfigSchema(%s): %v", name, err)
		} else {
			return schema
		}
	}
	// 插件已禁用/未加载时回退到落盘的 schema 缓存，保证配置对话框仍可渲染。
	if data, err := os.ReadFile(m.schemaCachePath(name)); err == nil {
		var schema map[string]interface{}
		if json.Unmarshal(data, &schema) == nil {
			return schema
		}
	}
	return map[string]interface{}{}
}

// schemaCachePath returns the persisted config-schema cache for a plugin
// (under plugins_config/<name>/ alongside config.json).
func (m *SubprocessManager) schemaCachePath(name string) string {
	return filepath.Join(m.dataDir, "plugins_config", sanitizePluginName(name), "config_schema.json")
}

// cacheConfigSchema persists a loaded plugin's config schema so the WebUI can
// render its config dialog even while the plugin is disabled (unloaded).
func (m *SubprocessManager) cacheConfigSchema(name string, meta *sdkv1.RegisterResponse) {
	if meta == nil || len(meta.ConfigSchemaJson) == 0 {
		return
	}
	path := m.schemaCachePath(name)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, meta.ConfigSchemaJson, 0o644)
}

// Components returns the plugin's behavior components (commands / llm tools /
// hooks) consumed by the WebUI "行为" detail page. Empty map when the plugin
// is not loaded.
func (m *SubprocessManager) Components(name string) map[string]interface{} {
	inst := m.instanceByName(name)
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
	if len(out) == 0 {
		return nil
	}
	return out
}

// configPath returns plugins_config/<name>/config.json. 插件配置项统一存放在
// data/plugins_config/ 下、按插件名分文件夹，与源码/运行时数据(data/plugins/)
// 分开，方便用户直接编辑配置文件。
func (m *SubprocessManager) configPath(name string) string {
	return filepath.Join(m.dataDir, "plugins_config", sanitizePluginName(name), "config.json")
}

// writeMetadataConfig writes the plugin's metadata.json content to the top of
// its plugins_config/<name>/config.json so the WebUI/config editor can display
// the packaged plugin info (name/desc/author/version/repo/cgo) alongside the
// runtime config. Existing config keys (if any) are preserved underneath.
func (m *SubprocessManager) writeMetadataConfig(name string, meta *PluginMetadata) {
	if meta == nil {
		return
	}
	path := m.configPath(name)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

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
	od.Set("cgo", cgo)

	// Preserve any pre-existing config (e.g. from a previous install) after the
	// metadata block.
	if data, err := os.ReadFile(path); err == nil {
		if existing, err := config.ParseOrderedJSON(data); err == nil {
			for _, k := range existing.Keys() {
				if od.Has(k) {
					continue
				}
				if v, ok := existing.Get(k); ok {
					od.Set(k, v)
				}
			}
		}
	}
	out, err := json.MarshalIndent(od, "", "  ")
	if err != nil {
		logger.I18nWarn("writeMetadataConfig(%s): %v", name, err)
		return
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		logger.I18nWarn("writeMetadataConfig(%s): %v", name, err)
	}
}

// LoadConfig reads the plugin config from plugins_config/<name>/config.json.
func (m *SubprocessManager) LoadConfig(name string) map[string]interface{} {
	data, err := os.ReadFile(m.configPath(name))
	if err != nil {
		return map[string]interface{}{}
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return map[string]interface{}{}
	}
	return cfg
}

// FlatSchema returns the plugin's config schema as a flat {key: {type,...}}
// map (the "items" the WebUI renders). It normalizes the
// {"type":"object","properties":{...}} form (used by the Go SDK's
// ConfigSchema) to the properties map, and recursively converts nested
// "properties" into "items" (the shape the WebUI AstrBotConfig renders for
// object groups), so both flat and grouped layouts work.
func (m *SubprocessManager) FlatSchema(name string) map[string]interface{} {
	schema := m.ConfigSchema(name)
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

// LoadConfigWithDefaults returns the plugin config merged with its schema
// defaults, so the WebUI config dialog shows every field (with defaults) even
// before the user saves anything.
func (m *SubprocessManager) LoadConfigWithDefaults(name string) map[string]interface{} {
	cfg := m.LoadConfig(name)
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	mergeSchemaDefaults(cfg, m.FlatSchema(name))
	return cfg
}

// mergeSchemaDefaults fills cfg with each schema key's "default" value (and
// recurses into object groups) when the key is absent.
func mergeSchemaDefaults(cfg, schema map[string]interface{}) {
	for key, metaAny := range schema {
		meta, ok := metaAny.(map[string]interface{})
		if !ok {
			continue
		}
		if def, ok := meta["default"]; ok {
			if _, exists := cfg[key]; !exists {
				cfg[key] = def
			}
			continue
		}
		if itemsAny, ok := meta["items"].(map[string]interface{}); ok {
			cur, _ := cfg[key].(map[string]interface{})
			if cur == nil {
				cur = map[string]interface{}{}
			}
			mergeSchemaDefaults(cur, itemsAny)
			if len(cur) > 0 {
				cfg[key] = cur
			}
		}
	}
}

// SaveConfig persists the plugin config to plugins/<name>/config.json.
func (m *SubprocessManager) SaveConfig(name string, cfg map[string]interface{}) error {
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	path := m.configPath(name)
	_ = os.MkdirAll(filepath.Dir(path), 0755)
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// pluginDataRoot returns the unified per-plugin data root directory
// (data/plugins_data/<id>). Plugins are launched with this as their working
// directory, so their relative-path data files land here.
func (m *SubprocessManager) pluginDataRoot(id string) string {
	return filepath.Join(m.dataDir, "plugins_data", sanitizeID(id))
}

// PluginDataDir returns the per-plugin data directory (data/plugins_data/<id>),
// creating it if needed.
func (m *SubprocessManager) PluginDataDir(id string) string {
	dir := m.pluginDataRoot(id)
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

// docsPath returns the per-plugin docs directory (plugins/<name>) where
// README.md/CHANGELOG.md are cached at install time.
func (m *SubprocessManager) docsPath(name string) string {
	return filepath.Join(m.dataDir, "plugins", sanitizePluginName(name))
}

// Readme returns the plugin's README content, reading from the locally cached
// copy captured at install time. When the cache is missing (plugin installed
// before caching was added) it falls back to fetching from the plugin's repo
// URL. Returns an empty string when no README is available.
func (m *SubprocessManager) Readme(id string) string {
	name := m.resolveName(id)
	if name == "" {
		return ""
	}
	for _, file := range []string{"README.md", "readme.md"} {
		if content := m.readCachedDoc(name, file); content != "" {
			return content
		}
	}
	return m.fetchRepoDoc(name, []string{"README.md", "readme.md"})
}

// Changelog returns the plugin's CHANGELOG content with the same cache-first,
// repo-fallback semantics as Readme.
func (m *SubprocessManager) Changelog(id string) string {
	name := m.resolveName(id)
	if name == "" {
		return ""
	}
	for _, file := range []string{"CHANGELOG.md", "changelog.md"} {
		if content := m.readCachedDoc(name, file); content != "" {
			return content
		}
	}
	return m.fetchRepoDoc(name, []string{"CHANGELOG.md", "changelog.md"})
}

// resolveName maps a plugin id/name to the canonical plugin name (used for the
// docs directory). 查不到实例/manifest 条目时返回空字符串，避免把调用方传入的
// 原始 id（可能含路径穿越字符）当作文档目录名使用。
func (m *SubprocessManager) resolveName(id string) string {
	if inst := m.instanceByName(id); inst != nil {
		return inst.Name
	}
	if man, err := LoadManifest(m.manifestPath()); err == nil {
		if e := man.Get(id); e != nil {
			if e.Name != "" {
				return e.Name
			}
			return e.ID
		}
	}
	return ""
}

// readCachedDoc reads a cached doc file from the plugin docs directory.
// name 再次经 sanitizePluginName 归一化（拒绝 /、\、.、.. 等穿越字符），
// 即使上游传入异常值也不会逃逸 data/plugins 目录。
func (m *SubprocessManager) readCachedDoc(name, file string) string {
	content, err := os.ReadFile(filepath.Join(m.docsPath(sanitizePluginName(name)), file))
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
	client := &http.Client{Timeout: 15 * time.Second}
	for _, rawURL := range rawURLs {
		resp, err := client.Get(rawURL)
		if err != nil {
			continue
		}
		content, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK && len(content) > 0 {
			return string(content)
		}
	}
	return ""
}

// repoURLFor returns the plugin's repository URL (manifest Repo, else Source).
func (m *SubprocessManager) repoURLFor(name string) string {
	if man, err := LoadManifest(m.manifestPath()); err == nil {
		for i := range man.Plugins {
			e := &man.Plugins[i]
			if e.Name == name || e.ID == name {
				if e.Repo != "" {
					return e.Repo
				}
				return e.Source
			}
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
