package dingtalk

import (
	"os"
	"testing"
)

// TestMain 隔离数据目录：把 ASTRBOT_DATA_PATH 指向临时目录，避免适配器默认
// 数据目录把 state.json 等运行时文件写进包目录（此类文件曾被误提交）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "astrbot-dingtalk-test-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("ASTRBOT_DATA_PATH", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
