// Package pipeline implements the message processing stages.
// Ported from astrbot/core/pipeline/
//
// UmoAutoNameRecorder 对齐 Python v4.28.0 (#9909)：唤醒成功时异步记录 UMO
// 可读名称（群名/用户名）到数据库，供 WebUI 展示。
// Python 实现参考 astrbot/core/pipeline/waking_check/umo_auto_name.py。
package pipeline

import (
	"container/list"
	"strings"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

const maxUmoAutoNameCacheSize = 10000

// UmoNameStore 解耦 pipeline 对 *db.Database 的直接依赖（对齐 Python 的
// db_helper 接口），便于测试替换。
type UmoNameStore interface {
	UpsertUmoAutoName(umo, creatorSenderID, autoName string) error
	// UpsertUmoSender 记录每个 UMO 下最新观测到的发送者名称（对齐 py v4.29.4
	// umo_auto_name → db.upsert_umo_sender）。
	UpsertUmoSender(umo, senderID, senderName string) error
}

// UmoAutoNameRecorder 对齐 Python umo_auto_name.UmoAutoNameRecorder：
// 有界缓存（LRU，容量 10000）+ pending 队列 + 后台 goroutine 顺序 flush +
// 失败且 cache 命中则弹 cache。
type UmoAutoNameRecorder struct {
	store    UmoNameStore
	configID string

	mu       sync.Mutex
	cache    *list.List               // UMO 有序列表，队尾为最近使用
	cacheMap map[string]*list.Element // umo -> *list.Element（存 *cacheEntry）
	pending  *list.List               // UMO 有序列表，队尾为最新
	pendMap  map[string]*list.Element // umo -> *list.Element（存 *pendEntry）

	// 发送者名称记录（对齐 py v4.29.4 _sender_cache/_sender_pending），
	// key = umo + "\x00" + senderID。
	senderCache    *list.List
	senderCacheMap map[string]*list.Element
	senderPending  *list.List
	senderPendMap  map[string]*list.Element

	writerCh  chan struct{}
	writerWG  sync.WaitGroup
	closeCh   chan struct{}
	closeOnce sync.Once
}

type cacheEntry struct {
	umo      string
	autoName string
}

type pendEntry struct {
	umo             string
	creatorSenderID string
	autoName        string
}

// NewUmoAutoNameRecorder 创建记录器并启动后台 writer goroutine。
func NewUmoAutoNameRecorder(store UmoNameStore, configID string) *UmoAutoNameRecorder {
	r := &UmoAutoNameRecorder{
		store:          store,
		configID:       configID,
		cache:          list.New(),
		cacheMap:       make(map[string]*list.Element),
		pending:        list.New(),
		pendMap:        make(map[string]*list.Element),
		writerCh:       make(chan struct{}, 1),
		closeCh:        make(chan struct{}),
		senderCache:    list.New(),
		senderCacheMap: make(map[string]*list.Element),
		senderPending:  list.New(),
		senderPendMap:  make(map[string]*list.Element),
	}
	if store != nil {
		r.writerWG.Add(1)
		go r.writerLoop()
	}
	return r
}

// Stop 关闭后台 writer goroutine，等待其退出。
func (r *UmoAutoNameRecorder) Stop() {
	r.closeOnce.Do(func() { close(r.closeCh) })
	r.writerWG.Wait()
}

// getEventAutoName 对齐 Python umo_alias.get_event_auto_name(event, fallback_to_id=False)：
// 优先 group_name（从 event.MessageObj.Group.GroupName），无 group 时用 sender_name
// （event.Source.SenderName），fallback_to_id=False 意味着拿不到名字就返回空。
func getEventAutoName(event *core.Event) string {
	if event.MessageObj != nil && event.MessageObj.Group != nil {
		if name := normalizeUmoName(event.MessageObj.Group.GroupName); name != "" {
			return name
		}
	}
	if name := normalizeUmoName(event.Source.SenderName); name != "" {
		return name
	}
	return ""
}

func normalizeUmoName(s string) string {
	n := []rune(s)
	if len(n) > 255 {
		n = n[:255]
	}
	return string(n)
}

// Schedule 对齐 Python UmoAutoNameRecorder.schedule：唤醒成功时调用，
// 将 UMO 可读名称入队，由后台 goroutine 顺序 flush 到数据库。
func (r *UmoAutoNameRecorder) Schedule(event *core.Event) {
	if r.store == nil {
		return
	}
	umo := event.UnifiedMsgOrigin()
	autoName := getEventAutoName(event)
	senderID := event.Source.SenderID
	senderName := normalizeUmoName(event.Source.SenderName)

	r.mu.Lock()
	defer r.mu.Unlock()

	// 会话名称（autoName 非空才记录）与发送者名称（senderID 非空才记录）
	// 分别入队；发送者变化即使在群名不变时也必须记录（对齐 py v4.29.4）。
	if autoName != "" {
		r.enqueueLocked(r.cache, r.cacheMap, r.pending, r.pendMap, umo, senderID, autoName)
	}
	if senderID != "" {
		r.enqueueLocked(r.senderCache, r.senderCacheMap, r.senderPending, r.senderPendMap,
			umo+"\x00"+senderID, senderID, senderName)
	}

	if r.pending.Len() == 0 && r.senderPending.Len() == 0 {
		return
	}

	// 唤醒后台 writer
	select {
	case r.writerCh <- struct{}{}:
	default:
	}
}

// enqueueLocked 对齐 py v4.29.4 schedule 的每身份入队逻辑：
//   - name 与 cache 相同（或 name 为空且 cache 非空）时仅移动 LRU；
//   - 否则写入 cache/pending，超限时淘汰队首（pending 淘汰时同步弹 cache）。
func (r *UmoAutoNameRecorder) enqueueLocked(cache *list.List, cacheMap map[string]*list.Element, pending *list.List, pendMap map[string]*list.Element, key, senderID, name string) {
	if elem, ok := cacheMap[key]; ok {
		entry := elem.Value.(*cacheEntry)
		if entry.autoName == name || (name == "" && entry.autoName != "") {
			cache.MoveToBack(elem)
			return
		}
		entry.autoName = name
		cache.MoveToBack(elem)
	} else {
		elem := cache.PushBack(&cacheEntry{umo: key, autoName: name})
		cacheMap[key] = elem
		if cache.Len() > maxUmoAutoNameCacheSize {
			if front := cache.Front(); front != nil {
				cache.Remove(front)
				delete(cacheMap, front.Value.(*cacheEntry).umo)
			}
		}
	}

	if elem, ok := pendMap[key]; ok {
		entry := elem.Value.(*pendEntry)
		entry.creatorSenderID = senderID
		entry.autoName = name
		pending.MoveToBack(elem)
	} else {
		elem := pending.PushBack(&pendEntry{umo: key, creatorSenderID: senderID, autoName: name})
		pendMap[key] = elem
		if pending.Len() > maxUmoAutoNameCacheSize {
			if front := pending.Front(); front != nil {
				entry := front.Value.(*pendEntry)
				pending.Remove(front)
				delete(pendMap, entry.umo)
				// 对齐 Python：pending 溢出时，若 cache 命中则弹 cache。
				if ce, ok := cacheMap[entry.umo]; ok && ce.Value.(*cacheEntry).autoName == entry.autoName {
					cache.Remove(ce)
					delete(cacheMap, entry.umo)
				}
			}
		}
	}
}

// writerLoop 顺序 flush pending 队列到数据库（对齐 Python _flush）。
func (r *UmoAutoNameRecorder) writerLoop() {
	defer r.writerWG.Done()
	for {
		select {
		case <-r.closeCh:
			r.flushPending()
			return
		case <-r.writerCh:
			r.flushPending()
		}
	}
}

func (r *UmoAutoNameRecorder) flushPending() {
	for {
		var key, senderID, name string
		isSender := false
		r.mu.Lock()
		var front *list.Element
		// 会话队列优先，其次发送者队列（对齐 py is_sender = not self._pending）。
		if r.pending.Len() > 0 {
			front = r.pending.Front()
		} else if r.senderPending.Len() > 0 {
			front = r.senderPending.Front()
			isSender = true
		}
		if front == nil {
			r.mu.Unlock()
			return
		}
		entry := front.Value.(*pendEntry)
		key = entry.umo
		senderID = entry.creatorSenderID
		name = entry.autoName
		if isSender {
			r.senderPending.Remove(front)
			delete(r.senderPendMap, key)
		} else {
			r.pending.Remove(front)
			delete(r.pendMap, key)
		}
		r.mu.Unlock()

		var err error
		if isSender {
			umo := key
			if idx := strings.IndexByte(key, 0); idx >= 0 {
				umo = key[:idx]
			}
			err = r.store.UpsertUmoSender(umo, senderID, name)
		} else {
			err = r.store.UpsertUmoAutoName(key, senderID, name)
		}
		if err != nil {
			logger.Warn("UMO 名称持久化失败 key=%s: %v", key, err)
			// 对齐 Python：失败且 cache 命中则弹 cache。
			r.mu.Lock()
			if isSender {
				if ce, ok := r.senderCacheMap[key]; ok && ce.Value.(*cacheEntry).autoName == name {
					r.senderCache.Remove(ce)
					delete(r.senderCacheMap, key)
				}
			} else if ce, ok := r.cacheMap[key]; ok && ce.Value.(*cacheEntry).autoName == name {
				r.cache.Remove(ce)
				delete(r.cacheMap, key)
			}
			r.mu.Unlock()
		}
	}
}
