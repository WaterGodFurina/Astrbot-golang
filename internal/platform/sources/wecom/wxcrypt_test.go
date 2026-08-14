package wecom

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// newCipherBlock 构造 AES 块（测试辅助）。
func newCipherBlock(key []byte) (cipher.Block, error) { return aes.NewCipher(key) }

// aesCBCDecrypt AES-CBC 解密（测试辅助）。
func aesCBCDecrypt(block cipher.Block, key, ct []byte) []byte {
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, key[:16]).CryptBlocks(pt, ct)
	return pt
}

// sha1Hex 计算字符串的 SHA1 十六进制（测试辅助）。
func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// makeTestCrypto 构造测试用加解密器。
func makeTestCrypto(t *testing.T) *WXBizMsgCrypt {
	t.Helper()
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	encodingAESKey := base64.StdEncoding.EncodeToString(keyBytes)[:43]
	c, err := NewWXBizMsgCrypt("test_token", encodingAESKey, "ww1234567890abcdef")
	if err != nil {
		t.Fatalf("构造加解密器失败: %v", err)
	}
	return c
}

// encryptForTest 手工加密（与 WXBizMsgCrypt.Encrypt 相同流程，用于交叉验证）。
func encryptForTest(t *testing.T, c *WXBizMsgCrypt, plain string) string {
	t.Helper()
	enc, err := c.Encrypt(plain)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	return enc
}

// TestEncryptDecryptRoundTrip AES 加解密往返。
func TestEncryptDecryptRoundTrip(t *testing.T) {
	c := makeTestCrypto(t)
	plain := `<xml><ToUserName><![CDATA[toUser]]></ToUserName><Content><![CDATA[你好，AstrBot]]></Content></xml>`
	enc := encryptForTest(t, c, plain)
	dec, err := c.Decrypt(enc)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if dec != plain {
		t.Errorf("解密结果不一致: got %q want %q", dec, plain)
	}
}

// TestDecryptCorpIDMismatch 解密时 corpid 不匹配应报错。
func TestDecryptCorpIDMismatch(t *testing.T) {
	c := makeTestCrypto(t)
	enc, err := c.Encrypt("hello")
	if err != nil {
		t.Fatal(err)
	}
	// 篡改尾部（corpid 区域）：base64 解码后整体解密，但换个 receiveID 校验
	other, err := NewWXBizMsgCrypt("test_token", c.encodingAESKeyString(t), "wrong_corpid")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Decrypt(enc); err != ErrInvalidCorpID {
		t.Errorf("期望 ErrInvalidCorpID，got %v", err)
	}
}

// encodingAESKeyString 辅助：从已有实例反向取 key。
func (c *WXBizMsgCrypt) encodingAESKeyString(t *testing.T) string {
	t.Helper()
	// 重新构造：用同一 key base64（43 位截断）
	s := base64.StdEncoding.EncodeToString(c.key)
	return s[:43]
}

// TestSignatureVerification 签名校验：正确签名通过，错误签名拒绝。
func TestSignatureVerification(t *testing.T) {
	c := makeTestCrypto(t)
	timestamp := "1700000000"
	nonce := "1234567890"
	encrypt := "some_encrypted_payload"
	sig := c.GetSignature(timestamp, nonce, encrypt)
	if sig == "" || len(sig) != 40 {
		t.Fatalf("签名格式异常: %q", sig)
	}
	if c.GetSignature(timestamp, nonce, "tampered") == sig {
		t.Error("篡改数据后签名不应相同")
	}
	// 期望值与 Python 侧实现一致：sort([token, timestamp, nonce, encrypt]) 后 sha1
	want := sha1HexSorted([]string{"test_token", timestamp, nonce, encrypt})
	if sig != want {
		t.Errorf("签名不一致: got %s want %s", sig, want)
	}
}

// sha1HexSorted 本地计算排序后 SHA1（与 Python hashlib.sha1 等价验证）。
func sha1HexSorted(parts []string) string {
	arr := append([]string{}, parts...)
	for i := 0; i < len(arr); i++ {
		for j := i + 1; j < len(arr); j++ {
			if arr[j] < arr[i] {
				arr[i], arr[j] = arr[j], arr[i]
			}
		}
	}
	return sha1Hex(strings.Join(arr, ""))
}

// TestCheckSignatureEchostr URL 验证：CheckSignature 返回解密后的 echostr。
func TestCheckSignatureEchostr(t *testing.T) {
	c := makeTestCrypto(t)
	timestamp := "1700000000"
	nonce := "abcdef"
	echostr := encryptForTest(t, c, "verify_me_123456")
	sig := c.GetSignature(timestamp, nonce, echostr)
	dec, err := c.CheckSignature(sig, timestamp, nonce, echostr)
	if err != nil {
		t.Fatalf("CheckSignature 失败: %v", err)
	}
	if dec != "verify_me_123456" {
		t.Errorf("echostr 解密结果: got %q want %q", dec, "verify_me_123456")
	}

	// 错误签名应拒绝
	if _, err := c.CheckSignature("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", timestamp, nonce, echostr); err != ErrInvalidSignature {
		t.Errorf("期望 ErrInvalidSignature，got %v", err)
	}
}

// TestDecryptMessageXML 解密加密 XML 回调。
func TestDecryptMessageXML(t *testing.T) {
	c := makeTestCrypto(t)
	plain := `<xml><ToUserName><![CDATA[corp]]></ToUserName><FromUserName><![CDATA[user1]]></FromUserName><CreateTime>1700000000</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[hello]]></Content><MsgId>1234567890</MsgId><AgentID>1000002</AgentID></xml>`
	enc := encryptForTest(t, c, plain)
	xmlBody := `<xml><Encrypt><![CDATA[` + enc + `]]></Encrypt></xml>`
	timestamp := "1700000001"
	nonce := "nonce123"
	sig := c.GetSignature(timestamp, nonce, enc)
	dec, err := c.DecryptMessage([]byte(xmlBody), sig, timestamp, nonce)
	if err != nil {
		t.Fatalf("DecryptMessage 失败: %v", err)
	}
	if !strings.Contains(dec, "<Content><![CDATA[hello]]></Content>") {
		t.Errorf("解密内容异常: %s", dec)
	}
}

// TestEncryptMessageXML EncryptMessage 生成的 XML 可被 DecryptMessage 验证。
func TestEncryptMessageXML(t *testing.T) {
	c := makeTestCrypto(t)
	timestamp := "1700000002"
	nonce := "nonce456"
	wrapped, err := c.EncryptMessage("reply_content", nonce, timestamp)
	if err != nil {
		t.Fatalf("EncryptMessage 失败: %v", err)
	}
	if !strings.Contains(wrapped, "<Encrypt>") || !strings.Contains(wrapped, "<MsgSignature>") {
		t.Errorf("加密 XML 缺少节点: %s", wrapped)
	}
}

// TestDecodeAESKey 非 32 字节密钥应报错。
func TestDecodeAESKey(t *testing.T) {
	if _, err := NewWXBizMsgCrypt("t", "short_key", "c"); err != ErrInvalidAESKey {
		t.Errorf("期望 ErrInvalidAESKey，got %v", err)
	}
}

// TestPKCS7Layout 验证密文布局：随机串(16) + 长度(4) + 明文 + corpid。
func TestPKCS7Layout(t *testing.T) {
	c := makeTestCrypto(t)
	enc := encryptForTest(t, c, "payload123")
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	// 解密后长度必须是 32 的倍数（PKCS7 块大小 32）
	if len(raw)%32 != 0 {
		t.Errorf("密文长度 %d 不是 32 的倍数", len(raw))
	}
	// 单独解密查看内部结构
	block, _ := newCipherBlock(c.key)
	pt := aesCBCDecrypt(block, c.key, raw)
	pad := int(pt[len(pt)-1])
	pt = pt[:len(pt)-pad]
	contentLen := int(binary.BigEndian.Uint32(pt[16:20]))
	if string(pt[20:20+contentLen]) != "payload123" {
		t.Errorf("布局异常: content=%q", pt[20:20+contentLen])
	}
}

// TestDecryptRejectsInconsistentPadding 回归 L-45.3：篡改内部 padding 字节（保持末字节不变）
// 时，旧的"只校验末字节"实现会解密成功，加固后必须拒绝。
func TestDecryptRejectsInconsistentPadding(t *testing.T) {
	c := makeTestCrypto(t)
	enc, err := c.Encrypt("hello")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	// 篡改倒数第 2 个字节（属于 padding 区域），末字节（pad 值）保持不变
	raw[len(raw)-2] ^= 0xff
	tampered := base64.StdEncoding.EncodeToString(raw)
	if _, err := c.Decrypt(tampered); err != ErrBadPadding {
		t.Errorf("内部 padding 字节不一致时应返回 ErrBadPadding，got %v", err)
	}
}
