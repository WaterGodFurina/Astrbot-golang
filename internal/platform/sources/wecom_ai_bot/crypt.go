// 企业微信智能机器人消息加解密模块。
// 1:1 移植自 WXBizJsonMsgCrypt.py（企业微信官方 JSON 加解密示例）：
//   - AES-256-CBC，密钥为 EncodingAESKey base64 解码（32 字节），IV 取密钥前 16 字节；
//   - PKCS7 填充（块大小 32）；
//   - 签名：SHA1(token, timestamp, nonce, encrypt) 排序后拼接；
//   - 密文结构：16 位随机数字字符串 + 4 字节网络字节序长度 + 明文 + receiveid（本平台为空串）。
package wecom_ai_bot

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1" // #nosec G505 -- sha1 为企业微信智能机器人签名（协议要求），非密码学哈希用途
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 错误码（对应 ierror.py）
const (
	MsgCryptOK                     = 0
	MsgCryptValidateSignatureError = -40001 // 签名验证错误
	MsgCryptParseJSONError         = -40002 // json 解析失败
	MsgCryptComputeSignatureError  = -40003 // sha 加密生成签名失败
	MsgCryptIllegalAESKey          = -40004 // 无效的 aes key
	MsgCryptValidateCorpidError    = -40005 // 校验接收者失败（本平台 receiveid 为空串）
	MsgCryptEncryptAESError        = -40006 // 加密失败
	MsgCryptDecryptAESError        = -40007 // 解密失败
	MsgCryptIllegalBuffer          = -40008 // 非法数据（解密后结构校验失败）
)

// WXBizJsonMsgCrypt 企业微信智能机器人 JSON 消息加解密器。
type WXBizJsonMsgCrypt struct {
	token     string
	receiveID string // 本平台固定为空串
	key       []byte
}

// NewWXBizJsonMsgCrypt 构造加解密器。
func NewWXBizJsonMsgCrypt(token, encodingAESKey, receiveID string) (*WXBizJsonMsgCrypt, error) {
	key, err := decodeJSONAESKey(encodingAESKey)
	if err != nil {
		return nil, err
	}
	return &WXBizJsonMsgCrypt{token: token, receiveID: receiveID, key: key}, nil
}

// decodeJSONAESKey 解码 EncodingAESKey：base64 解码后必须是 32 字节
// （对应 Python `base64.b64decode(key + "=")` 补位）。
func decodeJSONAESKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	candidates := []string{s}
	if rem := len(s) % 4; rem != 0 {
		candidates = append(candidates, s+strings.Repeat("=", 4-rem))
	}
	for _, c := range candidates {
		if b, err := base64.StdEncoding.DecodeString(c); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	return nil, errors.New("[error]: EncodingAESKey invalid")
}

// GetSHA1 计算安全签名：SHA1 对 [token, timestamp, nonce, encrypt] 排序后拼接的字符串。
// 对应 SHA1.getSHA1，返回 (错误码, 签名)。
func (c *WXBizJsonMsgCrypt) GetSHA1(timestamp, nonce, encrypt string) (int, string) {
	arr := []string{c.token, timestamp, nonce, encrypt}
	sort.Strings(arr)
	// #nosec G401 -- sha1 为企业微信智能机器人签名协议要求的签名算法，非密码学哈希用途
	sum := sha1.Sum([]byte(strings.Join(arr, ""))) // nosemgrep: go.lang.security.audit.crypto.use_of_weak_crypto.use-of-sha1
	return MsgCryptOK, hex.EncodeToString(sum[:])
}

// random16Digit 生成 16 位随机数字字符串（对应 Prpcrypt.get_random_str）。
func random16Digit() string {
	// [1000000000000000, 9999999999999999]
	max := big.NewInt(9000000000000000)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return "1000000000000000"
	}
	return strconv.FormatInt(n.Int64()+1000000000000000, 10)
}

// encrypt 加密明文：16 位随机数字 + 4 字节网络字节序长度 + 明文 + receiveid，
// PKCS7 填充（块大小 32）后 AES-256-CBC 加密并 base64。
// 对应 Prpcrypt.encrypt，返回 (错误码, base64 密文)。
func (c *WXBizJsonMsgCrypt) encrypt(text string) (int, string) {
	random := []byte(random16Digit())
	content := []byte(text)
	buf := make([]byte, 0, 16+4+len(content)+len(c.receiveID))
	buf = append(buf, random...)
	var lenBytes [4]byte
	// 明文长度写入 4 字节网络字节序字段（协议要求 uint32）。明文为普通
	// 回调消息，远小于 4GiB，uint32 转换不可能溢出，故无需边界校验。
	binary.BigEndian.PutUint32(lenBytes[:], uint32(len(content)))
	buf = append(buf, lenBytes[:]...)
	buf = append(buf, content...)
	buf = append(buf, c.receiveID...)

	padded := pkcs7Encode32(buf)
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return MsgCryptEncryptAESError, ""
	}
	out := make([]byte, len(padded))
	// #nosec G407 -- IV 为企业微信协议固定的"密钥前 16 字节"派生值，非可配置的硬编码 IV
	cipher.NewCBCEncrypter(block, c.key[:16]).CryptBlocks(out, padded)
	return MsgCryptOK, base64.StdEncoding.EncodeToString(out)
}

// decrypt 解密 base64 密文：去填充 → 去 16 位随机串 → 解析长度 → 校验 receiveid。
// 对应 Prpcrypt.decrypt，返回 (错误码, 明文)。
func (c *WXBizJsonMsgCrypt) decrypt(text string) (int, string) {
	ct, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return MsgCryptDecryptAESError, ""
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return MsgCryptDecryptAESError, ""
	}
	block, err := aes.NewCipher(c.key)
	if err != nil {
		return MsgCryptDecryptAESError, ""
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, c.key[:16]).CryptBlocks(pt, ct)

	// 去除填充（Python: pad = ord(decrypted[-1]); if pad < 1 or pad > 32: pad = 0）
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > 32 || pad > len(pt) {
		return MsgCryptIllegalBuffer, ""
	}
	for i := len(pt) - pad; i < len(pt); i++ {
		if pt[i] != byte(pad) {
			return MsgCryptIllegalBuffer, ""
		}
	}
	if len(pt) < 16+pad {
		return MsgCryptIllegalBuffer, ""
	}
	// 去掉 16 位随机字符串
	content := pt[16 : len(pt)-pad]
	if len(content) < 4 {
		return MsgCryptIllegalBuffer, ""
	}
	msgLenUint := binary.BigEndian.Uint32(content[:4])
	// 对端声明的明文长度必须落在剩余字节范围内（len(content)-4 已确保非负）。
	if msgLenUint > uint32(len(content)-4) {
		return MsgCryptIllegalBuffer, ""
	}
	msgLen := int(msgLenUint)
	jsonContent := string(content[4 : 4+msgLen])
	fromReceiveID := string(content[4+msgLen:])
	if fromReceiveID != c.receiveID {
		return MsgCryptValidateCorpidError, ""
	}
	return MsgCryptOK, jsonContent
}

// encryptMessageJSON 生成加密响应 JSON（与 Python 模板格式一致）。
func encryptMessageJSON(encrypt, signature, timestamp, nonce string) string {
	return fmt.Sprintf(`{
        "encrypt": "%s",
        "msgsignature": "%s",
        "timestamp": "%s",
        "nonce": "%s"
    }`, encrypt, signature, timestamp, nonce)
}

// ExtractEncrypt 从 POST 数据 JSON 中提取密文字段（对应 JsonParse.extract）。
func ExtractEncrypt(postData []byte) (int, string) {
	var body struct {
		Encrypt string `json:"encrypt"`
	}
	if err := json.Unmarshal(postData, &body); err != nil {
		return MsgCryptParseJSONError, ""
	}
	if body.Encrypt == "" {
		return MsgCryptParseJSONError, ""
	}
	return MsgCryptOK, body.Encrypt
}

// VerifyURL 验证回调 URL：校验签名并解密 echostr。
// 对应 WXBizJsonMsgCrypt.VerifyURL，返回 (错误码, 解密后的 echostr)。
func (c *WXBizJsonMsgCrypt) VerifyURL(msgSignature, timestamp, nonce, echostr string) (int, string) {
	ret, signature := c.GetSHA1(timestamp, nonce, echostr)
	if ret != MsgCryptOK {
		return ret, ""
	}
	if subtle.ConstantTimeCompare([]byte(signature), []byte(msgSignature)) != 1 {
		return MsgCryptValidateSignatureError, ""
	}
	return c.decrypt(echostr)
}

// EncryptMsg 加密回复消息并包装为 JSON。
// 对应 WXBizJsonMsgCrypt.EncryptMsg，返回 (错误码, JSON 字符串)。
func (c *WXBizJsonMsgCrypt) EncryptMsg(replyMsg, nonce, timestamp string) (int, string) {
	ret, encrypt := c.encrypt(replyMsg)
	if ret != MsgCryptOK {
		return ret, ""
	}
	if timestamp == "" {
		timestamp = strconv.FormatInt(time.Now().Unix(), 10)
	}
	ret, signature := c.GetSHA1(timestamp, nonce, encrypt)
	if ret != MsgCryptOK {
		return ret, ""
	}
	return MsgCryptOK, encryptMessageJSON(encrypt, signature, timestamp, nonce)
}

// DecryptMsg 解密回调 POST 数据：提取密文 → 校验签名 → 解密。
// 对应 WXBizJsonMsgCrypt.DecryptMsg，返回 (错误码, 明文)。
func (c *WXBizJsonMsgCrypt) DecryptMsg(postData []byte, msgSignature, timestamp, nonce string) (int, string) {
	ret, encrypt := ExtractEncrypt(postData)
	if ret != MsgCryptOK {
		return ret, ""
	}
	ret, signature := c.GetSHA1(timestamp, nonce, encrypt)
	if ret != MsgCryptOK {
		return ret, ""
	}
	if subtle.ConstantTimeCompare([]byte(signature), []byte(msgSignature)) != 1 {
		return MsgCryptValidateSignatureError, ""
	}
	return c.decrypt(encrypt)
}

// pkcs7Encode32 按 32 字节块进行 PKCS7 填充。
func pkcs7Encode32(data []byte) []byte {
	amount := 32 - len(data)%32
	if amount == 0 {
		amount = 32
	}
	return append(data, bytes.Repeat([]byte{byte(amount)}, amount)...)
}
