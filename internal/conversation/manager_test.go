package conversation

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
)

// openTestDB 打开临时目录里的 SQLite 库（懒加载回归测试用）。
func openTestDB(t *testing.T) *db.Database {
	t.Helper()
	d, err := db.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

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

// TestLazyHistoryLoadAndPersist: 启动仅载元数据（history 懒加载）——首次
// 访问按需补载；AppendHistory/持久化不得以截断历史覆盖 DB 完整历史
// （内存优化回归）。
func TestLazyHistoryLoadAndPersist(t *testing.T) {
	database := openTestDB(t)
	umo := "aiocqhttp:FriendMessage:lazyuser"

	// 预置：一个带 3 条历史的会话直接写 DB（模拟重启前的旧数据）。
	seedHist := []map[string]interface{}{
		{"role": "user", "content": "旧消息1"},
		{"role": "assistant", "content": "旧回复1"},
		{"role": "user", "content": "旧消息2"},
	}
	b, _ := json.Marshal(seedHist)
	if err := database.CreateConversation(umo, umo, umo, string(b), "标题", ""); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	m := NewManager(database)
	m.loadFromDB()

	// 元数据已载，history 未补载。
	m.mu.RLock()
	conv := m.byCID[m.current[umo]]
	loaded := conv.historyLoaded
	m.mu.RUnlock()
	if conv == nil {
		t.Fatal("conversation metadata must be loaded at startup")
	}
	if loaded {
		t.Fatal("history must NOT be loaded at startup (lazy)")
	}

	// 首次读取 → 补载完整历史。
	hist := m.GetConversationHistory(umo)
	if len(hist) != 3 {
		t.Fatalf("lazy-loaded history length = %d, want 3", len(hist))
	}
	if hist[0]["content"] != "旧消息1" {
		t.Fatalf("lazy-loaded content mismatch: %v", hist[0])
	}

	// 追加新消息后持久化，DB 内容 = 3 旧 + 1 新（不得被截断覆盖）。
	m.AppendHistory(umo, "assistant", "新回复")
	row, found, err := database.GetConversationByID(m.GetCurrConversationID(umo))
	if err != nil || !found {
		t.Fatalf("persisted row missing: %v found=%v", err, found)
	}
	var saved []map[string]interface{}
	if err := json.Unmarshal([]byte(row.Content), &saved); err != nil {
		t.Fatalf("saved history unmarshal: %v", err)
	}
	if len(saved) != 4 {
		t.Fatalf("saved history length = %d, want 4 (3 old + 1 new)", len(saved))
	}
	if saved[3]["content"] != "新回复" {
		t.Fatalf("saved last entry = %v, want 新回复", saved[3])
	}
}
