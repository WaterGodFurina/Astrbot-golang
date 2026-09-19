// AstrBot 数据目录解析（对齐 Python astrbot/core/utils/astrbot_path.py）。
// 统一在此集中解析，避免各处写死相对 CWD 的 "data/..." 字面量——进程 CWD
// 非项目根时，那些字面量会指向错误位置（数据"凭空消失"/重复创建空目录）。
package utils

import (
	"os"
	"path/filepath"
	"strings"
)

// AstrbotRoot 返回 AstrBot 根目录：优先 ASTRBOT_ROOT 环境变量，否则进程
// 当前工作目录（对齐 Python get_astrbot_root）。返回值尽量为绝对路径。
func AstrbotRoot() string {
	if root := strings.TrimSpace(os.Getenv("ASTRBOT_ROOT")); root != "" {
		if abs, err := filepath.Abs(root); err == nil {
			return abs
		}
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// AstrbotDataDir 返回数据目录：优先 ASTRBOT_DATA_PATH（既有 Go 侧约定，见
// t2i/tts/weixin_oc），否则 <AstrbotRoot>/data。
func AstrbotDataDir() string {
	if dp := strings.TrimSpace(os.Getenv("ASTRBOT_DATA_PATH")); dp != "" {
		if abs, err := filepath.Abs(dp); err == nil {
			return abs
		}
		return dp
	}
	return filepath.Join(AstrbotRoot(), "data")
}

// DataPath 拼接数据目录下的相对路径（自动处理平台分隔符）。
func DataPath(elem ...string) string {
	return filepath.Join(append([]string{AstrbotDataDir()}, elem...)...)
}
