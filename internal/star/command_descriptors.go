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
		// 经快照读取 filters/Enabled：dashboard 写入口（SetHandlerPermission
		// 等）在写锁内修改 EventFilters/Enabled，快照拷贝使本遍历与写者互斥。
		filters, enabled, ok := registry.SnapshotFilters(h.HandlerFullName)
		if !ok {
			continue
		}
		// Only handlers carrying a command filter are commands. Plugin message
		// filters (e.g. "plugin_filter_<id>_<name>") match EventTypeFilter too
		// but have no CommandFilter and must not appear in the command list.
		hasCommandFilter := false
		for _, f := range filters {
			if _, ok := f.(*CommandFilter); ok {
				hasCommandFilter = true
				break
			}
		}
		if !hasCommandFilter {
			continue
		}
		// 用快照副本构建描述符：BuildDescriptor/permissionFor/aliases 读取
		// 的都是同一份拷贝，避免无锁访问共享 EventFilters。
		clone := *h
		clone.Enabled = enabled
		clone.EventFilters = filters
		desc := BuildDescriptor(registry, &clone)
		if desc == nil {
			continue
		}
		desc.HandlerName = clone.HandlerName
		desc.PluginName = pluginNameFor(&clone)
		desc.Reserved = isReservedModule(clone.HandlerModulePath)
		// aliases
		for _, filter := range clone.EventFilters {
			if cf, ok := filter.(*CommandFilter); ok {
				desc.Aliases = cf.Aliases()
				break
			}
		}
		// permission
		desc.Permission = permissionFor(&clone)
		// 组命令 type 统一为 "group"（对齐 Python command_management 的
		// command_type，WebUI 类型筛选按该值匹配）。
		if desc.IsGroup {
			desc.CommandType = "group"
		}
		descriptors = append(descriptors, desc)
	}

	// conflict detection: same effective_command from different handlers.
	// 只统计启用的指令（对齐 Python _group_conflicts）。
	seen := map[string]int{}
	for _, d := range descriptors {
		if d.EffectiveCommand != "" && d.Enabled {
			seen[d.EffectiveCommand]++
		}
	}
	for _, d := range descriptors {
		if d.EffectiveCommand != "" && d.Enabled && seen[d.EffectiveCommand] > 1 {
			d.HasConflict = true
		}
	}

	// 聚合子命令到虚拟组条目：子进程插件协议只上报子命令的 parent_group
	// （组本身不是 handler），这里按 "pluginID + parent_group" 合成组节点
	//（type=group），并把子命令挂到其 sub_commands；组节点的 effective
	// command = 组名，enabled = 任一子命令启用。
	groups := map[string]*CommandDescriptor{}
	var groupOrder []string
	for _, d := range descriptors {
		if !d.IsSubCommand || d.ParentSignature == "" {
			continue
		}
		key := d.PluginName + "\x00" + d.ParentSignature
		g, ok := groups[key]
		if !ok {
			g = &CommandDescriptor{
				HandlerFullName:  "plugin_" + d.PluginName + "_group_" + d.ParentSignature,
				HandlerName:      d.ParentSignature,
				PluginName:       d.PluginName,
				CommandType:      "group",
				IsGroup:          true,
				CurrentFragment:  d.ParentSignature,
				EffectiveCommand: d.ParentSignature,
				OriginalCommand:  d.ParentSignature,
				Enabled:          false,
				SubCommands:      []*CommandDescriptor{},
			}
			groups[key] = g
			groupOrder = append(groupOrder, key)
		}
		g.SubCommands = append(g.SubCommands, d)
		if d.Enabled {
			g.Enabled = true
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

	result := make([]*CommandDescriptor, 0, len(descriptors)+len(groupOrder))
	for _, key := range groupOrder {
		result = append(result, groups[key])
	}
	for _, d := range descriptors {
		// 子命令已挂到组条目的 sub_commands，不再平铺输出。
		if d.IsSubCommand && d.ParentSignature != "" {
			continue
		}
		result = append(result, d)
	}
	return result
}

// pluginNameFor derives the owning plugin name for a handler.
func pluginNameFor(h *StarHandlerMetadata) string {
	// 优先返回 handler 上的现成归属字段：子进程插件注册时写入 inst.ID，
	// 而 "plugin_" 前缀剥离会得到 "<id>_<cmdName>" 导致 WebUI 归属错误。
	if h.PluginName != "" {
		return h.PluginName
	}
	// Legacy .so plugin commands use HandlerFullName "plugin_<name>".
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

// ApplyCommandConfigs applies persisted command configs (enabled / renamed /
// permission) to the runtime handlers at startup. All mutations go through the
// registry's locked entry points so they cannot race the message pipeline's
// GetFilterHandlers reads.
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
		if enabled, ok := rec["enabled"].(bool); ok {
			if enabled {
				registry.Enable(fullName)
			} else {
				registry.Disable(fullName)
			}
		}
		if cmd, ok := rec["effective_command"].(string); ok && cmd != "" {
			_, _ = RenameCommand(registry, fullName, cmd)
		}
		if perm, ok := rec["permission"].(string); ok && perm != "" {
			registry.SetHandlerPermission(fullName, perm)
		}
	}
}
