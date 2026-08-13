// Package star - command collection for the dashboard command management UI.
package star

import "sort"

// CollectCommandDescriptors returns descriptors for all filter-type handlers
// (commands, groups and sub-commands), mirroring
// astrbot/core/star/command_management.py list_commands().
// Disabled commands are included so they can be re-enabled from the UI.
func CollectCommandDescriptors(registry *StarHandlerRegistry) []*CommandDescriptor {
	if registry == nil {
		return nil
	}
	var descriptors []*CommandDescriptor
	for _, h := range registry.All() {
		if h.EventType != EventTypeFilter {
			continue
		}
		// Only handlers carrying a command filter are commands. Plugin message
		// filters (e.g. "plugin_filter_<id>_<name>") match EventTypeFilter too
		// but have no CommandFilter and must not appear in the command list.
		hasCommandFilter := false
		for _, f := range h.EventFilters {
			if _, ok := f.(*CommandFilter); ok {
				hasCommandFilter = true
				break
			}
		}
		if !hasCommandFilter {
			continue
		}
		desc := BuildDescriptor(registry, h)
		if desc == nil {
			continue
		}
		desc.HandlerName = h.HandlerName
		desc.PluginName = pluginNameFor(h)
		desc.Reserved = isReservedModule(h.HandlerModulePath)
		// aliases
		for _, filter := range h.EventFilters {
			if cf, ok := filter.(*CommandFilter); ok {
				desc.Aliases = cf.Aliases()
				break
			}
		}
		// permission
		desc.Permission = permissionFor(h)
		// sub_command marker (bridge registers group commands as plain filters)
		if desc.IsGroup {
			desc.CommandType = "command_group"
		}
		descriptors = append(descriptors, desc)
	}

	// conflict detection: same effective_command from different handlers
	seen := map[string]int{}
	for _, d := range descriptors {
		if d.EffectiveCommand != "" {
			seen[d.EffectiveCommand]++
		}
	}
	for _, d := range descriptors {
		if d.EffectiveCommand != "" && seen[d.EffectiveCommand] > 1 {
			d.HasConflict = true
		}
	}

	sort.SliceStable(descriptors, func(i, j int) bool {
		a := descriptors[i].EffectiveCommand
		if a == "" {
			a = "(zzz)"
		}
		b := descriptors[j].EffectiveCommand
		if b == "" {
			b = "(zzz)"
		}
		return a < b
	})
	return descriptors
}

// pluginNameFor derives the plugin name from a handler's module path.
func pluginNameFor(h *StarHandlerMetadata) string {
	// Bridged .so plugin commands use HandlerFullName "plugin_<name>".
	if h.HandlerFullName != "" {
		if len(h.HandlerFullName) > 7 && h.HandlerFullName[:7] == "plugin_" {
			return h.HandlerFullName[7:]
		}
	}
	if isReservedModule(h.HandlerModulePath) {
		return "astrbot"
	}
	return ""
}

// isReservedModule reports whether the handler belongs to the built-in commands.
func isReservedModule(modulePath string) bool {
	return modulePath == "astrbot.builtin_stars.builtin_commands.main"
}

// permissionFor extracts the permission level from a handler's filters.
func permissionFor(h *StarHandlerMetadata) string {
	for _, filter := range h.EventFilters {
		if pf, ok := filter.(*PermissionFilter); ok {
			switch pf.Permission() {
			case PermissionAdmin:
				return "admin"
			case PermissionMember:
				return "member"
			}
		}
	}
	return "member"
}

// SetHandlerPermission updates (or inserts) the permission filter of a handler.
func SetHandlerPermission(h *StarHandlerMetadata, permission string) {
	if h == nil {
		return
	}
	var target PermissionType
	switch permission {
	case "admin":
		target = PermissionAdmin
	default:
		target = PermissionMember
	}
	for i, filter := range h.EventFilters {
		if _, ok := filter.(*PermissionFilter); ok {
			h.EventFilters[i] = NewPermissionFilter(target)
			return
		}
	}
	h.EventFilters = append(h.EventFilters, NewPermissionFilter(target))
}

// ApplyCommandConfigs applies persisted command configs (enabled / renamed /
// permission) to the runtime handlers at startup.
// records: map[handler_full_name] -> {enabled, effective_command, permission}
func ApplyCommandConfigs(registry *StarHandlerRegistry, records map[string]interface{}) {
	if registry == nil || records == nil {
		return
	}
	for fullName, raw := range records {
		rec, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		handler := registry.Get(fullName)
		if handler == nil {
			continue
		}
		if enabled, ok := rec["enabled"].(bool); ok {
			handler.Enabled = enabled
		}
		if cmd, ok := rec["effective_command"].(string); ok && cmd != "" {
			_, _ = RenameCommand(registry, fullName, cmd)
		}
		if perm, ok := rec["permission"].(string); ok && perm != "" {
			SetHandlerPermission(handler, perm)
		}
	}
}
