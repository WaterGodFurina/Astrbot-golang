package star

import (
	"testing"
)

// TestIssue9366_CommandGroupRenameSyncsChildren verifies the fix for issue #9366:
// When a command group is renamed, sub-commands must match the new prefix,
// not the old one.
func TestIssue9366_CommandGroupRenameSyncsChildren(t *testing.T) {
	// Create a command group "foo" with a sub-command "bar"
	group := NewCommandGroupFilter("foo", nil, nil)

	// Add a sub-command "bar" to the group
	subCmd := NewCommandFilter("bar", nil, group)

	// The group's own complete name is "foo"
	groupNames := group.GetCompleteCommandNames()
	if len(groupNames) == 0 || groupNames[0] != "foo" {
		t.Errorf("group expected 'foo', got %v", groupNames)
	}

	// The sub-command's complete name should be "foo bar"
	subNames := subCmd.GetCompleteCommandNames()
	if len(subNames) == 0 || subNames[0] != "foo bar" {
		t.Errorf("sub-command expected 'foo bar', got %v", subNames)
	}

	// Now rename the group to "baz"
	group.SetGroupName("baz")

	// After rename, the group's complete name should be "baz"
	newGroupNames := group.GetCompleteCommandNames()
	if len(newGroupNames) == 0 || newGroupNames[0] != "baz" {
		t.Errorf("after rename, expected group 'baz', got %v", newGroupNames)
	}

	// CRITICAL: sub-command should now resolve to "baz bar", NOT "foo bar"
	newSubNames := subCmd.GetCompleteCommandNames()
	if len(newSubNames) == 0 || newSubNames[0] != "baz bar" {
		t.Errorf(
			"BUG #9366: sub-command did not update after group rename!\n"+
				"  expected: 'baz bar'\n"+
				"  got:      '%s'\n"+
				"  Sub-commands must follow parent group renames.",
			firstOrEmpty(newSubNames),
		)
	}

	// The old prefix "foo bar" should no longer match
	oldCtx := &FilterContext{MessageStr: "foo bar", IsAtOrWake: true}
	if subCmd.Match(oldCtx) {
		t.Error(
			"BUG #9366: sub-command still matches old prefix 'foo bar' after rename!\n" +
				"The old prefix should no longer trigger the handler.",
		)
	}

	// The new prefix "baz bar" should match
	newCtx := &FilterContext{MessageStr: "baz bar", IsAtOrWake: true}
	if !subCmd.Match(newCtx) {
		t.Error("sub-command should match new prefix 'baz bar' after rename")
	}
}

// TestIssue9366_NestedGroupRename verifies that nested group renames
// propagate to deeply nested sub-commands.
func TestIssue9366_NestedGroupRename(t *testing.T) {
	parent := NewCommandGroupFilter("old_root", nil, nil)
	child := NewCommandGroupFilter("child", nil, parent)
	leaf := NewCommandFilter("leaf", nil, child)

	// Initially: "old_root child leaf"
	names := leaf.GetCompleteCommandNames()
	if len(names) == 0 || names[0] != "old_root child leaf" {
		t.Errorf("expected 'old_root child leaf', got %v", names)
	}

	// Rename parent
	parent.SetGroupName("root")

	// leaf should now be "root child leaf"
	newNames := leaf.GetCompleteCommandNames()
	if len(newNames) == 0 || newNames[0] != "root child leaf" {
		t.Errorf(
			"nested rename did not propagate to leaf!\n"+
				"  expected: 'root child leaf'\n"+
				"  got:      '%s'",
			firstOrEmpty(newNames),
		)
	}
}

// TestCommandGroup_AddAndRemoveSubCommand verifies sub-command management.
func TestCommandGroup_AddAndRemoveSubCommand(t *testing.T) {
	group := NewCommandGroupFilter("cmd", nil, nil)
	sub := NewCommandFilter("sub", nil, group)

	subs := group.GetSubCommandFilters()
	if len(subs) != 1 {
		t.Errorf("expected 1 sub-command, got %d", len(subs))
	}

	group.RemoveSubCommandFilter(sub)
	subs = group.GetSubCommandFilters()
	if len(subs) != 0 {
		t.Errorf("expected 0 sub-commands after removal, got %d", len(subs))
	}
}

func firstOrEmpty(s []string) string {
	if len(s) > 0 {
		return s[0]
	}
	return ""
}
