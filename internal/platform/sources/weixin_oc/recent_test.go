package weixin_oc

import (
	"testing"
	"time"

	ilink "github.com/dobest1024/go-weixin-ilink"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// TestRecentCacheWindowMatch: 时间窗就近匹配回填被引用消息。
func TestRecentCacheWindowMatch(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	now := time.Now()
	a.cacheRecentMessage("u1", recentMessage{
		messageID:   "m1",
		senderID:    "u1",
		senderNick:  "u1",
		timestamp:   now.Unix(),
		timestampMs: now.UnixMilli(),
		components:  []message.Component{&message.Plain{Text: "原始消息"}},
		messageStr:  "原始消息",
	})
	m := a.matchRecentReply("u1", now.UnixMilli()+2000)
	if m.matched == nil || m.matched.messageID != "m1" || m.strategy != "nearest-message-by-timestamp" {
		t.Fatalf("时间窗匹配失败: %+v", m)
	}
	// 超出窗口不应命中。
	if m := a.matchRecentReply("u1", now.UnixMilli()+120_000); m.matched != nil {
		t.Fatalf("超出 60s 窗口不应命中: %+v", m)
	}
	// 其他会话不应命中。
	if m := a.matchRecentReply("u2", now.UnixMilli()); m.matched != nil {
		t.Fatalf("其他会话不应命中: %+v", m)
	}
}

// TestBuildReplyDirectRef: 内嵌文本走 direct-ref-msg 策略。
func TestBuildReplyDirectRef(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	ref := &ilink.RefMessage{
		MessageItem: &ilink.MessageItem{
			Type:     ilink.ItemTypeText,
			MsgID:    "ref-1",
			TextItem: &ilink.TextItem{Text: "引用原文"},
		},
		Title: "引用原文",
	}
	reply := a.buildReplyFromRefMsg("u1", ref)
	if reply == nil || reply.MessageStr != "引用原文" || reply.MessageID != "ref-1" {
		t.Fatalf("直接引用匹配失败: %+v", reply)
	}
}

// TestBuildReplyFallsBackToWindowMatch: 无内嵌文本时回退时间窗匹配。
func TestBuildReplyFallsBackToWindowMatch(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	now := time.Now()
	a.cacheRecentMessage("u1", recentMessage{
		messageID:   "m-orig",
		senderID:    "u1",
		timestampMs: now.UnixMilli(),
		components:  []message.Component{&message.Plain{Text: "原始消息"}},
		messageStr:  "被引用的缓存消息",
	})
	// 引用为媒体（无文本），create_time_ms 指向缓存消息。
	ref := &ilink.RefMessage{
		MessageItem: &ilink.MessageItem{
			Type:         ilink.ItemTypeImage,
			CreateTimeMs: now.UnixMilli() + 1000,
		},
		Title: "图片",
	}
	reply := a.buildReplyFromRefMsg("u1", ref)
	if reply == nil || reply.MessageID != "m-orig" || reply.MessageStr != "被引用的缓存消息" {
		t.Fatalf("时间窗回填失败: %+v", reply)
	}
}

// TestBuildReplyTitleFallback: 无缓存命中时以 title 兜底。
func TestBuildReplyTitleFallback(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx"}, nil, nil)
	ref := &ilink.RefMessage{
		MessageItem: &ilink.MessageItem{
			Type:         ilink.ItemTypeImage,
			CreateTimeMs: time.Now().UnixMilli(),
		},
		Title: "一张图片",
	}
	reply := a.buildReplyFromRefMsg("u1", ref)
	if reply == nil || reply.MessageStr != "一张图片" {
		t.Fatalf("title 兜底失败: %+v", reply)
	}
}

// TestRecentSessionPrune: 会话缓存 TTL 与容量上限淘汰。
func TestRecentSessionPrune(t *testing.T) {
	a := New(map[string]interface{}{
		"id":                                    "wx",
		"weixin_oc_max_recent_message_sessions": float64(2),
		"weixin_oc_recent_session_cache_ttl_s":  float64(60),
	}, nil, nil)
	if a.maxRecentSessions != 2 {
		t.Fatalf("maxRecentSessions 配置未生效: %d", a.maxRecentSessions)
	}
	for _, sid := range []string{"s1", "s2", "s3"} {
		a.cacheRecentMessage(sid, recentMessage{messageID: "m", timestampMs: time.Now().UnixMilli()})
	}
	// 淘汰在下一次缓存访问时触发（对齐本体 _get_recent_message_cache 先 prune 的语义）。
	a.cacheRecentMessage("s3", recentMessage{messageID: "m2", timestampMs: time.Now().UnixMilli()})
	a.recentMu.Lock()
	n := len(a.recentMessages)
	a.recentMu.Unlock()
	if n > 2 {
		t.Fatalf("会话缓存应淘汰至 2，实际 %d", n)
	}
}
