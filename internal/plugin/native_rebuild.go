package plugin

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NativeRebuildStatus 是 Native（进程内 .so）插件异步重编译的状态，供 WebUI
// 弹窗展示。宿主加载 .so 失败时（最常见：宿主 Go 版本升级后，旧的预编译 .so
// 与新宿主 ABI 不兼容，plugin.Open 报 "plugin was built with a different
// version of ..."），不阻塞启动/WebUI，改为后台重编译源码，并在此记录进度。
type NativeRebuildStatus struct {
	// State: "" (无) / "running" / "done" / "failed"。
	State string `json:"state"`
	// Message 面向用户的可读说明（中文，随前端 i18n 可后续本地化）。
	Message string `json:"message"`
	// Reason 是触发重编译的原始错误摘要（截断）。
	Reason string `json:"reason,omitempty"`
	// UpdatedAt 最近一次状态更新时间（Unix 秒）。
	UpdatedAt int64 `json:"updated_at"`
}

// nativeRebuildStatus 返回指定插件当前的重编译状态（nil 表示无）。
func (m *SubprocessManager) nativeRebuildStatus(id string) *NativeRebuildStatus {
	m.nativeRebuildMu.Lock()
	defer m.nativeRebuildMu.Unlock()
	if s, ok := m.nativeRebuilds[id]; ok {
		cp := *s
		return &cp
	}
	return nil
}

func (m *SubprocessManager) setNativeRebuildStatus(id string, st *NativeRebuildStatus) {
	st.UpdatedAt = time.Now().Unix()
	m.nativeRebuildMu.Lock()
	m.nativeRebuilds[id] = st
	m.nativeRebuildMu.Unlock()
}

func (m *SubprocessManager) clearNativeRebuildStatus(id string) {
	m.nativeRebuildMu.Lock()
	delete(m.nativeRebuilds, id)
	m.nativeRebuildMu.Unlock()
}

// isNativeABIMismatchErr reports whether a plugin.Open error looks like an ABI /
// Go-version incompatibility (as opposed to a missing file or a genuine symbol
// error), which is the case an automatic recompile can fix.
func isNativeABIMismatchErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, s := range []string{
		"was built with a different version of",
		"plugin was built with a different version",
		"different version of package",
		"plugin is incompatible",
		"runtime: no plugin module data",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// scheduleNativeRebuild 在后台异步重编译指定 Native 插件的源码到原 .so 路径，
// 并在成功后重新加载。它绝不阻塞调用方（启动/WebUI）；状态经
// nativeRebuildStatus 暴露给前端。幂等：同一插件的重编译进行中时直接返回。
func (m *SubprocessManager) scheduleNativeRebuild(id, artifact string, cause error) {
	m.nativeRebuildMu.Lock()
	if cur, ok := m.nativeRebuilds[id]; ok && cur.State == "running" {
		m.nativeRebuildMu.Unlock()
		return
	}
	m.nativeRebuildMu.Unlock()

	reason := ""
	if cause != nil {
		reason = singleLineTruncate(cause.Error(), 300)
	}
	m.setNativeRebuildStatus(id, &NativeRebuildStatus{
		State:   "running",
		Message: "Native 插件链接库加载失败，正在后台重新编译…",
		Reason:  reason,
	})
	logger.I18nWarn("Native 插件 %s 加载失败（疑似 Go 版本/ABI 不兼容），已启动后台重编译: %v", id, cause)

	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		if err := m.rebuildNativeFromSource(ctx, id, artifact); err != nil {
			m.setNativeRebuildStatus(id, &NativeRebuildStatus{
				State:   "failed",
				Message: "Native 插件自动重编译失败，请在 WebUI 重新安装该插件。",
				Reason:  singleLineTruncate(err.Error(), 300),
			})
			logger.I18nWarn("Native 插件 %s 后台重编译失败: %v", id, err)
			return
		}
		// 重编译成功后重新加载（换名后 plugin.Open 新路径）。
		if _, err := m.LoadLang(ctx, id, artifact, "go"); err != nil {
			m.setNativeRebuildStatus(id, &NativeRebuildStatus{
				State:   "failed",
				Message: "Native 插件已重编译但仍无法加载，请重启 AstrBot 或在 WebUI 重新安装。",
				Reason:  singleLineTruncate(err.Error(), 300),
			})
			logger.I18nWarn("Native 插件 %s 重编译成功但重新加载失败: %v", id, err)
			return
		}
		m.clearNativeRebuildStatus(id)
		logger.I18nInfo("Native 插件 %s 已自动重编译并重新加载", id)
		if m.OnInstancesChanged != nil {
			m.OnInstancesChanged()
		}
	}()
}

// rebuildNativeFromSource 就地重编译已安装 Native 插件的本地源码到原 .so 路径。
// 复用现有编译器探测（zig cc / musl-gcc / gcc / clang），不新增依赖；不跑
// StaticScan（安装时已扫描）。Go 版本升级时，工具链会用新版本重新构建，产出
// 与当前宿主 ABI 匹配的 .so。
func (m *SubprocessManager) rebuildNativeFromSource(ctx context.Context, id, artifact string) error {
	srcDest := filepath.Join(m.dataDir, "plugins", sanitizeID(id))
	if err := ensureMainGo(srcDest); err != nil {
		return fmt.Errorf("本地源码缺失（%s）: %w", srcDest, err)
	}
	if err := m.compiler.Prepare(srcDest, goModuleNameOf(srcDest, nil)); err != nil {
		return fmt.Errorf("prepare module: %w", err)
	}
	_ = m.compiler.Tidy(ctx, srcDest)
	if err := m.compiler.Vet(ctx, srcDest); err != nil {
		return fmt.Errorf("go vet: %w", err)
	}
	// 走与安装一致的 C 编译器探测（Native 必须 CGO）。
	cc, cxx, err := ensureCCompiler(ctx, InstallOptions{})
	if err != nil {
		return fmt.Errorf("resolve C compiler: %w", err)
	}
	absArtifact := artifact
	if a, aerr := filepath.Abs(artifact); aerr == nil {
		absArtifact = a
	}
	if err := os.MkdirAll(filepath.Dir(absArtifact), 0o755); err != nil {
		return fmt.Errorf("创建产物目录: %w", err)
	}
	// Go plugin 不能对已加载路径二次 Open/replace：先移除旧 .so，再写入新产物。
	_ = os.Remove(absArtifact)
	if err := m.compiler.BuildNative(ctx, srcDest, absArtifact, nil, cc, cxx, nil); err != nil {
		return fmt.Errorf("build native: %w", err)
	}
	return nil
}

// singleLineTruncate collapses newlines and truncates s to max runes for
// embedding in a status message.
func singleLineTruncate(s string, max int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
