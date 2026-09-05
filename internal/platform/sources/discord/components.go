package discord

import (
	"strconv"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Discord Json 组件通道：插件发送 message.Json(type="discord_embed"/"discord_view")
// 组件，这里解析为 discordgo 的 MessageEmbed / Components。
// 对齐本体 components.py（DiscordEmbed/DiscordButton/DiscordView）与
// discord_platform_event.py:255-264 的解析语义。View 交互接收本体也未启用，
// 仅做渲染发送面。

// jsonStr 取 Json.data 中 string 字段（缺失/类型不符返回空串）。
func jsonStr(data map[string]interface{}, key string) string {
	v, _ := data[key].(string)
	return v
}

// jsonBool 取 Json.data 中 bool 字段。
func jsonBool(data map[string]interface{}, key string) bool {
	v, _ := data[key].(bool)
	return v
}

// jsonColor 取 Json.data 中 color 字段（容忍 float64/int）。
func jsonColor(data map[string]interface{}, key string) int {
	switch v := data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// parseDiscordEmbed 将 Json(type="discord_embed") 的 data 转为 discordgo.MessageEmbed。
// 字段语义对齐本体 DiscordEmbed.to_discord_embed：title/description/color/url/
// thumbnail/image/footer/fields[{name,value,inline}]。thumbnail/image/footer 均为 URL。
func parseDiscordEmbed(data map[string]interface{}) *discordgo.MessageEmbed {
	embed := &discordgo.MessageEmbed{
		Title:       jsonStr(data, "title"),
		Description: jsonStr(data, "description"),
		Color:       jsonColor(data, "color"),
		URL:         jsonStr(data, "url"),
	}
	if v := jsonStr(data, "thumbnail"); v != "" {
		embed.Thumbnail = &discordgo.MessageEmbedThumbnail{URL: v}
	}
	if v := jsonStr(data, "image"); v != "" {
		embed.Image = &discordgo.MessageEmbedImage{URL: v}
	}
	if v := jsonStr(data, "footer"); v != "" {
		embed.Footer = &discordgo.MessageEmbedFooter{Text: v}
	}
	if fields, ok := data["fields"].([]interface{}); ok {
		for _, f := range fields {
			fm, ok := f.(map[string]interface{})
			if !ok {
				continue
			}
			embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
				Name:   jsonStr(fm, "name"),
				Value:  jsonStr(fm, "value"),
				Inline: jsonBool(fm, "inline"),
			})
		}
	}
	return embed
}

// buttonStyleByName 将本体 DiscordButton.style 字符串映射为 discordgo ButtonStyle
// （getattr(discord.ButtonStyle, style, primary) 的等价实现）。
func buttonStyleByName(style string) discordgo.ButtonStyle {
	switch strings.ToLower(style) {
	case "secondary", "grey", "gray":
		return discordgo.SecondaryButton
	case "success", "green":
		return discordgo.SuccessButton
	case "danger", "red":
		return discordgo.DangerButton
	case "link":
		return discordgo.LinkButton
	default: // primary / blurple / 未知样式回退 primary
		return discordgo.PrimaryButton
	}
}

// parseDiscordButton 将单个按钮 dict 转为 discordgo.Button。
// 字段语义对齐本体 DiscordButton：label/custom_id/style/emoji/url/disabled。
func parseDiscordButton(fm map[string]interface{}) *discordgo.Button {
	btn := &discordgo.Button{
		Label:    jsonStr(fm, "label"),
		CustomID: jsonStr(fm, "custom_id"),
		URL:      jsonStr(fm, "url"),
		Disabled: jsonBool(fm, "disabled"),
		Style:    buttonStyleByName(jsonStr(fm, "style")),
	}
	if emoji := jsonStr(fm, "emoji"); emoji != "" {
		btn.Emoji = discordgo.ComponentEmoji{Name: emoji}
	}
	// URL 按钮强制 link 样式（本体：component.url 存在时 style=ButtonStyle.link）。
	if btn.URL != "" {
		btn.Style = discordgo.LinkButton
	}
	return btn
}

// parseDiscordView 将 Json(type="discord_view") 的 data 转为 discordgo 组件行。
// 本体 DiscordView.components 为 DiscordButton 列表，全部放入一个 ActionsRow。
func parseDiscordView(data map[string]interface{}) []discordgo.MessageComponent {
	raw, ok := data["components"].([]interface{})
	if !ok {
		return nil
	}
	row := &discordgo.ActionsRow{}
	for _, item := range raw {
		fm, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		row.Components = append(row.Components, parseDiscordButton(fm))
	}
	if len(row.Components) == 0 {
		return nil
	}
	return []discordgo.MessageComponent{row}
}

// parseDiscordJsonComponent 解析 message.Json 组件：
// 返回 (embed, viewComponents, 是否识别)。未识别的 type 返回 false 由调用方忽略。
func parseDiscordJsonComponent(data map[string]interface{}) (*discordgo.MessageEmbed, []discordgo.MessageComponent, bool) {
	switch jsonStr(data, "type") {
	case "discord_embed":
		return parseDiscordEmbed(data), nil, true
	case "discord_view":
		return nil, parseDiscordView(data), true
	}
	return nil, nil, false
}
