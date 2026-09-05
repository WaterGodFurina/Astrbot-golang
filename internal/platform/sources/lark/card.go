package lark

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcardkit "github.com/larksuite/oapi-sdk-go/v3/service/cardkit/v1"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// 飞书卡片能力：对齐本体 lark_event.py
//   - lark_collapsible_panel_reasoning 交互卡片/折叠面板（:264-412, 507-533）
//   - CardKit 流式卡片（:761-1077，创建→增量更新→关闭）

const reasoningPanelType = "lark_collapsible_panel_reasoning"

// jsonStrSafe 从 map 取 string 字段（缺失/类型不符返回空串）。
func jsonStrSafe(data map[string]interface{}, key string) string {
	v, _ := data[key].(string)
	return v
}

// buildCollapsiblePanelElement 构造折叠面板组件
// （对齐本体 _build_collapsible_panel_element）。
func buildCollapsiblePanelElement(reasoningContent, title string, expanded bool) map[string]interface{} {
	return map[string]interface{}{
		"tag":              "collapsible_panel",
		"expanded":         expanded,
		"background_color": "grey",
		"padding":          "8px 8px 8px 8px",
		"margin":           "4px 0px 4px 0px",
		"border": map[string]interface{}{
			"color":         "grey",
			"corner_radius": "6px",
		},
		"header": map[string]interface{}{
			"title": map[string]interface{}{
				"tag":     "plain_text",
				"content": title,
			},
			"background_color": "grey",
		},
		"elements": []interface{}{
			map[string]interface{}{
				"tag":     "markdown",
				"content": reasoningContent,
			},
		},
	}
}

// buildReasoningCollapsiblePanel 构造整个 reasoning 卡片 JSON
// （对齐本体 _build_reasoning_collapsible_panel）。
func buildReasoningCollapsiblePanel(reasoningContent, title string) map[string]interface{} {
	return map[string]interface{}{
		"schema": "2.0",
		"body": map[string]interface{}{
			"elements": []interface{}{
				buildCollapsiblePanelElement(reasoningContent, title, false),
			},
		},
	}
}

// buildReasoningCard 将消息链（Json 标记 + Plain）构造成混合卡片 JSON
// （对齐本体 _build_reasoning_card）：非 Plain 非 reasoning Json 的组件返回 nil。
func buildReasoningCard(chain []message.Component) map[string]interface{} {
	elements := []interface{}{}
	for _, comp := range chain {
		switch c := comp.(type) {
		case *message.Json:
			if c.Data == nil || jsonStrSafe(c.Data, "type") != reasoningPanelType {
				continue
			}
			content := strings.TrimSpace(jsonStrSafe(c.Data, "content"))
			if content == "" {
				continue
			}
			title := jsonStrSafe(c.Data, "title")
			if title == "" {
				title = "💭 Thinking"
			}
			expanded := false
			if v, ok := c.Data["expanded"].(bool); ok {
				expanded = v
			}
			elements = append(elements, buildCollapsiblePanelElement(content, title, expanded))
		case *message.Plain:
			if c.Text != "" {
				elements = append(elements, map[string]interface{}{
					"tag":     "markdown",
					"content": c.Text,
				})
			}
		default:
			return nil
		}
	}
	if len(elements) == 0 {
		return nil
	}
	return map[string]interface{}{
		"schema": "2.0",
		"body": map[string]interface{}{
			"elements": elements,
		},
	}
}

// createCard 通过 cardkit 创建卡片实体并返回 card_id（对齐 _send_interactive_card
// 的创建部分；SDK 自带 cardkit 服务，直接使用）。
func createCard(ctx context.Context, client *lark.Client, cardJSON map[string]interface{}) (string, error) {
	data, err := json.Marshal(cardJSON)
	if err != nil {
		return "", err
	}
	req := larkcardkit.NewCreateCardReqBuilder().
		Body(larkcardkit.NewCreateCardReqBodyBuilder().
			Type("card_json").
			Data(string(data)).
			Build()).
		Build()
	resp, err := client.Cardkit.V1.Card.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() {
		return "", fmt.Errorf("lark create card failed(%d): %s", resp.Code, resp.Msg)
	}
	if resp.Data == nil || resp.Data.CardId == nil || *resp.Data.CardId == "" {
		return "", fmt.Errorf("lark create card 成功但未返回 card_id")
	}
	return *resp.Data.CardId, nil
}

// sendInteractiveCard 创建卡片实体并发送 interactive 消息
// （对齐本体 _send_interactive_card）。
func sendInteractiveCard(ctx context.Context, client *lark.Client, cardJSON map[string]interface{}, replyMessageID, receiveID, receiveIDType string) error {
	cardID, err := createCard(ctx, client, cardJSON)
	if err != nil {
		logger.Error("创建飞书卡片失败: %v", err)
		return err
	}
	content, _ := json.Marshal(map[string]interface{}{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	return sendImMessage(ctx, client, string(content), "interactive", replyMessageID, receiveID, receiveIDType)
}

// sendCollapsibleReasoningPanel 发送单个折叠面板卡片
// （对齐本体 _send_collapsible_reasoning_panel）。
func sendCollapsibleReasoningPanel(ctx context.Context, client *lark.Client, reasoningContent, title, replyMessageID, receiveID, receiveIDType string) error {
	if reasoningContent == "" {
		return nil
	}
	cardJSON := buildReasoningCollapsiblePanel(reasoningContent, title)
	return sendInteractiveCard(ctx, client, cardJSON, replyMessageID, receiveID, receiveIDType)
}

// createStreamingCard 创建开启流式更新模式的卡片实体，返回 card_id
// （对齐本体 _create_streaming_card 的卡面配置）。
func createStreamingCard(ctx context.Context, client *lark.Client) (string, error) {
	cardJSON := map[string]interface{}{
		"schema": "2.0",
		"header": map[string]interface{}{
			"title": map[string]interface{}{"content": "", "tag": "plain_text"},
		},
		"config": map[string]interface{}{
			"streaming_mode": true,
			"summary":        map[string]interface{}{"content": ""},
			"streaming_config": map[string]interface{}{
				"print_frequency_ms": map[string]interface{}{"default": 50},
				"print_step":         map[string]interface{}{"default": 2},
				"print_strategy":     "fast",
			},
		},
		"body": map[string]interface{}{
			"elements": []interface{}{
				map[string]interface{}{
					"tag":        "markdown",
					"content":    "",
					"element_id": "markdown_1",
				},
			},
		},
	}
	return createCard(ctx, client, cardJSON)
}

// updateStreamingText 调用 CardKit 流式更新文本接口，向 markdown_1 组件推送全量文本
// （对齐本体 _update_streaming_text）。
func updateStreamingText(ctx context.Context, client *lark.Client, cardID, content string, sequence int) error {
	req := larkcardkit.NewContentCardElementReqBuilder().
		CardId(cardID).
		ElementId("markdown_1").
		Body(larkcardkit.NewContentCardElementReqBodyBuilder().
			Content(content).
			Sequence(sequence).
			Uuid(uuidStr()).
			Build()).
		Build()
	resp, err := client.Cardkit.V1.CardElement.Content(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark streaming update failed(%d): %s", resp.Code, resp.Msg)
	}
	return nil
}

// closeStreamingMode 关闭卡片的流式更新模式（对齐本体 _close_streaming_mode）。
func closeStreamingMode(ctx context.Context, client *lark.Client, cardID string, sequence int) error {
	settings, _ := json.Marshal(map[string]interface{}{
		"config": map[string]interface{}{"streaming_mode": false},
	})
	req := larkcardkit.NewSettingsCardReqBuilder().
		CardId(cardID).
		Body(larkcardkit.NewSettingsCardReqBodyBuilder().
			Settings(string(settings)).
			Sequence(sequence).
			Uuid(uuidStr()).
			Build()).
		Build()
	resp, err := client.Cardkit.V1.Card.Settings(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark close streaming failed(%d): %s", resp.Code, resp.Msg)
	}
	return nil
}

// sendCardMessage 将卡片实体作为 interactive 消息发送（对齐本体 _send_card_message）。
func sendCardMessage(ctx context.Context, client *lark.Client, cardID, replyMessageID, receiveID, receiveIDType string) error {
	content, _ := json.Marshal(map[string]interface{}{
		"type": "card",
		"data": map[string]string{"card_id": cardID},
	})
	return sendImMessage(ctx, client, string(content), "interactive", replyMessageID, receiveID, receiveIDType)
}

// streamCard 会话级流式卡片状态：card_id + 递增 sequence。
// Adapter 实现 platform.StreamFragmenter（对齐本体 send_streaming 的
// 创建卡片 → 增量更新 → 关闭流式模式流程）。
type streamCard struct {
	cardID   string
	sequence int
}

// StreamStart 创建流式卡片实体并发送，返回卡片 ID（对齐 _create_streaming_card +
// _send_card_message）。text 为首个增量文本，立即推送给 markdown_1。
func (a *Adapter) StreamStart(sessionID, text string) (string, error) {
	if a.client == nil {
		return "", fmt.Errorf("lark: client not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cardID, err := createStreamingCard(ctx, a.client)
	if err != nil {
		return "", err
	}
	receiveIDType := "open_id"
	receiveID := sessionID
	if a.isGroupConv(sessionID) {
		receiveIDType = "chat_id"
		if idx := strings.Index(receiveID, "%"); idx >= 0 {
			receiveID = receiveID[idx+1:]
		}
	}
	// 流式卡片回复原消息（对齐本体 _send_card_message 的 reply_message_id）。
	if err := sendCardMessage(ctx, a.client, cardID, a.lookupReplyID(sessionID), receiveID, receiveIDType); err != nil {
		return "", err
	}
	sc := &streamCard{cardID: cardID, sequence: 1}
	if err := updateStreamingText(ctx, a.client, cardID, text, sc.sequence); err != nil {
		logger.Debug("飞书流式卡片首次推送失败 (ignored): %v", err)
	}
	a.mu.Lock()
	a.streamCards[sessionID] = sc
	a.mu.Unlock()
	logger.Info("飞书流式输出: 使用 CardKit 流式卡片 %s", cardID)
	return cardID, nil
}

// StreamUpdate 增量更新流式卡片（全量文本推送，sequence 严格递增）。
func (a *Adapter) StreamUpdate(sessionID, msgID, text string) error {
	if a.client == nil {
		return fmt.Errorf("lark: client not ready")
	}
	a.mu.Lock()
	sc, ok := a.streamCards[sessionID]
	if ok && sc.cardID == "" {
		sc.cardID = msgID
	}
	a.mu.Unlock()
	if !ok {
		sc = &streamCard{cardID: msgID, sequence: 0}
	}
	sc.sequence++
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := updateStreamingText(ctx, a.client, sc.cardID, text, sc.sequence); err != nil {
		sc.sequence--
		return err
	}
	return nil
}

// StreamEnd 补发最终文本并关闭卡片流式模式（对齐 _flush_and_close_card）。
func (a *Adapter) StreamEnd(sessionID, msgID, text string) error {
	if a.client == nil {
		return fmt.Errorf("lark: client not ready")
	}
	a.mu.Lock()
	sc, ok := a.streamCards[sessionID]
	if ok {
		delete(a.streamCards, sessionID)
	}
	a.mu.Unlock()
	if !ok {
		sc = &streamCard{cardID: msgID}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// 先补发最终文本，再关闭流式模式（sequence 持续递增）。
	if text != "" {
		sc.sequence++
		if err := updateStreamingText(ctx, a.client, sc.cardID, text, sc.sequence); err != nil {
			logger.Debug("飞书流式卡片补发最终文本失败 (ignored): %v", err)
		}
	}
	sc.sequence++
	if err := closeStreamingMode(ctx, a.client, sc.cardID, sc.sequence); err != nil {
		return err
	}
	logger.Debug("飞书流式模式已关闭: %s", sc.cardID)
	return nil
}
