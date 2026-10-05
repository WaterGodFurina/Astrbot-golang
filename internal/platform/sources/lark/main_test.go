package lark

import (
	"os"
	"testing"
)

// TestMain 隔离数据目录：把 ASTRBOT_DATA_PATH 指向临时目录，避免私聊路由
// 映射等运行时文件写进包目录（lark_private_chat_map.json 曾被误提交）。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "astrbot-lark-test-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("ASTRBOT_DATA_PATH", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
