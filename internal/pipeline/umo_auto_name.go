// Package pipeline implements the message processing stages.
// Ported from astrbot/core/pipeline/
//
// UmoAutoNameRecorder 对齐 Python v4.28.0 (#9909)：唤醒成功时异步记录 UMO
// 可读名称（群名/用户名）到数据库，供 WebUI 展示。
// Python 实现参考 astrbot/core/pipeline/waking_check/umo_auto_name.py。
package pipeline

import (
	"container/list"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

const maxUmoAutoNameCacheSize = 10000

// UmoNameStore 解耦 pipeline 对 *db.Database 的直接依赖（对齐 Python 的
// db_helper 接口），便于测试替换。
type UmoNameStore interface {
	UpsertUmoAutoName(umo, creatorSenderID, autoName string) error
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
		store:    store,
		configID: configID,
		cache:    list.New(),
		cacheMap: make(map[string]*list.Element),
		pending:  list.New(),
		pendMap:  make(map[string]*list.Element),
		writerCh: make(chan struct{}, 1),
		closeCh:  make(chan struct{}),
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
	if autoName == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// 缓存命中且值未变：仅移动 LRU，不入 pending（对齐 Python 的去重语义）。
	if elem, ok := r.cacheMap[umo]; ok {
		entry := elem.Value.(*cacheEntry)
		if entry.autoName == autoName {
			r.cache.MoveToBack(elem)
			return
		}
		entry.autoName = autoName
		r.cache.MoveToBack(elem)
	} else {
		elem := r.cache.PushBack(&cacheEntry{umo: umo, autoName: autoName})
		r.cacheMap[umo] = elem
		if r.cache.Len() > maxUmoAutoNameCacheSize {
			front := r.cache.Front()
			if front != nil {
				r.cache.Remove(front)
				delete(r.cacheMap, front.Value.(*cacheEntry).umo)
			}
		}
	}

	// 入 pending 队列
	if elem, ok := r.pendMap[umo]; ok {
		entry := elem.Value.(*pendEntry)
		entry.creatorSenderID = event.Source.SenderID
		entry.autoName = autoName
		r.pending.MoveToBack(elem)
	} else {
		elem := r.pending.PushBack(&pendEntry{
			umo:             umo,
			creatorSenderID: event.Source.SenderID,
			autoName:        autoName,
		})
		r.pendMap[umo] = elem
		if r.pending.Len() > maxUmoAutoNameCacheSize {
			front := r.pending.Front()
			if front != nil {
				entry := front.Value.(*pendEntry)
				r.pending.Remove(front)
				delete(r.pendMap, entry.umo)
				// 对齐 Python：pending 溢出时，若 cache 命中则弹 cache。
				if ce, ok := r.cacheMap[entry.umo]; ok && ce.Value.(*cacheEntry).autoName == entry.autoName {
					r.cache.Remove(ce)
					delete(r.cacheMap, entry.umo)
				}
			}
		}
	}

	// 唤醒后台 writer
	select {
	case r.writerCh <- struct{}{}:
	default:
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
		var umo, creatorSenderID, autoName string
		r.mu.Lock()
		front := r.pending.Front()
		if front == nil {
			r.mu.Unlock()
			return
		}
		entry := front.Value.(*pendEntry)
		umo = entry.umo
		creatorSenderID = entry.creatorSenderID
		autoName = entry.autoName
		r.pending.Remove(front)
		delete(r.pendMap, umo)
		r.mu.Unlock()

		if err := r.store.UpsertUmoAutoName(umo, creatorSenderID, autoName); err != nil {
			logger.Warn("UMO 自动名称持久化失败 umo=%s: %v", umo, err)
			// 对齐 Python：失败且 cache 命中则弹 cache。
			r.mu.Lock()
			if ce, ok := r.cacheMap[umo]; ok && ce.Value.(*cacheEntry).autoName == autoName {
				r.cache.Remove(ce)
				delete(r.cacheMap, umo)
			}
			r.mu.Unlock()
		}
	}
}
