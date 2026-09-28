package qqofficial

import "testing"

// TestBuildQuotedReplyMessageReference 覆盖引用消息 id 优先取
// message_reference.message_id（对齐 Python qqofficial_platform_adapter.py:773-819），
// 以及空引用返回 nil。
func TestBuildQuotedReplyMessageReference(t *testing.T) {
	a := &Adapter{}
	d := map[string]interface{}{
		"message_reference": map[string]interface{}{"message_id": "ref-1"},
		"msg_elements": []interface{}{
			map[string]interface{}{"id": "elem-1", "content": " hello "},
		},
	}
	c := a.buildQuotedReply(d)
	if c == nil {
		t.Fatal("expected non-nil reply")
	}
	if c.MessageID != "ref-1" {
		t.Errorf("MessageID = %q, want ref-1", c.MessageID)
	}
	if c.MessageStr != "hello" {
		t.Errorf("MessageStr = %q, want hello", c.MessageStr)
	}

	// 无 message_reference 时回退 msg_elements[0].id。
	d2 := map[string]interface{}{
		"msg_elements": []interface{}{
			map[string]interface{}{"id": "elem-2", "content": "x"},
		},
	}
	if c2 := a.buildQuotedReply(d2); c2 == nil || c2.MessageID != "elem-2" {
		t.Errorf("fallback reply = %#v, want MessageID elem-2", c2)
	}

	// 空引用 → nil（不回退空 Reply）。
	if c3 := a.buildQuotedReply(map[string]interface{}{}); c3 != nil {
		t.Errorf("empty reply = %#v, want nil", c3)
	}
}
