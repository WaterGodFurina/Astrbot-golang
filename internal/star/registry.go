// Package star implements the star (handler) registration and dispatch system.
// Ported from astrbot/core/star/star_handler.py and star.py
package star

import (
	"fmt"
	"sort"
	"sync"
)

// EventType classifies the kind of handler event.
type EventType int

const (
	EventTypeFilter                  EventType = iota // Regular message filter handler
	EventTypeOnAstrMessageEvent                       // OnAstrMessageEvent
	EventTypeOnMessageReceivedEvent                   // OnMessageReceivedEvent (before pipeline)
	EventTypeOnPreProcessEvent                        // OnPreProcessEvent
	EventTypeOnResultHandlingEvent                    // OnResultHandlingEvent (before result decorate)
	EventTypeOnDecoratingResultEvent                  // OnDecoratingResultEvent (before sending)
	EventTypeOnAfterMessageSentEvent                  // OnAfterMessageSentEvent (after sending)
	EventTypeOnSessionResumeEvent                     // OnSessionResumeEvent
	EventTypeOnLLMRequestEvent                        // OnLLMRequestEvent (before LLM call)
	EventTypeOnLLMResponseEvent                       // OnLLMResponseEvent (after LLM call)
	EventTypeOnToolCallEvent                          // OnToolCallEvent
	EventTypeOnUnloadEvent                            // OnUnloadEvent (plugin unload)
)

func (et EventType) String() string {
	names := []string{
		"Filter",
		"OnAstrMessageEvent",
		"OnMessageReceivedEvent",
		"OnPreProcessEvent",
		"OnResultHandlingEvent",
		"OnDecoratingResultEvent",
		"OnAfterMessageSentEvent",
		"OnSessionResumeEvent",
		"OnLLMRequestEvent",
		"OnLLMResponseEvent",
		"OnToolCallEvent",
		"OnUnloadEvent",
	}
	if int(et) < len(names) {
		return names[et]
	}
	return fmt.Sprintf("EventType(%d)", int(et))
}

// HandlerFunc is the function signature for a star handler.
// It receives an event interface{} (to avoid import cycles) and returns an error.
type HandlerFunc func(event interface{}) error

// HandlerFilter is the interface for filters that determine if a handler should run.
type HandlerFilter interface {
	// Match returns true if the handler should process this event.
	Match(ctx *FilterContext) bool
	// FilterType returns the type name of this filter.
	FilterType() string
}

// StarHandlerMetadata describes a registered handler.
type StarHandlerMetadata struct {
	HandlerFullName   string `json:"handler_full_name"`   // module_handler
	HandlerName       string `json:"handler_name"`        // method name
	HandlerModulePath string `json:"handler_module_path"` // module path
	// PluginName identifies the owning plugin (inst.ID) so session rules can
	// enable/disable specific plugins. Empty = built-in/system (never filtered).
	PluginName    string                 `json:"plugin_name,omitempty"`
	Handler       HandlerFunc            `json:"-"`
	EventType     EventType              `json:"event_type"`
	EventFilters  []HandlerFilter        `json:"-"`
	Desc          string                 `json:"desc,omitempty"`
	ExtrasConfigs map[string]interface{} `json:"extras_configs,omitempty"`
	Enabled       bool                   `json:"enabled"`
}

// IsEnabled returns true if the handler is enabled.
func (h *StarHandlerMetadata) IsEnabled() bool { return h.Enabled }

// Priority returns the handler's priority (default 0).
func (h *StarHandlerMetadata) Priority() int {
	if v, ok := h.ExtrasConfigs["priority"]; ok {
		if p, ok := v.(int); ok {
			return p
		}
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// StarMetadata describes a plugin (star).
type StarMetadata struct {
	Name               string      `json:"name"`
	Author             string      `json:"author"`
	Desc               string      `json:"desc"`
	ShortDesc          string      `json:"short_desc"`
	Version            string      `json:"version"`
	Repo               string      `json:"repo"`
	StarModulePath     string      `json:"star_module_path"`
	StarClsType        interface{} `json:"-"`
	Activated          bool        `json:"activated"`
	Reserved           bool        `json:"reserved"`
	SupportedPlatforms []string    `json:"supported_platforms,omitempty"`
	AstrBotVersion     string      `json:"astrbot_version,omitempty"`
}

// PluginID returns the plugin identifier.
func (s *StarMetadata) PluginID() string {
	name := s.Name
	if name == "" {
		name = "unknown"
	}
	author := s.Author
	if author == "" {
		author = "unknown"
	}
	return fmt.Sprintf("%s/%s", author, name)
}

func (s *StarMetadata) String() string {
	return fmt.Sprintf("Plugin %s (%s) by %s: %s", s.Name, s.Version, s.Author, s.Desc)
}

// StarRegistry maintains all registered stars (plugins).
type StarRegistry struct {
	mu      sync.RWMutex
	stars   []*StarMetadata
	starMap map[string]*StarMetadata // key = module path
}

// NewStarRegistry creates a new registry.
func NewStarRegistry() *StarRegistry {
	return &StarRegistry{starMap: make(map[string]*StarMetadata)}
}

// Register adds a star.
func (r *StarRegistry) Register(meta *StarMetadata) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stars = append(r.stars, meta)
	r.starMap[meta.StarModulePath] = meta
}

// Get returns a star by module path.
func (r *StarRegistry) Get(modulePath string) *StarMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.starMap[modulePath]
}

// All returns all registered stars.
func (r *StarRegistry) All() []*StarMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*StarMetadata, len(r.stars))
	copy(result, r.stars)
	return result
}

// StarHandlerRegistry maintains all registered handlers.
type StarHandlerRegistry struct {
	mu          sync.RWMutex
	handlers    []*StarHandlerMetadata
	handlersMap map[string]*StarHandlerMetadata // key = handler_full_name
}

// NewStarHandlerRegistry creates a new handler registry.
func NewStarHandlerRegistry() *StarHandlerRegistry {
	return &StarHandlerRegistry{handlersMap: make(map[string]*StarHandlerMetadata)}
}

// Append adds a handler, maintaining priority order.
func (r *StarHandlerRegistry) Append(h *StarHandlerMetadata) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h.ExtrasConfigs == nil {
		h.ExtrasConfigs = make(map[string]interface{})
	}
	if _, ok := h.ExtrasConfigs["priority"]; !ok {
		h.ExtrasConfigs["priority"] = 0
	}
	if !h.Enabled {
		h.Enabled = true
	}
	r.handlers = append(r.handlers, h)
	r.handlersMap[h.HandlerFullName] = h
	// sort by priority (lower first)
	sort.SliceStable(r.handlers, func(i, j int) bool {
		return r.handlers[i].Priority() < r.handlers[j].Priority()
	})
}

// Remove deletes a handler by full name.
func (r *StarHandlerRegistry) Remove(fullName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.handlersMap, fullName)
	for i, h := range r.handlers {
		if h.HandlerFullName == fullName {
			r.handlers = append(r.handlers[:i], r.handlers[i+1:]...)
			break
		}
	}
}

// Get returns a handler by full name.
func (r *StarHandlerRegistry) Get(fullName string) *StarHandlerMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlersMap[fullName]
}

// All returns all handlers (sorted by priority).
func (r *StarHandlerRegistry) All() []*StarHandlerMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*StarHandlerMetadata, len(r.handlers))
	copy(result, r.handlers)
	return result
}

// GetHandlersByEventType returns handlers matching the event type.
// If pluginsName is non-empty, only handlers from those plugins are returned.
// Ownership is matched via PluginName: subprocess plugin handlers all share the
// constant module path "data.plugins", so module-path matching could not tell
// them apart; the real plugin identity lives in PluginName.
func (r *StarHandlerRegistry) GetHandlersByEventType(et EventType, pluginsName []string) []*StarHandlerMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*StarHandlerMetadata
	for _, h := range r.handlers {
		if h.EventType != et || !h.Enabled {
			continue
		}
		if len(pluginsName) > 0 {
			matched := false
			for _, pName := range pluginsName {
				if h.PluginName == pName {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}
		result = append(result, h)
	}
	return result
}

// GetFilterHandlers returns all filter-type handlers.
func (r *StarHandlerRegistry) GetFilterHandlers() []*StarHandlerMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var result []*StarHandlerMetadata
	for _, h := range r.handlers {
		if h.EventType == EventTypeFilter && h.Enabled {
			result = append(result, h)
		}
	}
	return result
}

// Disable disables a handler by full name.
func (r *StarHandlerRegistry) Disable(fullName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.handlersMap[fullName]; ok {
		h.Enabled = false
	}
}

// Enable enables a handler by full name.
func (r *StarHandlerRegistry) Enable(fullName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if h, ok := r.handlersMap[fullName]; ok {
		h.Enabled = true
	}
}

// Clear removes all handlers.
func (r *StarHandlerRegistry) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers = nil
	r.handlersMap = make(map[string]*StarHandlerMetadata)
}
