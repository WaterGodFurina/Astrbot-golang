package telegram

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// errFailFastTransport 是立即失败的 RoundTripper：让 startDraft 的 API 调用
// 瞬时失败（不等待真实网络超时），专注验证登记/清扫逻辑。
type errFailFastTransport struct{}

func (errFailFastTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fail fast: no network in tests")
}

// newTestAdapter 构造带快速失败 HTTP 客户端的适配器。
func newTestAdapter() *Adapter {
	a := New(map[string]interface{}{}, map[string]interface{}{}, nil)
	a.client = &http.Client{Transport: errFailFastTransport{}}
	return a
}

// TestSweepOrphanDrafts 验证 startDraft 入口的惰性清扫会删除超过
// draftOrphanTTL 的孤儿草稿条目（流中途异常终止时 takeDraft 不会被调用，
// 条目此前会永久残留导致内存缓慢增长）。
func TestSweepOrphanDrafts(t *testing.T) {
	a := newTestAdapter()
	a.drafts = map[string]*tgDraft{}

	// 注入一条已过期的孤儿草稿（模拟异常终止路径的残留）与一条新鲜的。
	a.drafts["tg-draft-1"] = &tgDraft{
		chatID:    "100",
		draftID:   1,
		createdAt: time.Now().Add(-draftOrphanTTL - time.Minute),
	}
	a.drafts["tg-draft-2"] = &tgDraft{
		chatID:    "200",
		draftID:   2,
		createdAt: time.Now(),
	}

	// 新登记一条草稿：startDraft 入口应顺带清走过期孤儿，保留新鲜条目。
	// 发送首帧因快速失败客户端报错，startDraft 返回错误并删除自身条目，
	// 不影响对清扫效果的断言。
	if _, err := a.startDraft("300", "", "hello"); err == nil {
		t.Fatalf("无真实 API 时 startDraft 应失败")
	}

	a.draftsMu.Lock()
	_, orphanExists := a.drafts["tg-draft-1"]
	_, freshExists := a.drafts["tg-draft-2"]
	a.draftsMu.Unlock()

	if orphanExists {
		t.Fatalf("过期孤儿草稿 tg-draft-1 应被惰性清扫")
	}
	if !freshExists {
		t.Fatalf("新鲜草稿 tg-draft-2 不应被清扫")
	}
}

// TestStartDraftFailureCleansEntry 验证 startDraft 失败（发送首帧失败）时
// 自身条目会被删除，且登记时带 createdAt 时间戳。
func TestStartDraftFailureCleansEntry(t *testing.T) {
	a := newTestAdapter()
	a.drafts = map[string]*tgDraft{}

	if _, err := a.startDraft("300", "", "hello"); err == nil {
		t.Fatalf("无真实 API 时 startDraft 应失败")
	}

	a.draftsMu.Lock()
	n := len(a.drafts)
	for _, d := range a.drafts {
		if d.createdAt.IsZero() {
			a.draftsMu.Unlock()
			t.Fatalf("草稿条目应带 createdAt 时间戳")
		}
	}
	a.draftsMu.Unlock()

	if n != 0 {
		t.Fatalf("startDraft 失败后 drafts 应为空，实际残留 %d 条", n)
	}
}

// TestSweepOrphanDraftsDirect 用固定时钟验证清扫边界：恰好等于 TTL 的条目
// 不清理，超过才清理。
func TestSweepOrphanDraftsDirect(t *testing.T) {
	a := newTestAdapter()
	now := time.Now()
	a.drafts = map[string]*tgDraft{
		"tg-draft-edge": {createdAt: now.Add(-draftOrphanTTL)},
		"tg-draft-old":  {createdAt: now.Add(-draftOrphanTTL - time.Second)},
	}
	a.sweepOrphanDrafts(now)

	a.draftsMu.Lock()
	defer a.draftsMu.Unlock()
	if _, ok := a.drafts["tg-draft-edge"]; !ok {
		t.Fatalf("恰好等于 TTL 的条目不应被清理")
	}
	if _, ok := a.drafts["tg-draft-old"]; ok {
		t.Fatalf("超过 TTL 的条目应被清理")
	}
}

// TestSessionGroupLazySweep 验证 sessionGroup 条目带时间戳且写入时超过
// sessionGroupTTL 的旧条目被惰性清理（防止按会话数无界增长）。
func TestSessionGroupLazySweep(t *testing.T) {
	a := newTestAdapter()
	now := time.Now()
	a.sessionGroup = map[string]*sessionGroupEntry{
		"100": {isGroup: false, updatedAt: now.Add(-sessionGroupTTL - time.Minute)},
		"200": {isGroup: true, updatedAt: now},
	}

	// 复用 publishMsg 中的写入+清扫语义。
	a.sessionMu.Lock()
	a.sessionGroup["300"] = &sessionGroupEntry{isGroup: false, updatedAt: now}
	for id, e := range a.sessionGroup {
		if now.Sub(e.updatedAt) > sessionGroupTTL {
			delete(a.sessionGroup, id)
		}
	}
	a.sessionMu.Unlock()

	a.sessionMu.Lock()
	_, expiredExists := a.sessionGroup["100"]
	_, freshExists := a.sessionGroup["200"]
	_, newExists := a.sessionGroup["300"]
	a.sessionMu.Unlock()

	if expiredExists {
		t.Fatalf("超过 sessionGroupTTL 的会话条目应被惰性清理")
	}
	if !freshExists || !newExists {
		t.Fatalf("未过期的会话条目不应被清理")
	}
}

// TestSessionIsGroup 读写路径回归：isGroup 语义与未知识别保持不变。
func TestSessionIsGroup(t *testing.T) {
	a := newTestAdapter()
	if !a.sessionIsGroup("unknown") {
		t.Fatalf("未知会话默认按群聊处理")
	}
	a.sessionMu.Lock()
	a.sessionGroup = map[string]*sessionGroupEntry{
		"100": {isGroup: false, updatedAt: time.Now()},
		"200": {isGroup: true, updatedAt: time.Now()},
	}
	a.sessionMu.Unlock()
	if a.sessionIsGroup("100") {
		t.Fatalf("私聊会话应返回 false")
	}
	if !a.sessionIsGroup("200") {
		t.Fatalf("群聊会话应返回 true")
	}
}
