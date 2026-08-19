package knowledgebase

import (
	"strings"
	"testing"
)

// TestChunkTextNoGapAtNewlineTruncation verifies that truncating a chunk at a
// newline inside the (chunkSize/2, step) window does not silently drop the
// text between the truncation point and the next chunk start (M-57).
func TestChunkTextNoGapAtNewlineTruncation(t *testing.T) {
	// chunkSize=512, overlap=50 → step=462. The newline sits at rune 300
	// (> chunkSize/2 but < step): the old code truncated at 300 while advancing
	// by 462, losing runes [300, 462).
	// Newline sits at rune 300 (> chunkSize/2 but < step): the old code
	// truncated at 300 while advancing by 462, losing runes [300, 462).
	text := strings.Repeat("a", 300) + "\n" + "SENTINEL_MARKER" + strings.Repeat("b", 2000)
	chunks := ChunkText(text, 512, 50)
	if len(chunks) == 0 {
		t.Fatal("expected non-empty chunks")
	}
	found := false
	for _, c := range chunks {
		if strings.Contains(c, "SENTINEL_MARKER") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("SENTINEL_MARKER lost by newline truncation")
	}
	// Every non-whitespace rune of the source must appear in some chunk.
	joined := strings.Join(chunks, "")
	for _, r := range text {
		if r == '\n' || r == ' ' {
			continue
		}
		if !strings.ContainsRune(joined, r) {
			t.Fatalf("rune %q is missing from the chunks", r)
		}
	}
}

// TestChunkTextNoGapMidWindow exercises the same guarantee for newlines placed
// throughout the whole chunk window to catch regressions at the boundaries.
func TestChunkTextNoGapMidWindow(t *testing.T) {
	for _, nlPos := range []int{257, 300, 400, 461} {
		text := strings.Repeat("x", nlPos) + "\n" + "Y" + strings.Repeat("z", 3000)
		chunks := ChunkText(text, 512, 50)
		joined := strings.Join(chunks, "")
		for _, r := range text {
			if r == '\n' {
				continue
			}
			if !strings.ContainsRune(joined, r) {
				t.Fatalf("newline at %d: rune %q is missing", nlPos, r)
			}
		}
	}
}
