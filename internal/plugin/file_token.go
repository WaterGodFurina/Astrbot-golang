package plugin

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultFileTokenTTL 是 RegisterFileToken 未显式指定 TTL 时的默认有效期。
// 文件令牌用于向下游（sandbox runtime/插件回传的媒体引用）公开宿主侧文件，
// 有效期应短于会话可能的消费窗口、长于同轮消息的处理时长；对齐 blob 存储的
// 短 TTL 语义（10min）并留一倍余量。
const DefaultFileTokenTTL = 30 * time.Minute

// ErrFileTokenNotFound / ErrFileTokenExpired 是文件令牌查询的两种失败语义：
// 未登记/已清理 → 404，登记过但超过 TTL → 410（Gone，对齐 HTTP 语义）。
var (
	ErrFileTokenNotFound = errors.New("file token not found")
	ErrFileTokenExpired  = errors.New("file token expired")
)

// fileTokenEntry 记录令牌对应的宿主侧文件（登记时已 EvalSymlinks 解析为
// 真实绝对路径）与过期时间。
type fileTokenEntry struct {
	path      string
	expiresAt time.Time
}

// FileTokenRegistry 是宿主侧文件令牌注册表：随机 uuid4 令牌 → 绝对路径的
// 并发安全映射，带 TTL 与过期清理。下游凭不可枚举的 token 经 dashboard 公开
// 路由 GET /api/file/{token} 读取文件，避免暴露真实路径。
type FileTokenRegistry struct {
	mu         sync.Mutex
	tokens     map[string]fileTokenEntry
	defaultTTL time.Duration
}

// NewFileTokenRegistry 创建注册表。defaultTTL <=0 时使用 DefaultFileTokenTTL。
func NewFileTokenRegistry(defaultTTL time.Duration) *FileTokenRegistry {
	if defaultTTL <= 0 {
		defaultTTL = DefaultFileTokenTTL
	}
	return &FileTokenRegistry{
		tokens:     map[string]fileTokenEntry{},
		defaultTTL: defaultTTL,
	}
}

// Register 校验 path 存在且为普通文件（EvalSymlinks 解析符号链接后判定），
// 登记为随机 uuid4 令牌并返回。ttl <=0 用注册表默认 TTL。
func (r *FileTokenRegistry) Register(path string, ttl time.Duration) (string, error) {
	if r == nil {
		return "", errors.New("file token registry not available")
	}
	if path == "" {
		return "", errors.New("file path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	// EvalSymlinks 解析符号链接后统一按真实路径校验与登记：下游凭 token
	// 读取时不再受链接目标变化影响，也避免经 /tmp 类链接目录绕过校验。
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("file not accessible: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("file not accessible: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("path is not a regular file")
	}
	if ttl <= 0 {
		ttl = r.defaultTTL
	}
	now := time.Now()
	token := uuid.NewString()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(now)
	r.tokens[token] = fileTokenEntry{path: resolved, expiresAt: now.Add(ttl)}
	return token, nil
}

// Lookup 返回令牌对应的文件路径。未登记返回 ErrFileTokenNotFound；已过期
// 返回 ErrFileTokenExpired（并顺带清除该条目）。
func (r *FileTokenRegistry) Lookup(token string) (string, error) {
	if r == nil || token == "" {
		return "", ErrFileTokenNotFound
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tokens[token]
	if !ok {
		return "", ErrFileTokenNotFound
	}
	if now.After(e.expiresAt) {
		delete(r.tokens, token)
		return "", ErrFileTokenExpired
	}
	return e.path, nil
}

// sweepLocked 清理全部已过期条目（惰性清理：Register/Lookup 时触发，
// 无需后台 goroutine；调用方须持有 r.mu）。
func (r *FileTokenRegistry) sweepLocked(now time.Time) {
	for t, e := range r.tokens {
		if now.After(e.expiresAt) {
			delete(r.tokens, t)
		}
	}
}
