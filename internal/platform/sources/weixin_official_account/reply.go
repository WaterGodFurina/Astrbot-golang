// 被动回复 XML 构建与加解密。
// 对齐本体 weixin_offacc_adapter.py（wechatpy create_reply / crypto.encrypt_message）
// 与 weixin_offacc_event.py 的 ImageReply / VoiceReply 渲染。
package weixin_official_account

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// textReplyXML 构建文本被动回复 XML（对应 wechatpy create_reply(text, msg).render()）。
func textReplyXML(fromUser, toUser, content string) string {
	return fmt.Sprintf(`<xml><ToUserName><![CDATA[%s]]></ToUserName><FromUserName><![CDATA[%s]]></FromUserName><CreateTime>%d</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[%s]]></Content></xml>`,
		fromUser, toUser, nowUnix(), content)
}

// imageReplyXML 构建图片被动回复 XML（对应 wechatpy ImageReply.render()）。
func imageReplyXML(fromUser, toUser, mediaID string) string {
	return fmt.Sprintf(`<xml><ToUserName><![CDATA[%s]]></ToUserName><FromUserName><![CDATA[%s]]></FromUserName><CreateTime>%d</CreateTime><MsgType><![CDATA[image]]></MsgType><Image><MediaId><![CDATA[%s]]></MediaId></Image></xml>`,
		fromUser, toUser, nowUnix(), mediaID)
}

// voiceReplyXML 构建语音被动回复 XML（对应 wechatpy VoiceReply.render()）。
func voiceReplyXML(fromUser, toUser, mediaID string) string {
	return fmt.Sprintf(`<xml><ToUserName><![CDATA[%s]]></ToUserName><FromUserName><![CDATA[%s]]></FromUserName><CreateTime>%d</CreateTime><MsgType><![CDATA[voice]]></MsgType><Voice><MediaId><![CDATA[%s]]></MediaId></Voice></xml>`,
		fromUser, toUser, nowUnix(), mediaID)
}

// nowUnix 返回当前秒级时间戳。
func nowUnix() int64 { return time.Now().Unix() }

// encryptMessage 将明文回复 XML 加密为安全模式 XML。
// 协议：AES-256-CBC（密钥为 EncodingAESKey base64 补位解码，IV 取密钥前 16 字节）、
// PKCS7 填充 32 字节块、密文结构 = 16 字节随机串 + 4 字节明文长度 + 明文 + appId；
// 签名 msg_signature = sha1(sort(token, timestamp, nonce, encrypt))。
// 对应 wechatpy WeChatCrypto.encrypt_message（实现参照宿主 wecom/wxcrypt.go 的 EncryptMessage）。
func (a *Adapter) encryptMessage(plainXML, nonce, timestamp string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(a.account.EncodingAESKey + "=")
	if err != nil || len(key) != 32 {
		return "", fmt.Errorf("无效的 EncodingAESKey")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("生成随机串失败: %w", err)
	}
	buf := make([]byte, 0, 16+4+len(plainXML)+len(a.account.AppId))
	buf = append(buf, random...)
	var lenBytes [4]byte
	// 明文长度写入 4 字节网络字节序字段；回复消息远小于 4GiB，无需溢出检查。
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(plainXML)))
	buf = append(buf, lenBytes[:]...)
	buf = append(buf, plainXML...)
	buf = append(buf, a.account.AppId...)

	// PKCS7 填充，块大小 32（AES-256）
	pad := 32 - len(buf)%32
	if pad == 0 {
		pad = 32
	}
	for i := 0; i < pad; i++ {
		buf = append(buf, byte(pad))
	}

	out := make([]byte, len(buf))
	// #nosec G407 -- IV 为微信协议固定的"密钥前 16 字节"派生值，非可配置的硬编码 IV
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(out, buf)
	encrypt := base64.StdEncoding.EncodeToString(out)

	signature := computeSHA1(a.account.PrivateToken, timestamp, nonce, encrypt)
	return fmt.Sprintf(`<xml><Encrypt><![CDATA[%s]]></Encrypt><MsgSignature><![CDATA[%s]]></MsgSignature><TimeStamp>%s</TimeStamp><Nonce><![CDATA[%s]]></Nonce></xml>`,
		encrypt, signature, timestamp, nonce), nil
}

// maybeEncrypt 若配置了 EncodingAESKey 且回复为明文 XML，则加密；
// 空回复回退 "success"。对应本体 _maybe_encrypt。
func (a *Adapter) maybeEncrypt(replyXML, nonce, timestamp string) string {
	if replyXML == "" {
		return "success"
	}
	if a.account.EncodingAESKey != "" && !strings.Contains(replyXML, "<Encrypt>") && nonce != "" && timestamp != "" {
		enc, err := a.encryptMessage(replyXML, nonce, timestamp)
		if err != nil {
			logger.Error("被动回复加密失败: %v", err)
			return "success"
		}
		return enc
	}
	return replyXML
}
