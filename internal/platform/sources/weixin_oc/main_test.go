package weixin_oc

import (
	"os"
	"testing"
)

// TestMain 隔离数据目录：把 ASTRBOT_DATA_PATH 指向临时目录，避免适配器持久化
// 文件（account/ctx/syncbuf 等）写进包目录。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "astrbot-weixin-oc-test-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("ASTRBOT_DATA_PATH", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
