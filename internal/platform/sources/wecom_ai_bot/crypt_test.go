package wecom_ai_bot

import (
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"
)

// makeTestJSONCrypt 构造测试用 JSON 加解密器。
func makeTestJSONCrypt(t *testing.T) *WXBizJsonMsgCrypt {
	t.Helper()
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatal(err)
	}
	encodingAESKey := base64.StdEncoding.EncodeToString(keyBytes)[:43]
	c, err := NewWXBizJsonMsgCrypt("test_token", encodingAESKey, "")
	if err != nil {
		t.Fatalf("构造加解密器失败: %v", err)
	}
	return c
}

// TestJSONCryptEncryptDecryptRoundTrip 加密解密往返。
func TestJSONCryptEncryptDecryptRoundTrip(t *testing.T) {
	c := makeTestJSONCrypt(t)
	plain := `{"msgtype":"text","text":{"content":"你好"}}`
	ret, encrypted := c.EncryptMsg(plain, "nonce123", "1700000000")
	if ret != MsgCryptOK || encrypted == "" {
		t.Fatalf("加密失败: ret=%d", ret)
	}
	// 响应包含所有字段
	for _, field := range []string{`"encrypt"`, `"msgsignature"`, `"timestamp"`, `"nonce"`} {
		if !strings.Contains(encrypted, field) {
			t.Errorf("加密响应缺少字段 %s: %s", field, encrypted)
		}
	}
	ret, decrypted := c.DecryptMsg([]byte(encrypted), "", "", "")
	_ = decrypted
	if ret != MsgCryptValidateSignatureError {
		t.Errorf("错误签名应返回 -40001，got %d", ret)
	}

	// 正确的签名验证与解密
	ret, sig := c.GetSHA1("1700000000", "nonce123", extractEncryptField(t, encrypted))
	if ret != MsgCryptOK {
		t.Fatal("GetSHA1 失败")
	}
	ret, decrypted = c.DecryptMsg([]byte(encrypted), sig, "1700000000", "nonce123")
	if ret != MsgCryptOK || decrypted != plain {
		t.Errorf("解密失败: ret=%d decrypted=%q", ret, decrypted)
	}
}

// extractEncryptField 从加密响应 JSON 中提取 encrypt 字段。
func extractEncryptField(t *testing.T, encryptedJSON string) string {
	t.Helper()
	// 简化解析：找到 "encrypt": "..." 的引号内容
	idx := strings.Index(encryptedJSON, `"encrypt": "`)
	if idx < 0 {
		t.Fatalf("缺少 encrypt 字段: %s", encryptedJSON)
	}
	rest := encryptedJSON[idx+len(`"encrypt": "`):]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("encrypt 字段格式异常: %s", encryptedJSON)
	}
	return rest[:end]
}

// TestJSONCryptVerifyURL URL 验证：echostr 解密往返。
func TestJSONCryptVerifyURL(t *testing.T) {
	c := makeTestJSONCrypt(t)
	// 手工构造 echostr：随机16位 + 长度 + 明文 + "" receiveid，加密
	ret, echostr := c.encrypt("verify_me")
	if ret != MsgCryptOK {
		t.Fatalf("加密 echostr 失败: %d", ret)
	}
	timestamp := "1700000001"
	nonce := "nonce456"
	_, sig := c.GetSHA1(timestamp, nonce, echostr)

	ret, result := c.VerifyURL(sig, timestamp, nonce, echostr)
	if ret != MsgCryptOK || result != "verify_me" {
		t.Errorf("VerifyURL 失败: ret=%d result=%q", ret, result)
	}

	// 错误签名
	ret, _ = c.VerifyURL("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", timestamp, nonce, echostr)
	if ret != MsgCryptValidateSignatureError {
		t.Errorf("错误签名应返回 -40001，got %d", ret)
	}
}

// TestJSONCryptExtractEncrypt 提取密文字段。
func TestJSONCryptExtractEncrypt(t *testing.T) {
	post := []byte(`{"encrypt":"abc123","timestamp":"1"}`)
	ret, encrypt := ExtractEncrypt(post)
	if ret != MsgCryptOK || encrypt != "abc123" {
		t.Errorf("ExtractEncrypt 失败: ret=%d encrypt=%q", ret, encrypt)
	}
	ret, _ = ExtractEncrypt([]byte(`{"foo":"bar"}`))
	if ret != MsgCryptParseJSONError {
		t.Errorf("缺少 encrypt 应返回 -40002，got %d", ret)
	}
}

// TestJSONCryptDecryptMessage 完整的 DecryptMsg 流程（模拟回调 POST 数据）。
func TestJSONCryptDecryptMessage(t *testing.T) {
	c := makeTestJSONCrypt(t)
	plain := `{"msgtype":"text","text":{"content":"hello"}}`
	timestamp := "1700000002"
	nonce := "nonce789"

	// 加密并构造 POST 数据（encrypt + 签名）
	ret, encrypted := c.EncryptMsg(plain, nonce, timestamp)
	if ret != MsgCryptOK {
		t.Fatal("加密失败")
	}
	ret, sig := c.GetSHA1(timestamp, nonce, extractEncryptField(t, encrypted))
	if ret != MsgCryptOK {
		t.Fatal("签名失败")
	}

	postData := []byte(`{"encrypt":"` + extractEncryptField(t, encrypted) + `"}`)
	ret, decrypted := c.DecryptMsg(postData, sig, timestamp, nonce)
	if ret != MsgCryptOK || decrypted != plain {
		t.Errorf("DecryptMsg 失败: ret=%d decrypted=%q", ret, decrypted)
	}
}

// TestJSONCryptIllegalBuffer 解密非法数据返回 -40008。
func TestJSONCryptIllegalBuffer(t *testing.T) {
	c := makeTestJSONCrypt(t)
	// 正常加密后篡改密文
	ret, encrypted := c.encrypt("hello")
	if ret != MsgCryptOK {
		t.Fatal("加密失败")
	}
	// 篡改一个字节（base64 解码后翻转）
	raw, _ := base64.StdEncoding.DecodeString(encrypted)
	if raw[len(raw)-1] == 0x01 {
		raw[len(raw)-1] = 0x02
	} else {
		raw[len(raw)-1] = 0x01
	}
	tampered := base64.StdEncoding.EncodeToString(raw)
	timestamp := "1"
	nonce := "n"
	_, sig := c.GetSHA1(timestamp, nonce, tampered)
	ret, _ = c.DecryptMsg([]byte(`{"encrypt":"`+tampered+`"}`), sig, timestamp, nonce)
	if ret == MsgCryptOK {
		t.Error("篡改数据不应解密成功")
	}
}
