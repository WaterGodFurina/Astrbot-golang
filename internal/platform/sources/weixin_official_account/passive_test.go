package weixin_official_account

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	wxmp "github.com/blusewang/wx/mp_api"
)

// buildPassiveRequest 构造明文模式的 POST 回调请求。
func buildPassiveRequest(a *Adapter, from, content, msgID string) *http.Request {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "n"
	xmlStr := `<xml><ToUserName><![CDATA[gh_app]]></ToUserName><FromUserName><![CDATA[` + from + `]]></FromUserName>
<CreateTime>` + ts + `</CreateTime><MsgType><![CDATA[text]]></MsgType><Content><![CDATA[` + content + `]]></Content><MsgId>` + msgID + `</MsgId></xml>`
	return httptest.NewRequest(http.MethodPost,
		"/callback/command?signature="+sigOf(a.account.PrivateToken, ts, nonce)+"&timestamp="+ts+"&nonce="+nonce,
		strings.NewReader(xmlStr))
}

// TestSplitPlainPunctuation: 长文本按标点切分且单段不超过 1024 字符。
func TestSplitPlainPunctuation(t *testing.T) {
	long := strings.Repeat("你好。", 600) // 3000 字符
	chunks := splitPlain(long, 1024)
	if len(chunks) < 2 {
		t.Fatalf("3000 字符应切分为多段，实际 %d", len(chunks))
	}
	for i, chunk := range chunks {
		if n := len([]rune(chunk)); n > 1024 {
			t.Errorf("分段 %d 超长: %d", i, n)
		}
	}
	joined := strings.Join(chunks, "")
	if joined != long {
		t.Error("切分后拼接应还原原文")
	}
	// 无标点文本硬切。
	noPunct := strings.Repeat("甲", 3000)
	chunks = splitPlain(noPunct, 1024)
	if len(chunks) != 3 {
		t.Errorf("无标点 3000 字符应切 3 段（1024/1024/952），实际 %d", len(chunks))
	}
	// 短文本不切分。
	if chunks := splitPlain("短文本", 1024); len(chunks) != 1 || chunks[0] != "短文本" {
		t.Errorf("短文本不应切分: %v", chunks)
	}
}

// TestUserStateTakeReply: future XML 优先弹出、缓存逐条弹出并附加提示、耗尽判定。
func TestUserStateTakeReply(t *testing.T) {
	msg := &wxmp.MessageData{FromUserName: "u", ToUserName: "gh"}
	st := newUserState(msg, "1", "预览")
	if st.exhausted() {
		t.Error("处理中的新状态不应判定耗尽")
	}
	st.appendPending(strings.Repeat("段", 1100))
	st.finish() // 触发分段（2 段）
	// 第一段弹出后仍有剩余 → 附加缓冲提示。
	if xml, ok, _ := st.takeReply(); !ok || !strings.Contains(xml, cachedMoreSuffix) {
		t.Fatalf("首段弹出后仍有剩余应附加提示: %q", xml)
	}
	// 最后一段弹出后无剩余 → 无提示（empty 仅表示缓冲耗尽，属正常返回）。
	if xml, ok, _ := st.takeReply(); !ok || strings.Contains(xml, cachedMoreSuffix) {
		t.Fatalf("末段弹出不应附加提示: %q", xml)
	}
	if !st.exhausted() {
		t.Error("弹出完毕后应判定耗尽")
	}
	// future XML 优先。
	st.setFutureXML(imageReplyXML("gh", "u", "media-1"))
	if xml, ok, _ := st.takeReply(); !ok || !strings.Contains(xml, "media-1") {
		t.Fatalf("future XML 应优先弹出: %q", xml)
	}
	if !st.exhausted() {
		t.Error("future XML 弹出后应判定耗尽")
	}
}

// TestEncryptMessageRoundTrip: 回复加密后可被同一密钥解密还原。
func TestEncryptMessageRoundTrip(t *testing.T) {
	keyStr := testAESKey(t)
	a := New(map[string]interface{}{"id": "wx", "token": "tok", "appid": "wxappid", "encoding_aes_key": keyStr}, nil, nil)
	plain := textReplyXML("gh_app", "o_openid", "加密回复内容")
	enc, err := a.encryptMessage(plain, "nonce1", "1234567890")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(enc, "<Encrypt>") || !strings.Contains(enc, "<MsgSignature>") {
		t.Fatalf("加密 XML 结构不完整: %q", enc)
	}
	// 使用 validateCiphertext 同源的解密路径还原：走 SDK ShouldDecode。
	var md wxmp.MessageData
	md.Encrypt = encXMLField(enc, "Encrypt")
	if err := md.ShouldDecode(keyStr); err != nil {
		t.Fatalf("SDK 解密失败: %v", err)
	}
	if md.Content != "加密回复内容" || md.FromUserName != "o_openid" {
		t.Fatalf("解密内容不符: %+v", md)
	}
}

// encXMLField 从加密 XML 中提取指定节点的 CDATA 内容。
func encXMLField(encXML, field string) string {
	start := strings.Index(encXML, "<"+field+"><![CDATA[")
	if start < 0 {
		return ""
	}
	start += len("<" + field + "><![CDATA[")
	end := strings.Index(encXML[start:], "]]>")
	if end < 0 {
		return ""
	}
	return encXML[start : start+end]
}

// TestMaybeEncryptPlaintextFallback: 无 aeskey 时回复保持明文。
func TestMaybeEncryptPlaintextFallback(t *testing.T) {
	a := New(map[string]interface{}{"id": "wx", "token": "tok"}, nil, nil)
	plain := textReplyXML("gh", "u", "hi")
	if got := a.maybeEncrypt(plain, "n", "1"); got != plain {
		t.Fatalf("无 aeskey 应返回明文: %q", got)
	}
	if got := a.maybeEncrypt("", "n", "1"); got != "success" {
		t.Fatalf("空回复应为 success: %q", got)
	}
}
