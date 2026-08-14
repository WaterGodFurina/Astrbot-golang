package conversation

import (
	"fmt"
	"sync"
	"testing"
)

func TestGetConversationHistoryDeepCopy(t *testing.T) {
	m := NewManager(nil)
	umo := "test:private:u1"
	m.AppendHistory(umo, "user", "hello")

	cid := m.GetCurrConversationID(umo)
	hist := m.GetConversationHistory(cid)
	if len(hist) != 1 || hist[0]["content"] != "hello" {
		t.Fatalf("history = %v", hist)
	}

	// Mutating the returned copy must not touch the stored conversation.
	hist[0]["content"] = "tampered"
	hist[0]["injected"] = true
	hist2 := m.GetConversationHistory(cid)
	if hist2[0]["content"] != "hello" {
		t.Fatalf("stored history mutated via returned copy: %v", hist2)
	}
	if _, ok := hist2[0]["injected"]; ok {
		t.Fatalf("stored history got injected key: %v", hist2)
	}
}

func TestGetConversationSnapshotIsolation(t *testing.T) {
	m := NewManager(nil)
	umo := "test:private:u2"
	m.AppendHistory(umo, "user", "a")
	m.AppendHistory(umo, "assistant", "b")

	cid := m.GetCurrConversationID(umo)
	snap := m.GetConversationSnapshot(cid)
	if snap == nil || len(snap.History) != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	snap.History[0]["content"] = "tampered"
	snap.Title = "tampered-title"

	hist := m.GetConversationHistory(cid)
	if hist[0]["content"] != "a" {
		t.Fatalf("snapshot write leaked into stored history: %v", hist)
	}
	if m.GetConversationSnapshot(cid).Title == "tampered-title" {
		t.Fatal("snapshot write leaked into stored title")
	}
}

func TestAppendHistoryDequeues(t *testing.T) {
	m := NewManager(nil)
	m.SetDequeueContextLength(2)
	umo := "test:private:u3"
	m.AppendHistory(umo, "user", "m1")
	m.AppendHistory(umo, "assistant", "m2")
	m.AppendHistory(umo, "user", "m3")

	cid := m.GetCurrConversationID(umo)
	hist := m.GetConversationHistory(cid)
	if len(hist) != 2 {
		t.Fatalf("expected 2 entries after dequeue, got %d", len(hist))
	}
	if hist[0]["content"] != "m2" || hist[1]["content"] != "m3" {
		t.Fatalf("expected the two most recent entries, got %v", hist)
	}
}

func TestAppendHistoryNoDequeueByDefault(t *testing.T) {
	m := NewManager(nil)
	umo := "test:private:u4"
	for i := 0; i < 5; i++ {
		m.AppendHistory(umo, "user", fmt.Sprintf("m%d", i))
	}
	cid := m.GetCurrConversationID(umo)
	if hist := m.GetConversationHistory(cid); len(hist) != 5 {
		t.Fatalf("default (no dequeue) must keep all history, got %d", len(hist))
	}
}

func TestAppendHistoryDequeueDisabledByZero(t *testing.T) {
	m := NewManager(nil)
	m.SetDequeueContextLength(0)
	umo := "test:private:u5"
	for i := 0; i < 3; i++ {
		m.AppendHistory(umo, "user", fmt.Sprintf("m%d", i))
	}
	cid := m.GetCurrConversationID(umo)
	if hist := m.GetConversationHistory(cid); len(hist) != 3 {
		t.Fatalf("dequeue 0 must disable truncation, got %d", len(hist))
	}
}

// TestConcurrentHistoryAccess is a race-detector regression test for M-32:
// AppendHistory (lock-held append + lock-held persist snapshot) must not race
// the lock-free readers (GetConversationHistory / GetConversationSnapshot /
// GetAllConversations / GetConversationByCID). Run with `go test -race`.
func TestConcurrentHistoryAccess(t *testing.T) {
	m := NewManager(nil)
	umo := "race:private:u1"

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 3000; i++ {
			m.AppendHistory(umo, "user", fmt.Sprintf("msg-%d", i))
		}
	}()

	for i := 0; i < 3000; i++ {
		cid := m.GetCurrConversationID(umo)
		if cid != "" {
			m.GetConversationHistory(cid)
			m.GetConversationSnapshot(cid)
		}
		m.GetAllConversations()
		m.GetConversationByCID(cid)
	}
	wg.Wait()
}
