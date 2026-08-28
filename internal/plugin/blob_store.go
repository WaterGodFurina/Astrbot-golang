// Blob store for large binary payloads crossing the plugin RPC boundary (P0-2).
//
// 大文件传输路径：插件把 >inline 阈值（1MB）的二进制交给宿主持久化，拿到
// 一个高熵随机 handle（FileReference），之后经 ReadBlob 分块读取。宿主统一
// 管理生命周期（TTL + 后台 GC + 启动清理），插件只传 handle，绝不传任意文件
// 路径，杜绝路径穿越。
//
// 生命周期：
//   - Create  落盘 data/blobs/<handle>，记录元数据
//   - Read    分块读（默认 1MB/块），刷新 last_access
//   - Info    元数据
//   - Release 主动失效标记（最终删除由 TTL/GC 判定，免疫"一插件释放、另一
//     插件仍在用"）
//   - 后台 GC  定时清理 released / last_access 超过 TTL 的 blob
//   - 启动清理 mtime 超过 TTL 的遗留件（Host 崩溃/重启场景）
package plugin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// BlobStore 管理插件大二进制（handle 制，安全 + TTL + GC）。
type BlobStore struct {
	dir       string
	ttl       time.Duration
	chunkSize int64
	mu        sync.Mutex
	meta      map[string]*blobMeta // handle_id -> meta
	stop      chan struct{}
	done      chan struct{}
}

type blobMeta struct {
	Size       int64     `json:"size"`
	MimeType   string    `json:"mime_type,omitempty"`
	Filename   string    `json:"filename,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	LastAccess time.Time `json:"last_access"`
	Released   bool      `json:"released,omitempty"`
}

var blobLogger = log.GetDefault().WithComponent("BlobStore")

// blobStoreGlobal 保存 Host 侧唯一的 BlobStore 实例（SetHostService 时创建）。
var blobStoreGlobal atomic.Value // *BlobStore

func setBlobStore(bs *BlobStore) { blobStoreGlobal.Store(bs) }
func getBlobStore() *BlobStore {
	if v := blobStoreGlobal.Load(); v != nil {
		return v.(*BlobStore)
	}
	return nil
}

// StopBlobStore 停止全局 BlobStore 的后台 GC（Host 关闭时调用）。
func StopBlobStore() {
	if bs := getBlobStore(); bs != nil {
		bs.Stop()
	}
}

// NewBlobStore 创建并启动 blob store。dir 为持久化根目录（data/blobs），
// ttl<=0 时使用默认 10 分钟；chunkSize<=0 时默认 1MB。
func NewBlobStore(dir string, ttl time.Duration, chunkSize int64) (*BlobStore, error) {
	if dir == "" {
		return nil, fmt.Errorf("blob dir is empty")
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if chunkSize <= 0 {
		chunkSize = 1 << 20
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir blob dir: %w", err)
	}
	s := &BlobStore{
		dir:       dir,
		ttl:       ttl,
		chunkSize: chunkSize,
		meta:      make(map[string]*blobMeta),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	s.gcStartup()
	go s.gcLoop()
	return s, nil
}

// Stop 停止后台 GC。
func (s *BlobStore) Stop() {
	select {
	case <-s.stop:
		return
	default:
		close(s.stop)
	}
	<-s.done
}

// blobPath 解析 handle 到磁盘路径并做 root containment 校验（防 handle 注入
// `../` 等路径穿越；Android/Termux 不假定普通权限模型）。
func (s *BlobStore) blobPath(handle string) (string, error) {
	if handle == "" {
		return "", fmt.Errorf("empty handle")
	}
	root, err := filepath.Abs(s.dir)
	if err != nil {
		return "", err
	}
	// handle 必须与文件名一致（由我们生成的 32hex），不信任任意传入路径。
	p := filepath.Join(root, filepath.Clean("/"+handle))
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", err
	}
	if rel == ".." || len(rel) >= 2 && rel[0] == '.' && rel[1] == '.' {
		return "", fmt.Errorf("handle escapes blob root: %q", handle)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		if rr, rerr := filepath.Rel(root, resolved); rerr == nil && (rr == ".." || len(rr) >= 2 && rr[0] == '.' && rr[1] == '.') {
			return "", fmt.Errorf("handle symlink escapes blob root: %q", handle)
		}
	}
	return abs, nil
}

// Create 持久化 data 并返回受控 FileReference handle。
func (s *BlobStore) Create(data []byte, mimeType, filename string, ttlSeconds int32) (sdkv1.FileReference, error) {
	if len(data) == 0 {
		return sdkv1.FileReference{}, fmt.Errorf("empty blob data")
	}
	handle, err := randomHandle()
	if err != nil {
		return sdkv1.FileReference{}, err
	}
	path, err := s.blobPath(handle)
	if err != nil {
		return sdkv1.FileReference{}, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return sdkv1.FileReference{}, fmt.Errorf("write blob: %w", err)
	}
	now := time.Now()
	ttl := s.ttl
	if ttlSeconds > 0 {
		ttl = time.Duration(ttlSeconds) * time.Second
	}
	s.mu.Lock()
	s.meta[handle] = &blobMeta{
		Size:       int64(len(data)),
		MimeType:   mimeType,
		Filename:   filename,
		CreatedAt:  now,
		LastAccess: now,
	}
	s.mu.Unlock()
	blobLogger.Debug("blob created: handle=%s size=%d ttl=%v", handle, len(data), ttl)
	return sdkv1.FileReference{
		HandleId:  handle,
		Size:      int64(len(data)),
		MimeType:  mimeType,
		Filename:  filename,
		ExpiresAt: now.Add(ttl).Unix(),
	}, nil
}

// Read 分块读取 blob，返回 (data, eof, totalSize)。offset<0 按 0；limit<=0 用
// store 默认块大小。读取会刷新 last_access（TTL 顺延）。
func (s *BlobStore) Read(handle string, offset int64, limit int32) ([]byte, bool, int64, error) {
	path, err := s.blobPath(handle)
	if err != nil {
		return nil, false, 0, err
	}
	f, err := os.Open(path) // #nosec G304 -- path 已经过 root containment 校验
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, 0, fmt.Errorf("blob not found: %s", handle)
		}
		return nil, false, 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, 0, err
	}
	total := info.Size()
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	chunk := int64(s.chunkSize)
	if limit > 0 {
		chunk = int64(limit)
	}
	buf := make([]byte, chunk)
	n, err := f.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, 0, err
	}
	data := buf[:n]
	eof := offset+int64(n) >= total
	s.mu.Lock()
	if m := s.meta[handle]; m != nil {
		m.LastAccess = time.Now()
	}
	s.mu.Unlock()
	return data, eof, total, nil
}

// Info 返回 blob 元数据。
func (s *BlobStore) Info(handle string) (sdkv1.FileReference, error) {
	if _, err := s.blobPath(handle); err != nil {
		return sdkv1.FileReference{}, err
	}
	path, err := s.blobPath(handle)
	if err != nil {
		return sdkv1.FileReference{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sdkv1.FileReference{}, fmt.Errorf("blob not found: %s", handle)
		}
		return sdkv1.FileReference{}, err
	}
	s.mu.Lock()
	m := s.meta[handle]
	s.mu.Unlock()
	ref := sdkv1.FileReference{HandleId: handle, Size: info.Size()}
	if m != nil {
		ref.MimeType = m.MimeType
		ref.Filename = m.Filename
		ref.ExpiresAt = m.LastAccess.Add(s.ttl).Unix()
	}
	return ref, nil
}

// Release 主动标记失效（TTL/GC 最终删除；对"另一插件仍在用"免疫）。
func (s *BlobStore) Release(handle string) error {
	if _, err := s.blobPath(handle); err != nil {
		return err
	}
	s.mu.Lock()
	if m := s.meta[handle]; m != nil {
		m.Released = true
		m.LastAccess = time.Now().Add(-s.ttl) // 立即进入可回收窗口
	}
	s.mu.Unlock()
	return nil
}

// gcStartup 清理上次退出遗留的过期 blob（Host 崩溃/重启场景）。
func (s *BlobStore) gcStartup() {
	now := time.Now()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		path := filepath.Join(s.dir, e.Name())
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > s.ttl {
			_ = os.Remove(path)
		}
	}
}

// gcLoop 后台 GC：清理 Released 或 last_access 超过 TTL 的 blob。
func (s *BlobStore) gcLoop() {
	defer close(s.done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.gcOnce()
		}
	}
}

func (s *BlobStore) gcOnce() {
	now := time.Now()
	s.mu.Lock()
	var toDelete []string
	for h, m := range s.meta {
		if m.Released || now.Sub(m.LastAccess) > s.ttl {
			toDelete = append(toDelete, h)
		}
	}
	for _, h := range toDelete {
		delete(s.meta, h)
	}
	s.mu.Unlock()
	for _, h := range toDelete {
		if path, err := s.blobPath(h); err == nil {
			_ = os.Remove(path)
			blobLogger.Debug("blob GC removed: %s", h)
		}
	}
}

func randomHandle() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ensure context 引用保留（后续可能用于带取消的读；保持 import 平衡）。
var _ = context.Background
