package plugin

import (
	"errors"
	"testing"
)

// TestIsProtocolMismatchErr 锁定启动兜底的触发判定：只有 Register 报告
// SDK 协议版本不匹配（旧编译二进制）才触发自动重编译，其它错误不误伤。
func TestIsProtocolMismatchErr(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{
			errors.New("plugin a Register: rpc error: code = FailedPrecondition desc = protocol version mismatch: SDK(plugin)=0 Host(P1)=2; please upgrade the SDK or Host to the same protocol version"),
			true,
		},
		{errors.New("some unrelated plugin error"), false},
		{nil, false},
		// 大小写敏感：仅匹配宿主/SDK 产出的原文，避免把用户报错文本误判为可自愈。
		{errors.New("PROTOCOL VERSION MISMATCH"), false},
	}
	for i, c := range cases {
		if got := isProtocolMismatchErr(c.err); got != c.want {
			t.Fatalf("case %d: isProtocolMismatchErr(%v) = %v, want %v", i, c.err, got, c.want)
		}
	}
}
