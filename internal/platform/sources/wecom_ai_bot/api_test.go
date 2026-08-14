package wecom_ai_bot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamMessageBuilders 流消息构建器 JSON 结构。
func TestStreamMessageBuilders(t *testing.T) {
	builder := WecomAIBotStreamMessageBuilder{}

	text := builder.MakeTextStream("sid_1", "你好", false)
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if m["msgtype"] != "stream" {
		t.Errorf("msgtype: %v", m["msgtype"])
	}
	stream, _ := m["stream"].(map[string]interface{})
	if stream["id"] != "sid_1" || stream["finish"] != false || stream["content"] != "你好" {
		t.Errorf("stream 内容异常: %v", stream)
	}

	img := builder.MakeImageStream("sid_2", []byte("image-data"), true)
	if err := json.Unmarshal([]byte(img), &m); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	stream, _ = m["stream"].(map[string]interface{})
	items, _ := stream["msg_item"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("msg_item 数量: %d", len(items))
	}
	item, _ := items[0].(map[string]interface{})
	if item["msgtype"] != "image" {
		t.Errorf("item msgtype: %v", item["msgtype"])
	}
	image, _ := item["image"].(map[string]interface{})
	if image["md5"] != CalculateImageMD5([]byte("image-data")) {
		t.Errorf("md5 不一致")
	}

	mixed := builder.MakeMixedStream("sid_3", "content", nil, false)
	json.Unmarshal([]byte(mixed), &m)
	stream, _ = m["stream"].(map[string]interface{})
	if stream["content"] != "content" {
		t.Errorf("mixed content: %v", stream["content"])
	}
	// 空内容时不带 content 字段（对应 Python 条件判断）
	mixedEmpty := builder.MakeMixedStream("sid_4", "", nil, true)
	json.Unmarshal([]byte(mixedEmpty), &m)
	stream, _ = m["stream"].(map[string]interface{})
	if _, ok := stream["content"]; ok {
		t.Errorf("空内容不应包含 content 字段")
	}

	plain := builder.MakeText("hello")
	json.Unmarshal([]byte(plain), &m)
	if m["msgtype"] != "text" {
		t.Errorf("MakeText msgtype: %v", m["msgtype"])
	}
}

// TestMessageParser 消息解析器。
func TestMessageParser(t *testing.T) {
	parser := WecomAIBotMessageParser{}

	textData := map[string]interface{}{
		"msgtype": "text",
		"text":    map[string]interface{}{"content": "你好"},
	}
	if got := parser.ParseTextMessage(textData); got != "你好" {
		t.Errorf("ParseTextMessage: %q", got)
	}

	imgData := map[string]interface{}{
		"image": map[string]interface{}{"url": "https://example.com/a.png"},
	}
	if got := parser.ParseImageMessage(imgData); got != "https://example.com/a.png" {
		t.Errorf("ParseImageMessage: %q", got)
	}

	streamData := map[string]interface{}{
		"stream": map[string]interface{}{
			"id": "s1", "finish": true, "content": "c",
			"msg_item": []interface{}{map[string]interface{}{"msgtype": "image"}},
		},
	}
	parsed := parser.ParseStreamMessage(streamData)
	if parsed["id"] != "s1" || parsed["finish"] != true {
		t.Errorf("ParseStreamMessage: %v", parsed)
	}
	if len(parsed["msg_item"].([]interface{})) != 1 {
		t.Errorf("msg_item 数量异常")
	}

	mixedData := map[string]interface{}{
		"mixed": map[string]interface{}{
			"msg_item": []interface{}{map[string]interface{}{"msgtype": "text"}},
		},
	}
	if items := parser.ParseMixedMessage(mixedData); len(items) != 1 {
		t.Errorf("ParseMixedMessage: %v", items)
	}

	eventData := map[string]interface{}{"event": map[string]interface{}{"eventtype": "enter_chat"}}
	if ev := parser.ParseEventMessage(eventData); ev["eventtype"] != "enter_chat" {
		t.Errorf("ParseEventMessage: %v", ev)
	}
}

// TestProcessEncryptedImage 加密图片下载解密（httptest 模拟图片服务器）。
func TestProcessEncryptedImage(t *testing.T) {
	// 构造 AES-256 密钥与 IV（密钥前 16 字节）
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	plainImage := []byte("fake-encrypted-image-content")
	// PKCS7 填充（块大小 16 即可，Python 侧填充以密文长度为准）
	pad := 16 - len(plainImage)%16
	padded := append(plainImage, byte(pad))
	for i := 1; i < pad; i++ {
		padded = append(padded, byte(pad))
	}
	block, _ := aes.NewCipher(key)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key[:16]).CryptBlocks(ct, padded)
	encryptedBase64 := base64.StdEncoding.EncodeToString(ct)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(ct)
	}))
	defer srv.Close()

	client := NewWecomAIBotAPIClient("token", base64.StdEncoding.EncodeToString(key)[:43])
	ok, data := client.ProcessEncryptedImage(srv.URL, base64.StdEncoding.EncodeToString(key))
	if !ok {
		t.Fatalf("图片处理失败: %s", data)
	}
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("结果 base64 解码失败: %v", err)
	}
	if string(decoded) != "fake-encrypted-image-content" {
		t.Errorf("解密结果不一致: %q", decoded)
	}
	_ = encryptedBase64
}

// TestVerifyURLAPI API 客户端 URL 验证（含 API 客户端层面的封装）。
func TestVerifyURLAPI(t *testing.T) {
	c := makeTestJSONCrypt(t)
	client := NewWecomAIBotAPIClient("test_token", c.encodingAESKeyString(t))

	_, echostr := c.encrypt("echo_ok")
	timestamp := "1700000000"
	nonce := "nn"
	_, sig := c.GetSHA1(timestamp, nonce, echostr)

	if got := client.VerifyURL(sig, timestamp, nonce, echostr); got != "echo_ok" {
		t.Errorf("VerifyURL: %q", got)
	}
	if got := client.VerifyURL("bad", timestamp, nonce, echostr); got != "verify fail" {
		t.Errorf("错误签名应返回 verify fail: %q", got)
	}
}

// encodingAESKeyString 反向获取 EncodingAESKey（测试辅助）。
func (c *WXBizJsonMsgCrypt) encodingAESKeyString(t *testing.T) string {
	t.Helper()
	s := base64.StdEncoding.EncodeToString(c.key)
	return s[:43]
}

// TestEncryptMessageAPI API 客户端加密消息（验证响应可解密）。
func TestEncryptMessageAPI(t *testing.T) {
	c := makeTestJSONCrypt(t)
	client := NewWecomAIBotAPIClient("test_token", c.encodingAESKeyString(t))
	encrypted := client.EncryptMessage(`{"msgtype":"text"}`, "nonce", "timestamp")
	if encrypted == "" {
		t.Fatal("加密失败")
	}
	if !strings.Contains(encrypted, `"encrypt"`) {
		t.Errorf("加密响应格式异常: %s", encrypted)
	}
}
