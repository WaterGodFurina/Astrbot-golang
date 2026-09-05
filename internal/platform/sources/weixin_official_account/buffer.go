// 被动回复 5 秒窗口的用户缓冲与消息排重。
// 对齐本体 weixin_offacc_adapter.py:72-74, 164-298, 355-386：
//   - 微信服务器要求 5 秒内回复，本体预留 4.0s 窗口等待管线结果；
//   - 超时返回【正在思考…】占位符，完整回复进入 user_buffer（cached_xml），
//     用户回复任意文字（或微信同 msg_id 重试）时逐条弹出；
//   - wexin_event_workers 以 msg_id 排重（微信失败会重推 3 次），
//     180s 内同 msg_id 复用同一 future 等待结果。
package weixin_official_account

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	wxmp "github.com/blusewang/wx/mp_api"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

const (
	// wxMsgTimeOut 被动回复等待窗口。微信要求 5 秒内响应，预留 1 秒网络开销（本体 _wx_msg_time_out = 4.0）。
	wxMsgTimeOut = 4 * time.Second
	// workerWaitTTL 排重 worker 的最长等待时间（本体 asyncio.wait_for(shield(future), 180)）。
	workerWaitTTL = 180 * time.Second
	// plainMaxLength 被动文本回复单条最大长度（本体 split_plain 默认 1024）。
	plainMaxLength = 1024
	// cachedMoreSuffix 弹出缓存后仍有剩余时附加的提示（本体同款文案）。
	cachedMoreSuffix = "\n【后续消息还在缓冲中，回复任意文字继续获取】"
	// workerTimeoutReply 排重等待超时的兜底回复（本体 callback 超时文案）。
	workerTimeoutReply = "处理消息超时，请稍后再试。"
)

// userState 单个用户（from_user → state）的被动回复缓冲状态。
// 对应本体 handle_callback 中 self.user_buffer[from_user] 的 dict：
// msg_id / preview / task / cached_xml / started_at。
type userState struct {
	fromUser  string // 用户 openid（回复的 ToUserName）
	toUser    string // 公众号原始 ID（回复的 FromUserName）
	msgID     string // 触发本状态的消息 id（微信重试时用于识别）
	preview   string // 消息预览文本（占位符展示）
	startedAt time.Time

	mu        sync.Mutex
	active    bool     // 管线是否仍在处理（对应本体 task 未完成）
	futureXML string   // 图片/语音被动回复 XML（对应本体 future.set_result(xml)）
	pending   string   // 流式/分段发送期间暂存的文本（管线结束后统一分段入缓存）
	cached    []string // 被动文本回复分段缓存（对应本体 cached_xml 列表）
	doneCh    chan struct{}
	doneOnce  sync.Once
}

func newUserState(msg *wxmp.MessageData, msgID, preview string) *userState {
	return &userState{
		fromUser:  msg.FromUserName,
		toUser:    msg.ToUserName,
		msgID:     msgID,
		preview:   preview,
		startedAt: time.Now(),
		active:    true,
		doneCh:    make(chan struct{}),
	}
}

// isActive 报告管线是否仍在处理中。
func (st *userState) isActive() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.active
}

// appendPending 追加待发送文本（流式/分段 Send 的内容先暂存，
// 管线结束后由 finish 统一分段入缓存，对齐本体 send_streaming 的整段缓冲语义）。
func (st *userState) appendPending(text string) {
	st.mu.Lock()
	st.pending += text
	st.mu.Unlock()
}

// setFutureXML 记录图片/语音被动回复 XML（对应本体 future.set_result(xml)）。
func (st *userState) setFutureXML(xml string) {
	st.mu.Lock()
	st.futureXML = xml
	st.mu.Unlock()
}

// finish 标记管线处理结束：刷入暂存文本并关闭 done 信号。
func (st *userState) finish() {
	st.mu.Lock()
	st.active = false
	st.flushPendingLocked()
	st.mu.Unlock()
	st.doneOnce.Do(func() { close(st.doneCh) })
}

// flushPendingLocked 将暂存文本按 1024 分段放入缓存。调用方须持有 st.mu。
func (st *userState) flushPendingLocked() {
	if st.pending == "" {
		return
	}
	st.cached = append(st.cached, splitPlain(st.pending, plainMaxLength)...)
	st.pending = ""
}

// waitDone 在 timeout 内等待管线结束信号。
func (st *userState) waitDone(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-st.doneCh:
	case <-timer.C:
	}
}

// takeReply 弹出一条待回复内容：优先图片/语音被动 XML，其次文本分段缓存。
// 返回 (回复内容, 是否有内容, 弹出后状态是否已耗尽)。
// 对应本体 handle_callback 中 cached_xml 的逐条弹出逻辑。
func (st *userState) takeReply() (string, bool, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.futureXML != "" {
		xml := st.futureXML
		st.futureXML = ""
		return xml, true, st.exhaustedLocked()
	}
	if len(st.cached) > 0 {
		text := st.cached[0]
		st.cached = st.cached[1:]
		// 弹出后仍有剩余时附加提示（本体：cached_xml 还有内容时拼接提示）
		if len(st.cached) > 0 {
			text += cachedMoreSuffix
		}
		return text, true, st.exhaustedLocked()
	}
	return "", false, st.exhaustedLocked()
}

// exhaustedLocked 报告状态是否已耗尽（管线结束且无任何待回复内容）。
// 耗尽后可从 userBuffer 中移除（对应本体 pop user_buffer）。
func (st *userState) exhaustedLocked() bool {
	return !st.active && st.futureXML == "" && st.pending == "" && len(st.cached) == 0
}

// msgWorker 一条消息的处理worker：用于同 msg_id 排重（对应本体 wexin_event_workers）。
type msgWorker struct {
	st   *userState
	done chan struct{}
}

// splitPlain 将长文本按标点切分为不超过 maxLen 字符（按 Unicode 字符数，对齐
// 本体 weixin_offacc_event.py:37-81 split_plain）。
func splitPlain(plain string, maxLen int) []string {
	runes := []rune(plain)
	if len(runes) <= maxLen {
		return []string{plain}
	}
	result := make([]string, 0, len(runes)/maxLen+1)
	start := 0
	for start < len(runes) {
		// 剩余长度不足 maxLen 时直接收尾
		if start+maxLen >= len(runes) {
			result = append(result, string(runes[start:]))
			break
		}
		end := start + maxLen
		cut := end
		// 从末尾向前搜索分割标点符号
		for i := end; i > start; i-- {
			if i-1 < len(runes) && strings.ContainsRune("。！？.!?\n;；", runes[i-1]) {
				cut = i
				break
			}
		}
		result = append(result, string(runes[start:cut]))
		start = cut
	}
	return result
}

// messageKey 生成消息排重键。优先 MsgId；部分消息类型缺失时回退 MsgID/来源组合。
func messageKey(msg *wxmp.MessageData) string {
	if msg.MsgId > 0 {
		return strconv.FormatInt(msg.MsgId, 10)
	}
	if msg.MsgID > 0 {
		return "m" + strconv.FormatInt(msg.MsgID, 10)
	}
	return fmt.Sprintf("%s-%d", msg.FromUserName, msg.CreateTime)
}

// msgPreview 生成消息预览文本，供占位符使用（本体 _preview）。
func msgPreview(msg *wxmp.MessageData) string {
	switch msg.MsgType {
	case "text":
		t := strings.TrimSpace(msg.Content)
		r := []rune(t)
		if len(r) == 0 {
			return "空消息"
		}
		if len(r) > 24 {
			return string(r[:24]) + "..."
		}
		return t
	case "image":
		return "图片"
	case "voice":
		return "语音"
	default:
		return msg.MsgType
	}
}

// launchWorker 注册排重 worker 并监听管线完成信号：管线结束后刷入暂存文本、
// 关闭等待信号；worker 条目保留 workerWaitTTL（180s），期间同 msg_id 的微信重推
// 复用同一结果而不重复进入管线（对齐本体 wexin_event_workers 的 future 存活期）。
func (a *Adapter) launchWorker(msgID string, st *userState, pipelineDone *core.PipelineDone) {
	a.workersMu.Lock()
	if _, dup := a.workers[msgID]; dup {
		a.workersMu.Unlock()
		// 已有同 msg_id 的 worker（理论不可达：上游已排重），不重复启动。
		return
	}
	w := &msgWorker{st: st, done: make(chan struct{})}
	a.workers[msgID] = w
	a.workersMu.Unlock()

	go func() {
		timer := time.NewTimer(workerWaitTTL)
		defer timer.Stop()
		select {
		case <-pipelineDone.Done():
			st.finish()
		case <-timer.C:
			// 180s 兜底：管线迟迟未结束时放弃等待（本体 wait_for 超时）。
			st.finish()
		case <-a.stopCh:
			st.finish()
		}
		close(w.done)
		// 保留条目至 TTL 结束：期间重试命中排重表，不重复处理。
		select {
		case <-timer.C:
		case <-a.stopCh:
		}
		a.workersMu.Lock()
		delete(a.workers, msgID)
		a.workersMu.Unlock()
		// 状态已耗尽时顺手清理用户缓冲，避免条目滞留。
		if st.exhausted() {
			a.deleteUserState(st)
		}
	}()
}

// exhausted 报告状态是否已耗尽（带锁包装）。
func (st *userState) exhausted() bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.exhaustedLocked()
}

// getUserState 返回用户的被动回复缓冲状态。
func (a *Adapter) getUserState(sessionID string) *userState {
	a.userMu.Lock()
	defer a.userMu.Unlock()
	return a.userBuffer[sessionID]
}

// deleteUserState 从用户缓冲移除 state（仅当仍是当前注册的实例）。
func (a *Adapter) deleteUserState(st *userState) {
	a.userMu.Lock()
	if cur, ok := a.userBuffer[st.fromUser]; ok && cur == st {
		delete(a.userBuffer, st.fromUser)
	}
	a.userMu.Unlock()
}
