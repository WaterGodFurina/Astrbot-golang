// stdio_log_filter.go — go-plugin client logger 过滤器。
//
// Python 插件 SDK 的 GRPCStdio 保活流（stdio.py）每秒 yield 一个空的
// StdioData（channel 未设 = 枚举默认 0 = INVALID），go-plugin 宿主端
// grpc_stdio.go recvLoop 对每条打 `unknown channel, dropping` WARN，
// 十几个插件即每秒十几条刷屏。此处包装 hclog.Logger 吞掉该条特定消息，
// 其余日志原样透传（SDK 侧修正前先在宿主侧降噪；无需发 SDK 新版）。
package plugin

import (
	"strings"

	"github.com/hashicorp/go-hclog"
)

// stdioFilterLogger 包装 hclog.Logger，仅丢弃 go-plugin stdio 对 SDK
// 保活空包（channel=INVALID）的告警。
type stdioFilterLogger struct {
	hclog.Logger
}

// drop 判断该消息是否应被过滤。
func (l stdioFilterLogger) drop(msg string, args []interface{}) bool {
	if msg != "unknown channel, dropping" {
		return false
	}
	// 仅过滤 channel=INVALID 的空保活包；其余未知 channel 仍告警。
	for i := 0; i+1 < len(args); i += 2 {
		if key, ok := args[i].(string); ok && key == "channel" {
			return strings.HasSuffix(strings.ToLower(stringifyHCLogArg(args[i+1])), "invalid")
		}
	}
	return false
}

func stringifyHCLogArg(v interface{}) string {
	type stringer interface{ String() string }
	if s, ok := v.(stringer); ok {
		return s.String()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func (l stdioFilterLogger) Warn(msg string, args ...interface{}) {
	if l.drop(msg, args) {
		return
	}
	l.Logger.Warn(msg, args...)
}

func (l stdioFilterLogger) Log(level hclog.Level, msg string, args ...interface{}) {
	if level == hclog.Warn && l.drop(msg, args) {
		return
	}
	l.Logger.Log(level, msg, args...)
}

// Named/ResetNamed/With 必须重包装：go-plugin 经 Named("stdio") 派生
// stdio client 的 logger，直接透传会丢失过滤能力。

func (l stdioFilterLogger) Named(name string) hclog.Logger {
	return stdioFilterLogger{Logger: l.Logger.Named(name)}
}

func (l stdioFilterLogger) ResetNamed(name string) hclog.Logger {
	return stdioFilterLogger{Logger: l.Logger.ResetNamed(name)}
}

func (l stdioFilterLogger) With(args ...interface{}) hclog.Logger {
	return stdioFilterLogger{Logger: l.Logger.With(args...)}
}
