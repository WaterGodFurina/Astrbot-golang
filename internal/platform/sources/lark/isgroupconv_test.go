package lark

import "testing"

// TestIsGroupConvUniqueSession 验证开启 unique_session 后群会话 id
// "sender%oc_group" 仍被识别为群聊（对齐 Python send_by_session 用
// session.message_type 判群）。
func TestIsGroupConvUniqueSession(t *testing.T) {
	a := &Adapter{}
	cases := []struct {
		sessionID string
		want      bool
	}{
		{"oc_group", true},
		{"ou_sender%oc_group", true},
		{"ou_user", false},
		{"ou_user%ou_peer", false},
		{"", false},
	}
	for _, c := range cases {
		if got := a.isGroupConv(c.sessionID); got != c.want {
			t.Errorf("isGroupConv(%q) = %v, want %v", c.sessionID, got, c.want)
		}
	}
}
