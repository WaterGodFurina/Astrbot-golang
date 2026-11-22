package dashboard

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedWebUIComplete 保证 //go:embed 覆盖 web/dist 下的所有文件。
// 普通 `web/dist/*` 会跳过以 `_` / `.` 开头的文件，而 Vite 会产出
// `_commonjsHelpers-*.js` 这类被路由懒加载依赖的 chunk；一旦漏嵌，登录后
// 动态 import 会 404，页面卡在登录界面无法进入。
func TestEmbeddedWebUIComplete(t *testing.T) {
	disk := os.DirFS("web/dist")
	err := fs.WalkDir(disk, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := "web/dist/" + filepath.ToSlash(p)
		if _, err := webFS.ReadFile(name); err != nil {
			t.Errorf("embedded WebUI 缺少 %s（检查 //go:embed 是否使用 all: 前缀）: %v", p, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 web/dist 失败: %v", err)
	}
}
