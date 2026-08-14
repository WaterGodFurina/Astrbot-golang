// 企业微信（WeCom）消息加解密模块。
// 1:1 移植自 Python 的 wechatpy.enterprise.crypto.WeChatCrypto（WXBizMsgCrypt）：
//   - AES-256-CBC，密钥为 EncodingAESKey 做 base64 解码（32 字节），IV 取密钥前 16 字节；
//   - PKCS7 填充（块大小 32）；
//   - 签名：SHA1(token, timestamp, nonce, encrypt) 排序后拼接；
//   - 密文结构：16 字节随机串 + 4 字节网络字节序明文长度 + 明文 + corpid。
package wecom

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// 错误定义（与 wechatpy 的 InvalidSignatureException / InvalidCorpIdException 语义对应）
var (
	// ErrInvalidSignature 签名校验失败（msg_signature 与本地计算的 SHA1 不一致）
	ErrInvalidSignature = errors.New("签名校验失败，请检查配置")
	// ErrInvalidCorpID 解密后尾部携带的 corpid 与配置不一致
	ErrInvalidCorpID = errors.New("解密失败，corpid 不匹配，请检查配置")
	// ErrInvalidAESKey EncodingAESKey 无效（base64 解码后不是 32 字节）
	ErrInvalidAESKey = errors.New("EncodingAESKey 无效，应为 43 位 base64 字符串")
	// ErrBadPadding 解密后填充非法或数据被篡改
	ErrBadPadding = errors.New("解密失败，数据非法")
)

// WXBizMsgCrypt 企业微信应用消息加解密器（对应 wechatpy 的 WeChatCrypto）。
type WXBizMsgCrypt struct {
	token     string // 回调 Token
	receiveID string // 接收者 ID（企业微信应用为 corpid）
	key       []byte // AES-256 密钥
}

// NewWXBizMsgCrypt 构造加解密器。
// encodingAESKey 为 43 位 base64 字符串，Python 侧通过 `base64.b64decode(key + "=")` 补位解码。
func NewWXBizMsgCrypt(token, encodingAESKey, receiveID string) (*WXBizMsgCrypt, error) {
	key, err := decodeAESKey(encodingAESKey)
	if err != nil {
		return nil, err
	}
	return &WXBizMsgCrypt{
		token:     token,
		receiveID: receiveID,
		key:       key,
	}, nil
}

// decodeAESKey 解码 EncodingAESKey：base64 解码并校验为 32 字节。
func decodeAESKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	candidates := []string{s}
	if rem := len(s) % 4; rem != 0 {
		// 与 Python `key + "="` 一致的补位方式
		candidates = append(candidates, s+strings.Repeat("=", 4-rem))
	}
	for _, c := range candidates {
		if b, err := base64.StdEncoding.DecodeString(c); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, ErrInvalidAESKey
}

// GetSignature 计算安全签名：SHA1 对 [token, timestamp, nonce, encrypt] 排序后拼接的字符串。
func (c *WXBizMsgCrypt) GetSignature(timestamp, nonce, encrypt string) string {
	arr := []string{c.token, timestamp, nonce, encrypt}
	sort.Strings(arr)
	sum := sha1.Sum([]byte(strings.Join(arr, "")))
	return hex.EncodeToString(sum[:])
}

// Encrypt 加密明文：随机串(16) + 网络字节序长度(4) + 明文 + corpid，PKCS7 填充后 AES-256-CBC 加密并 base64。
func (c *WXBizMsgCrypt) Encrypt(raw string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("生成随机串失败: %w", err)
	}
	content := []byte(raw)
	buf := make([]byte, 0, 16+4+len(content)+len(c.receiveID))
	buf = append(buf, random...)
	var lenBytes [4]byte
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(content)))
	buf = append(buf, lenBytes[:]...)
	buf = append(buf, content...)
	buf = append(buf, c.receiveID...)

	padded := pkcs7Encode(buf, aes.BlockSize*2) // 块大小 32（AES-256）
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, c.key[:16]).CryptBlocks(out, padded)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Decrypt 解密 base64 密文，校验尾部 corpid 并返回明文。
func (c *WXBizMsgCrypt) Decrypt(encrypted string) (string, error) {
	ct, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", ErrBadPadding
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, c.key[:16]).CryptBlocks(pt, ct)

	// 去除 PKCS7 填充（Python: padding = plain_text[-1]; content = plain_text[16:-padding]）
	padLen := int(pt[len(pt)-1])
	if padLen <= 0 || padLen > 32 || padLen > len(pt) {
		return "", ErrBadPadding
	}
	pt = pt[:len(pt)-padLen]
	if len(pt) < 20 {
		return "", ErrBadPadding
	}
	contentLen := int(binary.BigEndian.Uint32(pt[16:20]))
	if contentLen > len(pt)-20 {
		return "", ErrBadPadding
	}
	content := string(pt[20 : 20+contentLen])
	fromCorpID := string(pt[20+contentLen:])
	if fromCorpID != c.receiveID {
		return "", ErrInvalidCorpID
	}
	return content, nil
}

// CheckSignature 验证 URL 有效性：校验 msg_signature（对 echostr 计算签名）并解密 echostr。
// 对应 Python WeChatCrypto.check_signature。
func (c *WXBizMsgCrypt) CheckSignature(msgSignature, timestamp, nonce, echostr string) (string, error) {
	if c.GetSignature(timestamp, nonce, echostr) != msgSignature {
		return "", ErrInvalidSignature
	}
	return c.Decrypt(echostr)
}

// decryptXML 回调 XML 的 Encrypt 节点
type decryptXML struct {
	Encrypt string `xml:"Encrypt"`
}

// DecryptMessage 解密回调 POST 数据（加密 XML）：校验签名后解密并返回明文 XML。
// 对应 Python WeChatCrypto.decrypt_message。
func (c *WXBizMsgCrypt) DecryptMessage(postData []byte, msgSignature, timestamp, nonce string) (string, error) {
	var xmlBody decryptXML
	if err := xml.Unmarshal(postData, &xmlBody); err != nil {
		return "", fmt.Errorf("解析回调 XML 失败: %w", err)
	}
	encrypt := strings.TrimSpace(xmlBody.Encrypt)
	if encrypt == "" {
		return "", errors.New("回调 XML 缺少 Encrypt 节点")
	}
	if c.GetSignature(timestamp, nonce, encrypt) != msgSignature {
		return "", ErrInvalidSignature
	}
	return c.Decrypt(encrypt)
}

// EncryptMessage 加密回复消息并包装为加密 XML。
// 对应 Python WeChatCrypto.encrypt_message。
func (c *WXBizMsgCrypt) EncryptMessage(msg, nonce, timestamp string) (string, error) {
	encrypt, err := c.Encrypt(msg)
	if err != nil {
		return "", err
	}
	signature := c.GetSignature(timestamp, nonce, encrypt)
	return fmt.Sprintf(`<xml>
<Encrypt><![CDATA[%s]]></Encrypt>
<MsgSignature><![CDATA[%s]]></MsgSignature>
<TimeStamp>%s</TimeStamp>
<Nonce><![CDATA[%s]]></Nonce>
</xml>`, encrypt, signature, timestamp, nonce), nil
}

// pkcs7Encode 按 blockSize 进行 PKCS7 填充。
func pkcs7Encode(data []byte, blockSize int) []byte {
	amount := blockSize - len(data)%blockSize
	if amount == 0 {
		amount = blockSize
	}
	return append(data, bytes.Repeat([]byte{byte(amount)}, amount)...)
}
