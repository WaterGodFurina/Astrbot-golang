// Package misskey implements a Misskey platform adapter.
// 移植自 astrbot/core/platform/sources/misskey/misskey_utils.py
package misskey

import (
	"fmt"
	"os"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// FileIDExtractor 从 API 响应中提取文件 ID 的帮助类（无状态，对应 FileIDExtractor）。
type FileIDExtractor struct{}

// ExtractFileID 依次尝试 createdFile.id / file.id / id 三个路径提取文件 ID。
func (FileIDExtractor) ExtractFileID(result map[string]interface{}) string {
	paths := []func(map[string]interface{}) interface{}{
		func(r map[string]interface{}) interface{} {
			if created, ok := r["createdFile"].(map[string]interface{}); ok {
				return created["id"]
			}
			return nil
		},
		func(r map[string]interface{}) interface{} {
			if f, ok := r["file"].(map[string]interface{}); ok {
				return f["id"]
			}
			return nil
		},
		func(r map[string]interface{}) interface{} {
			return r["id"]
		},
	}
	for _, p := range paths {
		if v := p(result); v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// MessagePayloadBuilder 构建不同类型消息负载的帮助类（无状态，对应 MessagePayloadBuilder）。
type MessagePayloadBuilder struct{}

// BuildChatPayload 构建私聊消息负载 {toUserId, text, fileId}。
func (MessagePayloadBuilder) BuildChatPayload(userID, text, fileID string) map[string]interface{} {
	payload := map[string]interface{}{"toUserId": userID}
	if text != "" {
		payload["text"] = text
	}
	if fileID != "" {
		payload["fileId"] = fileID
	}
	return payload
}

// BuildRoomPayload 构建房间消息负载 {toRoomId, text, fileId}。
func (MessagePayloadBuilder) BuildRoomPayload(roomID, text, fileID string) map[string]interface{} {
	payload := map[string]interface{}{"toRoomId": roomID}
	if text != "" {
		payload["text"] = text
	}
	if fileID != "" {
		payload["fileId"] = fileID
	}
	return payload
}

// BuildNotePayload 构建发帖负载 {text, fileIds, ...kwargs}。
func (MessagePayloadBuilder) BuildNotePayload(text string, fileIDs []string, kwargs map[string]interface{}) map[string]interface{} {
	payload := make(map[string]interface{})
	if text != "" {
		payload["text"] = text
	}
	if len(fileIDs) > 0 {
		payload["fileIds"] = fileIDs
	}
	for k, v := range kwargs {
		payload[k] = v
	}
	return payload
}

// serializeMessageChain 将消息链序列化为文本字符串（对应 serialize_message_chain）。
// 返回 (文本, 是否包含 @ 提及)。
func serializeMessageChain(chain []message.Component) (string, bool) {
	var textParts []string
	hasAt := false

	processComponent := func(component message.Component) string {
		switch c := component.(type) {
		case *message.Plain:
			return c.Text
		case *message.File:
			// 为文件组件返回占位符，但适配器仍会处理原组件
			return "[文件]"
		case *message.Image:
			// 为图片组件返回占位符，但适配器仍会处理原组件
			return "[图片]"
		case *message.At:
			hasAt = true
			// 优先使用 name 字段（用户名），避免生成 @<user_id> 这样的无效提及
			if c.Name != "" {
				return "@" + c.Name
			}
			return "@" + c.TargetID
		}
		// 其他组件若带 text 字段则拼接
		if c, ok := component.(*message.Unknown); ok {
			if strings.Contains(c.Text, "@") {
				hasAt = true
			}
			return c.Text
		}
		return component.String()
	}

	for _, component := range chain {
		if node, ok := component.(*message.Node); ok && len(node.Content) > 0 {
			for _, nodeComp := range node.Content {
				if result := processComponent(nodeComp); result != "" {
					textParts = append(textParts, result)
				}
			}
		} else {
			if result := processComponent(component); result != "" {
				textParts = append(textParts, result)
			}
		}
	}
	return strings.Join(textParts, ""), hasAt
}

// resolveMessageVisibility 解析消息可见性设置（对应 resolve_message_visibility）。
// 优先从 user_cache 解析，其次从 raw_message 解析。
func resolveMessageVisibility(userID string, userCache map[string]map[string]interface{}, selfID string, rawMessage map[string]interface{}, defaultVisibility string) (string, []string) {
	visibility := defaultVisibility
	var visibleUserIDs []string

	// 优先从 user_cache 解析
	if userID != "" && userCache != nil {
		if userInfo, ok := userCache[userID]; ok {
			originalVisibility, _ := userInfo["visibility"].(string)
			if originalVisibility == "" {
				originalVisibility = defaultVisibility
			}
			if originalVisibility == "specified" {
				visibility = "specified"
				usersToInclude := []string{userID}
				if selfID != "" {
					usersToInclude = append(usersToInclude, selfID)
				}
				seen := make(map[string]bool)
				for _, uid := range usersToInclude {
					if uid != "" {
						seen[uid] = true
					}
				}
				if raw, ok := userInfo["visible_user_ids"].([]string); ok {
					for _, uid := range raw {
						if uid != "" {
							seen[uid] = true
						}
					}
				}
				for uid := range seen {
					visibleUserIDs = append(visibleUserIDs, uid)
				}
			} else {
				visibility = originalVisibility
			}
			return visibility, visibleUserIDs
		}
	}

	// 回退到从 raw_message 解析
	if rawMessage != nil {
		originalVisibility, _ := rawMessage["visibility"].(string)
		if originalVisibility == "" {
			originalVisibility = defaultVisibility
		}
		if originalVisibility == "specified" {
			visibility = "specified"
			usersToInclude := []string{}
			if senderID, ok := rawMessage["userId"].(string); ok && senderID != "" {
				usersToInclude = append(usersToInclude, senderID)
			}
			if selfID != "" {
				usersToInclude = append(usersToInclude, selfID)
			}
			seen := make(map[string]bool)
			for _, uid := range usersToInclude {
				seen[uid] = true
			}
			if raw, ok := rawMessage["visibleUserIds"].([]interface{}); ok {
				for _, uid := range raw {
					if s, ok := uid.(string); ok && s != "" {
						seen[s] = true
					}
				}
			}
			for uid := range seen {
				visibleUserIDs = append(visibleUserIDs, uid)
			}
		} else {
			visibility = originalVisibility
		}
	}
	return visibility, visibleUserIDs
}

// isSessionIDOfPrefix 检查 session_id 是否为指定前缀的有效会话（对应 is_valid_*_session_id）。
// 仅当格式为 "<prefix>%<非空且非 unknown>" 时有效。
func isSessionIDOfPrefix(sessionID, prefix string) bool {
	if !strings.Contains(sessionID, "%") {
		return false
	}
	parts := strings.SplitN(sessionID, "%", 2)
	return len(parts) == 2 &&
		parts[0] == prefix &&
		parts[1] != "" &&
		parts[1] != "unknown"
}

// IsValidUserSessionID 检查是否为聊天用户会话 ID（chat% 前缀）。
func IsValidUserSessionID(sessionID string) bool {
	return isSessionIDOfPrefix(sessionID, "chat")
}

// IsValidRoomSessionID 检查是否为房间会话 ID（room% 前缀）。
func IsValidRoomSessionID(sessionID string) bool {
	return isSessionIDOfPrefix(sessionID, "room")
}

// IsValidChatSessionID 检查是否为聊天会话 ID（chat% 前缀）。
func IsValidChatSessionID(sessionID string) bool {
	return isSessionIDOfPrefix(sessionID, "chat")
}

// ExtractUserIDFromSessionID 从 session_id 中提取用户 ID（对应 extract_user_id_from_session_id）。
func ExtractUserIDFromSessionID(sessionID string) string {
	if strings.Contains(sessionID, "%") {
		parts := strings.SplitN(sessionID, "%", 2)
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return sessionID
}

// ExtractRoomIDFromSessionID 从 session_id 中提取房间 ID（对应 extract_room_id_from_session_id）。
func ExtractRoomIDFromSessionID(sessionID string) string {
	if strings.Contains(sessionID, "%") {
		parts := strings.SplitN(sessionID, "%", 2)
		if len(parts) >= 2 && parts[0] == "room" {
			return parts[1]
		}
	}
	return sessionID
}

// AddAtMentionIfNeeded 如果需要且没有 @用户，则添加 @用户（对应 add_at_mention_if_needed）。
// 仅在有有效 username 时才添加，避免生成 @<user_id> 这样的无效提及。
func AddAtMentionIfNeeded(text string, userInfo map[string]interface{}, hasAt bool) string {
	if hasAt || userInfo == nil {
		return text
	}
	username, _ := userInfo["username"].(string)
	if username == "" {
		return text
	}
	mention := "@" + username
	if !strings.HasPrefix(text, mention) {
		text = strings.TrimSpace(mention + "\n" + text)
	}
	return text
}

// createFileComponent 创建文件组件和描述文本（对应 create_file_component）。
// 对应 Image(url, file=name) / Record / Video / File 组件映射。
func createFileComponent(fileInfo map[string]interface{}) (message.Component, string) {
	fileURL, _ := fileInfo["url"].(string)
	fileName, _ := fileInfo["name"].(string)
	if fileName == "" {
		fileName = "未知文件"
	}
	fileType, _ := fileInfo["type"].(string)

	switch {
	case strings.HasPrefix(fileType, "image/"):
		return &message.Image{URL: fileURL, File: fileName}, fmt.Sprintf("图片[%s]", fileName)
	case strings.HasPrefix(fileType, "audio/"):
		// Python 在此处将音频下载并转码为 wav；Go 侧无转码能力，直接引用原文件
		return &message.Record{URL: fileURL, File: fileName}, fmt.Sprintf("音频[%s]", fileName)
	case strings.HasPrefix(fileType, "video/"):
		return &message.Video{URL: fileURL}, fmt.Sprintf("视频[%s]", fileName)
	default:
		return &message.File{Name: fileName, URL: fileURL}, fmt.Sprintf("文件[%s]", fileName)
	}
}

// processFiles 处理文件列表，添加到消息组件中并返回文本描述（对应 process_files）。
func processFiles(messageObj *platform.AstrBotMessage, files []interface{}, includeTextParts bool) []string {
	var fileParts []string
	for _, f := range files {
		fileInfo, ok := f.(map[string]interface{})
		if !ok {
			continue
		}
		component, partText := createFileComponent(fileInfo)
		messageObj.Message = append(messageObj.Message, component)
		if includeTextParts {
			fileParts = append(fileParts, partText)
		}
	}
	return fileParts
}

// FormatPoll 将 Misskey 的 poll 对象格式化为可读字符串（对应 format_poll）。
func FormatPoll(poll map[string]interface{}) string {
	if len(poll) == 0 {
		return ""
	}
	multiple, _ := poll["multiple"].(bool)
	textChoices := []string{}
	if choices, ok := poll["choices"].([]interface{}); ok {
		for idx, c := range choices {
			cMap, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			text, _ := cMap["text"].(string)
			votes := 0.0
			if v, ok := cMap["votes"].(float64); ok {
				votes = v
			}
			textChoices = append(textChoices, fmt.Sprintf("(%d) %s [%d票]", idx+1, text, int(votes)))
		}
	}
	parts := []string{"[投票]"}
	if multiple {
		parts = append(parts, "允许多选")
	} else {
		parts = append(parts, "单选")
	}
	if len(textChoices) > 0 {
		parts = append(parts, "选项: "+strings.Join(textChoices, ", "))
	}
	return strings.Join(parts, " ")
}

// SenderInfo 发送者信息（对应 extract_sender_info 的返回字典）。
type SenderInfo struct {
	Sender   map[string]interface{}
	SenderID string
	Nickname string
	Username string
}

// ExtractSenderInfo 提取发送者信息（对应 extract_sender_info）。
// is_chat 时发送者在 fromUser 字段，否则在 user 字段。
func ExtractSenderInfo(rawData map[string]interface{}, isChat bool) SenderInfo {
	var sender map[string]interface{}
	senderID := ""
	if isChat {
		sender, _ = rawData["fromUser"].(map[string]interface{})
		if sender != nil {
			senderID, _ = sender["id"].(string)
		}
		if senderID == "" {
			senderID, _ = rawData["fromUserId"].(string)
		}
	} else {
		sender, _ = rawData["user"].(map[string]interface{})
		if sender != nil {
			senderID, _ = sender["id"].(string)
		}
	}
	nickname := ""
	username := ""
	if sender != nil {
		username, _ = sender["username"].(string)
		nickname, _ = sender["name"].(string)
		if nickname == "" {
			nickname = username
		}
	}
	return SenderInfo{
		Sender:   sender,
		SenderID: senderID,
		Nickname: nickname,
		Username: username,
	}
}

// CreateBaseMessage 创建基础消息对象（对应 create_base_message）。
// session_id 格式: "chat%<user_id>" / "room%<room_id>" / "note%<user_id>"。
func CreateBaseMessage(rawData map[string]interface{}, senderInfo SenderInfo, botSelfID string, isChat bool, roomID string) *platform.AstrBotMessage {
	m := platform.NewAstrBotMessage()
	m.RawMessage = rawData
	m.Message = []message.Component{}

	m.Sender = platform.MessageMember{
		UserID:   senderInfo.SenderID,
		Nickname: senderInfo.Nickname,
	}

	sessionPrefix := "note"
	sessionID := ""
	if roomID != "" {
		sessionPrefix = "room"
		sessionID = fmt.Sprintf("%s%%%s", sessionPrefix, roomID)
		m.Type = platform.GroupMessage
		m.Group = &platform.Group{GroupID: roomID}
	} else if isChat {
		sessionPrefix = "chat"
		sessionID = fmt.Sprintf("%s%%%s", sessionPrefix, senderInfo.SenderID)
		m.Type = platform.FriendMessage
	} else {
		sessionID = fmt.Sprintf("%s%%%s", sessionPrefix, senderInfo.SenderID)
		m.Type = platform.OtherMessage
	}
	if senderInfo.SenderID == "" && roomID == "" {
		sessionID = fmt.Sprintf("%s%%unknown", sessionPrefix)
	}
	m.SessionID = sessionID
	m.MessageID, _ = rawData["id"].(string)
	m.SelfID = botSelfID
	return m
}

// ProcessAtMention 处理 @提及逻辑（对应 process_at_mention）。
// 文本以 @bot_username 开头时拆分为 At + Plain 组件。
func ProcessAtMention(messageObj *platform.AstrBotMessage, rawText, botUsername, botSelfID string) ([]string, string) {
	messageParts := []string{}
	if rawText == "" {
		return messageParts, ""
	}
	if botUsername != "" && strings.HasPrefix(rawText, "@"+botUsername) {
		atMention := "@" + botUsername
		messageObj.Message = append(messageObj.Message, &message.At{TargetID: botSelfID})
		remainingText := strings.TrimSpace(rawText[len(atMention):])
		if remainingText != "" {
			messageObj.Message = append(messageObj.Message, &message.Plain{Text: remainingText})
			messageParts = append(messageParts, remainingText)
		}
		return messageParts, remainingText
	}
	messageObj.Message = append(messageObj.Message, &message.Plain{Text: rawText})
	messageParts = append(messageParts, rawText)
	return messageParts, rawText
}

// CacheUserInfo 缓存用户信息（对应 cache_user_info）。
// 缓存中包含 visibility / visible_user_ids / reply_to_note_id，用于回复与可见性解析。
func CacheUserInfo(userCache map[string]map[string]interface{}, senderInfo SenderInfo, rawData map[string]interface{}, botSelfID string, isChat bool) {
	var cacheData map[string]interface{}
	if isChat {
		cacheData = map[string]interface{}{
			"username":         senderInfo.Username,
			"nickname":         senderInfo.Nickname,
			"visibility":       "specified",
			"visible_user_ids": []string{botSelfID, senderInfo.SenderID},
		}
	} else {
		visibility, _ := rawData["visibility"].(string)
		if visibility == "" {
			visibility = "public"
		}
		var visibleIDs []string
		if raw, ok := rawData["visibleUserIds"].([]interface{}); ok {
			for _, uid := range raw {
				if s, ok := uid.(string); ok {
					visibleIDs = append(visibleIDs, s)
				}
			}
		}
		replyToNoteID, _ := rawData["id"].(string)
		cacheData = map[string]interface{}{
			"username":         senderInfo.Username,
			"nickname":         senderInfo.Nickname,
			"visibility":       visibility,
			"visible_user_ids": visibleIDs,
			// 保存原消息 ID，用于回复时作为 reply_id
			"reply_to_note_id": replyToNoteID,
		}
	}
	userCache[senderInfo.SenderID] = cacheData
}

// CacheRoomInfo 缓存房间信息（对应 cache_room_info）。
func CacheRoomInfo(userCache map[string]map[string]interface{}, rawData map[string]interface{}, botSelfID string) {
	roomData, _ := rawData["toRoom"].(map[string]interface{})
	roomID, _ := rawData["toRoomId"].(string)
	if roomData != nil && roomID != "" {
		roomCacheKey := "room:" + roomID
		roomName, _ := roomData["name"].(string)
		roomDescription, _ := roomData["description"].(string)
		ownerID, _ := roomData["ownerId"].(string)
		userCache[roomCacheKey] = map[string]interface{}{
			"room_id":          roomID,
			"room_name":        roomName,
			"room_description": roomDescription,
			"owner_id":         ownerID,
			"visibility":       "specified",
			"visible_user_ids": []string{botSelfID},
		}
	}
}

// ResolveComponentURLOrPath 尝试从组件解析可上传的远程 URL 或本地路径（对应 resolve_component_url_or_path）。
// 返回 (url_candidate, local_path)，两者都可能为空。
func ResolveComponentURLOrPath(comp message.Component) (string, string) {
	urlCandidate := ""
	localPath := ""

	isURL := func(v string) bool { return strings.HasPrefix(v, "http") }

	// 1. 异步方法（convert_to_file_path / get_file / register_to_file_service）：
	//    Go 组件无异步接口，仅在 Image 上按 URL 下载语义处理（由调用方决定）。
	// 2. 回退到同步属性（file / url / path / src / source）
	switch c := comp.(type) {
	case *message.Image:
		if c.URL != "" && isURL(c.URL) {
			urlCandidate = c.URL
		}
		if c.Path != "" {
			localPath = c.Path
		} else if c.File != "" {
			localPath = c.File
		}
	case *message.File:
		if c.URL != "" && isURL(c.URL) {
			urlCandidate = c.URL
		}
		if c.Path != "" {
			localPath = c.Path
		}
	case *message.Record:
		if c.URL != "" && isURL(c.URL) {
			urlCandidate = c.URL
		}
		if c.Path != "" {
			localPath = c.Path
		} else if c.File != "" {
			localPath = c.File
		}
	case *message.Video:
		if c.URL != "" && isURL(c.URL) {
			urlCandidate = c.URL
		}
		if c.Path != "" {
			localPath = c.Path
		}
	case *message.Unknown:
		if strings.HasPrefix(c.Text, "http") {
			urlCandidate = c.Text
		} else if c.Text != "" {
			localPath = c.Text
		}
	}

	return urlCandidate, localPath
}

// fileUploader 是 API 客户端的上传能力接口（供 uploadLocalWithRetries 使用）。
type fileUploader interface {
	UploadFile(localPath, name, folderID string) (string, error)
}

// UploadLocalWithRetries 尝试本地上传，返回 file id 或空字符串（对应 upload_local_with_retries）。
// 上传失败直接返回空串，由上层处理错误。
func UploadLocalWithRetries(api fileUploader, localPath, preferredName, folderID string) string {
	if api == nil {
		return ""
	}
	fileID, err := api.UploadFile(localPath, preferredName, folderID)
	if err != nil {
		logger.Debug("Misskey 本地上传失败 %s: %v", localPath, err)
		return ""
	}
	return fileID
}

// isComponentFileLike 判断组件是否可能携带文件/URL 信息（对应 Python 的 has_file_components 判断）。
func isComponentFileLike(comp message.Component) bool {
	switch comp.(type) {
	case *message.Image, *message.File:
		return true
	}
	switch c := comp.(type) {
	case *message.Record:
		return c.URL != "" || c.Path != "" || c.File != "" || c.FileID != "" || c.Base64 != ""
	case *message.Video:
		return c.URL != "" || c.Path != "" || c.FileID != ""
	}
	return false
}

// isFileExists 判断本地文件是否存在（用于清理临时文件）。
func isFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
