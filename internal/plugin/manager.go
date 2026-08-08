// Package plugin implements the Go .so plugin loading system.
//
// Unlike the Python AstrBot which loads plugins via importlib, the Go version
// uses Go's built-in `plugin` package to dynamically load compiled .so shared
// libraries. Each plugin is compiled with `go build -buildmode=plugin`.
//
// Plugin Interface Contract:
//
// Each .so plugin must export the following symbols:
//
//	func PluginName() string
//	func PluginVersion() string
//	func PluginDescription() string
//	func Init(ctx *plugin.Context) error
//	func RegisterHandlers(reg *plugin.HandlerRegistry)
//	func Cleanup() error
//
// Plugins interact with AstrBot through the plugin.Context API, which
// provides access to the event bus, config, database, provider manager,
// and other core services.
package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"strings"
	"sync"

	"github.com/AstrBotDevs/AstrBot/internal/log"
)

var logger = log.GetDefault().WithComponent("Plugin")

// Context provides plugin access to AstrBot core services.
type Context struct {
	DataDir   string
	ConfigDir string
	PluginDir string
	Logger    *log.ComponentLogger
	Cancel    context.CancelFunc
}

// GetConfig loads the calling plugin's config (plugins/<pluginDir>/<name>/config.json).
// The plugin name is derived from the directory the .so lives in, falling back
// to the file name without extension.
func (c *Context) GetConfig(pluginName string) map[string]interface{} {
	if pluginName == "" {
		return map[string]interface{}{}
	}
	base := c.PluginDir
	if base == "" {
		base = "data/plugins"
	}
	data, err := os.ReadFile(filepath.Join(base, pluginName, "config.json"))
	if err != nil {
		return map[string]interface{}{}
	}
	var cfg map[string]interface{}
	if json.Unmarshal(data, &cfg) != nil {
		return map[string]interface{}{}
	}
	return cfg
}

// HandlerRegistry collects command/event handlers from plugins.
type HandlerRegistry struct {
	mu       sync.RWMutex
	commands []CommandHandler
	filters  []FilterHandler
	hooks    []HookHandler
}

// CommandHandler defines a command handler registered by a plugin.
type CommandHandler struct {
	Name        string
	Aliases     []string
	Description string
	Usage       string
	Permission  string
	Handler     func(ctx context.Context, args []string) (string, error)
	// HandlerEx is the same as Handler but additionally receives the plugin
	// Context (config/data access). Preferred over Handler when set.
	HandlerEx func(pc *Context, ctx context.Context, args []string) (string, error)
}

// FilterHandler defines an event filter registered by a plugin.
type FilterHandler struct {
	Name   string
	Filter func(ctx context.Context, event interface{}) bool
}

// HookHandler defines a lifecycle hook registered by a plugin.
type HookHandler struct {
	Name    string
	Event   string
	Handler func(ctx context.Context) error
}

// NewHandlerRegistry creates an empty registry.
func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{}
}

// RegisterCommand adds a command handler.
func (r *HandlerRegistry) RegisterCommand(cmd CommandHandler) {
	r.mu.Lock()
	r.commands = append(r.commands, cmd)
	r.mu.Unlock()
}

// RegisterFilter adds an event filter.
func (r *HandlerRegistry) RegisterFilter(f FilterHandler) {
	r.mu.Lock()
	r.filters = append(r.filters, f)
	r.mu.Unlock()
}

// RegisterHook adds a lifecycle hook.
func (r *HandlerRegistry) RegisterHook(h HookHandler) {
	r.mu.Lock()
	r.hooks = append(r.hooks, h)
	r.mu.Unlock()
}

// Commands returns all registered command handlers.
func (r *HandlerRegistry) Commands() []CommandHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]CommandHandler, len(r.commands))
	copy(out, r.commands)
	return out
}

// Filters returns all registered filter handlers.
func (r *HandlerRegistry) Filters() []FilterHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]FilterHandler, len(r.filters))
	copy(out, r.filters)
	return out
}

// Hooks returns all registered hook handlers.
func (r *HandlerRegistry) Hooks() []HookHandler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]HookHandler, len(r.hooks))
	copy(out, r.hooks)
	return out
}

// LoadedPlugin represents a successfully loaded .so plugin.
type LoadedPlugin struct {
	Path        string
	Name        string
	Version     string
	Description string
	so          *plugin.Plugin
	registry    *HandlerRegistry
	cleanup     func() error
}

// Manager loads and manages .so plugins.
type Manager struct {
	mu            sync.RWMutex
	plugins       map[string]*LoadedPlugin
	failedPlugins []FailedPlugin
	ctx           *Context
}

// NewManager creates a plugin manager.
func NewManager(ctx *Context) *Manager {
	return &Manager{
		plugins: make(map[string]*LoadedPlugin),
		ctx:     ctx,
	}
}

// LoadPlugin loads a .so file from disk.
func (m *Manager) LoadPlugin(path string) (*LoadedPlugin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}

	if existing, ok := m.plugins[absPath]; ok {
		return existing, nil
	}

	if _, err := os.Stat(absPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("plugin file not found: %s", absPath)
	}

	so, err := plugin.Open(absPath)
	if err != nil {
		return nil, fmt.Errorf("plugin.Open(%s): %w", filepath.Base(absPath), err)
	}

	nameSym, err := so.Lookup("PluginName")
	if err != nil {
		return nil, fmt.Errorf("PluginName not found in %s: %w", absPath, err)
	}
	nameFn, ok := nameSym.(func() string)
	if !ok {
		return nil, fmt.Errorf("PluginName in %s has wrong signature", absPath)
	}

	versionSym, err := so.Lookup("PluginVersion")
	if err != nil {
		return nil, fmt.Errorf("PluginVersion not found in %s: %w", absPath, err)
	}
	versionFn, ok := versionSym.(func() string)
	if !ok {
		return nil, fmt.Errorf("PluginVersion in %s has wrong signature", absPath)
	}

	descSym, err := so.Lookup("PluginDescription")
	if err != nil {
		return nil, fmt.Errorf("PluginDescription not found in %s: %w", absPath, err)
	}
	descFn, ok := descSym.(func() string)
	if !ok {
		return nil, fmt.Errorf("PluginDescription in %s has wrong signature", absPath)
	}

	initSym, err := so.Lookup("Init")
	if err != nil {
		return nil, fmt.Errorf("Init not found in %s: %w", absPath, err)
	}
	initFn, ok := initSym.(func(*Context) error)
	if !ok {
		return nil, fmt.Errorf("Init in %s has wrong signature", absPath)
	}

	regSym, err := so.Lookup("RegisterHandlers")
	if err != nil {
		return nil, fmt.Errorf("RegisterHandlers not found in %s: %w", absPath, err)
	}
	regFn, ok := regSym.(func(*HandlerRegistry))
	if !ok {
		return nil, fmt.Errorf("RegisterHandlers in %s has wrong signature", absPath)
	}

	var cleanupFn func() error
	if cleanupSym, err := so.Lookup("Cleanup"); err == nil {
		if fn, ok := cleanupSym.(func() error); ok {
			cleanupFn = fn
		}
	}

	p := &LoadedPlugin{
		Path:        absPath,
		Name:        nameFn(),
		Version:     versionFn(),
		Description: descFn(),
		so:          so,
		registry:    NewHandlerRegistry(),
		cleanup:     cleanupFn,
	}

	if err := initFn(m.ctx); err != nil {
		return nil, fmt.Errorf("plugin %s Init() failed: %w", p.Name, err)
	}

	regFn(p.registry)

	m.plugins[absPath] = p
	logger.Info("Loaded plugin: %s v%s (%s) - %d commands, %d filters, %d hooks",
		p.Name, p.Version, p.Description,
		len(p.registry.commands), len(p.registry.filters), len(p.registry.hooks),
	)

	return p, nil
}

// LoadDir loads all .so files from a directory.
func (m *Manager) LoadDir(dir string) ([]*LoadedPlugin, []error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, []error{fmt.Errorf("read plugin dir %s: %w", dir, err)}
	}

	var loaded []*LoadedPlugin
	var errs []error
	m.failedPlugins = nil

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		// Disabled plugins (.so.disable) are skipped, not loaded.
		if strings.HasSuffix(entry.Name(), ".so.disable") {
			continue
		}
		if !strings.HasSuffix(entry.Name(), ".so") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		p, err := m.LoadPlugin(path)
		if err != nil {
			logger.Error("Failed to load %s: %v", entry.Name(), err)
			m.failedPlugins = append(m.failedPlugins, FailedPlugin{
				Name:    strings.TrimSuffix(entry.Name(), ".so"),
				Path:    path,
				Reason:  err.Error(),
				Enabled: false,
			})
			errs = append(errs, fmt.Errorf("%s: %w", entry.Name(), err))
			continue
		}
		loaded = append(loaded, p)
	}

	logger.Info("Loaded %d plugin(s) from %s (%d failed)", len(loaded), dir, len(errs))
	return loaded, errs
}

// Unload removes a plugin by path.
func (m *Manager) Unload(path string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	p, ok := m.plugins[absPath]
	if !ok {
		return fmt.Errorf("plugin not loaded: %s", absPath)
	}

	if p.cleanup != nil {
		if err := p.cleanup(); err != nil {
			logger.Warn("Plugin %s Cleanup() error: %v", p.Name, err)
		}
	}

	delete(m.plugins, absPath)
	logger.Info("Unloaded plugin: %s", p.Name)
	return nil
}

// UnloadAll unloads all plugins (called on shutdown).
func (m *Manager) UnloadAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for path, p := range m.plugins {
		if p.cleanup != nil {
			if err := p.cleanup(); err != nil {
				logger.Warn("Plugin %s Cleanup() error: %v", p.Name, err)
			}
		}
		delete(m.plugins, path)
	}
	logger.Info("All plugins unloaded")
}

// Get returns a loaded plugin by name.
func (m *Manager) Get(name string) *LoadedPlugin {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.plugins {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// List returns all loaded plugins.
func (m *Manager) List() []*LoadedPlugin {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*LoadedPlugin, 0, len(m.plugins))
	for _, p := range m.plugins {
		result = append(result, p)
	}
	return result
}

// ListInfo returns plugin info as serializable maps.
func (m *Manager) ListInfo() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(m.plugins))
	for _, p := range m.plugins {
		result = append(result, map[string]interface{}{
			"name":        p.Name,
			"version":     p.Version,
			"description": p.Description,
			"path":        p.Path,
			"loaded":      p.so != nil,
		})
	}
	return result
}

// AllCommands returns command handlers from all loaded plugins.
func (m *Manager) AllCommands() []CommandHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var cmds []CommandHandler
	for _, p := range m.plugins {
		cmds = append(cmds, p.registry.Commands()...)
	}
	return cmds
}

// AllHooks returns hook handlers from all loaded plugins.
func (m *Manager) AllHooks() []HookHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var hooks []HookHandler
	for _, p := range m.plugins {
		hooks = append(hooks, p.registry.Hooks()...)
	}
	return hooks
}

// AllFilters returns filter handlers from all loaded plugins.
func (m *Manager) AllFilters() []FilterHandler {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var filters []FilterHandler
	for _, p := range m.plugins {
		filters = append(filters, p.registry.Filters()...)
	}
	return filters
}

// TriggerHook runs all hooks matching the event name.
func (m *Manager) TriggerHook(ctx context.Context, event string) {
	hooks := m.AllHooks()
	for _, h := range hooks {
		if h.Event == event {
			if err := h.Handler(ctx); err != nil {
				logger.Error("Hook %s (%s) failed: %v", h.Name, h.Event, err)
			}
		}
	}
}
