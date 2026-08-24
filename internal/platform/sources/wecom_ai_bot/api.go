// 企业微信智能机器人 API 客户端。
// 1:1 移植自 wecomai_api.py：
//   - WecomAIBotAPIClient：消息加密解密、URL 验证、加密图片下载解密；
//   - WecomAIBotConstants：常量定义；
//   - WecomAIBotStreamMessageBuilder：流消息构建器；
//   - WecomAIBotMessageParser：消息解析器。
package wecom_ai_bot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" // #nosec G501 -- md5 用于图片流消息内容指纹（协议要求），非密码学用途
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// WecomAIBotConstants 企业微信智能机器人常量（对应 WecomAIBotConstants）。
type WecomAIBotConstants struct{}

// 消息类型
const (
	MSGTypeText   = "text"
	MSGTypeImage  = "image"
	MSGTypeMixed  = "mixed"
	MSGTypeStream = "stream"
	MSGTypeEvent  = "event"
)

// 流消息状态
const (
	STREAMContinue = false
	STREAMFinish   = true
)

// 错误码（与 WecomAIBotConstants 一致）
const (
	APISuccess                = 0
	APIDecryptError           = -40001
	APIValidateSignatureError = -40002
	APIParseXMLError          = -40003
	APIComputeSignatureError  = -40004
	APIIllegalAESKey          = -40005
	APIValidateAppIDError     = -40006
	APIEncryptAESError        = -40007
	APIIllegalBuffer          = -40008
)

// WecomAIBotAPIClient 企业微信智能机器人 API 客户端。
type WecomAIBotAPIClient struct {
	token          string
	encodingAESKey string
	wxcpt          *WXBizJsonMsgCrypt
}

// NewWecomAIBotAPIClient 构造 API 客户端（receiveid 固定为空串）。
func NewWecomAIBotAPIClient(token, encodingAESKey string) *WecomAIBotAPIClient {
	c := &WecomAIBotAPIClient{
		token:          token,
		encodingAESKey: encodingAESKey,
	}
	crypt, err := NewWXBizJsonMsgCrypt(token, encodingAESKey, "")
	if err != nil {
		logger.I18nError("EncodingAESKey 无效: %v", err)
		return c
	}
	c.wxcpt = crypt
	return c
}

// DecryptMessage 解密企业微信消息：校验签名并解密 JSON。
// 返回 (错误码, 解密后的消息数据字典)。
func (c *WecomAIBotAPIClient) DecryptMessage(encryptedData []byte, msgSignature, timestamp, nonce string) (int, map[string]interface{}) {
	if c.wxcpt == nil {
		return APIDecryptError, nil
	}
	ret, decrypted := c.wxcpt.DecryptMsg(encryptedData, msgSignature, timestamp, nonce)
	if ret != MsgCryptOK {
		logger.I18nError("消息解密失败，错误码: %d", ret)
		return ret, nil
	}
	if decrypted == "" {
		logger.I18nError("解密消息为空")
		return APIDecryptError, nil
	}
	var messageData map[string]interface{}
	if err := json.Unmarshal([]byte(decrypted), &messageData); err != nil {
		logger.I18nError("JSON 解析失败: %v, 原始消息: %s", err, decrypted)
		return APIParseXMLError, nil
	}
	logger.Debug("解密成功，消息内容: %v", messageData)
	return MsgCryptOK, messageData
}

// EncryptMessage 加密消息。
// 返回加密后的 JSON 字符串，失败时返回空字符串。
func (c *WecomAIBotAPIClient) EncryptMessage(plainMessage, nonce, timestamp string) string {
	if c.wxcpt == nil {
		logger.I18nError("加密失败: 加解密器未初始化")
		return ""
	}
	ret, encrypted := c.wxcpt.EncryptMsg(plainMessage, nonce, timestamp)
	if ret != MsgCryptOK {
		logger.I18nError("消息加密失败，错误码: %d", ret)
		return ""
	}
	return encrypted
}

// VerifyURL 验证回调 URL。
// 返回解密后的 echostr，失败返回 "verify fail"。
func (c *WecomAIBotAPIClient) VerifyURL(msgSignature, timestamp, nonce, echostr string) string {
	if c.wxcpt == nil {
		logger.I18nError("URL 验证失败: 加解密器未初始化")
		return "verify fail"
	}
	ret, echoResult := c.wxcpt.VerifyURL(msgSignature, timestamp, nonce, echostr)
	if ret != MsgCryptOK {
		logger.I18nError("URL 验证失败，错误码: %d", ret)
		return "verify fail"
	}
	logger.I18nInfo("URL 验证成功")
	if echoResult == "" {
		return "verify fail"
	}
	return echoResult
}

// ProcessEncryptedImage 下载并解密加密图片。
// 返回 (是否成功, 解密后的图片 base64 数据或错误信息)。
func (c *WecomAIBotAPIClient) ProcessEncryptedImage(imageURL, aesKeyBase64 string) (bool, string) {
	return processEncryptedImage(imageURL, aesKeyBase64)
}

// processEncryptedImage 下载并解密加密图片（对应 wecomai_utils.py 的同名函数）。
func processEncryptedImage(imageURL, aesKeyBase64 string) (bool, string) {
	logger.I18nInfo("开始下载加密图片: %s", imageURL)
	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Get(imageURL)
	if err != nil {
		msg := fmt.Sprintf("下载图片失败: %v", err)
		logger.Error("%s", msg)
		return false, msg
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("下载图片失败: HTTP %d", resp.StatusCode)
		logger.Error("%s", msg)
		return false, msg
	}
	// 与 webhook.go 的 32MiB 上限对齐, 防止超大图片一次性放大内存
	encryptedData, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		msg := fmt.Sprintf("下载图片失败: %v", err)
		logger.Error("%s", msg)
		return false, msg
	}
	logger.I18nInfo("图片下载成功，大小: %d 字节", len(encryptedData))

	// 准备 AES 密钥和 IV（IV 为密钥前 16 字节）
	aesKeyBase64 = strings.TrimSpace(aesKeyBase64)
	if aesKeyBase64 == "" {
		return false, "参数错误: AES 密钥不能为空"
	}
	pad := (4 - len(aesKeyBase64)%4) % 4
	aesKey, err := base64.StdEncoding.DecodeString(aesKeyBase64 + strings.Repeat("=", pad))
	if err != nil {
		return false, fmt.Sprintf("参数错误: %v", err)
	}
	if len(aesKey) != 32 {
		return false, "参数错误: 无效的AES密钥长度: 应为32字节"
	}
	iv := aesKey[:16]

	// 解密图片数据（AES-256-CBC）
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return false, fmt.Sprintf("图片处理异常: %v", err)
	}
	if len(encryptedData) == 0 || len(encryptedData)%aes.BlockSize != 0 {
		return false, "参数错误: 无效的密文长度"
	}
	decryptedData := make([]byte, len(encryptedData))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(decryptedData, encryptedData)

	// 去除 PKCS7 填充（Python: pad_len = decrypted_data[-1]; if pad_len > 32: raise）
	padLen := int(decryptedData[len(decryptedData)-1])
	if padLen < 1 || padLen > 32 || padLen > len(decryptedData) {
		return false, "参数错误: 无效的填充长度"
	}
	decryptedData = decryptedData[:len(decryptedData)-padLen]
	logger.I18nInfo("图片解密成功，解密后大小: %d 字节", len(decryptedData))

	// 转换为 base64 编码
	base64Data := base64.StdEncoding.EncodeToString(decryptedData)
	logger.I18nInfo("图片已转换为base64编码，编码后长度: %d", len(base64Data))
	return true, base64Data
}

// WecomAIBotStreamMessageBuilder 企业微信智能机器人流消息构建器。
type WecomAIBotStreamMessageBuilder struct{}

// MakeTextStream 构建文本流消息（对应 make_text_stream）。
func (WecomAIBotStreamMessageBuilder) MakeTextStream(streamID, content string, finish bool) string {
	plain := map[string]interface{}{
		"msgtype": MSGTypeStream,
		"stream": map[string]interface{}{
			"id":      streamID,
			"finish":  finish,
			"content": content,
		},
	}
	raw, _ := json.Marshal(plain)
	return string(raw)
}

// MakeImageStream 构建图片流消息（对应 make_image_stream）。
func (WecomAIBotStreamMessageBuilder) MakeImageStream(streamID string, imageData []byte, finish bool) string {
	// #nosec G401 -- md5 为图片流消息协议要求的内容指纹字段，非密码学用途。
	imageMD5 := md5.Sum(imageData) // nosemgrep: go.lang.security.audit.crypto.use_of_weak_crypto.use-of-md5
	imageBase64 := base64.StdEncoding.EncodeToString(imageData)
	plain := map[string]interface{}{
		"msgtype": MSGTypeStream,
		"stream": map[string]interface{}{
			"id":     streamID,
			"finish": finish,
			"msg_item": []map[string]interface{}{
				{
					"msgtype": MSGTypeImage,
					"image": map[string]interface{}{
						"base64": imageBase64,
						"md5":    hex.EncodeToString(imageMD5[:]),
					},
				},
			},
		},
	}
	raw, _ := json.Marshal(plain)
	return string(raw)
}

// MakeMixedStream 构建混合类型流消息（对应 make_mixed_stream）。
func (WecomAIBotStreamMessageBuilder) MakeMixedStream(streamID, content string, msgItems []interface{}, finish bool) string {
	stream := map[string]interface{}{
		"id":       streamID,
		"finish":   finish,
		"msg_item": msgItems,
	}
	if content != "" {
		stream["content"] = content
	}
	plain := map[string]interface{}{
		"msgtype": MSGTypeStream,
		"stream":  stream,
	}
	raw, _ := json.Marshal(plain)
	return string(raw)
}

// MakeText 构建文本消息（对应 make_text）。
func (WecomAIBotStreamMessageBuilder) MakeText(content string) string {
	plain := map[string]interface{}{
		"msgtype": "text",
		"text":    map[string]interface{}{"content": content},
	}
	raw, _ := json.Marshal(plain)
	return string(raw)
}

// WecomAIBotMessageParser 企业微信智能机器人消息解析器。
type WecomAIBotMessageParser struct{}

// ParseTextMessage 解析文本消息。
func (WecomAIBotMessageParser) ParseTextMessage(data map[string]interface{}) string {
	if sub, ok := data["text"].(map[string]interface{}); ok {
		if content, ok := sub["content"].(string); ok {
			return content
		}
	}
	logger.I18nWarn("文本消息解析失败")
	return ""
}

// ParseImageMessage 解析图片消息，返回图片 URL。
func (WecomAIBotMessageParser) ParseImageMessage(data map[string]interface{}) string {
	if sub, ok := data["image"].(map[string]interface{}); ok {
		if u, ok := sub["url"].(string); ok {
			return u
		}
	}
	logger.I18nWarn("图片消息解析失败")
	return ""
}

// ParseStreamMessage 解析流消息。
func (WecomAIBotMessageParser) ParseStreamMessage(data map[string]interface{}) map[string]interface{} {
	streamData, ok := data["stream"].(map[string]interface{})
	if !ok {
		logger.I18nWarn("流消息解析失败")
		return nil
	}
	msgItems, _ := streamData["msg_item"].([]interface{})
	if msgItems == nil {
		msgItems = []interface{}{}
	}
	return map[string]interface{}{
		"id":       streamData["id"],
		"finish":   streamData["finish"],
		"content":  streamData["content"],
		"msg_item": msgItems,
	}
}

// ParseMixedMessage 解析混合消息，返回消息项列表。
func (WecomAIBotMessageParser) ParseMixedMessage(data map[string]interface{}) []interface{} {
	if sub, ok := data["mixed"].(map[string]interface{}); ok {
		if items, ok := sub["msg_item"].([]interface{}); ok {
			return items
		}
	}
	logger.I18nWarn("混合消息解析失败")
	return nil
}

// ParseEventMessage 解析事件消息。
func (WecomAIBotMessageParser) ParseEventMessage(data map[string]interface{}) map[string]interface{} {
	if ev, ok := data["event"].(map[string]interface{}); ok {
		return ev
	}
	logger.I18nWarn("事件消息解析失败")
	return nil
}
