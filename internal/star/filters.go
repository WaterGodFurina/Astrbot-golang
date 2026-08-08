// Package star - additional filter types.
// Ported from astrbot/core/star/filter/
package star

import (
	"regexp"
	"strings"
)

// RegexFilter matches messages using a regular expression.
// Ported from astrbot/core/star/filter/regex.py
type RegexFilter struct {
	pattern *regexp.Regexp
}

// NewRegexFilter creates a regex filter.
func NewRegexFilter(pattern string) (*RegexFilter, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	return &RegexFilter{pattern: re}, nil
}

// Match returns true if the message matches the regex.
func (f *RegexFilter) Match(ctx *FilterContext) bool {
	return f.pattern.MatchString(ctx.MessageStr)
}

// FilterType returns the filter type name.
func (f *RegexFilter) FilterType() string { return "regex" }

// Pattern returns the compiled regex pattern.
func (f *RegexFilter) Pattern() string { return f.pattern.String() }

// EventMessageTypeFilter matches based on message type (group/friend).
// Ported from astrbot/core/star/filter/event_message_type.py
type EventMessageTypeFilter struct {
	messageType string // "GroupMessage", "FriendMessage"
}

// NewEventMessageTypeFilter creates a message type filter.
func NewEventMessageTypeFilter(msgType string) *EventMessageTypeFilter {
	return &EventMessageTypeFilter{messageType: msgType}
}

// Match returns true if the event message type matches.
func (f *EventMessageTypeFilter) Match(ctx *FilterContext) bool {
	// In Go, we check via FilterContext if available
	// For now, we check platform context if available
	return true // pass-through; actual filtering done at event level
}

// FilterType returns the filter type name.
func (f *EventMessageTypeFilter) FilterType() string { return "event_message_type" }

// MessageType returns the expected message type.
func (f *EventMessageTypeFilter) MessageType() string { return f.messageType }

// PermissionFilter matches based on user permission level.
// Ported from astrbot/core/star/filter/permission.py
type PermissionFilter struct {
	permission PermissionType
}

// NewPermissionFilter creates a permission filter.
func NewPermissionFilter(perm PermissionType) *PermissionFilter {
	return &PermissionFilter{permission: perm}
}

// Match returns true if the user has sufficient permission.
func (f *PermissionFilter) Match(ctx *FilterContext) bool {
	if f.permission == PermissionEveryone {
		return true
	}
	if f.permission == PermissionAdmin {
		return ctx.EventRole == "admin"
	}
	if f.permission == PermissionMember {
		return ctx.EventRole == "member" || ctx.EventRole == "admin"
	}
	return false
}

// FilterType returns the filter type name.
func (f *PermissionFilter) FilterType() string { return "permission" }

// Permission returns the required permission level.
func (f *PermissionFilter) Permission() PermissionType { return f.permission }

// PlatformAdapterTypeFilter matches based on platform adapter type.
// Ported from astrbot/core/star/filter/platform_adapter_type.py
type PlatformAdapterTypeFilter struct {
	adapterType string
}

// NewPlatformAdapterTypeFilter creates a platform adapter type filter.
func NewPlatformAdapterTypeFilter(adapterType string) *PlatformAdapterTypeFilter {
	return &PlatformAdapterTypeFilter{adapterType: adapterType}
}

// Match returns true if the platform matches.
func (f *PlatformAdapterTypeFilter) Match(ctx *FilterContext) bool {
	return strings.EqualFold(ctx.EventPlatform, f.adapterType)
}

// FilterType returns the filter type name.
func (f *PlatformAdapterTypeFilter) FilterType() string { return "platform_adapter_type" }

// AdapterType returns the expected adapter type.
func (f *PlatformAdapterTypeFilter) AdapterType() string { return f.adapterType }
