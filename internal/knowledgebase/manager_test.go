package knowledgebase

import (
	"context"
	"strings"
	"testing"
)

// TestIssue9529_RetrieveByUUID verifies the fix for issue #9529:
// When a UUID is passed in kb_names, the system should find the KB by ID
// as a fallback, not just by name.
func TestIssue9529_RetrieveByUUID(t *testing.T) {
	mgr := NewManager()
	ctx := context.Background()

	kb := &KnowledgeBase{
		KBID:   "550e8400-e29b-41d4-a716-446655440000",
		KBName: "My Documents",
	}
	_, err := mgr.CreateKB(kb)
	if err != nil {
		t.Fatalf("CreateKB failed: %v", err)
	}

	// Retrieve by name should reach the retrieval backend (not "not found")
	_, err = mgr.Retrieve(ctx, "test query", []string{"My Documents"}, 5, 5)
	if err == nil || strings.Contains(err.Error(), "not found") {
		t.Errorf("retrieve by name failed: %v", err)
	}

	// Retrieve by UUID should also work (this was the bug)
	_, err = mgr.Retrieve(ctx, "test query", []string{kb.KBID}, 5, 5)
	if err == nil || strings.Contains(err.Error(), "not found") {
		t.Errorf(
			"BUG #9529: retrieve by UUID failed!\n"+
				"  UUID: %s\n"+
				"  error: %v",
			kb.KBID, err,
		)
	}
}

// TestIssue9392_NoHardFailOnMissingDeps verifies the fix for issue #9392:
// "SuperKMeans is not defined" - faiss optional dependency failure.
// In Go, we gracefully degrade instead of raising NameError.
func TestIssue9392_NoHardFailOnMissingDeps(t *testing.T) {
	mgr := NewManager()

	_, err := mgr.CreateKB(&KnowledgeBase{
		KBName: "TestKB",
	})
	if err != nil {
		t.Fatalf("CreateKB failed: %v", err)
	}

	// Upload should not fail with "SuperKMeans is not defined"
	// Other errors (network) are acceptable
	err = mgr.UploadFromURL("TestKB", "https://example.com/doc.txt", 512, 50)
	if err != nil {
		msg := err.Error()
		if contains(msg, "SuperKMeans") {
			t.Errorf(
				"BUG #9392: got 'SuperKMeans is not defined' error!\n" +
					"The Go version should gracefully handle missing optional dependencies.",
			)
		}
	}
}

func contains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
