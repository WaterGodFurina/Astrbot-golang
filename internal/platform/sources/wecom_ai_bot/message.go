// 企业微信智能机器人消息发送逻辑。
// 1:1 移植自 wecomai_event.py 的 WecomAIBotMessageEvent：
//   - 消息链 → 输出队列（供 webhook 轮询聚合返回）；
//   - 长连接模式：通过 aibot_respond_msg 命令回复；
//   - 仅 webhook 推送模式：全部经消息推送 webhook 发送；
//   - webhook_client 配置时，不支持的组件（图片/文件等）先经 webhook 推送。
package wecom_ai_bot

import (
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// sendToBackQueue 将消息链写入输出队列（对应 WecomAIBotMessageEvent._send）。
// 返回最后处理的文本（At 或 Plain）。
func sendToBackQueue(chain *message.MessageChain, streamID string, queueMgr *WecomAIQueueMgr, streaming bool, suppressUnsupportedLog bool) string {
	backQueue := queueMgr.GetOrCreateBackQueue(streamID)
	if chain == nil {
		backQueue <- &QueueItem{Type: "end", Data: "", Streaming: false}
		return ""
	}

	data := ""
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.At:
			data = "@" + c.Name + " "
			backQueue <- &QueueItem{Type: "plain", Data: data, Streaming: streaming, SessionID: streamID}
		case *message.Plain:
			data = c.Text
			backQueue <- &QueueItem{Type: "plain", Data: data, Streaming: streaming, SessionID: streamID}
		case *message.Image:
			// 处理图片消息
			imageBase64, err := componentImageBase64(c)
			if err != nil {
				logger.I18nWarn("处理图片消息失败: %v", err)
				continue
			}
			if imageBase64 == "" {
				logger.I18nWarn("图片数据为空，跳过")
				continue
			}
			backQueue <- &QueueItem{Type: "image", ImageData: imageBase64, Streaming: streaming, SessionID: streamID}
		default:
			if !suppressUnsupportedLog {
				logger.I18nWarn("[WecomAI] 不支持的消息组件类型: %s, 跳过", comp.Type())
			}
		}
	}
	return data
}

// extractPlainTextFromChain 从消息链中提取纯文本（对应 _extract_plain_text_from_chain）。
// stripResult 为 true 时去除首尾空白（非流式发送），false 保留换行等格式（流式输出）。
func extractPlainTextFromChain(chain *message.MessageChain, stripResult bool) string {
	if chain == nil {
		return ""
	}
	var parts []string
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.At:
			parts = append(parts, "@"+c.Name+" ")
		case *message.Plain:
			parts = append(parts, c.Text)
		}
	}
	result := strings.Join(parts, "")
	if stripResult {
		return strings.TrimSpace(result)
	}
	return result
}

// markStreamComplete 标记流结束（对应 _mark_stream_complete）。
func markStreamComplete(streamID string, queueMgr *WecomAIQueueMgr) {
	backQueue := queueMgr.GetOrCreateBackQueue(streamID)
	backQueue <- &QueueItem{Type: "complete", Data: "", Streaming: false, SessionID: streamID}
}
