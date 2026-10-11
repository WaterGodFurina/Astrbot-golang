package dify

import (
	"io"
	"log"
)

// discardLogger 供测试注入的空日志。
func discardLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}
