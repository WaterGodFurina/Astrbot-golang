package utils

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestScheduleTempCleanupDeletesFile 验证注册的临时文件在短延时后被删除。
// TempCleanupDelay 是可参数化变量，测试中临时调小，避免等待默认 30 分钟。
func TestScheduleTempCleanupDeletesFile(t *testing.T) {
	origDelay := TempCleanupDelay
	TempCleanupDelay = 20 * time.Millisecond
	defer func() { TempCleanupDelay = origDelay }()

	dir := t.TempDir()
	path := filepath.Join(dir, "astrbot_test_voice.wav")
	if err := os.WriteFile(path, []byte("RIFF...."), 0o600); err != nil {
		t.Fatalf("写入测试临时文件失败: %v", err)
	}

	ScheduleTempCleanup(path)

	// 延时未到：文件应仍存在（保证消费窗口内可读）。
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("延时未到文件不应被删除: %v", err)
	}

	// 宽限等待删除完成。
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return // 已按预期删除
		}
		if time.Now().After(deadline) {
			t.Fatalf("注册清理后临时文件应被删除: %s", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestScheduleTempCleanupEmptyPath 验证空路径安全无操作（不 panic）。
func TestScheduleTempCleanupEmptyPath(t *testing.T) {
	origDelay := TempCleanupDelay
	TempCleanupDelay = time.Millisecond
	defer func() { TempCleanupDelay = origDelay }()

	ScheduleTempCleanup("")
}

// TestScheduleTempCleanupMissingFileOK 验证清理不存在的文件不报错
// （IsNotExist 被吞掉，对齐 mattermost/lark 现成清理函数语义）。
func TestScheduleTempCleanupMissingFileOK(t *testing.T) {
	origDelay := TempCleanupDelay
	TempCleanupDelay = time.Millisecond
	defer func() { TempCleanupDelay = origDelay }()

	ScheduleTempCleanup(filepath.Join(t.TempDir(), "astrbot_not_exist_9x.wav"))
	time.Sleep(50 * time.Millisecond)
}
