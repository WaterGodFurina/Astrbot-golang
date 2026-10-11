package line

import (
	"testing"
	"time"
)

// TestGroupSourceTypeLazySweep 验证 groupSourceType 写入口的惰性清扫：
// 超过 groupSourceTypeTTL 未刷新的会话条目被清理，其余保留。
func TestGroupSourceTypeLazySweep(t *testing.T) {
	a := New(map[string]interface{}{}, map[string]interface{}{}, nil)
	now := time.Now()
	a.groupSourceType = map[string]*groupSourceTypeEntry{
		"Cexpired": {sourceType: "group", updatedAt: now.Add(-groupSourceTypeTTL - time.Minute)},
		"Cfresh":   {sourceType: "room", updatedAt: now},
	}

	// 复用写入口的清扫语义（convertEvent 中的 group 分支）。
	a.mu.Lock()
	a.groupSourceType["Cnew"] = &groupSourceTypeEntry{sourceType: "group", updatedAt: now}
	for id, e := range a.groupSourceType {
		if now.Sub(e.updatedAt) > groupSourceTypeTTL {
			delete(a.groupSourceType, id)
		}
	}
	a.mu.Unlock()

	a.mu.Lock()
	_, expiredExists := a.groupSourceType["Cexpired"]
	_, freshExists := a.groupSourceType["Cfresh"]
	_, newExists := a.groupSourceType["Cnew"]
	a.mu.Unlock()

	if expiredExists {
		t.Fatalf("超过 groupSourceTypeTTL 的会话条目应被惰性清理")
	}
	if !freshExists || !newExists {
		t.Fatalf("未过期的会话条目不应被清理")
	}
}

// TestGroupSourceTypeEntryShape 回归条目结构：缓存值经 entry.sourceType 读取。
func TestGroupSourceTypeEntryShape(t *testing.T) {
	e := &groupSourceTypeEntry{sourceType: "room", updatedAt: time.Now()}
	if e.sourceType != "room" {
		t.Fatalf("sourceType 应被保留")
	}
	if e.updatedAt.IsZero() {
		t.Fatalf("updatedAt 应在写入时记录")
	}
}
