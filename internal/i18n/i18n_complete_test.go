package i18n

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEnUSCompleteness: en_US.json must be a large, Chinese-free key set
// (before the translation pass it only had 155 keys and fell back to Chinese
// for ~365 log strings).
func TestEnUSCompleteness(t *testing.T) {
	enData, err := os.ReadFile("locales/en_US.json")
	if err != nil {
		t.Fatal(err)
	}
	var en map[string]string
	if err := json.Unmarshal(enData, &en); err != nil {
		t.Fatal(err)
	}
	if len(en) < 400 {
		t.Errorf("en_US.json only has %d keys; expected the full ~520 key set", len(en))
	}
	for key, val := range en {
		if containsCJK(val) {
			t.Errorf("en_US value for %q contains Chinese: %q", key, val)
		}
		_ = key
	}
}

// TestEnUSTranslationApplied: representative keys translate in the en_US locale.
func TestEnUSTranslationApplied(t *testing.T) {
	// The translator loads embedded locales via LoadEmbeddedLocales.
	if err := LoadEmbeddedLocales(); err != nil {
		t.Fatal(err)
	}
	SetLocale("en_US")
	// Direct literal keys (vet requires constant format strings).
	if got := Get("Discord 机器人已连接, self_id=%s", "x"); !strings.Contains(got, "Discord bot connected") {
		t.Errorf("en_US Discord key = %q", got)
	}
	if got := Get("验证请求有效性成功。"); !strings.Contains(got, "validated") {
		t.Errorf("en_US validation key = %q", got)
	}
}

func containsCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return false
}

// TestScopedI18nKeysTranslated 防回归：§3.8 子系统（sandbox/cron/lifecycle）
// 源码中以字符串字面量出现的 I18n* 日志 key，必须都能在 en_US.json 找到译文
// （否则英文环境下会回退输出中文原文）。历史上 lifecycle/cron/sandbox 里这
// 14 条日志就是这样漏译的。仅扫描字面量：动态拼接的 key 无法静态判定，跳过。
func TestScopedI18nKeysTranslated(t *testing.T) {
	enData, err := os.ReadFile("locales/en_US.json")
	if err != nil {
		t.Fatal(err)
	}
	var en map[string]string
	if err := json.Unmarshal(enData, &en); err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, pkgDir := range []string{"sandbox", "cron", "lifecycle"} {
		root := filepath.Join("..", pkgDir)
		err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			f, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return parseErr
			}
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				switch sel.Sel.Name {
				case "I18nInfo", "I18nWarn", "I18nError", "I18nDebug":
				default:
					return true
				}
				if len(call.Args) == 0 {
					return true
				}
				lit, ok := call.Args[0].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				key, unquoteErr := strconv.Unquote(lit.Value)
				if unquoteErr != nil {
					return true
				}
				if _, ok := en[key]; !ok {
					t.Errorf("I18n key missing en_US translation: %q (%s)", key, fset.Position(lit.Pos()))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
