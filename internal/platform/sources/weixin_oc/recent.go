// 最近消息缓存与引用回复匹配。
// 对齐本体 weixin_oc_adapter.py:70-86, 1215-1345, 1347-1467：
//   - recent message 缓存：每会话 100 条 / 最多 500 会话 / TTL 1800s（均可配置）；
//   - 引用回复解析：ref_msg 内嵌内容直接匹配（direct-ref-msg）；
//     缺失时按 ref create_time_ms 在缓存中做 60s 时间窗就近匹配
//     （nearest-message-by-timestamp），回填被引用消息的完整内容。
package weixin_oc

import (
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
	ilink "github.com/dobest1024/go-weixin-ilink"
)

const (
	// defaultRecentMessageCacheSize 每会话最近消息缓存条数（本体 RECENT_MESSAGE_CACHE_SIZE）。
	defaultRecentMessageCacheSize = 100
	// defaultReplyMatchWindowMs 引用回复时间窗匹配窗口（本体 REPLY_MATCH_WINDOW_MS）。
	defaultReplyMatchWindowMs = 60_000
	// defaultRecentSessionCacheTTL 会话缓存 TTL（本体 RECENT_SESSION_CACHE_TTL_S）。
	defaultRecentSessionCacheTTL = 1800 * time.Second
	// defaultMaxRecentMessageSessions 最大缓存会话数（本体 MAX_RECENT_MESSAGE_SESSIONS）。
	defaultMaxRecentMessageSessions = 500
)

// recentMessage 缓存中的一条最近消息（本体 WeixinOCRecentMessage）。
type recentMessage struct {
	messageID   string
	senderID    string
	senderNick  string
	timestamp   int64 // 秒
	timestampMs int64
	components  []message.Component
	messageStr  string
	kind        string
}

// recentSessionCache 单个会话的消息缓存（本体 WeixinOCRecentSessionCache）。
type recentSessionCache struct {
	messages  []recentMessage
	updatedAt time.Time
}

// replyMatch 引用匹配结果（含匹配元信息，本体 WeixinOCReplyMeta.reply_to）。
type replyMatch struct {
	matched  *recentMessage
	strategy string
	distance int64
}

// inferMessageKindFromComponents 依据组件推断消息类型
// （本体 _infer_message_kind_from_components）。
func inferMessageKindFromComponents(components []message.Component) string {
	for _, comp := range components {
		switch comp.(type) {
		case *message.Plain:
			if plain, ok := comp.(*message.Plain); ok && strings.TrimSpace(plain.Text) != "" {
				return "text"
			}
		case *message.Image:
			return "image"
		case *message.Record:
			return "voice"
		case *message.File:
			return "file"
		case *message.Video:
			return "video"
		}
	}
	return "unknown"
}

// cacheRecentMessage 缓存一条最近消息（本体 _cache_recent_message）。
func (a *Adapter) cacheRecentMessage(sessionID string, msg recentMessage) {
	if sessionID == "" || msg.messageID == "" {
		return
	}
	if msg.kind == "" {
		msg.kind = inferMessageKindFromComponents(msg.components)
	}

	a.recentMu.Lock()
	defer a.recentMu.Unlock()
	a.pruneRecentSessionsLocked(time.Now())

	entry, ok := a.recentMessages[sessionID]
	if !ok {
		entry = &recentSessionCache{messages: make([]recentMessage, 0, a.recentCacheSize)}
		a.recentMessages[sessionID] = entry
	}
	entry.updatedAt = time.Now()
	entry.messages = append(entry.messages, msg)
	// 超出容量时丢弃最旧的消息（对齐 deque(maxlen=...)）。
	if len(entry.messages) > a.recentCacheSize {
		entry.messages = entry.messages[len(entry.messages)-a.recentCacheSize:]
	}
}

// pruneRecentSessionsLocked 清理过期/超量的会话缓存（本体 _prune_recent_message_caches）。
// 调用方须持有 a.recentMu。
func (a *Adapter) pruneRecentSessionsLocked(now time.Time) {
	if len(a.recentMessages) == 0 {
		return
	}
	for sessionID, entry := range a.recentMessages {
		if now.Sub(entry.updatedAt) > a.recentSessionTTL {
			delete(a.recentMessages, sessionID)
		}
	}
	// 超量时按最近更新时间淘汰最旧会话。
	overflow := len(a.recentMessages) - a.maxRecentSessions
	if overflow <= 0 {
		return
	}
	type sessionAge struct {
		id string
		at time.Time
	}
	ages := make([]sessionAge, 0, len(a.recentMessages))
	for id, entry := range a.recentMessages {
		ages = append(ages, sessionAge{id: id, at: entry.updatedAt})
	}
	for i := 0; i < overflow; i++ {
		oldest := -1
		for j := range ages {
			if ages[j].id == "" {
				continue
			}
			if oldest == -1 || ages[j].at.Before(ages[oldest].at) {
				oldest = j
			}
		}
		if oldest == -1 {
			break
		}
		delete(a.recentMessages, ages[oldest].id)
		ages[oldest].id = ""
	}
}

// matchRecentReply 按 ref create_time_ms 在缓存中做时间窗就近匹配
// （本体 _match_recent_reply：60s 窗口内取距离最近的一条）。
func (a *Adapter) matchRecentReply(sessionID string, refCreateTimeMs int64) replyMatch {
	if sessionID == "" || refCreateTimeMs <= 0 {
		return replyMatch{}
	}
	a.recentMu.Lock()
	defer a.recentMu.Unlock()
	a.pruneRecentSessionsLocked(time.Now())

	entry, ok := a.recentMessages[sessionID]
	if !ok {
		return replyMatch{}
	}
	var best *recentMessage
	var bestDist int64
	for i := range entry.messages {
		candidate := &entry.messages[i]
		dist := candidate.timestampMs - refCreateTimeMs
		if dist < 0 {
			dist = -dist
		}
		if dist > a.replyMatchWindowMs {
			continue
		}
		if best == nil || dist < bestDist {
			best = candidate
			bestDist = dist
		}
	}
	if best == nil {
		return replyMatch{}
	}
	return replyMatch{matched: best, strategy: "nearest-message-by-timestamp", distance: bestDist}
}

// buildReplyFromRef 从引用消息构建 Reply 组件：
//  1. ref_msg.message_item 内嵌文本 → 直接匹配（direct-ref-msg，置信度 1.0）；
//  2. 无内嵌文本（或引用媒体无法解析出文本）→ 时间窗就近匹配回填缓存内容；
//  3. 均不可得 → 仅以 ref title 兜底或返回 nil。
func (a *Adapter) buildReplyFromRef(sessionID string, c *ilink.Context) *message.Reply {
	if c == nil || c.Message == nil {
		return nil
	}
	var ref *ilink.RefMessage
	for i := range c.Message.ItemList {
		if c.Message.ItemList[i].RefMsg != nil {
			ref = c.Message.ItemList[i].RefMsg
			break
		}
	}
	if ref == nil {
		return nil
	}
	return a.buildReplyFromRefMsg(sessionID, ref)
}

// buildReplyFromRefMsg 处理单条 RefMessage（供 buildReplyFromRef 与测试复用）。
func (a *Adapter) buildReplyFromRefMsg(sessionID string, ref *ilink.RefMessage) *message.Reply {
	if ref == nil {
		return nil
	}
	mi := ref.MessageItem

	quotedText := ""
	refTimeMs := int64(0)
	refMsgID := ""
	if mi != nil {
		if mi.TextItem != nil {
			quotedText = strings.TrimSpace(mi.TextItem.Text)
		}
		refTimeMs = mi.CreateTimeMs
		refMsgID = mi.MsgID
	}

	// 1) 内嵌文本直接匹配。
	if quotedText != "" {
		return &message.Reply{
			MessageID:  refMsgID,
			MessageStr: quotedText,
			Chain:      []message.Component{&message.Plain{Text: quotedText}},
			CreatedAt:  time.UnixMilli(refTimeMs),
		}
	}

	// 2) 时间窗就近匹配：回填被引用消息的完整组件与文本（含媒体）。
	if m := a.matchRecentReply(sessionID, refTimeMs); m.matched != nil {
		msg := m.matched
		return &message.Reply{
			MessageID:  msg.messageID,
			SenderID:   msg.senderID,
			SenderNick: msg.senderNick,
			MessageStr: msg.messageStr,
			Chain:      append([]message.Component{}, msg.components...),
			CreatedAt:  time.UnixMilli(msg.timestampMs),
		}
	}

	// 3) 兜底：引用 title 摘要 / 引用媒体直接下载。
	if mi != nil {
		if comps := a.resolveRefItemComponents(mi); len(comps) > 0 {
			text := quotedText
			if text == "" {
				text = ref.Title
			}
			return &message.Reply{
				MessageID:  refMsgID,
				MessageStr: text,
				Chain:      comps,
				CreatedAt:  time.UnixMilli(refTimeMs),
			}
		}
	}
	if ref.Title != "" {
		return &message.Reply{
			MessageStr: ref.Title,
			Chain:      []message.Component{&message.Plain{Text: ref.Title}},
		}
	}
	return nil
}

// itemKindString 将 item 类型映射为 kind 文本（本体 _item_type_to_kind）。
// 预留给消息 kind 展示/调试场景复用。
func itemKindString(t int) string {
	switch t {
	case 1:
		return "text"
	case 2:
		return "image"
	case 3:
		return "voice"
	case 4:
		return "file"
	case 5:
		return "video"
	default:
		return "unknown"
	}
}

// recentOutboundID 生成出站消息的缓存 id。
func recentOutboundID() string {
	return "oc-out-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// intConfig 读取整型配置（JSON 数字可能为 float64），非法时回退默认值并应用下限
// （本体 _get_int_config）。
func intConfig(config map[string]interface{}, key string, def, minimum int) int {
	value := def
	if v, ok := config[key]; ok {
		switch n := v.(type) {
		case float64:
			value = int(n)
		case int:
			value = n
		case int64:
			value = int(n)
		case string:
			if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				value = parsed
			}
		}
	}
	if value < minimum {
		value = minimum
	}
	return value
}
