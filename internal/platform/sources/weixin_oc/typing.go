// 「对方正在输入」(typing) 状态管理。
// 对齐本体 weixin_oc_adapter.py:225-533 的 typing 会话（owners 集合 + keepalive
// 重发 + 延迟 cancel）与 weixin_oc_event.py:68-78 的 send_typing/stop_typing：
//   - 消息进入处理链时 start（首个 owner 触发 start + keepalive 循环）；
//   - 回复发出（Send）或适配器停止时 stop（owners 清空后发送 cancel）。
//
// SDK（go-weixin-ilink）的 typingManager 已封装 typing ticket 的获取/缓存/退避
// 与 start/stop 发送（context.go:176-183），本文件只负责会话编排与保活。
package weixin_oc

import (
	"sync"
	"time"

	ilink "github.com/dobest1024/go-weixin-ilink"
)

const (
	// defaultTypingKeepaliveInterval 默认 keepalive 重发间隔
	// （本体 weixin_oc_typing_keepalive_interval 默认 5s）。
	defaultTypingKeepaliveInterval = 5 * time.Second
	// typingOwnerTimeout owner 泄漏兜底：超过该时长无人 stop 时自动超时清理，
	// 避免 LLM 异常挂起导致 typing 状态永久残留。
	typingOwnerTimeout = 5 * time.Minute
)

// typingSession 单个用户的 typing 会话。
type typingSession struct {
	ctx       *ilink.Context // 消息上下文（承载 context_token 与 Ctx）
	owners    map[string]struct{}
	startedAt time.Time
	mu        sync.Mutex
	stopCh    chan struct{}
	stopped   bool
}

// typingManagerAdapter 管理所有用户的 typing 会话（按 user_id 索引）。
type typingManagerAdapter struct {
	mu       sync.Mutex
	sessions map[string]*typingSession
	interval time.Duration
}

func newTypingManagerAdapter(interval time.Duration) *typingManagerAdapter {
	if interval <= 0 {
		interval = defaultTypingKeepaliveInterval
	}
	return &typingManagerAdapter{
		sessions: make(map[string]*typingSession),
		interval: interval,
	}
}

// typingKeepaliveIntervalFromConfig 读取 weixin_oc_typing_keepalive_interval 配置
// （JSON 数字可能为 float64），非法时回退默认值。
func typingKeepaliveIntervalFromConfig(config map[string]interface{}) time.Duration {
	v, ok := config["weixin_oc_typing_keepalive_interval"]
	if !ok {
		return defaultTypingKeepaliveInterval
	}
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return time.Duration(n) * time.Second
		}
	case int:
		if n > 0 {
			return time.Duration(n) * time.Second
		}
	case string:
		if d, err := time.ParseDuration(n); err == nil && d > 0 {
			return d
		}
	}
	return defaultTypingKeepaliveInterval
}

// start 为一条消息事件开启 typing（owner 级别幂等）。
// 每条消息一个 owner id；同一用户并发多条消息共享同一会话的 keepalive。
func (tm *typingManagerAdapter) start(userID string, c *ilink.Context, ownerID string) {
	if c == nil || c.Message == nil || c.Message.ContextToken == "" {
		// 无 context_token 时 SDK 无法获取 typing ticket，直接跳过
		// （对齐本体 _typing_supported_for 的前置校验）。
		return
	}

	tm.mu.Lock()
	sess, isNew := tm.sessions[userID]
	if sess == nil {
		sess = &typingSession{
			owners:    make(map[string]struct{}),
			startedAt: time.Now(),
			stopCh:    make(chan struct{}),
		}
		tm.sessions[userID] = sess
		isNew = true
	} else {
		// 更新为最新消息的 context（ticket 随最新 context_token 刷新）。
		sess.ctx = c
	}
	tm.mu.Unlock()

	sess.mu.Lock()
	if sess.stopped {
		sess.mu.Unlock()
		return
	}
	_, dup := sess.owners[ownerID]
	sess.owners[ownerID] = struct{}{}
	firstOwner := len(sess.owners) == 1 && !dup
	sess.mu.Unlock()
	if dup {
		return
	}

	if isNew || firstOwner {
		// 首个 owner：发送 start 并启动 keepalive 循环。
		if err := c.Typing(); err != nil {
			logger.Warn("发送 typing 状态失败 user=%s: %v", userID, err)
		}
		go tm.keepalive(userID, sess)
	}
}

// stop 结束一个 owner 的 typing；owners 清空后发送 cancel 并回收会话。
func (tm *typingManagerAdapter) stop(userID, ownerID string) {
	tm.mu.Lock()
	sess := tm.sessions[userID]
	tm.mu.Unlock()
	if sess == nil {
		return
	}

	sess.mu.Lock()
	delete(sess.owners, ownerID)
	if len(sess.owners) > 0 {
		sess.mu.Unlock()
		return
	}
	// 最后一个 owner：关闭 keepalive 并发送 stop。
	if sess.stopped {
		sess.mu.Unlock()
		return
	}
	sess.stopped = true
	close(sess.stopCh)
	ctx := sess.ctx
	sess.mu.Unlock()

	if ctx != nil {
		if err := ctx.StopTyping(); err != nil {
			logger.Warn("取消 typing 状态失败 user=%s: %v", userID, err)
		}
	}
	tm.mu.Lock()
	delete(tm.sessions, userID)
	tm.mu.Unlock()
}

// stopAllOwners 结束该用户会话的全部 owner（回复发出时调用）。
func (tm *typingManagerAdapter) stopAllOwners(userID string) {
	tm.mu.Lock()
	sess := tm.sessions[userID]
	tm.mu.Unlock()
	if sess == nil {
		return
	}
	sess.mu.Lock()
	owners := make([]string, 0, len(sess.owners))
	for id := range sess.owners {
		owners = append(owners, id)
	}
	sess.mu.Unlock()
	for _, id := range owners {
		tm.stop(userID, id)
	}
}

// stopAll 停止全部会话（适配器 Stop 时调用，对齐本体 _cleanup_typing_tasks）。
func (tm *typingManagerAdapter) stopAll() {
	tm.mu.Lock()
	userIDs := make([]string, 0, len(tm.sessions))
	for id := range tm.sessions {
		userIDs = append(userIDs, id)
	}
	tm.mu.Unlock()
	for _, id := range userIDs {
		tm.stopAllOwners(id)
	}
}

// keepalive 周期性重发 typing start，维持"输入中"状态
// （对齐本体 _typing_keepalive_loop；ticket 的获取/缓存由 SDK typingManager 处理）。
func (tm *typingManagerAdapter) keepalive(userID string, sess *typingSession) {
	ticker := time.NewTicker(tm.interval)
	defer ticker.Stop()
	for {
		select {
		case <-sess.stopCh:
			return
		case <-ticker.C:
		}

		sess.mu.Lock()
		if sess.stopped || len(sess.owners) == 0 {
			sess.mu.Unlock()
			return
		}
		// owner 泄漏兜底：超时自动回收。
		if time.Since(sess.startedAt) > typingOwnerTimeout {
			sess.mu.Unlock()
			logger.Warn("typing 会话超时自动回收 user=%s", userID)
			tm.stopAllOwners(userID)
			return
		}
		ctx := sess.ctx
		sess.mu.Unlock()

		if ctx == nil {
			return
		}
		if err := ctx.Typing(); err != nil {
			logger.Debug("typing keepalive 发送失败 user=%s: %v", userID, err)
		}
	}
}
