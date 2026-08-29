package plugin

import (
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
)

func newHostTestDB(t *testing.T) *db.Database {
	t.Helper()
	d, err := db.New(filepath.Join(t.TempDir(), "astrbot_test.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() {
		if d != nil {
			_ = d.Close()
		}
	})
	return d
}

// TestPlatformMessageHistoryCRUD 覆盖 skills/history RPC 背后的宿主 db 方法：
// 插入 → 裁剪(max_messages) → 读取 → 更新 → 按 ID 删除（纯 sqlite，无环境依赖）。
func TestPlatformMessageHistoryCRUD(t *testing.T) {
	d := newHostTestDB(t)
	const platformID = "aiocqhttp"
	const userID = "g:10001"

	// 插入两条（RecordPlatformMessage 是插件 skills_service 的写入口）
	for _, content := range []string{`{"type":"user","message":["hi"]}`, `{"type":"bot","message":["你好"]}`} {
		if err := d.RecordPlatformMessage(platformID, userID, "u1", content); err != nil {
			t.Fatalf("RecordPlatformMessage: %v", err)
		}
	}

	// 读取：limit=1 → 最近一条
	rows, err := d.GetPlatformMessageHistory(platformID, userID, 1)
	if err != nil {
		t.Fatalf("GetPlatformMessageHistory: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("limit=1: want 1 row, got %d", len(rows))
	}
	if rows[0].Content != `{"type":"bot","message":["你好"]}` {
		t.Fatalf("latest content mismatch: %s", rows[0].Content)
	}

	// 更新 content 与 llm_checkpoint_id
	content := `{"type":"bot","message":["更新后"]}`
	ck := "ck-99"
	if _, err := d.UpdatePlatformMessageHistory(rows[0].ID, &content, &ck); err != nil {
		t.Fatalf("UpdatePlatformMessageHistory: %v", err)
	}
	updated, err := d.GetPlatformMessageHistory(platformID, userID, 10)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if len(updated) != 2 {
		t.Fatalf("after update want 2 rows, got %d", len(updated))
	}

	// Trim 保留 1 条（对应 InsertPMHistoryRequest.max_messages）
	if err := d.TrimPlatformMessageHistory(platformID, userID, 1); err != nil {
		t.Fatalf("TrimPlatformMessageHistory: %v", err)
	}
	trimmed, err := d.GetPlatformMessageHistory(platformID, userID, 10)
	if err != nil {
		t.Fatalf("Get after trim: %v", err)
	}
	if len(trimmed) != 1 {
		t.Fatalf("after trim want 1 row, got %d", len(trimmed))
	}

	// 按 ID 删除
	deleted, err := d.DeletePlatformMessageHistoryByID(trimmed[0].ID)
	if err != nil || !deleted {
		t.Fatalf("DeletePlatformMessageHistoryByID: deleted=%v err=%v", deleted, err)
	}
	remaining, err := d.GetPlatformMessageHistory(platformID, userID, 10)
	if err != nil {
		t.Fatalf("Get after delete: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("after delete want 0 rows, got %d", len(remaining))
	}
}

// TestPlatformMessageHistoryUpdateFields 验证 update 的字段粒度为"只改传的字段"。
func TestPlatformMessageHistoryUpdateFields(t *testing.T) {
	d := newHostTestDB(t)
	const platformID, userID = "telegram", "u1"
	if err := d.RecordPlatformMessage(platformID, userID, "s1", "hello"); err != nil {
		t.Fatalf("RecordPlatformMessage: %v", err)
	}
	rows, err := d.GetPlatformMessageHistory(platformID, userID, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	id := rows[0].ID

	// 只更新 checkpoint：content 保持
	ck := "ck-1"
	if _, err := d.UpdatePlatformMessageHistory(id, nil, &ck); err != nil {
		t.Fatalf("update checkpoint: %v", err)
	}
	got, _ := d.GetPlatformMessageHistory(platformID, userID, 1)
	if got[0].Content != "hello" {
		t.Fatalf("content should stay unchanged, got %s", got[0].Content)
	}

	// 只更新 content
	newContent := "world"
	if _, err := d.UpdatePlatformMessageHistory(id, &newContent, nil); err != nil {
		t.Fatalf("update content: %v", err)
	}
	got2, _ := d.GetPlatformMessageHistory(platformID, userID, 1)
	if got2[0].Content != "world" {
		t.Fatalf("content should be world, got %s", got2[0].Content)
	}
}

// TestPMHistoryDecodeEncodeHelpers 验证 host_service 的 content 编解码辅助：
// JSON 结构化往返 + 纯文本原样保留。
func TestPMHistoryDecodeEncodeHelpers(t *testing.T) {
	if got := encodePMHistoryContent(nil); got != "" {
		t.Fatalf("encode nil: want empty, got %q", got)
	}
	if got := encodePMHistoryContent("plain text"); got != "plain text" {
		t.Fatalf("encode string: want raw, got %q", got)
	}
	if got := encodePMHistoryContent(map[string]any{"message": []any{"hi"}, "type": "user"}); got != `{"message":["hi"],"type":"user"}` {
		t.Fatalf("encode map: want json, got %q", got)
	}
	if got := decodePMHistoryContent(`{"type":"user"}`); got == `{"type":"user"}` {
		t.Fatalf("decode json should not stay raw string, got %T", got)
	}
	if got := decodePMHistoryContent("plain"); got != "plain" {
		t.Fatalf("decode plain: want raw, got %v", got)
	}
}
