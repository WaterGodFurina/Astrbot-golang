package wecom

import (
	"strings"
	"testing"
)

// TestParseWecomTextMessage 解析文本消息 XML。
func TestParseWecomTextMessage(t *testing.T) {
	xmlData := `<xml>
<ToUserName><![CDATA[corpid]]></ToUserName>
<FromUserName><![CDATA[zhangsan]]></FromUserName>
<CreateTime>1348831860</CreateTime>
<MsgType><![CDATA[text]]></MsgType>
<Content><![CDATA[this is a test]]></Content>
<MsgId>1234567890123456</MsgId>
<AgentID>1000002</AgentID>
</xml>`
	msg, err := ParseWecomMessage(xmlData)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if msg.Type != "text" || msg.Content != "this is a test" {
		t.Errorf("解析结果异常: %+v", msg)
	}
	if msg.Source != "zhangsan" || msg.ID != "1234567890123456" || msg.Agent != "1000002" {
		t.Errorf("解析字段异常: %+v", msg)
	}
	if msg.Time != 1348831860 {
		t.Errorf("时间戳异常: %d", msg.Time)
	}
}

// TestParseWecomImageMessage 解析图片消息 XML。
func TestParseWecomImageMessage(t *testing.T) {
	xmlData := `<xml>
<ToUserName><![CDATA[corpid]]></ToUserName>
<FromUserName><![CDATA[lisi]]></FromUserName>
<CreateTime>1348831860</CreateTime>
<MsgType><![CDATA[image]]></MsgType>
<PicUrl><![CDATA[https://example.com/pic.jpg]]></PicUrl>
<MediaId><![CDATA[media_id_1]]></MediaId>
<MsgId>2234567890123456</MsgId>
<AgentID>1000002</AgentID>
</xml>`
	msg, err := ParseWecomMessage(xmlData)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if msg.Type != "image" || msg.PicURL != "https://example.com/pic.jpg" || msg.MediaID != "media_id_1" {
		t.Errorf("解析结果异常: %+v", msg)
	}
}

// TestParseWecomKFEvent 解析 kf_msg_or_event 事件。
func TestParseWecomKFEvent(t *testing.T) {
	xmlData := `<xml>
<ToUserName><![CDATA[corpid]]></ToUserName>
<FromUserName><![CDATA[sys]]></FromUserName>
<CreateTime>1348831860</CreateTime>
<MsgType><![CDATA[event]]></MsgType>
<Event><![CDATA[kf_msg_or_event]]></Event>
<Token><![CDATA[TOKEN_123]]></Token>
<OpenKfId><![CDATA[wkxxxxx]]></OpenKfId>
</xml>`
	msg, err := ParseWecomMessage(xmlData)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if msg.Event != "kf_msg_or_event" || msg.Token != "TOKEN_123" || msg.OpenKfID != "wkxxxxx" {
		t.Errorf("解析结果异常: %+v", msg)
	}
	if !msg.IsKFMsgOrEvent() {
		t.Error("IsKFMsgOrEvent 应为 true")
	}
}

// TestSplitPlain 长文本分割：长度限制与标点分割。
func TestSplitPlain(t *testing.T) {
	// 短文本不分割
	short := "hello"
	if got := SplitPlain(short); len(got) != 1 || got[0] != short {
		t.Errorf("短文本不应分割: %v", got)
	}
	// 长文本分割
	long := strings.Repeat("a", 2050)
	chunks := SplitPlain(long)
	if len(chunks) != 2 {
		t.Errorf("应分为 2 段，got %d", len(chunks))
	}
	if len(chunks[0]) > 2048 {
		t.Errorf("第一段超过 2048: %d", len(chunks[0]))
	}
	// 标点优先分割
	punctuated := strings.Repeat("a", 2040) + "。" + strings.Repeat("b", 100)
	chunks = SplitPlain(punctuated)
	if len(chunks) != 2 {
		t.Fatalf("应分为 2 段，got %d", len(chunks))
	}
	if !strings.HasSuffix(chunks[0], "。") {
		t.Errorf("应在标点处分割: %q", chunks[0][len(chunks[0])-5:])
	}
}

// TestExtractWecomMediaFilename 从 Content-Disposition 提取文件名。
func TestExtractWecomMediaFilename(t *testing.T) {
	cases := []struct {
		disposition string
		want        string
	}{
		{`attachment; filename="report.pdf"`, "report.pdf"},
		{`attachment; filename*=UTF-8''%E6%B5%8B%E8%AF%95.txt`, "测试.txt"},
		{`inline; filename="a/b/c.txt"`, "c.txt"},
		{"", ""},
	}
	for _, c := range cases {
		got := ExtractWecomMediaFilename(c.disposition)
		if got != c.want {
			t.Errorf("disposition=%q: got %q want %q", c.disposition, got, c.want)
		}
	}
}

// TestSplitPlainBoundary 恰好 2048 字符不分割。
func TestSplitPlainBoundary(t *testing.T) {
	text := strings.Repeat("x", 2048)
	chunks := SplitPlain(text)
	if len(chunks) != 1 {
		t.Errorf("2048 字符不应分割: %d", len(chunks))
	}
}
