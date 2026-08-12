package plugin

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/mod/modfile"
)

// PluginMetadata is the parsed contents of a plugin package's metadata.json.
// Every installable plugin package must ship a metadata.json at its root. It
// records the plugin's identity, source URL, and the cgo opt-in flag.
//
// It mirrors the Python AstrBot plugin metadata (name/desc/author/version/
// repo) and adds the Go-specific `cgo` field: when the plugin's dependencies
// require a C compiler (cgo), it MUST set `"cgo": true`; an empty/absent cgo
// defaults to false (pure Go, CGO_ENABLED=0).
type PluginMetadata struct {
	// Name is the plugin's canonical name (also used as the module package
	// name and the install id).
	Name string `json:"name"`
	// Description is a one-line summary of the plugin.
	Description string `json:"desc"`
	// Author of the plugin.
	Author string `json:"author"`
	// Version of the plugin (semver-ish string).
	Version string `json:"version"`
	// Repo is the plugin's source repository URL (used for reinstall/update
	// and docs fetching).
	Repo string `json:"repo"`
	// Homepage is optional project homepage/documentation URL.
	Homepage string `json:"homepage,omitempty"`
	// Entry is the Go source file to compile as the plugin's main package
	// (conventionally "main.go"). When empty the compiler builds ./... .
	Entry string `json:"entry,omitempty"`
	// Cgo indicates the plugin requires cgo and therefore a C compiler
	// (Clang/GCC) on the host to build. Defaults to false when absent.
	Cgo *bool `json:"cgo"`
}

// RequiresCgo reports whether the plugin opts into cgo. An absent/empty field
// means "no" (pure Go build).
func (m *PluginMetadata) RequiresCgo() bool {
	return m != nil && m.Cgo != nil && *m.Cgo
}

// validatePluginName rejects plugin names that could escape the per-plugin
// directory layout when joined into file paths. 允许字母数字与 - _，拒绝
// /、\、.、.. 与空白等目录穿越/歧义字符。
func validatePluginName(name string) error {
	t := strings.TrimSpace(name)
	if t == "" {
		return fmt.Errorf("插件名不能为空")
	}
	if strings.ContainsAny(t, "/\\") || strings.Contains(t, ".") ||
		strings.Contains(t, "..") || strings.ContainsFunc(t, unicode.IsSpace) {
		return fmt.Errorf("插件名包含非法字符（不允许 /、\\、.、.. 及空白）: %q", name)
	}
	return nil
}

// sanitizePluginName returns a name safe to join into file paths: valid names
// pass through unchanged; invalid ones (legacy manifests, tampered metadata,
// attacker-controlled Register() output) are replaced with plugin-<hash> so no
// path traversal or data-dir escape can occur.
func sanitizePluginName(name string) string {
	if validatePluginName(name) == nil {
		return strings.TrimSpace(name)
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("plugin-%x", sum[:8])
}

// moduleName returns a safe Go module path derived from the plugin name.
func (m *PluginMetadata) moduleName() string {
	return "example.com/astrbot-plugin/" + sanitizeID(strings.ToLower(m.Name))
}

// ReadPluginMetadata loads and validates a plugin's metadata.json from the
// extracted source directory. It returns an error describing the problem when
// the file is missing or malformed, so installers can surface a friendly
// message instead of a cryptic build failure.
func ReadPluginMetadata(srcDir string) (*PluginMetadata, error) {
	p := filepath.Join(srcDir, "metadata.json")
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("插件缺少 metadata.json（记录插件名称/地址等，参考 AstrBot 插件规范）")
		}
		return nil, fmt.Errorf("读取 metadata.json: %w", err)
	}
	var meta PluginMetadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("解析 metadata.json 失败: %w", err)
	}
	if strings.TrimSpace(meta.Name) == "" {
		return nil, fmt.Errorf("metadata.json 缺少必填字段 name")
	}
	meta.Name = strings.TrimSpace(meta.Name)
	// name 会被拼进 plugins_config/<name>/ 与 plugins/<name>/ 路径，必须是
	// 安全的目录名，拒绝路径穿越（/、\、..、.、空白）。
	if err := validatePluginName(meta.Name); err != nil {
		return nil, err
	}
	if meta.Cgo == nil {
		// 显式补齐：避免嵌套表示二义。为空则默认否。
		no := false
		meta.Cgo = &no
	}
	return &meta, nil
}

// ensureMainGo guards the requirement that every plugin zip has a main.go
// (identifiable entry) at its root, mirroring the metadata.json requirement.
func ensureMainGo(srcDir string) error {
	if _, err := os.Stat(filepath.Join(srcDir, "main.go")); err != nil {
		return fmt.Errorf("插件源码缺少 main.go（插件以 main.go 识别）")
	}
	return nil
}

// goModuleNameOf tries to read the plugin's own go.mod module path from the
// source tree; falls back to a generated module path based on the metadata
// name when there is no go.mod.
func goModuleNameOf(srcDir string, meta *PluginMetadata) string {
	if data, err := os.ReadFile(filepath.Join(srcDir, "go.mod")); err == nil {
		if f, err := modfile.Parse("go.mod", data, nil); err == nil && f.Module != nil && f.Module.Mod.Path != "" {
			return f.Module.Mod.Path
		}
	}
	if meta != nil {
		return meta.moduleName()
	}
	return "example.com/astrbot-plugin/plugin"
}
