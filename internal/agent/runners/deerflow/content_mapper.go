package deerflow

// 移植 Python deerflow_content_mapper.py。

import (
	"encoding/base64"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// isLikelyBase64Image 对齐 Python is_likely_base64_image。
func isLikelyBase64Image(value string) bool {
	if strings.Contains(value, " ") {
		return false
	}
	compact := strings.ReplaceAll(strings.ReplaceAll(value, "\n", ""), "\r", "")
	if compact == "" || len(compact) < 32 || len(compact)%4 != 0 {
		return false
	}
	if _, err := base64.StdEncoding.DecodeString(compact); err != nil {
		return false
	}
	return true
}

// ImageResolver 解析 URL/data/base64 为 Image 组件（对齐 image_component_from_url）。
type ImageResolver func(url interface{}) *message.Image

// ImageComponentFromURL 对齐 Python image_component_from_url。
func ImageComponentFromURL(url interface{}) *message.Image {
	s, ok := url.(string)
	if !ok {
		return nil
	}
	normalized := strings.TrimSpace(s)
	if normalized == "" {
		return nil
	}
	if strings.HasPrefix(normalized, "http://") || strings.HasPrefix(normalized, "https://") {
		return message.ImageFromURL(normalized)
	}
	if !strings.HasPrefix(normalized, "data:") {
		return nil
	}
	header, payload, found := strings.Cut(normalized, ",")
	if !found {
		return nil
	}
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return nil
	}
	compact := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(payload, "\n", ""), "\r", ""))
	if compact == "" {
		return nil
	}
	if _, err := base64.StdEncoding.DecodeString(compact); err != nil {
		return nil
	}
	return message.ImageFromBase64(compact)
}

// appendComponentsFromContent 对齐 Python append_components_from_content。
func appendComponentsFromContent(content interface{}, components *[]message.Component, resolver ImageResolver) {
	switch v := content.(type) {
	case string:
		if v != "" {
			*components = append(*components, &message.Plain{Text: v})
		}
		return
	case []interface{}:
		for _, item := range v {
			appendComponentsFromContent(item, components, resolver)
		}
		return
	}
	m, ok := content.(map[string]interface{})
	if !ok {
		return
	}
	itemType := strings.ToLower(interfaceToString(m["type"]))
	if itemType == "text" {
		if s, ok := m["text"].(string); ok && s != "" {
			*components = append(*components, &message.Plain{Text: s})
		}
		return
	}
	if itemType == "image_url" {
		imageURL := m["image_url"]
		if payload, ok := imageURL.(map[string]interface{}); ok {
			imageURL = payload["url"]
		}
		if comp := resolver(imageURL); comp != nil {
			*components = append(*components, comp)
		}
		return
	}
	if c, ok := m["content"]; ok {
		appendComponentsFromContent(c, components, resolver)
		return
	}
	if kw, ok := m["kwargs"].(map[string]interface{}); ok {
		if c, ok := kw["content"]; ok {
			appendComponentsFromContent(c, components, resolver)
		}
	}
}

// buildChainFromAIContent 对齐 Python build_chain_from_ai_content。
func buildChainFromAIContent(content interface{}, resolver ImageResolver) *message.MessageChain {
	components := []message.Component{}
	appendComponentsFromContent(content, &components, resolver)
	if len(components) > 0 {
		return message.NewMessageChain(components...)
	}
	if fb := extractText(content); fb != "" {
		return message.NewMessageChain(&message.Plain{Text: fb})
	}
	return message.NewMessageChain()
}

func interfaceToString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
