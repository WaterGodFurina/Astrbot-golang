// Package star implements the plugin/command management system.
// Ported from astrbot/core/star/
//
// Bug fix for issue #9366: command group rename doesn't sync child commands.
// The Python code cached parent_command_names in each CommandFilter at
// registration time. When rename_command updated the group's group_name via
// _set_filter_fragment, child filters' parent_command_names still held the old
// value, so sub-commands kept matching the old prefix.
//
// In Go, we store a live pointer to the parent group filter (not a cached
// string). When the group is renamed, we recursively invalidate the cached
// command-name lists of all children. This ensures sub-commands always
// resolve against the current group name.
package star

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// PermissionType controls who can invoke a command.
type PermissionType int

const (
	PermissionEveryone PermissionType = iota
	PermissionMember
	PermissionAdmin
)

// FilterContext carries the event and config needed for filter evaluation.
type FilterContext struct {
	MessageStr    string
	IsAtOrWake    bool
	Config        map[string]interface{}
	EventSenderID string
	EventPlatform string
	EventRole     string
	// EventMessageType is the platform message type ("GroupMessage" /
	// "FriendMessage"), used by EventMessageTypeFilter.
	EventMessageType string
}

// CommandFilter matches a single command name.
type CommandFilter struct {
	mu                sync.RWMutex
	commandName       string
	originalName      string
	alias             map[string]struct{}
	parentGroup       *CommandGroupFilter
	parentCmdNames    []string // cached
	cmplCmdNames      []string // cached complete names
	customFilters     []CustomFilter
	subCommandFilters []HandlerFilter
	handlerMetadata   *StarHandlerMetadata
}

// NewCommandFilter creates a command filter.
func NewCommandFilter(name string, alias []string, parent *CommandGroupFilter) *CommandFilter {
	cf := &CommandFilter{
		commandName:  name,
		originalName: name,
		alias:        make(map[string]struct{}),
		parentGroup:  parent,
	}
	for _, a := range alias {
		cf.alias[a] = struct{}{}
	}
	if parent != nil {
		parent.AddSubCommandFilter(cf)
	}
	return cf
}

// CommandName returns the current command name.
func (cf *CommandFilter) CommandName() string {
	cf.mu.RLock()
	defer cf.mu.RUnlock()
	return cf.commandName
}

// HasParentCommand reports whether this command belongs to a command group
// (mirrors Python's parent_command_names != [""] check; used by platform
// adapters that only register top-level commands).
func (cf *CommandFilter) HasParentCommand() bool {
	cf.mu.RLock()
	defer cf.mu.RUnlock()
	return cf.parentGroup != nil
}

// Aliases returns the registered aliases.
func (cf *CommandFilter) Aliases() []string {
	cf.mu.RLock()
	defer cf.mu.RUnlock()
	result := make([]string, 0, len(cf.alias))
	for a := range cf.alias {
		result = append(result, a)
	}
	return result
}

// SetCommandName updates the command name and invalidates caches.
func (cf *CommandFilter) SetCommandName(name string) {
	cf.mu.Lock()
	cf.commandName = name
	cf.cmplCmdNames = nil
	cf.mu.Unlock()
	// Invalidate parent's cache too since our name changed
	if cf.parentGroup != nil {
		cf.parentGroup.invalidateChildCaches()
	}
}

// Match checks if the filter matches the context.
func (cf *CommandFilter) Match(ctx *FilterContext) bool {
	if !ctx.IsAtOrWake {
		return false
	}
	names := cf.GetCompleteCommandNames()
	msg := strings.TrimSpace(ctx.MessageStr)
	for _, n := range names {
		if msg == n || strings.HasPrefix(msg, n+" ") {
			return true
		}
	}
	return false
}

// FilterType returns the filter type name.
func (cf *CommandFilter) FilterType() string { return "command" }

// GetCompleteCommandNames returns all matching command names (including aliases
// and parent prefixes). Recomputed from live parent state (never stale).
func (cf *CommandFilter) GetCompleteCommandNames() []string {
	cf.mu.RLock()
	if cf.cmplCmdNames != nil {
		cf.mu.RUnlock()
		return cf.cmplCmdNames
	}
	cf.mu.RUnlock()

	cf.mu.Lock()
	defer cf.mu.Unlock()
	// Double-check after acquiring write lock
	if cf.cmplCmdNames != nil {
		return cf.cmplCmdNames
	}

	// Get parent names from live parent (not cached string).
	parentNames := []string{""}
	if cf.parentGroup != nil {
		parentNames = cf.parentGroup.GetCompleteCommandNames()
	}

	candidates := []string{cf.commandName}
	for a := range cf.alias {
		candidates = append(candidates, a)
	}

	var result []string
	for _, parent := range parentNames {
		for _, candidate := range candidates {
			if parent == "" {
				result = append(result, candidate)
			} else {
				result = append(result, parent+" "+candidate)
			}
		}
	}

	cf.cmplCmdNames = result
	return result
}

// CommandGroupFilter matches a command group (parent of sub-commands).
type CommandGroupFilter struct {
	mu                sync.RWMutex
	groupName         string
	originalName      string
	alias             map[string]struct{}
	subCommandFilters []HandlerFilter
	customFilters     []CustomFilter
	parentGroup       *CommandGroupFilter
	cmplCmdNames      []string
}

// NewCommandGroupFilter creates a group filter.
func NewCommandGroupFilter(name string, alias []string, parent *CommandGroupFilter) *CommandGroupFilter {
	gf := &CommandGroupFilter{
		groupName:    name,
		originalName: name,
		alias:        make(map[string]struct{}),
		parentGroup:  parent,
	}
	for _, a := range alias {
		gf.alias[a] = struct{}{}
	}
	if parent != nil {
		parent.AddSubCommandFilter(gf)
	}
	return gf
}

// GroupName returns the current group name.
func (gf *CommandGroupFilter) GroupName() string {
	gf.mu.RLock()
	defer gf.mu.RUnlock()
	return gf.groupName
}

// SetGroupName updates the group name and recursively invalidates all
// child caches. This is the fix for issue #9366.
func (gf *CommandGroupFilter) SetGroupName(name string) {
	gf.mu.Lock()
	gf.groupName = name
	gf.cmplCmdNames = nil
	gf.mu.Unlock()
	// Issue #9366 fix: recursively invalidate all child command caches
	gf.invalidateChildCaches()
	// Also invalidate parent's cache
	if gf.parentGroup != nil {
		gf.parentGroup.invalidateChildCaches()
	}
}

// invalidateChildCaches clears cached command names for this group and all
// descendants. This ensures sub-commands recompute their parent prefixes
// after a rename.
func (gf *CommandGroupFilter) invalidateChildCaches() {
	gf.mu.Lock()
	gf.cmplCmdNames = nil
	subs := make([]HandlerFilter, len(gf.subCommandFilters))
	copy(subs, gf.subCommandFilters)
	gf.mu.Unlock()

	for _, sub := range subs {
		switch s := sub.(type) {
		case *CommandFilter:
			s.mu.Lock()
			s.cmplCmdNames = nil
			s.mu.Unlock()
		case *CommandGroupFilter:
			s.invalidateChildCaches()
		}
	}
}

// AddSubCommandFilter adds a child filter.
func (gf *CommandGroupFilter) AddSubCommandFilter(f HandlerFilter) {
	gf.mu.Lock()
	gf.subCommandFilters = append(gf.subCommandFilters, f)
	gf.mu.Unlock()
}

// GetSubCommandFilters returns a copy of the sub-command filter list.
func (gf *CommandGroupFilter) GetSubCommandFilters() []HandlerFilter {
	gf.mu.RLock()
	defer gf.mu.RUnlock()
	subs := make([]HandlerFilter, len(gf.subCommandFilters))
	copy(subs, gf.subCommandFilters)
	return subs
}

// RemoveSubCommandFilter removes a child filter.
func (gf *CommandGroupFilter) RemoveSubCommandFilter(f HandlerFilter) {
	gf.mu.Lock()
	defer gf.mu.Unlock()
	for i, sub := range gf.subCommandFilters {
		if sub == f {
			gf.subCommandFilters = append(gf.subCommandFilters[:i], gf.subCommandFilters[i+1:]...)
			return
		}
	}
}

// GetCompleteCommandNames returns all matching names for this group.
func (gf *CommandGroupFilter) GetCompleteCommandNames() []string {
	gf.mu.RLock()
	if gf.cmplCmdNames != nil {
		defer gf.mu.RUnlock()
		return gf.cmplCmdNames
	}
	gf.mu.RUnlock()

	gf.mu.Lock()
	defer gf.mu.Unlock()
	if gf.cmplCmdNames != nil {
		return gf.cmplCmdNames
	}

	parentNames := []string{""}
	if gf.parentGroup != nil {
		parentNames = gf.parentGroup.GetCompleteCommandNames()
	}

	candidates := []string{gf.groupName}
	for a := range gf.alias {
		candidates = append(candidates, a)
	}

	var result []string
	for _, parent := range parentNames {
		for _, candidate := range candidates {
			if parent == "" {
				result = append(result, candidate)
			} else {
				result = append(result, parent+" "+candidate)
			}
		}
	}

	gf.cmplCmdNames = result
	return result
}

// Match checks if the filter matches.
func (gf *CommandGroupFilter) Match(ctx *FilterContext) bool {
	if !ctx.IsAtOrWake {
		return false
	}
	names := gf.GetCompleteCommandNames()
	msg := strings.TrimSpace(ctx.MessageStr)
	for _, n := range names {
		if msg == n || strings.HasPrefix(msg, n+" ") {
			return true
		}
	}
	return false
}

// FilterType returns the filter type name.
func (gf *CommandGroupFilter) FilterType() string { return "command_group" }

// CustomFilter is a user-defined filter function.
type CustomFilter func(ctx *FilterContext) bool

// HandlerRegistry stores all registered handlers.
type HandlerRegistry struct {
	mu       sync.RWMutex
	handlers map[string]*StarHandlerMetadata
}

// NewHandlerRegistry creates an empty registry.
func NewHandlerRegistry() *HandlerRegistry {
	return &HandlerRegistry{handlers: make(map[string]*StarHandlerMetadata)}
}

// Register adds a handler.
func (r *HandlerRegistry) Register(h *StarHandlerMetadata) {
	r.mu.Lock()
	r.handlers[h.HandlerFullName] = h
	r.mu.Unlock()
}

// Get retrieves a handler by full name.
func (r *HandlerRegistry) Get(fullName string) *StarHandlerMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[fullName]
}

// All returns all handlers (sorted by name).
func (r *HandlerRegistry) All() []*StarHandlerMetadata {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]*StarHandlerMetadata, 0, len(r.handlers))
	for _, h := range r.handlers {
		result = append(result, h)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].HandlerFullName < result[j].HandlerFullName
	})
	return result
}

// CommandDescriptor describes a command for the management API.
type CommandDescriptor struct {
	HandlerFullName  string               `json:"handler_full_name"`
	HandlerName      string               `json:"handler_name"`
	PluginName       string               `json:"plugin"`
	ModulePath       string               `json:"module_path"`
	Description      string               `json:"description"`
	CommandType      string               `json:"type"` // "command" | "group" | "sub_command"
	ParentSignature  string               `json:"parent_signature"`
	OriginalCommand  string               `json:"original_command"`
	EffectiveCommand string               `json:"effective_command"`
	CurrentFragment  string               `json:"current_fragment"`
	Aliases          []string             `json:"aliases"`
	Permission       string               `json:"permission"`
	Enabled          bool                 `json:"enabled"`
	IsGroup          bool                 `json:"is_group"`
	IsSubCommand     bool                 `json:"is_sub_command"`
	HasConflict      bool                 `json:"has_conflict"`
	Reserved         bool                 `json:"reserved"`
	SubCommands      []*CommandDescriptor `json:"sub_commands"`
}

// HandlerLookup is the minimal registry interface used by descriptor helpers.
type HandlerLookup interface {
	Get(fullName string) *StarHandlerMetadata
}

// RenameCommand renames a command or group and propagates the change.
// FIXED #9366: When renaming a group, all child command filters' cached
// parent names are invalidated, so they recompute against the new name.
func RenameCommand(registry HandlerLookup, handlerFullName, newFragment string) (*CommandDescriptor, error) {
	handler := registry.Get(handlerFullName)
	if handler == nil {
		return nil, fmt.Errorf("handler not found: %s", handlerFullName)
	}

	newFragment = strings.TrimSpace(newFragment)
	if newFragment == "" {
		return nil, fmt.Errorf("command name cannot be empty")
	}

	// Find the primary command/group filter
	for _, filter := range handler.EventFilters {
		switch f := filter.(type) {
		case *CommandFilter:
			f.SetCommandName(newFragment)
		case *CommandGroupFilter:
			f.SetGroupName(newFragment) // This recursively invalidates children
		}
	}

	return BuildDescriptor(registry, handler), nil
}

// BuildDescriptor creates a descriptor from a handler.
func BuildDescriptor(registry HandlerLookup, handler *StarHandlerMetadata) *CommandDescriptor {
	desc := &CommandDescriptor{
		HandlerFullName: handler.HandlerFullName,
		HandlerName:     handler.HandlerName,
		ModulePath:      handler.HandlerModulePath,
		Description:     handler.Desc,
		Enabled:         handler.Enabled,
	}

	for _, filter := range handler.EventFilters {
		switch f := filter.(type) {
		case *CommandFilter:
			desc.CommandType = "command"
			desc.CurrentFragment = f.CommandName()
			desc.EffectiveCommand = f.GetCompleteCommandNames()[0]
		case *CommandGroupFilter:
			desc.CommandType = "group"
			desc.IsGroup = true
			desc.CurrentFragment = f.GroupName()
			names := f.GetCompleteCommandNames()
			if len(names) > 0 {
				desc.EffectiveCommand = names[0]
			}
		}
	}

	return desc
}

// ComposeCommand joins a parent signature and fragment.
func ComposeCommand(parentSig, fragment string) string {
	parentSig = strings.TrimSpace(parentSig)
	fragment = strings.TrimSpace(fragment)
	if parentSig == "" {
		return fragment
	}
	if fragment == "" {
		return parentSig
	}
	return parentSig + " " + fragment
}
