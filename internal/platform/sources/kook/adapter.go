package kook

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// 与 Python kook_adapter.py 一致的正则
var (
	// KOOK_AT_SELECTOR_REGEX = r"\((met|rol)\)([^()]+)\(\1\)"
	// Go 的 RE2 正则不支持反向引用, 因此用 findAtSelectors 状态机模拟, 见下方实现
	atMentionPrefixRegex = regexp.MustCompile(`^@\S+(\s*-\s*\S+)?\s*`)
)

// atSelector 对应 KOOK_AT_SELECTOR_REGEX 的一次匹配结果。
type atSelector struct {
	tag    string // "met" 或 "rol"
	target string // 括号中的目标
	start  int
	end    int
}

// findAtSelectors 手工解析 "(met)...(met)" 与 "(rol)...(rol)" 选择器。
// 由于 Go 正则 (RE2) 不支持反向引用 \( \1 \), 这里用状态机模拟 Python 的
// KOOK_AT_SELECTOR_REGEX: r"\((met|rol)\)([^()]+)\(\1\)"。
func findAtSelectors(content string) []atSelector {
	var result []atSelector
	i := 0
	for i < len(content) {
		if content[i] != '(' {
			i++
			continue
		}
		// 尝试匹配开启标签 "(met)" 或 "(rol)"
		tag := ""
		if i+5 <= len(content) {
			switch content[i : i+5] {
			case "(met)":
				tag = "met"
			case "(rol)":
				tag = "rol"
			}
		}
		if tag == "" {
			i++
			continue
		}
		// 目标内容不含括号, 因此向后找到第一个 '(' 即为目标结束位置
		targetStart := i + 5
		j := targetStart
		for j < len(content) && content[j] != '(' {
			j++
		}
		target := content[targetStart:j]
		// 需要匹配闭合标签 "(" + tag + ")", 且目标非空 ([^()]+ 要求至少一个字符)
		closeLen := len(tag) + 2
		if len(target) > 0 && j+closeLen <= len(content) && content[j:j+closeLen] == "("+tag+")" {
			result = append(result, atSelector{tag: tag, target: target, start: i, end: j + closeLen})
			i = j + closeLen
			continue
		}
		// 不匹配, 跳过开启标签继续向后扫描
		i += 5
	}
	return result
}

// Adapter 实现 KOOK 平台适配器。
// 对应 Python kook_adapter.py 的 KookPlatformAdapter。
type Adapter struct {
	config   map[string]interface{}
	settings map[string]interface{}

	EventBus *core.EventBus

	kookConfig *KookConfig
	client     *KookClient
	rolesCache *RolesRecord

	// 主循环控制
	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc
	stopCh  chan struct{}

	// 记录接收到的频道消息的 target_id (频道 id), 用于发送时区分频道消息/私聊消息
	knownChannelsMu sync.Mutex
	knownChannels   map[string]bool
}

// New 创建 KOOK 适配器 (对应 Python __init__)。
func New(config, settings map[string]interface{}, eventBus *core.EventBus) *Adapter {
	a := &Adapter{
		config:        config,
		settings:      settings,
		EventBus:      eventBus,
		stopCh:        make(chan struct{}),
		knownChannels: make(map[string]bool),
	}
	kc := &KookConfig{}
	kc.fromConfig(config)
	a.kookConfig = kc
	a.client = NewKookClient(kc, a.onReceived)
	a.rolesCache = NewRolesRecord(a.client.httpClient)
	a.rolesCache.SetToken(kc.Token)
	logger.Debug("[KOOK] 配置: id=%s enable=%v", kc.ID, kc.Enable)
	return a
}

// SetEventBus 注入事件总线 (实现 platform.EventBusSetter)。
func (a *Adapter) SetEventBus(bus platform.EventBus) {
	if eb, ok := bus.(*core.EventBus); ok {
		a.EventBus = eb
	}
}

// ID 返回适配器实例 id。
func (a *Adapter) ID() string { return a.kookConfig.ID }

// Type 返回平台类型名。
func (a *Adapter) Type() string { return "kook" }

// Start 启动适配器 (对应 Python run + _main_loop)。
func (a *Adapter) Start(ctx context.Context) error {
	a.running.Store(true)
	a.ctx, a.cancel = context.WithCancel(ctx)
	logger.I18nInfo("[KOOK] 启动KOOK适配器")
	go a.mainLoop()
	return nil
}

// Stop 停止适配器。
func (a *Adapter) Stop() error {
	a.running.Store(false)
	if a.cancel != nil {
		a.cancel()
	}
	if a.client != nil {
		a.client.Close()
	}
	logger.I18nInfo("[KOOK] 适配器已关闭")
	return nil
}

// mainLoop 主循环, 处理连接和重连 (对应 Python _main_loop)。
func (a *Adapter) mainLoop() {
	consecutiveFailures := 0
	maxConsecutiveFailures := a.kookConfig.MaxConsecutiveFailures
	maxRetryDelay := a.kookConfig.MaxRetryDelay

	for a.running.Load() {
		logger.I18nInfo("[KOOK] 尝试连接KOOK服务器...")
		// 获取机器人信息
		a.client.GetBotInfo(a.ctx)
		a.rolesCache.SetBotID(a.client.BotID())
		success := a.client.Connect(a.ctx)

		if success {
			logger.I18nInfo("[KOOK] 连接成功，开始监听消息")
			consecutiveFailures = 0 // 重置失败计数
			// Connect 会阻塞到连接结束
			if a.running.Load() {
				logger.I18nWarn("[KOOK] 连接断开，准备重连")
			}
			continue
		}
		if !a.running.Load() {
			return
		}
		consecutiveFailures++
		logger.I18nError("[KOOK] 连接失败，连续失败次数: %d", consecutiveFailures)
		if consecutiveFailures >= maxConsecutiveFailures {
			logger.I18nError("[KOOK] 连续失败次数过多，停止重连")
			return
		}
		// 指数退避 (饱和运算, 钳制移位指数, 防止溢出为 0/负数使延迟归零退化为热循环)
		exp := consecutiveFailures
		if exp > 30 {
			exp = 30
		}
		waitTime := int64(1) << exp
		if waitTime > int64(maxRetryDelay) {
			waitTime = int64(maxRetryDelay)
		}
		logger.I18nInfo("[KOOK] 等待 %d 秒后重试...", waitTime)
		select {
		case <-a.ctx.Done():
			return
		case <-time.After(time.Duration(waitTime) * time.Second):
		}
	}
}

// onReceived 处理收到的消息事件 (对应 Python _on_received)。
func (a *Adapter) onReceived(data *kookMessageEventData) {
	logger.Debug("[KOOK] 收到来自 %q 渠道的消息, 消息类型为: %s(%d)",
		string(data.ChannelType), kookMsgTypeName(data.Type), int(data.Type))
	switch data.Type {
	case KookMsgKMarkdown, KookMsgCard:
		// 忽略机器人自身的消息
		if data.AuthorID == a.client.BotID() {
			logger.Debug("[KOOK] 判断此消息为来自机器人自身的消息, 忽略此消息")
			return
		}
		abm := a.convertMessage(data)
		if abm != nil {
			a.handleMsg(abm)
		}
	case KookMsgSystem:
		// 系统消息: 角色更新通知时刷新角色id缓存
		extraType := rawTypeToString(data.Extra.Type)
		switch KookRoleExtraType(extraType) {
		case KookRoleAdded, KookRoleDeleted, KookRoleUpdated:
			// 此时 target_id 就是频道id(guild_id)
			guildID := data.TargetID
			logger.I18nInfo("[KOOK] 收到频道 %q 的角色更新通知, 类型为 %q, 刷新角色id缓存", guildID, extraType)
			if gid, err := strconv.ParseInt(guildID, 10, 64); err == nil {
				a.rolesCache.ClearGuildRolesCache(gid)
			}
		default:
			logger.Debug("[KOOK] 判断此消息为 %q 类型的系统通知, 因未实现此消息的处理流程而忽略此消息",
				extraType)
		}
	default:
		logger.Debug("[KOOK] 未处理的消息类型: %s(%d)", kookMsgTypeName(data.Type), int(data.Type))
	}
}

// rawTypeToString 将 json.RawMessage 形式的 extra.type 解析为字符串
// (系统消息时 type 为 str, 普通消息时 type 为 int)。
func rawTypeToString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// convertMessage 将 KOOK 消息事件转换为 AstrBotMessage (对应 Python convert_message)。
func (a *Adapter) convertMessage(data *kookMessageEventData) *platform.AstrBotMessage {
	abm := platform.NewAstrBotMessage()
	abm.RawMessage = data
	abm.SelfID = a.client.BotID()

	channelType := data.ChannelType
	authorID := data.AuthorID
	// channel_type 定义: https://developer.kookapp.cn/doc/event/event-introduction
	switch channelType {
	case KookChannelGroup:
		sessionID := data.TargetID
		if sessionID == "" {
			sessionID = "unknown"
		}
		abm.Type = platform.GroupMessage
		abm.Group = &platform.Group{GroupID: sessionID}
		abm.SessionID = sessionID
		a.rememberChannel(sessionID)
	case KookChannelPerson:
		abm.Type = platform.FriendMessage
		abm.SessionID = data.AuthorID
		if abm.SessionID == "" {
			abm.SessionID = "unknown"
		}
	case KookChannelBroadcast:
		sessionID := data.TargetID
		if sessionID == "" {
			sessionID = "unknown"
		}
		abm.Type = platform.OtherMessage
		abm.Group = &platform.Group{GroupID: sessionID}
		abm.SessionID = sessionID
		a.rememberChannel(sessionID)
	default:
		logger.I18nError("[KOOK] 不支持的频道类型: %q", string(channelType))
		return nil
	}

	nickname := "unknown"
	if data.Extra.Author != nil {
		nickname = data.Extra.Author.Username
	}
	abm.Sender = platform.MessageMember{
		UserID:   authorID,
		Nickname: nickname,
	}
	abm.MessageID = data.MsgID
	if abm.MessageID == "" {
		abm.MessageID = "unknown"
	}
	abm.Timestamp = data.MsgTimestamp

	switch data.Type {
	case KookMsgKMarkdown:
		comps, msgStr := a.parseKMarkdownMessage(data)
		abm.Message = comps
		abm.MessageStr = msgStr
	case KookMsgCard:
		comps, msgStr, err := a.parseCardMessage(data)
		if err != nil {
			logger.I18nError("[KOOK] 卡片消息解析失败: %v", err)
			logger.I18nError("[KOOK] 原始消息内容: %s", string(data.Content))
			abm.MessageStr = "[卡片消息解析失败]"
			abm.Message = []message.Component{&message.Plain{Text: "[卡片消息解析失败]"}}
		} else {
			abm.Message = comps
			abm.MessageStr = msgStr
		}
	default:
		logger.I18nWarn("[KOOK] 不支持的kook消息类型: %q", kookMsgTypeName(data.Type))
		abm.MessageStr = "[不支持的消息类型]"
		abm.Message = []message.Component{&message.Plain{Text: "[不支持的消息类型]"}}
	}
	return abm
}

// rememberChannel 记录频道消息的 target_id, 用于发送时区分频道/私聊。
func (a *Adapter) rememberChannel(targetID string) {
	if targetID == "" {
		return
	}
	a.knownChannelsMu.Lock()
	a.knownChannels[targetID] = true
	a.knownChannelsMu.Unlock()
}

// isKnownChannel 判断 target_id 是否为已接收过消息的频道 id。
func (a *Adapter) isKnownChannel(targetID string) bool {
	a.knownChannelsMu.Lock()
	defer a.knownChannelsMu.Unlock()
	return a.knownChannels[targetID]
}

// parseKMarkdownMessage 解析 kmarkdown 消息 (对应 Python _parse_kmarkdown_message)。
func (a *Adapter) parseKMarkdownMessage(data *kookMessageEventData) ([]message.Component, string) {
	var kmarkdown *kookKMarkdown
	if data.Extra.KMarkdown != nil {
		kmarkdown = data.Extra.KMarkdown
	}
	// 无法处理可能会收到的道具消息 content, 只能保留原样
	content := contentToString(data.Content)
	if kmarkdown == nil {
		logger.I18nError("[KOOK] 无法转换 %q 消息, 消息中找不到kmarkdown字段", kookMsgTypeName(KookMsgKMarkdown))
		return []message.Component{}, ""
	}

	rawContent := kmarkdown.RawContent
	if rawContent == "" {
		rawContent = content
	}

	mentionNameMap := make(map[string]string)
	for _, item := range kmarkdown.MentionPart {
		if item.ID == "" {
			continue
		}
		mentionNameMap[item.ID] = item.Username
	}

	return a.convertTextToComponents(content, rawContent, kmarkdown.MentionRolePart, data.Extra.GuildID, mentionNameMap)
}

// contentToString 将消息 content 转换为字符串 (道具消息的 dict 直接序列化)。
func contentToString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// 非字符串 (如道具消息的 dict), 序列化保留原样
	b, err := json.Marshal(raw)
	if err != nil {
		return ""
	}
	return string(b)
}

// kookCtx 返回适配器的运行上下文; 未启动时回退到 background。
func (a *Adapter) kookCtx() context.Context {
	if a.ctx != nil {
		return a.ctx
	}
	return context.Background()
}

// convertTextToComponents 将文本中的 @ 选择器转换为消息组件。
// 对应 Python _convert_text_message_to_component。
//
// KOOK 平台有角色(role)的概念, 他表示拥有某一类权限的许多用户, 且角色本身也有
// 自己的 id, 与正常用户 id 不同。而在频道中是可以 `@` 角色的, 而想要知道 bot
// 是否属于某个角色, 需要通过 /user/view 接口获取当前 bot 账号的某个频道下所属
// 角色的 id。在确定机器人需要响应某个 `(rol)xxx(rol)` 时, 需要将角色 id 替换成
// 当前的 bot id, 包装成 At 机器人自己, 而 At 的 name 就保留角色名称。如果没有
// 查询到角色 id 或者 bot 不属于某类角色, 则不处理此 `(rol)xxx(rol)`。
func (a *Adapter) convertTextToComponents(content, rawContent string, mentionRolePart []kookMarkdownMentionRolePart, guildID string, mentionNameMap map[string]string) ([]message.Component, string) {
	botID := a.client.BotID()
	botNickname := a.client.BotNickname()
	botUsername := a.client.BotUsername()
	if mentionNameMap == nil {
		mentionNameMap = map[string]string{}
	}

	var components []message.Component
	cursor := 0
	roleMentionCounter := -1

	for _, match := range findAtSelectors(content) {
		if match.start > cursor {
			plainText := strings.TrimSpace(content[cursor:match.start])
			if plainText != "" {
				components = append(components, &message.Plain{Text: plainText})
			}
		}

		mentionTarget := strings.TrimSpace(match.target)
		if match.tag == "met" && mentionTarget == "all" {
			components = append(components, &message.AtAll{})
		} else if match.tag == "rol" {
			roleMentionCounter++
			var roleID int64
			roleMentionName := mentionTarget
			if mentionRolePart != nil && len(mentionRolePart) > roleMentionCounter {
				roleMentionName = mentionRolePart[roleMentionCounter].Name
				roleID = mentionRolePart[roleMentionCounter].RoleID
				// bot 自身的角色 mention: 直接包装成 At 机器人自己, name 保留角色名称
				if botNickname == roleMentionName || botUsername == roleMentionName {
					components = append(components, &message.At{TargetID: botID, Name: roleMentionName})
					cursor = match.end
					continue
				}
			}
			// 目标不是数字且未从 mention_role_part 获取到 role_id 时跳过
			if !isDigits(mentionTarget) && roleID == 0 {
				cursor = match.end
				continue
			}
			if roleID == 0 {
				roleID, _ = strconv.ParseInt(mentionTarget, 10, 64)
			}
			// 没有频道 id 无法判断 bot 是否属于该角色, 跳过
			if guildID == "" {
				cursor = match.end
				continue
			}
			if !isDigits(guildID) {
				cursor = match.end
				continue
			}
			gid, _ := strconv.ParseInt(guildID, 10, 64)
			if !a.rolesCache.HasRoleInChannel(a.kookCtx(), roleID, gid) {
				cursor = match.end
				continue
			}
			components = append(components, &message.At{TargetID: botID, Name: roleMentionName})
		} else if mentionTarget != "" {
			components = append(components, &message.At{
				TargetID: mentionTarget,
				Name:     mentionNameMap[mentionTarget],
			})
		}
		cursor = match.end
	}

	if cursor < len(content) {
		tailText := strings.TrimSpace(content[cursor:])
		if tailText != "" {
			components = append(components, &message.Plain{Text: tailText})
		}
	}

	messageStr := strings.TrimSpace(rawContent)
	if len(components) > 0 {
		for _, comp := range components {
			switch c := comp.(type) {
			case *message.Plain:
				if strings.TrimSpace(c.Text) == "" {
					continue
				}
			case *message.At:
				if c.TargetID == botID {
					// 去掉消息开头的 "@昵称 - id " 前缀
					messageStr = atMentionPrefixRegex.ReplaceAllString(messageStr, "")
					messageStr = strings.TrimSpace(messageStr)
				}
			}
			break
		}
	}
	if len(components) == 0 {
		if messageStr != "" {
			components = []message.Component{&message.Plain{Text: messageStr}}
		} else {
			components = []message.Component{}
		}
	}
	return components, messageStr
}

// isDigits 判断字符串是否为纯数字。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// parseCardMessage 解析卡片消息 (对应 Python _parse_card_message)。
func (a *Adapter) parseCardMessage(data *kookMessageEventData) ([]message.Component, string, error) {
	content := contentToString(data.Content)
	guildID := data.Extra.GuildID

	var cardList []kookCardMessage
	if err := json.Unmarshal([]byte(content), &cardList); err != nil {
		return nil, "", err
	}

	var textParts []string
	var images []string
	type fileItem struct {
		moduleType KookModuleType
		title      string
		src        string
	}
	var files []fileItem

	for _, card := range cardList {
		for _, rawModule := range card.Modules {
			var meta kookCardModuleMeta
			if err := json.Unmarshal(rawModule, &meta); err != nil {
				continue
			}
			switch meta.Type {
			case ModuleSection:
				if text := a.handleSectionText(rawModule); text != "" {
					textParts = append(textParts, text)
				}
			case ModuleContainer, ModuleImageGroup:
				urls := a.handleImageGroup(rawModule)
				images = append(images, urls...)
				textParts = append(textParts, strings.Repeat(" [image]", len(urls)))
			case ModuleHeader:
				var header kookCardHeaderModule
				if err := json.Unmarshal(rawModule, &header); err == nil {
					textParts = append(textParts, header.Text.Content)
				}
			case ModuleFile, ModuleAudio, ModuleVideo:
				var fileModule kookCardFileModule
				if err := json.Unmarshal(rawModule, &fileModule); err == nil {
					files = append(files, fileItem{moduleType: fileModule.Type, title: fileModule.Title, src: fileModule.Src})
					textParts = append(textParts, " ["+string(fileModule.Type)+"]")
				}
			default:
				logger.Debug("[KOOK] 跳过或未处理模块: %s", string(meta.Type))
			}
		}
	}

	text := strings.Join(textParts, "")
	var msgComps []message.Component
	if text != "" {
		// 卡片文本同样需要解析 (met)/(rol) 选择器
		comps, newText := a.convertTextToComponents(text, text, nil, guildID, nil)
		msgComps = append(msgComps, comps...)
		text = newText
	}
	for _, imgURL := range images {
		msgComps = append(msgComps, &message.Image{URL: imgURL})
	}
	for _, f := range files {
		switch f.moduleType {
		case ModuleFile:
			msgComps = append(msgComps, &message.File{Name: f.title, URL: f.src})
		case ModuleVideo:
			msgComps = append(msgComps, &message.Video{URL: f.src})
		case ModuleAudio:
			// Python 使用 MediaResolver 将音频转为 wav; Go 侧 EnsureWAV 为占位实现
			// (保持原文件), 这里直接把音频下载到临时目录
			path := utils.TempFilePath(".wav")
			if err := utils.DownloadFile(a.kookCtx(), f.src, path); err != nil {
				logger.I18nWarn("[KOOK] 下载音频文件失败: %v", err)
				continue
			}
			msgComps = append(msgComps, &message.Record{File: path, URL: path})
		default:
			logger.I18nWarn("[KOOK] 跳过未知文件类型: %s", string(f.moduleType))
		}
	}
	return msgComps, text, nil
}

// handleSectionText 专门处理 Section 里的文本提取 (对应 Python _handle_section_text)。
func (a *Adapter) handleSectionText(rawModule json.RawMessage) string {
	var section kookCardSectionModule
	if err := json.Unmarshal(rawModule, &section); err != nil {
		return ""
	}
	// Section 的 text 可能是 plain-text/kmarkdown 元素 (含 content), 也可能是 paragraph 结构
	var textEl kookCardTextElement
	if err := json.Unmarshal(section.Text, &textEl); err == nil {
		return textEl.Content
	}
	return ""
}

// handleImageGroup 专门处理图片组/容器里的合法 URL 提取 (对应 Python _handle_image_group)。
func (a *Adapter) handleImageGroup(rawModule json.RawMessage) []string {
	var module kookCardImageGroupModule
	if err := json.Unmarshal(rawModule, &module); err != nil {
		return nil
	}
	var validURLs []string
	for _, el := range module.Elements {
		if !strings.HasPrefix(el.Src, "http://") && !strings.HasPrefix(el.Src, "https://") {
			logger.I18nWarn("[KOOK] 屏蔽非http图片url: %s", el.Src)
			continue
		}
		validURLs = append(validURLs, el.Src)
	}
	return validURLs
}

// handleMsg 发布消息事件到事件总线。
func (a *Adapter) handleMsg(abm *platform.AstrBotMessage) {
	if a.EventBus == nil {
		return
	}
	event := &core.Event{
		Type: core.EventMessage,
		Source: core.EventSource{
			Platform:   "kook",
			PlatformID: a.ID(),
			SelfID:     a.client.BotID(),
			SenderID:   abm.Sender.UserID,
			SenderName: abm.Sender.Nickname,
			ConvID:     abm.SessionID,
			IsGroup:    abm.Type == platform.GroupMessage || abm.Type == platform.OtherMessage,
		},
		Message:    &message.MessageChain{Chain: abm.Message},
		MessageStr: abm.MessageStr,
		Timestamp:  time.Unix(abm.Timestamp, 0),
		MessageObj: &core.MessageObj{
			MessageID:   abm.MessageID,
			SelfID:      a.client.BotID(),
			SessionID:   abm.SessionID,
			MessageType: string(abm.Type),
			Platform:    "kook",
			MessageStr:  abm.MessageStr,
			RawMessage:  abm.RawMessage,
		},
		Metadata: map[string]interface{}{},
	}
	if err := a.EventBus.Publish(event); err != nil {
		logger.I18nError("[KOOK] 发布消息事件失败: %v", err)
	}
}

// orderMessage 对应 Python kook_event.py 的 OrderMessage。
type orderMessage struct {
	index   int
	text    string
	msgType KookMessageType
	replyID string
}

// Send 发送消息链到指定会话 (对应 Python KookEvent.send)。
// sessionID 为频道 id (群聊/广播) 或用户 id (私聊)。
func (a *Adapter) Send(sessionID string, chain *message.MessageChain) error {
	if chain == nil {
		return nil
	}
	// 构造有序消息列表 (对应 Python _wrap_message)
	orders := make([]orderMessage, 0, len(chain.Chain))
	for index, comp := range chain.Chain {
		order, err := a.buildOrderMessage(index, comp)
		if err != nil {
			// 构造一个虚假的 OrderMessage, 让用户知道这里本来有张图但坏了
			// 这样后面的发送循环就能把它当成普通文本发出去
			logger.I18nError("[KOOK] 构造消息失败: %v", err)
			orders = append(orders, orderMessage{index: index, text: err.Error(), msgType: KookMsgText})
			continue
		}
		orders = append(orders, order)
	}
	// 按 index 排序 (对应 Python sort(key=lambda x: x.index))
	for i := 1; i < len(orders); i++ {
		for j := i; j > 0 && orders[j].index < orders[j-1].index; j-- {
			orders[j], orders[j-1] = orders[j-1], orders[j]
		}
	}

	// 根据是否接收过该频道的消息判断是频道消息还是私聊消息
	// (对应 Python 中按会话的消息类型选择发送接口)
	msgType := platform.FriendMessage
	if a.isKnownChannel(sessionID) {
		msgType = platform.GroupMessage
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	replyID := ""
	var errors []error
	for _, item := range orders {
		if item.replyID != "" {
			replyID = item.replyID
		}
		if item.text == "" {
			logger.Debug("[KOOK] 跳过空消息,类型为 %q", kookMsgTypeName(item.msgType))
			continue
		}
		if err := a.client.SendText(ctx, sessionID, item.text, msgType, item.msgType, replyID); err != nil {
			// 发送失败时尝试把错误信息当普通文本发送 (对应 Python)
			if err2 := a.client.SendText(ctx, sessionID, err.Error(), msgType, KookMsgText, replyID); err2 != nil {
				logger.I18nError("[KOOK] 发送错误信息失败: %v", err2)
			}
			errors = append(errors, err)
		}
	}
	if len(errors) > 0 {
		msgs := make([]string, 0, len(errors))
		for _, e := range errors {
			msgs = append(msgs, e.Error())
		}
		logger.I18nError("[KOOK] 发送消息时出现 %d 个错误: %s", len(errors), strings.Join(msgs, "\n"))
	}
	return nil
}

// React 给消息添加表情回应 (实现 platform.Reactor)。
func (a *Adapter) React(sessionID, messageID, emoji string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return a.client.React(ctx, messageID, emoji)
}

// buildOrderMessage 将单个消息组件包装为可发送的 OrderMessage。
// 对应 Python kook_event.py 的 _wrap_message。
func (a *Adapter) buildOrderMessage(index int, comp message.Component) (orderMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch c := comp.(type) {
	case *message.Image:
		src := firstNonEmpty(c.File, c.Path, c.URL)
		if src == "" && c.Base64 != "" {
			tmp := saveBase64ToTemp(c.Base64)
			if tmp == "" {
				return orderMessage{}, fmt.Errorf("图片 base64 解码失败")
			}
			src = tmp
		}
		url, err := a.client.UploadAsset(ctx, src)
		return orderMessage{index: index, text: url, msgType: KookMsgImage}, err
	case *message.Video:
		src := firstNonEmpty(c.Path, c.URL)
		url, err := a.client.UploadAsset(ctx, src)
		return orderMessage{index: index, text: url, msgType: KookMsgVideo}, err
	case *message.File:
		src := firstNonEmpty(c.Path, c.URL)
		url, err := a.client.UploadAsset(ctx, src)
		return orderMessage{index: index, text: url, msgType: KookMsgFile}, err
	case *message.Record:
		src := firstNonEmpty(c.File, c.Path, c.URL)
		if src == "" && c.Base64 != "" {
			tmp := saveBase64ToTemp(c.Base64)
			if tmp == "" {
				return orderMessage{}, fmt.Errorf("音频 base64 解码失败")
			}
			src = tmp
		}
		url, err := a.client.UploadAsset(ctx, src)
		if err != nil {
			return orderMessage{}, err
		}
		title := c.Text
		if title == "" {
			title = filepath.Base(src)
		}
		// 音频使用卡片消息发送 (对应 Python 的 FileModule AUDIO 卡片)
		card := buildAudioCard(url, title)
		return orderMessage{index: index, text: card, msgType: KookMsgCard}, nil
	case *message.Plain:
		return orderMessage{index: index, text: c.Text, msgType: KookMsgKMarkdown}, nil
	case *message.At:
		return orderMessage{index: index, text: "(met)" + c.TargetID + "(met)", msgType: KookMsgKMarkdown}, nil
	case *message.AtAll:
		return orderMessage{index: index, text: "(met)all(met)", msgType: KookMsgKMarkdown}, nil
	case *message.Reply:
		return orderMessage{index: index, text: "", replyID: c.MessageID, msgType: KookMsgKMarkdown}, nil
	case *message.Json:
		// kook卡片json外层得是一个列表
		jsonData := []interface{}{c.Data}
		data, err := json.Marshal(jsonData)
		if err != nil {
			return orderMessage{}, err
		}
		// 考虑到 kook 可能会更改消息结构, 为了能让插件开发者自行根据 kook 文档
		// 描述填卡片 json 内容, 故不做模型校验
		return orderMessage{index: index, text: string(data), msgType: KookMsgCard}, nil
	default:
		return orderMessage{}, fmt.Errorf("kook适配器尚未实现对 %q 消息类型的支持", string(comp.Type()))
	}
}

// buildAudioCard 构造音频卡片消息 JSON。
// 对应 Python: KookCardMessageContainer([KookCardMessage(modules=[FileModule(AUDIO, title, src)])]).to_json()
func buildAudioCard(url, title string) string {
	card := []map[string]interface{}{
		{
			"type": string(ModuleCard),
			"modules": []map[string]interface{}{
				{
					"type":  string(ModuleAudio),
					"title": title,
					"src":   url,
				},
			},
		},
	}
	data, _ := json.Marshal(card)
	return string(data)
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// saveBase64ToTemp 将 base64 数据解码为临时文件, 返回临时文件路径。
func saveBase64ToTemp(b64 string) string {
	data, err := decodeBase64(b64)
	if err != nil {
		logger.I18nWarn("[KOOK] base64 解码失败: %v", err)
		return ""
	}
	tmp, err := os.CreateTemp("", "astrbot-kook-*")
	if err != nil {
		logger.I18nWarn("[KOOK] 创建临时文件失败: %v", err)
		return ""
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return ""
	}
	_ = tmp.Close()
	return name
}

// decodeBase64 解码 base64 数据 (兼容 URL 安全编码)。
func decodeBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if data, err := base64.StdEncoding.DecodeString(s); err == nil {
		return data, nil
	}
	return base64.URLEncoding.DecodeString(s)
}
