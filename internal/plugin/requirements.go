package plugin

import (
	"bufio"
	"os"
	"strings"
)

// requirements.txt 解析与共享 Runtime 依赖冲突检测（方案第 9 节）。
//
// 边界声明：shared Runtime 与 python-grpc **共用同一个宿主 venv**，没有
// per-plugin 环境隔离（解释器 / site-packages / 包版本都是全局的）。因此当
// 多个插件被加载进同一个共享进程时，对同一包的**互斥版本约束**会让共享进程
// 产生不可预料（且依赖加载顺序）的导入结果。本文件提供「检测 → 拒绝加入
// shared → 回退独立进程」的最小决策依据；绝不假装 shared 有环境隔离。
//
// 无法解决（诚实声明，见审查报告）：真正需要并存不同包版本时，宿主当前没有
// per-plugin venv；回退 grpc 只是换进程边界，venv 仍是同一个。彻底解决需要
// 每插件独立 venv（超出本次最小修改范围）。

// parseRequirements 解析 requirements.txt 为「规范化包名 → 版本约束」映射。
// 版本约束为空串表示未固定（任意版本，与任何约束兼容）。解析失败返回 nil
// （不存在/不可读 = 无依赖声明）。仅做保守解析，不追求完整 PEP 508 语法。
func parseRequirements(path string) map[string]string {
	f, err := os.Open(path) // #nosec G304 -- 读取插件自身 requirements.txt
	if err != nil {
		return nil
	}
	defer f.Close()

	reqs := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue // 空行 / 注释 / -r、--index-url 等选项行
		}
		if i := strings.Index(line, " #"); i >= 0 {
			line = strings.TrimSpace(line[:i]) // 行内注释
		}
		if i := strings.Index(line, ";"); i >= 0 {
			line = strings.TrimSpace(line[:i]) // 环境标记（; python_version<"3.9"）
		}
		// 包名 = 版本约束/扩展标记/空格之前的片段。
		name := line
		if i := strings.IndexAny(name, "[<>!=~ \t"); i >= 0 {
			name = name[:i]
		}
		name = normalizeReqName(name)
		if name == "" {
			continue
		}
		spec := ""
		if i := strings.IndexAny(line, "<>=!~"); i >= 0 {
			spec = strings.TrimSpace(line[i:])
		}
		// 同一包多次出现：保留第一个非空约束（更强的信息）。
		if prev, ok := reqs[name]; !ok || prev == "" {
			reqs[name] = spec
		}
	}
	return reqs
}

// normalizeReqName 按 PEP 503 规范化发行包名：小写 + 连续 [-_.] 折叠为单 '-'。
func normalizeReqName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	b.Grow(len(name))
	prevDash := false
	for _, r := range name {
		if r == '-' || r == '_' || r == '.' {
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
			continue
		}
		b.WriteRune(r)
		prevDash = false
	}
	return strings.Trim(b.String(), "-")
}

// normalizeSpec 去除版本约束中的空白与大小写差异，便于比较。
func normalizeSpec(spec string) string {
	return strings.ToLower(strings.ReplaceAll(spec, " ", ""))
}

// requirementsConflict 报告两份依赖声明是否存在冲突：同一包在两个插件中都被
// 约束为**非空且不同**的版本约束即冲突（保守策略：宁可多隔离一个进程，也不
// 让共享进程出现不确定导入）。返回冲突包名与两边的约束。
func requirementsConflict(a, b map[string]string) (pkg, specA, specB string, conflict bool) {
	for name, sa := range a {
		sb, ok := b[name]
		if !ok || sa == "" || sb == "" {
			continue
		}
		if normalizeSpec(sa) != normalizeSpec(sb) {
			return name, sa, sb, true
		}
	}
	return "", "", "", false
}
