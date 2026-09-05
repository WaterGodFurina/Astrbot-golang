package lark

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// parseMessageComponents parses Lark message content into AstrBot components
// (mirrors lark_adapter.py _parse_message_components).
func (a *Adapter) parseMessageComponents(ctx context.Context, messageID, messageType string, content map[string]interface{}, atMap map[string]*message.At) []message.Component {
	components := []message.Component{}

	switch messageType {
	case "text":
		text, _ := content["text"].(string)
		parts := strings.Split(text, "@_user_")
		for i, part := range parts {
			segment := strings.TrimSpace(part)
			if i > 0 {
				// "@_user_<id>" placeholder; reconstruct key
				key := "@_user_"
				if idx := strings.IndexAny(segment, " \t\n"); idx >= 0 {
					key += segment[:idx]
					segment = strings.TrimSpace(segment[idx:])
				} else {
					key += segment
					segment = ""
				}
				if at, ok := atMap[key]; ok {
					components = append(components, at)
				}
				if segment != "" {
					components = append(components, &message.Plain{Text: segment})
				}
			} else if segment != "" {
				components = append(components, &message.Plain{Text: segment})
			}
		}
		return components

	case "post", "image":
		var compList []map[string]interface{}
		if messageType == "image" {
			if key, ok := content["image_key"].(string); ok {
				compList = []map[string]interface{}{{"tag": "img", "image_key": key}}
			}
		} else {
			compList = parsePostContent(content)
		}
		for _, comp := range compList {
			tag, _ := comp["tag"].(string)
			switch tag {
			case "at":
				userKey, _ := comp["user_id"].(string)
				if at, ok := atMap[userKey]; ok {
					components = append(components, at)
				}
			case "text":
				text := strings.TrimSpace(toString(comp["text"]))
				if text != "" {
					components = append(components, &message.Plain{Text: text})
				}
			case "a":
				text := strings.TrimSpace(toString(comp["text"]))
				href := strings.TrimSpace(toString(comp["href"]))
				if text != "" && href != "" {
					components = append(components, &message.Plain{Text: fmt.Sprintf("%s(%s)", text, href)})
				} else if text != "" {
					components = append(components, &message.Plain{Text: text})
				}
			case "img":
				imageKey := strings.TrimSpace(toString(comp["image_key"]))
				if imageKey == "" {
					continue
				}
				if messageID == "" {
					logger.I18nWarn("飞书图片消息缺少 message_id")
					continue
				}
				data, err := a.downloadMessageResource(ctx, messageID, imageKey, "image")
				if err != nil || data == nil {
					continue
				}
				components = append(components, &message.Image{Base64: base64.StdEncoding.EncodeToString(data)})
			case "media":
				fileKey := strings.TrimSpace(toString(comp["file_key"]))
				fileName := strings.TrimSpace(toString(comp["file_name"]))
				if fileName == "" {
					fileName = "lark_media.mp4"
				}
				if fileKey == "" || messageID == "" {
					continue
				}
				path := a.downloadFileToTemp(ctx, messageID, fileKey, "post_media", fileName, ".mp4")
				if path != "" {
					components = append(components, &message.Video{Path: path})
				}
			}
		}
		return components

	case "file":
		fileKey := strings.TrimSpace(toString(content["file_key"]))
		fileName := strings.TrimSpace(toString(content["file_name"]))
		if fileName == "" {
			fileName = "lark_file"
		}
		if fileKey == "" || messageID == "" {
			return components
		}
		path := a.downloadFileToTemp(ctx, messageID, fileKey, "file", fileName, "")
		if path != "" {
			components = append(components, &message.File{Name: fileName, Path: path})
		}
		return components

	case "audio":
		fileKey := strings.TrimSpace(toString(content["file_key"]))
		if fileKey == "" || messageID == "" {
			return components
		}
		path := a.downloadFileToTemp(ctx, messageID, fileKey, "audio", "", ".opus")
		if path != "" {
			components = append(components, &message.Record{File: path, URL: path, Path: path})
		}
		return components

	case "media":
		fileKey := strings.TrimSpace(toString(content["file_key"]))
		fileName := strings.TrimSpace(toString(content["file_name"]))
		if fileName == "" {
			fileName = "lark_media.mp4"
		}
		if fileKey == "" || messageID == "" {
			return components
		}
		path := a.downloadFileToTemp(ctx, messageID, fileKey, "media", fileName, ".mp4")
		if path != "" {
			components = append(components, &message.Video{Path: path})
		}
		return components
	}

	return components
}

// downloadMessageResource downloads an im message resource (images/files).
func (a *Adapter) downloadMessageResource(ctx context.Context, messageID, fileKey, resourceType string) ([]byte, error) {
	if a.client == nil {
		return nil, fmt.Errorf("lark: client not ready")
	}
	req := larkim.NewGetMessageResourceReqBuilder().
		MessageId(messageID).
		FileKey(fileKey).
		Type(resourceType).
		Build()
	resp, err := a.client.Im.MessageResource.Get(ctx, req)
	if err != nil {
		logger.Error("下载飞书消息资源失败 type=%s: %v", resourceType, err)
		return nil, err
	}
	if !resp.Success() {
		logger.Error("下载飞书消息资源失败 type=%s code=%d: %s", resourceType, resp.Code, resp.Msg)
		return nil, fmt.Errorf("lark resource download failed")
	}
	if resp.File == nil {
		logger.Error("飞书消息资源响应中不包含文件流: %s", fileKey)
		return nil, fmt.Errorf("lark resource empty")
	}

	return io.ReadAll(resp.File)
}

// tempMediaCleanupDelay 是临时媒体文件的清理延迟：消息在事件总线中同步处理，
// 回复在数十秒内完成，延迟清理保证媒体在消费期间可用。
const tempMediaCleanupDelay = 30 * time.Minute

// scheduleTempCleanup 在延迟后删除临时文件（消费方处理完媒体后执行清理）。
func scheduleTempCleanup(path string) {
	if path == "" {
		return
	}
	time.AfterFunc(tempMediaCleanupDelay, func() {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			logger.Debug("清理飞书临时文件失败 %s: %v", path, err)
		}
	})
}

// downloadFileToTemp saves a message resource to a temp file.
func (a *Adapter) downloadFileToTemp(ctx context.Context, messageID, fileKey, resourceType, fileName, defaultSuffix string) string {
	data, err := a.downloadMessageResource(ctx, messageID, fileKey, "file")
	if err != nil || data == nil {
		return ""
	}
	suffix := filepath.Ext(fileName)
	if suffix == "" {
		suffix = defaultSuffix
	}
	tmp, err := os.CreateTemp("", "lark_"+resourceType+"_*"+suffix)
	if err != nil {
		logger.Error("创建飞书临时文件失败: %v", err)
		return ""
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return ""
	}
	_ = tmp.Close()
	scheduleTempCleanup(name)
	return name
}

// buildReplyFromParentID fetches the parent message and builds a Reply
// component (mirrors _build_reply_from_parent_id).
func (a *Adapter) buildReplyFromParentID(ctx context.Context, parentMessageID string) *message.Reply {
	if a.client == nil {
		return nil
	}
	req := larkim.NewGetMessageReqBuilder().MessageId(parentMessageID).Build()
	resp, err := a.client.Im.Message.Get(ctx, req)
	if err != nil || !resp.Success() || resp.Data == nil || resp.Data.Items == nil || len(resp.Data.Items) == 0 {
		return nil
	}
	parent := resp.Data.Items[0]
	quotedID := parentMessageID
	if parent.MessageId != nil && *parent.MessageId != "" {
		quotedID = *parent.MessageId
	}
	quotedSender := "unknown"
	if parent.Sender != nil && parent.Sender.Id != nil && *parent.Sender.Id != "" {
		quotedSender = *parent.Sender.Id
	}
	quotedType := ""
	if parent.MsgType != nil {
		quotedType = *parent.MsgType
	}
	quotedContent := map[string]interface{}{}
	if parent.Body != nil && parent.Body.Content != nil {
		_ = json.Unmarshal([]byte(*parent.Body.Content), &quotedContent)
	}
	quotedAtMap := map[string]*message.At{}
	if parent.Mentions != nil {
		for _, m := range parent.Mentions {
			if m == nil || m.Id == nil {
				continue
			}
			key := ""
			if m.Key != nil {
				key = *m.Key
			}
			name := ""
			if m.Name != nil {
				name = *m.Name
			}
			quotedAtMap[key] = &message.At{TargetID: *m.Id, Name: name}
		}
	}
	quotedChain := a.parseMessageComponents(ctx, quotedID, quotedType, quotedContent, quotedAtMap)
	quotedText := buildMessageStr(quotedChain)
	nickname := "unknown"
	if quotedSender != "unknown" {
		nickname = quotedSender
		if len(nickname) > 8 {
			nickname = nickname[:8]
		}
	}
	reply := &message.Reply{
		MessageID:  quotedID,
		Chain:      quotedChain,
		SenderID:   quotedSender,
		SenderNick: nickname,
		MessageStr: quotedText,
	}
	return reply
}

// sendMessageChain sends a chain to a Lark session, separating file/audio/
// video components from the rich-text post (mirrors lark_event.py).
func sendMessageChain(ctx context.Context, client *lark.Client, chain *message.MessageChain, replyMessageID, receiveID, receiveIDType string) error {
	if client == nil {
		return fmt.Errorf("lark: client not ready")
	}
	var fileComps []*message.File
	var audioComps []*message.Record
	var videoComps []*message.Video
	otherComps := []message.Component{}

	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.File:
			fileComps = append(fileComps, c)
		case *message.Record:
			audioComps = append(audioComps, c)
		case *message.Video:
			videoComps = append(videoComps, c)
		default:
			otherComps = append(otherComps, comp)
		}
	}

	var errs []error
	if len(otherComps) > 0 {
		if err := sendRichText(ctx, client, otherComps, replyMessageID, receiveID, receiveIDType); err != nil {
			logger.I18nWarn("发送飞书富文本消息失败: %v", err)
			errs = append(errs, err)
		}
	}
	for _, f := range fileComps {
		if err := sendFileMessage(ctx, client, f, replyMessageID, receiveID, receiveIDType); err != nil {
			logger.I18nWarn("发送飞书文件失败: %v", err)
			errs = append(errs, err)
		}
	}
	for _, rec := range audioComps {
		if err := sendAudioMessage(ctx, client, rec, replyMessageID, receiveID, receiveIDType); err != nil {
			logger.I18nWarn("发送飞书音频失败: %v", err)
			errs = append(errs, err)
		}
	}
	for _, v := range videoComps {
		if err := sendMediaMessage(ctx, client, v, replyMessageID, receiveID, receiveIDType); err != nil {
			logger.I18nWarn("发送飞书视频失败: %v", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// hasReasoningMarker 检查组件中是否存在折叠面板标记
// （对齐本体 send_message_chain 的 has_reasoning_marker）。
func hasReasoningMarker(comps []message.Component) bool {
	for _, comp := range comps {
		if js, ok := comp.(*message.Json); ok && js.Data != nil && jsonStrSafe(js.Data, "type") == reasoningPanelType {
			return true
		}
	}
	return false
}

// sendRichText converts non-file components into a Lark post message
// (mirrors _convert_to_lark + post sending). 遇到
// Json(type=lark_collapsible_panel_reasoning) 标记时：
//   - 链中无附件组件（sendRichText 收到的 comps 已排除文件/音频/视频）且全部
//     组件可入卡时，合成单张 reasoning 卡片发送（对齐本体 :451-471）；
//   - 否则逐个标记先 flush 普通内容再发送折叠面板卡片（对齐本体 :507-533）；
//     卡片发送失败时回退为 "🤔 title: content" 的 Plain 文本继续参与后续 post。
func sendRichText(ctx context.Context, client *lark.Client, comps []message.Component, replyMessageID, receiveID, receiveIDType string) error {
	// 单卡片路径（对齐本体 _build_reasoning_card + _send_interactive_card）。
	if hasReasoningMarker(comps) {
		if cardJSON := buildReasoningCard(comps); cardJSON != nil {
			if err := sendInteractiveCard(ctx, client, cardJSON, replyMessageID, receiveID, receiveIDType); err == nil {
				return nil
			} else {
				logger.I18nWarn("发送飞书 reasoning 卡片失败，回退到逐段发送: %v", err)
			}
		}
	}

	buffered := []message.Component{}
	flush := func() error {
		if len(buffered) == 0 {
			return nil
		}
		postContent := convertToLark(ctx, client, buffered)
		buffered = []message.Component{}
		if len(postContent) == 0 {
			return nil
		}
		wrapped, _ := json.Marshal(map[string]interface{}{
			"zh_cn": map[string]interface{}{
				"title":   "",
				"content": postContent,
			},
		})
		return sendImMessage(ctx, client, string(wrapped), "post", replyMessageID, receiveID, receiveIDType)
	}

	var errs []error
	for _, comp := range comps {
		if js, ok := comp.(*message.Json); ok && js.Data != nil && jsonStrSafe(js.Data, "type") == reasoningPanelType {
			// 折叠面板标记：先发送已缓冲的普通内容。
			if err := flush(); err != nil {
				logger.I18nWarn("发送飞书富文本消息失败: %v", err)
				errs = append(errs, err)
			}
			reasonText := strings.TrimSpace(jsonStrSafe(js.Data, "content"))
			if reasonText == "" {
				continue
			}
			panelTitle := jsonStrSafe(js.Data, "title")
			if panelTitle == "" {
				panelTitle = "💭 Thinking"
			}
			if err := sendCollapsibleReasoningPanel(ctx, client, reasonText, panelTitle, replyMessageID, receiveID, receiveIDType); err != nil {
				logger.I18nWarn("发送飞书折叠面板卡片失败: %v", err)
				// 回退为纯文本（对齐本体 :524-529）。
				buffered = append(buffered, &message.Plain{Text: "🤔 " + panelTitle + ": " + reasonText})
			}
			continue
		}
		buffered = append(buffered, comp)
	}
	if err := flush(); err != nil {
		logger.I18nWarn("发送飞书富文本消息失败: %v", err)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// convertToLark converts components into Lark post content (mirrors
// _convert_to_lark). Images are uploaded inline; files/audio/video are
// returned separately by the caller.
func convertToLark(ctx context.Context, client *lark.Client, comps []message.Component) [][]map[string]interface{} {
	ret := [][]map[string]interface{}{}
	stage := []map[string]interface{}{}
	flush := func() {
		if len(stage) > 0 {
			ret = append(ret, stage)
			stage = []map[string]interface{}{}
		}
	}
	for _, comp := range comps {
		switch c := comp.(type) {
		case *message.Plain:
			stage = append(stage, map[string]interface{}{"tag": "md", "text": c.Text})
		case *message.At:
			stage = append(stage, map[string]interface{}{"tag": "at", "user_id": c.TargetID, "style": []interface{}{}})
		case *message.Image:
			path := c.Path
			if path == "" {
				path = c.File
			}
			tempPath := ""
			if path == "" && c.Base64 != "" {
				tempPath = writeTempBase64(c.Base64, "lark_img")
				path = tempPath
			}
			if path == "" {
				var err error
				path, tempPath, err = resolveMediaPath(ctx, "", c.URL)
				if err != nil {
					logger.Error("无法下载飞书图片: %v", err)
					continue
				}
			}
			if path == "" {
				logger.Error("飞书图片路径为空，无法上传")
				continue
			}
			key, err := uploadImage(ctx, client, path)
			if tempPath != "" {
				_ = os.Remove(tempPath)
			}
			if err != nil {
				logger.Error("无法上传飞书图片: %v", err)
				continue
			}
			flush()
			ret = append(ret, []map[string]interface{}{{"tag": "img", "image_key": key}})
		default:
			logger.Warn("飞书暂时不支持消息段: %T", comp)
		}
	}
	flush()
	return ret
}

// sendImMessage sends or replies an im message (mirrors _send_im_message).
func sendImMessage(ctx context.Context, client *lark.Client, content, msgType, replyMessageID, receiveID, receiveIDType string) error {
	if replyMessageID != "" {
		req := larkim.NewReplyMessageReqBuilder().
			MessageId(replyMessageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				Content(content).
				MsgType(msgType).
				Uuid(uuidStr()).
				ReplyInThread(false).
				Build()).
			Build()
		resp, err := client.Im.Message.Reply(ctx, req)
		if err != nil {
			return err
		}
		if !resp.Success() {
			return fmt.Errorf("lark reply failed(%d): %s", resp.Code, resp.Msg)
		}
		return nil
	}
	if receiveIDType == "" || receiveID == "" {
		return fmt.Errorf("lark: 主动发送消息时 receive_id 和 receive_id_type 不能为空")
	}
	req := larkim.NewCreateMessageReqBuilder().
		ReceiveIdType(receiveIDType).
		Body(larkim.NewCreateMessageReqBodyBuilder().
			ReceiveId(receiveID).
			Content(content).
			MsgType(msgType).
			Uuid(uuidStr()).
			Build()).
		Build()
	resp, err := client.Im.Message.Create(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success() {
		return fmt.Errorf("lark send failed(%d): %s", resp.Code, resp.Msg)
	}
	return nil
}

// uploadFile uploads a file (stream type) and returns the file_key
// (mirrors _upload_lark_file). durationMs > 0 时附带媒体时长（毫秒）。
func uploadFile(ctx context.Context, client *lark.Client, path, fileType string, durationMs ...int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	body := larkim.NewCreateFileReqBodyBuilder().
		FileType(fileType).
		FileName(filepath.Base(path)).
		File(f)
	if len(durationMs) > 0 && durationMs[0] > 0 {
		body.Duration(durationMs[0])
	}
	req := larkim.NewCreateFileReqBuilder().
		Body(body.Build()).
		Build()
	resp, err := client.Im.File.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() || resp.Data == nil || resp.Data.FileKey == nil {
		return "", fmt.Errorf("lark upload failed(%d): %s", resp.Code, resp.Msg)
	}
	return *resp.Data.FileKey, nil
}

// uploadImage uploads an image (message type) and returns the image_key.
func uploadImage(ctx context.Context, client *lark.Client, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	req := larkim.NewCreateImageReqBuilder().
		Body(larkim.NewCreateImageReqBodyBuilder().
			ImageType("message").
			Image(f).
			Build()).
		Build()
	resp, err := client.Im.Image.Create(ctx, req)
	if err != nil {
		return "", err
	}
	if !resp.Success() || resp.Data == nil || resp.Data.ImageKey == nil {
		return "", fmt.Errorf("lark image upload failed(%d): %s", resp.Code, resp.Msg)
	}
	return *resp.Data.ImageKey, nil
}

// sendFileMessage sends a File component (mirrors _send_file_message).
func sendFileMessage(ctx context.Context, client *lark.Client, comp *message.File, replyMessageID, receiveID, receiveIDType string) error {
	path, tempPath, err := resolveMediaPath(ctx, comp.Path, comp.URL)
	if err != nil {
		return err
	}
	if tempPath != "" {
		defer os.Remove(tempPath)
	}
	if path == "" {
		return fmt.Errorf("lark: 文件路径为空")
	}
	key, err := uploadFile(ctx, client, path, "stream")
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"file_key": key})
	return sendImMessage(ctx, client, string(content), "file", replyMessageID, receiveID, receiveIDType)
}

// sendAudioMessage sends a Record component (mirrors _send_audio_message):
// ffmpeg 转 opus（libopus/单声道/16kHz）→ ffprobe 时长 → file_type=opus 上传
// → audio 消息发送；转码失败降级按 stream 原样上传 file 消息。
func sendAudioMessage(ctx context.Context, client *lark.Client, comp *message.Record, replyMessageID, receiveID, receiveIDType string) error {
	path := comp.Path
	if path == "" {
		path = comp.File
	}
	tempPath := ""
	if path == "" {
		var err error
		path, tempPath, err = resolveMediaPath(ctx, "", comp.URL)
		if err != nil {
			return err
		}
	}
	if tempPath != "" {
		defer os.Remove(tempPath)
	}
	if path == "" {
		return fmt.Errorf("lark: 音频路径为空")
	}

	// 转换为 opus 格式（对齐本体：失败继续直接上传原文件）。
	audioPath, converted := convertAudioToOpus(ctx, path)
	if converted {
		defer safeRemove(audioPath)
		fileType := "opus"
		duration := getMediaDuration(audioPath)
		key, err := uploadFile(ctx, client, audioPath, fileType, duration)
		if err != nil {
			return err
		}
		content, _ := json.Marshal(map[string]string{"file_key": key})
		return sendImMessage(ctx, client, string(content), "audio", replyMessageID, receiveID, receiveIDType)
	}

	// 降级：opus 转码不可用时按 stream 上传原文件发送 file 消息（原有回退路径）。
	key, err := uploadFile(ctx, client, path, "stream")
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"file_key": key})
	return sendImMessage(ctx, client, string(content), "file", replyMessageID, receiveID, receiveIDType)
}

// sendMediaMessage sends a Video component (mirrors _send_media_message):
// ffmpeg 转 mp4（libx264/aac）→ ffprobe 时长 → file_type=mp4 上传 → media 消息发送。
func sendMediaMessage(ctx context.Context, client *lark.Client, comp *message.Video, replyMessageID, receiveID, receiveIDType string) error {
	path, tempPath, err := resolveMediaPath(ctx, comp.Path, comp.URL)
	if err != nil {
		return err
	}
	if tempPath != "" {
		defer os.Remove(tempPath)
	}
	if path == "" {
		return fmt.Errorf("lark: 视频路径为空")
	}

	// 转换为 mp4 格式（对齐本体：失败继续直接上传原文件）。
	videoPath, converted := convertVideoToMp4(ctx, path)
	if converted {
		defer safeRemove(videoPath)
		duration := getMediaDuration(videoPath)
		key, err := uploadFile(ctx, client, videoPath, "mp4", duration)
		if err != nil {
			return err
		}
		content, _ := json.Marshal(map[string]string{"file_key": key})
		return sendImMessage(ctx, client, string(content), "media", replyMessageID, receiveID, receiveIDType)
	}

	// 降级：mp4 转码不可用时按 stream 上传原文件（保留 media 消息类型，与
	// 原有回退一致地依赖飞书服务端识别格式）。
	key, err := uploadFile(ctx, client, path, "stream")
	if err != nil {
		return err
	}
	content, _ := json.Marshal(map[string]string{"file_key": key})
	return sendImMessage(ctx, client, string(content), "media", replyMessageID, receiveID, receiveIDType)
}

// resolveMediaPath 将组件的本地路径/URL 解析为可上传路径：
// 路径为空且 URL 为 http(s) 时先下载到临时文件（经 SSRF 校验与大小上限），
// 返回的 tempPath 非空时调用方应在发送后 os.Remove。
func resolveMediaPath(ctx context.Context, path, rawURL string) (string, string, error) {
	if path != "" {
		return path, "", nil
	}
	if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
		return "", "", nil
	}
	data, err := platform.SafeDownloadBytes(ctx, rawURL, 50<<20)
	if err != nil {
		return "", "", err
	}
	tmp, err := os.CreateTemp("", "lark_dl_*")
	if err != nil {
		return "", "", err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", "", err
	}
	_ = tmp.Close()
	return name, name, nil
}

// writeTempBase64 decodes a base64 payload to a temp file.
func writeTempBase64(b64, prefix string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return ""
	}
	tmp, err := os.CreateTemp("", prefix+"_*")
	if err != nil {
		return ""
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return ""
	}
	_ = tmp.Close()
	return name
}

// uuidStr returns a random hex uuid (no dash).
func uuidStr() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// toString converts a JSON value to string.
func toString(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
