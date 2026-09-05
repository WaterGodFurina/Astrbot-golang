// 发送面共用基础设施：从 qqofficial_message_event.py 的
// _post_send_one / _send_with_markdown_fallback 与 qqofficial_platform_adapter.py 的
// _send_by_session_common 抽取，供 WS（本包）与 Webhook 两个适配器模式复用。
package qqofficial

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand" // nosemgrep: go.lang.security.audit.crypto.math_random.math-random-used -- #nosec G404: 仅用于 msg_seq 消息序号（对齐 Python random.randint(1,10000)），非安全随机
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 发送场景（对齐 Python session scene）
const (
	SceneFriend  = "friend"  // C2C 私聊
	SceneGroup   = "group"   // QQ 群
	SceneChannel = "channel" // 频道子频道
)

// 关键错误文案（对齐 Python MARKDOWN_NOT_ALLOWED_ERROR / STREAM_MARKDOWN_NEWLINE_ERROR）
const (
	// markdownNotAllowedError QQ 服务端拒绝原生 markdown 时的错误文案
	markdownNotAllowedError = "不允许发送原生 markdown"
	// streamMarkdownNewlineError QQ 流式 markdown 分片换行校验失败文案
	streamMarkdownNewlineError = `流式消息md分片需要\n结束`
)

// MaxSendAttempts QQ API 瞬时错误最大尝试次数（对齐 Python _qqofficial_retry max_attempts=5）
const MaxSendAttempts = 5

// passiveMsgWindow 被动回复 msg_id 有效窗口。
// QQ 官方 msg_id 的被动回复时效约为 5 分钟；缓存超时后视为主动消息发送。
const passiveMsgWindow = 5 * time.Minute

// PostFunc 发起一次带鉴权的 POST 请求并返回解析后的 JSON 响应。
type PostFunc func(path string, payload map[string]interface{}) (map[string]interface{}, error)

// APIPoster 抽象 WS / Webhook 两个适配器共用的 API 能力，
// 供发送回退与分片上传复用（对齐 Python 两模式共用 QQOfficialMessageEvent 的发送辅助）。
type APIPoster interface {
	// PostJSON 发起带鉴权的 POST 请求（实现应已包含瞬时错误重试）。
	PostJSON(path string, payload map[string]interface{}) (map[string]interface{}, error)
	// HTTPClient 返回 HTTP 客户端（分片上传直连 COS presigned URL 时使用）。
	HTTPClient() *http.Client
}

// APIError 表示 QQ 开放平台返回的结构化错误（HTTP 状态码 + 业务错误码）。
// 对齐 botpy.errors 分类（ServerError=5xx、Forbidden=403、NotFound=404、
// MethodNotAllowed=405），业务 code 取自响应体的 code/biz_code 字段。
type APIError struct {
	Status  int    // HTTP 状态码
	Code    int    // QQ 业务错误码（无则为 0）
	Message string // 错误描述
}

func (e *APIError) Error() string {
	return fmt.Sprintf("QQ 接口错误 (%d): %s", e.Status, e.Message)
}

// ExtractBizCode 从响应体提取业务错误码（code 或 biz_code，均无时为 0）。
func ExtractBizCode(data map[string]interface{}) int {
	if data == nil {
		return 0
	}
	for _, key := range []string{"code", "biz_code"} {
		switch v := data[key].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case int64:
			return int(v)
		}
	}
	return 0
}

// apiErrOf 把 (响应, 错误) 归一为 *APIError：响应体带非 0 业务 code 时同样视为错误
// （对齐 Python：status >= 400 或 code not in (None, 0) 均为失败）。
func apiErrOf(res map[string]interface{}, err error) *APIError {
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			return apiErr
		}
		return &APIError{Message: err.Error()}
	}
	if code := ExtractBizCode(res); code != 0 {
		msg, _ := res["message"].(string)
		if msg == "" {
			msg, _ = res["msg"].(string)
		}
		return &APIError{Status: http.StatusOK, Code: code, Message: msg}
	}
	return nil
}

// isServerError 判断是否 5xx 服务端错误（对齐 botpy.errors.ServerError）。
func isServerError(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status >= 500
}

// isSendFallbackable 判断错误是否允许走发送回退链
// （对齐 Python _QQOFFICIAL_SEND_API_ERRORS：Forbidden/MethodNotAllowed/NotFound/ServerError）。
func isSendFallbackable(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Status {
	case http.StatusForbidden, http.StatusNotFound, http.StatusMethodNotAllowed:
		return true
	}
	return apiErr.Status >= 500
}

// isTransientError 判断是否瞬时错误：HTTP 5xx（含 500/504）、网络超时与传输层错误
// （对齐 Python retry 列表：ServerError / OSError / asyncio.TimeoutError）。
func isTransientError(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 500
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// 连接拒绝/重置等传输层错误（Python OSError）同样重试
		return true
	}
	return false
}

// RetryTransient 对瞬时错误做指数退避重试：
// HTTP 5xx（500/504 等）与超时/网络错误最多 attempts 次尝试，
// 等待序列 2s/4s/8s/16s 封顶 30s（对齐 tenacity
// wait_exponential(multiplier=2, min=2, max=30)）。非瞬时错误立即返回。
func RetryTransient(attempts int, fn func() (map[string]interface{}, error)) (map[string]interface{}, error) {
	if attempts <= 0 {
		attempts = MaxSendAttempts
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		res, err := fn()
		if err == nil {
			return res, nil
		}
		if !isTransientError(err) {
			return nil, err
		}
		lastErr = err
		if attempt < attempts-1 {
			wait := retryBackoff(attempt)
			logger.I18nWarn("[QQOfficial] QQ API 瞬时错误 (%d/%d): %v，%.0fs 后重试", attempt+1, attempts, err, wait.Seconds())
			time.Sleep(wait)
		}
	}
	return nil, lastErr
}

// retryBackoff 第 attempt 次重试前的等待时长（2s 起，倍增，封顶 30s）。
func retryBackoff(attempt int) time.Duration {
	wait := time.Duration(2<<attempt) * time.Second
	if wait < 2*time.Second {
		wait = 2 * time.Second
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	return wait
}

// MsgIDInfo 描述会话缓存的被动回复 msg_id 及其记录时间，
// 用于区分被动回复（msg_id 新鲜）与主动消息发送。
type MsgIDInfo struct {
	ID string
	At time.Time
}

// Fresh 判断 msg_id 是否仍在被动回复有效期内。
func (m MsgIDInfo) Fresh() bool {
	return m.ID != "" && !m.At.IsZero() && time.Since(m.At) <= passiveMsgWindow
}

// Proactive 判断本次发送是否属于主动消息（msg_id 缺失或已超时）。
func (m MsgIDInfo) Proactive() bool { return !m.Fresh() }

// BuildSendPayload 构造 QQ 消息发送载荷（对齐 Python _post_send_one 的 payload 分支）：
//   - 有媒体：msg_type=7 + media，移除 markdown，content 兜底
//   - useMarkdown：msg_type=2 + MarkdownPayload(content)（默认模式）
//   - 否则：msg_type=0 + content（对齐 use_markdown_ is False）
//
// scene 为 friend/group 时附加随机 msg_seq（频道 API 不使用 v2 msg_type/msg_seq）。
func BuildSendPayload(scene, plainText string, useMarkdown bool, msgID string, media map[string]interface{}) map[string]interface{} {
	payload := map[string]interface{}{}
	if media != nil {
		payload["media"] = media
		payload["msg_type"] = 7
		if plainText != "" {
			payload["content"] = plainText
		}
	} else if useMarkdown {
		payload["msg_type"] = 2
		if plainText != "" {
			payload["markdown"] = map[string]string{"content": plainText}
		}
	} else {
		payload["content"] = plainText
		payload["msg_type"] = 0
	}
	if msgID != "" {
		payload["msg_id"] = msgID
	}
	if scene == SceneFriend || scene == SceneGroup {
		payload["msg_seq"] = rand.Intn(10000) + 1 // #nosec G404 -- QQ 官方 API msg_seq 消息序号（对齐 Python random.randint(1,10000)），非安全随机
	}
	return payload
}

// clonePayload 浅拷贝载荷，回退重试时修改副本不影响原载荷。
func clonePayload(payload map[string]interface{}) map[string]interface{} {
	clone := make(map[string]interface{}, len(payload))
	for k, v := range payload {
		clone[k] = v
	}
	return clone
}

// ensureStreamNewline 为流式载荷的 markdown/content 内容补 \n 结尾
// （对齐 Python :537-553：流式 markdown 分片必须以 \n 结束）。
func ensureStreamNewline(payload map[string]interface{}) {
	if md, ok := payload["markdown"].(map[string]string); ok {
		if c := md["content"]; c != "" && !strings.HasSuffix(c, "\n") {
			md["content"] = c + "\n"
		}
	}
	if c, ok := payload["content"].(string); ok && c != "" && !strings.HasSuffix(c, "\n") {
		payload["content"] = c + "\n"
	}
}

// SendWithMarkdownFallback 执行发送并实现 Python _send_with_markdown_fallback 的回退链：
//  1. 首次发送失败且载荷带 msg_id → 去 msg_id 主动重发一次（:519-530）
//  2. 服务端错误且流式分片换行校验失败 → 修正 \n 后重试一次（:535-553）
//  3. markdown 被拒（"不允许发送原生 markdown"）→ 回退 content 模式重发（:555-574）
//
// stream 非 nil 时启用流式换行修正分支。返回首次成功请求的响应。
func SendWithMarkdownFallback(post PostFunc, path string, payload map[string]interface{}, plainText string, stream map[string]interface{}) (map[string]interface{}, error) {
	ret, err := post(path, payload)
	if err == nil {
		return ret, nil
	}
	if !isSendFallbackable(err) {
		return nil, err
	}
	// 1) 被动回复失败 → 去 msg_id 主动重发一次
	if _, hasMsgID := payload["msg_id"]; hasMsgID {
		logger.I18nInfo("[QQOfficial] 回复消息失败: %v，尝试使用主动发送接口。", err)
		retryPayload := clonePayload(payload)
		delete(retryPayload, "msg_id")
		ret2, err2 := post(path, retryPayload)
		if err2 == nil {
			logger.I18nInfo("[QQOfficial] 使用主动发送接口发送成功。")
			return ret2, nil
		}
		err = err2
		payload = retryPayload
	}
	if !isServerError(err) {
		return nil, err
	}
	// 2) 流式分片换行校验失败 → 修正后重试一次
	if stream != nil && strings.Contains(err.Error(), streamMarkdownNewlineError) {
		logger.I18nWarn("[QQOfficial] 流式 markdown 分片换行校验失败，已修正后重试一次。")
		retryPayload := clonePayload(payload)
		ensureStreamNewline(retryPayload)
		return post(path, retryPayload)
	}
	// 3) markdown 被拒且存在可回退文本 → content 模式重发
	if _, hasMD := payload["markdown"]; !hasMD || plainText == "" || !strings.Contains(err.Error(), markdownNotAllowedError) {
		return nil, err
	}
	logger.I18nWarn("[QQOfficial] markdown 发送被拒绝，回退到 content 模式重试。")
	retryPayload := clonePayload(payload)
	delete(retryPayload, "markdown")
	retryPayload["content"] = plainText
	if mt, ok := retryPayload["msg_type"].(int); ok && mt == 2 {
		retryPayload["msg_type"] = 0
	}
	if stream != nil {
		if c, _ := retryPayload["content"].(string); c != "" && !strings.HasSuffix(c, "\n") {
			retryPayload["content"] = c + "\n"
		}
	}
	return post(path, retryPayload)
}

// resolveRecvKey 返回场景对应的接收者字段名（对齐 Python openid / group_openid）。
func resolveRecvKey(scene string) string {
	if scene == SceneGroup {
		return "group_openid"
	}
	return "openid"
}

// mediaBasePath 返回场景对应的媒体 API 基路径。
func mediaBasePath(scene, targetID string) string {
	if scene == SceneGroup {
		return "/v2/groups/" + targetID
	}
	return "/v2/users/" + targetID
}

// mediaPath 返回场景对应的媒体上传 API 路径。
func mediaPath(scene, targetID string) string {
	return mediaBasePath(scene, targetID) + "/files"
}

// UploadMedia 上传媒体文件（对齐 Python upload_group_and_c2c_media + 分片上传分支）：
//   - URL：QQ 支持直接传 url，无需下载
//   - 本地文件超过 10MB：走 QQ 分片上传协议（upload_prepare/part PUT/upload_part_finish/merge）
//   - 其余：读取并 base64 直传（兼容调用方直接传 base64 的旧路径）
func UploadMedia(poster APIPoster, scene, targetID, fileData string, fileType int, fileName string) (map[string]interface{}, error) {
	data := fileData
	if strings.HasPrefix(data, "data:") {
		parts := strings.SplitN(data, ",", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("无效的 data: URI 文件数据")
		}
		data = parts[1]
	}
	if strings.HasPrefix(data, "http://") || strings.HasPrefix(data, "https://") {
		payload := map[string]interface{}{
			"file_type":           fileType,
			"srv_send_msg":        false,
			"url":                 data,
			resolveRecvKey(scene): targetID,
		}
		if fileName != "" {
			payload["file_name"] = fileName
		}
		return poster.PostJSON(mediaPath(scene, targetID), payload)
	}
	if fi, err := statRegular(data); err == nil {
		if fi.Size() > ChunkedUploadThreshold {
			// 清单 7：>10MB 本地文件走分片上传（对齐 QQOFFICIAL_CHUNKED_UPLOAD_THRESHOLD）
			if fileName == "" {
				fileName = baseName(data)
			}
			return NewChunkedUploader(poster).Upload(scene, targetID, data, fileType, fileName, false)
		}
		b64, err := fileToBase64(data)
		if err != nil {
			return nil, err
		}
		payload := map[string]interface{}{
			"file_type":           fileType,
			"srv_send_msg":        false,
			"file_data":           b64,
			resolveRecvKey(scene): targetID,
		}
		if fileName != "" {
			payload["file_name"] = fileName
		}
		return poster.PostJSON(mediaPath(scene, targetID), payload)
	}
	// 兼容调用方直接传 base64 的旧路径
	payload := map[string]interface{}{
		"file_type":           fileType,
		"srv_send_msg":        false,
		"file_data":           data,
		resolveRecvKey(scene): targetID,
	}
	if fileName != "" {
		payload["file_name"] = fileName
	}
	return poster.PostJSON(mediaPath(scene, targetID), payload)
}

// toJSONString 序列化（仅用于错误信息）。
func toJSONString(v interface{}) string {
	data, _ := json.Marshal(v)
	return string(data)
}
