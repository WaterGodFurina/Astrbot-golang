package plugin

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// newTestBlobStore 用极短 TTL 便于测试 GC；dir 用临时目录。
func newTestBlobStore(t *testing.T) *BlobStore {
	t.Helper()
	bs, err := NewBlobStore(t.TempDir(), 200*time.Millisecond, 64, defaultMaxBlobSize, defaultMaxBlobTTL) // 64B 块便于测分块
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	t.Cleanup(bs.Stop)
	return bs
}

func TestBlobCreateReadRelease(t *testing.T) {
	bs := newTestBlobStore(t)
	data := bytes.Repeat([]byte("abcdefgh"), 100) // 800B
	ref, err := bs.Create(data, "application/octet-stream", "big.bin", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if ref.HandleId == "" || len(ref.HandleId) != 32 {
		t.Fatalf("handle must be 32 hex, got %q", ref.HandleId)
	}
	if ref.Size != int64(len(data)) {
		t.Fatalf("size mismatch: %d vs %d", ref.Size, len(data))
	}

	// 分块读完整还原。
	var got []byte
	var total int64
	offset := int64(0)
	for {
		chunk, eof, totalSize, err := bs.Read(ref.HandleId, offset, 64)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got = append(got, chunk...)
		total = totalSize
		offset += int64(len(chunk))
		if eof {
			break
		}
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("roundtrip mismatch: %d vs %d bytes", len(got), len(data))
	}
	if total != int64(len(data)) {
		t.Fatalf("total mismatch: %d", total)
	}

	// Release 后（TTL 极短）Read 应最终失败。
	if err := bs.Release(ref.HandleId); err != nil {
		t.Fatalf("Release: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	bs.gcOnce()
	if _, _, _, err := bs.Read(ref.HandleId, 0, 64); err == nil {
		t.Fatalf("expected error after release+gc")
	}
}

func TestBlobPathTraversalRejected(t *testing.T) {
	bs := newTestBlobStore(t)
	// handle 注入路径穿越必须被拒（不落盘、不可读）。
	for _, evil := range []string{"../../etc/passwd", "/etc/passwd", "..", "a/../b"} {
		if _, _, _, err := bs.Read(evil, 0, 64); err == nil {
			t.Fatalf("traversal handle %q must be rejected", evil)
		}
	}
}

func TestBlobGCRemovesExpired(t *testing.T) {
	bs := newTestBlobStore(t)
	ref, err := bs.Create([]byte("hello world"), "", "x.txt", 0)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// TTL 200ms，等待过期后 gcOnce 应清理。
	time.Sleep(400 * time.Millisecond)
	bs.gcOnce()
	if _, _, _, err := bs.Read(ref.HandleId, 0, 64); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected expired blob gone, got err=%v", err)
	}
}

// TestBlobQuotaAndTTLClamp: 单 blob 超过大小上限必须拒绝；ttlSeconds 请求
// 超过 maxTTL 时必须被钳到上限（不能被插件任意延长）。
func TestBlobQuotaAndTTLClamp(t *testing.T) {
	bs, err := NewBlobStore(t.TempDir(), 10*time.Second, 64, 1024, 5*time.Second)
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	t.Cleanup(bs.Stop)

	if _, err := bs.Create(bytes.Repeat([]byte("x"), 2048), "", "big.bin", 0); err == nil {
		t.Fatal("blob exceeding max size must be rejected")
	}

	before := time.Now().Unix()
	ref, err := bs.Create([]byte("ok"), "", "small.bin", 3600)
	if err != nil {
		t.Fatalf("Create within limit: %v", err)
	}
	if ref.ExpiresAt > before+6 {
		t.Fatalf("ttlSeconds must be clamped to maxTTL: expires_at=%d", ref.ExpiresAt)
	}
}
