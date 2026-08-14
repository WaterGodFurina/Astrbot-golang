// 企业微信智能机器人队列管理器。
// 1:1 移植自 wecomai_queue_mgr.py：
//   - 输入队列：StreamID → 接收用户消息的队列（有监听器协程处理）；
//   - 输出队列：StreamID → 机器人响应队列（供 webhook 轮询聚合）；
//   - 待处理响应缓存与已完成流缓存，支持过期清理。
package wecom_ai_bot

import (
	"sync"
	"time"
)

// QueueItem 队列元素（输入队列与输出队列共用）。
type QueueItem struct {
	// Type 消息类型：plain / image / break / end / complete（输出队列）
	Type string
	// Data 文本数据
	Data string
	// ImageData 图片 base64 数据
	ImageData string
	// Streaming 是否为流式（累加）输出
	Streaming bool
	// SessionID 会话 ID（即 stream_id）
	SessionID string

	// 以下字段仅输入队列使用
	// MessageData 原始消息数据
	MessageData map[string]interface{}
	// CallbackParams 回调参数（nonce/timestamp/req_id 等）
	CallbackParams map[string]string
}

// PendingResponse 待处理的响应缓存。
type PendingResponse struct {
	CallbackParams map[string]string
	Timestamp      time.Time
}

// WecomAIQueueMgr 企业微信智能机器人队列管理器。
type WecomAIQueueMgr struct {
	mu sync.Mutex

	queues       map[string]chan *QueueItem // StreamID → 输入队列
	backQueues   map[string]chan *QueueItem // StreamID → 输出队列
	pendingResponses map[string]*PendingResponse
	completedStreams map[string]time.Time
	queueCloseEvents map[string]chan struct{}
	listenerCancels  map[string]func()

	listenerCallback func(*QueueItem)

	queueMaxSize    int
	backQueueMaxSize int
}

// NewWecomAIQueueMgr 构造队列管理器（queue_maxsize=128, back_queue_maxsize=512）。
func NewWecomAIQueueMgr() *WecomAIQueueMgr {
	return &WecomAIQueueMgr{
		queues:           make(map[string]chan *QueueItem),
		backQueues:       make(map[string]chan *QueueItem),
		pendingResponses: make(map[string]*PendingResponse),
		completedStreams: make(map[string]time.Time),
		queueCloseEvents: make(map[string]chan struct{}),
		listenerCancels:  make(map[string]func()),
		queueMaxSize:     128,
		backQueueMaxSize: 512,
	}
}

// GetOrCreateQueue 获取或创建指定会话的输入队列。
func (m *WecomAIQueueMgr) GetOrCreateQueue(sessionID string) chan *QueueItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q, ok := m.queues[sessionID]; ok {
		return q
	}
	q := make(chan *QueueItem, m.queueMaxSize)
	m.queues[sessionID] = q
	closeEvt := make(chan struct{})
	m.queueCloseEvents[sessionID] = closeEvt
	m.startListenerLocked(sessionID)
	logger.Debug("[WecomAI] 创建输入队列: %s", sessionID)
	return q
}

// GetOrCreateBackQueue 获取或创建指定会话的输出队列。
func (m *WecomAIQueueMgr) GetOrCreateBackQueue(sessionID string) chan *QueueItem {
	m.mu.Lock()
	defer m.mu.Unlock()
	if q, ok := m.backQueues[sessionID]; ok {
		return q
	}
	q := make(chan *QueueItem, m.backQueueMaxSize)
	m.backQueues[sessionID] = q
	logger.Debug("[WecomAI] 创建输出队列: %s", sessionID)
	return q
}

// RemoveQueues 移除指定会话的所有队列。
// markFinished 为 true 时标记为已正常结束（写入 completedStreams）。
func (m *WecomAIQueueMgr) RemoveQueues(sessionID string, markFinished bool) {
	m.RemoveQueue(sessionID)

	m.mu.Lock()
	if _, ok := m.backQueues[sessionID]; ok {
		delete(m.backQueues, sessionID)
		logger.Debug("[WecomAI] 移除输出队列: %s", sessionID)
	}
	if _, ok := m.pendingResponses[sessionID]; ok {
		delete(m.pendingResponses, sessionID)
		logger.Debug("[WecomAI] 移除待处理响应: %s", sessionID)
	}
	if markFinished {
		m.completedStreams[sessionID] = time.Now()
		logger.Debug("[WecomAI] 标记流已结束: %s", sessionID)
	}
	m.mu.Unlock()
}

// RemoveQueue 仅移除输入队列和对应监听任务。
func (m *WecomAIQueueMgr) RemoveQueue(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.queues[sessionID]; ok {
		delete(m.queues, sessionID)
		logger.Debug("[WecomAI] 移除输入队列: %s", sessionID)
	}
	if closeEvt, ok := m.queueCloseEvents[sessionID]; ok {
		select {
		case <-closeEvt:
		default:
			close(closeEvt)
		}
		delete(m.queueCloseEvents, sessionID)
	}
	if cancel, ok := m.listenerCancels[sessionID]; ok {
		cancel()
		delete(m.listenerCancels, sessionID)
	}
}

// HasQueue 检查是否存在输入队列。
func (m *WecomAIQueueMgr) HasQueue(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.queues[sessionID]
	return ok
}

// HasBackQueue 检查是否存在输出队列。
func (m *WecomAIQueueMgr) HasBackQueue(sessionID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.backQueues[sessionID]
	return ok
}

// SetPendingResponse 设置待处理的响应参数。
func (m *WecomAIQueueMgr) SetPendingResponse(sessionID string, callbackParams map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pendingResponses[sessionID] = &PendingResponse{
		CallbackParams: callbackParams,
		Timestamp:      time.Now(),
	}
	logger.Debug("[WecomAI] 设置待处理响应: %s", sessionID)
}

// GetPendingResponse 获取待处理的响应参数。
func (m *WecomAIQueueMgr) GetPendingResponse(sessionID string) *PendingResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingResponses[sessionID]
}

// IsStreamFinished 判断 stream 是否在短期内已结束（对应 is_stream_finished，默认 60 秒窗口）。
func (m *WecomAIQueueMgr) IsStreamFinished(sessionID string, maxAgeSeconds int) bool {
	if maxAgeSeconds <= 0 {
		maxAgeSeconds = 60
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	finishedAt, ok := m.completedStreams[sessionID]
	if !ok {
		return false
	}
	if time.Since(finishedAt) > time.Duration(maxAgeSeconds)*time.Second {
		delete(m.completedStreams, sessionID)
		return false
	}
	return true
}

// CleanupExpiredResponses 清理过期的待处理响应（对应 cleanup_expired_responses，
// 默认 300 秒；已完成流缓存 60 秒过期）。
func (m *WecomAIQueueMgr) CleanupExpiredResponses(maxAgeSeconds int) {
	if maxAgeSeconds <= 0 {
		maxAgeSeconds = 300
	}
	now := time.Now()
	m.mu.Lock()
	var expired []string
	for sessionID, resp := range m.pendingResponses {
		if now.Sub(resp.Timestamp) > time.Duration(maxAgeSeconds)*time.Second {
			expired = append(expired, sessionID)
		}
	}
	m.mu.Unlock()
	for _, sessionID := range expired {
		m.RemoveQueues(sessionID, false)
		logger.Debug("[WecomAI] 清理过期响应及队列: %s", sessionID)
	}
	m.mu.Lock()
	for sessionID, finishedAt := range m.completedStreams {
		if now.Sub(finishedAt) > 60*time.Second {
			delete(m.completedStreams, sessionID)
		}
	}
	m.mu.Unlock()
}

// SetListener 注册队列监听回调，并为已存在的输入队列启动监听器。
func (m *WecomAIQueueMgr) SetListener(callback func(*QueueItem)) {
	m.mu.Lock()
	m.listenerCallback = callback
	sessions := make([]string, 0, len(m.queues))
	for sessionID := range m.queues {
		sessions = append(sessions, sessionID)
	}
	m.mu.Unlock()
	for _, sessionID := range sessions {
		m.startListenerIfNeeded(sessionID)
	}
}

// startListenerLocked 在持锁状态下启动监听器（内部方法）。
func (m *WecomAIQueueMgr) startListenerLocked(sessionID string) {
	if m.listenerCallback == nil {
		return
	}
	if cancel, ok := m.listenerCancels[sessionID]; ok && cancel != nil {
		return
	}
	queue := m.queues[sessionID]
	closeEvt := m.queueCloseEvents[sessionID]
	if queue == nil || closeEvt == nil {
		return
	}
	stop := make(chan struct{})
	m.listenerCancels[sessionID] = func() { close(stop) }
	go m.listenToQueue(sessionID, queue, closeEvt, stop)
	logger.Debug("[WecomAI] 为会话启动监听器: %s", sessionID)
}

// startListenerIfNeeded 为指定会话启动监听器（未持锁版本）。
func (m *WecomAIQueueMgr) startListenerIfNeeded(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startListenerLocked(sessionID)
}

// listenToQueue 监听输入队列并回调处理（对应 _listen_to_queue）。
func (m *WecomAIQueueMgr) listenToQueue(sessionID string, queue chan *QueueItem, closeEvt chan struct{}, stop chan struct{}) {
	defer func() {
		if r := recover(); r != nil {
			logger.I18nWarn("会话 %s 队列监听器异常退出: %v", sessionID, r)
		}
	}()
	for {
		select {
		case item := <-queue:
			if item == nil {
				continue
			}
			m.mu.Lock()
			cb := m.listenerCallback
			m.mu.Unlock()
			if cb == nil {
				continue
			}
			// 回调内部保证不 panic（Python 中 try/except 包裹）
			func() {
				defer func() {
					if r := recover(); r != nil {
						logger.I18nError("处理会话 %s 消息时发生错误: %v", sessionID, r)
					}
				}()
				cb(item)
			}()
		case <-closeEvt:
			return
		case <-stop:
			return
		}
	}
}

// GetStats 获取队列统计信息（对应 get_stats）。
func (m *WecomAIQueueMgr) GetStats() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return map[string]int{
		"input_queues":      len(m.queues),
		"output_queues":     len(m.backQueues),
		"pending_responses": len(m.pendingResponses),
	}
}
