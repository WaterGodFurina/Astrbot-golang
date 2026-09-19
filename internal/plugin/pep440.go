package plugin

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
)

// 本文件实现 PEP 440 版本与版本约束（specifier）解析/比较，用于插件 metadata
// 的 astrbot_version 兼容校验，对齐 Python AstrBot 的
// PluginManager._validate_astrbot_version_specifier（依赖 packaging 的
// Version / SpecifierSet）。支持的语法：
//
//	>=1.0.0   <=1.2.3   >1.0   <2.0
//	==1.2.3   !=1.2.3   ==1.2.*   !=1.*
//	~=1.0     ~=1.4.5
//	===1.2.3
//	逗号组合（AND）: >=4.16,<5
//
// 同时覆盖 epoch（1!2.0）、pre（a/b/rc）、post、dev、local（+abc）与 v 前缀。

// pep440Version 是解析后的 PEP 440 版本。
type pep440Version struct {
	raw     string
	epoch   int
	release []int
	preKind int // -1=无 pre；0=a，1=b，2=rc
	preNum  int
	hasPre  bool
	postNum int
	hasPost bool
	devNum  int
	hasDev  bool
	local   []string
}

// pep440Re 对齐 packaging 的官方 VERSION_PATTERN（含可选 v 前缀）。分组：
// 1 epoch / 2 release / 3 pre_l / 4 pre_n / 5 post_n1 / 6 post_l /
// 7 post_n2 / 8 dev_l / 9 dev_n / 10 local。
var pep440Re = regexp.MustCompile(`(?i)^\s*v?` +
	`(?:(\d+)!)?` +
	`(\d+(?:\.\d+)*)` +
	`(?:[-_.]?(alpha|a|beta|b|preview|pre|c|rc)[-_.]?(\d*))?` +
	`(?:(?:-(\d+))|(?:[-_.]?(post|rev|r)[-_.]?(\d*)))?` +
	`(?:[-_.]?(dev)[-_.]?(\d*))?` +
	`(?:\+([a-z0-9]+(?:[-_.][a-z0-9]+)*))?` +
	`\s*$`)

// parsePEP440 解析一个 PEP 440 版本字符串。
func parsePEP440(s string) (*pep440Version, error) {
	m := pep440Re.FindStringSubmatch(s)
	if m == nil {
		return nil, fmt.Errorf("非法 PEP 440 版本: %q", s)
	}
	v := &pep440Version{raw: strings.TrimSpace(s), preKind: -1}
	if m[1] != "" {
		v.epoch, _ = strconv.Atoi(m[1])
	}
	for _, seg := range strings.Split(m[2], ".") {
		n, _ := strconv.Atoi(seg)
		v.release = append(v.release, n)
	}
	if m[3] != "" {
		v.hasPre = true
		switch strings.ToLower(m[3]) {
		case "a", "alpha":
			v.preKind = 0
		case "b", "beta":
			v.preKind = 1
		case "c", "rc", "pre", "preview":
			v.preKind = 2
		}
		if m[4] != "" {
			v.preNum, _ = strconv.Atoi(m[4])
		}
	}
	switch {
	case m[5] != "":
		v.hasPost = true
		v.postNum, _ = strconv.Atoi(m[5])
	case m[6] != "":
		v.hasPost = true
		if m[7] != "" {
			v.postNum, _ = strconv.Atoi(m[7])
		}
	}
	if m[8] != "" {
		v.hasDev = true
		if m[9] != "" {
			v.devNum, _ = strconv.Atoi(m[9])
		}
	}
	if m[10] != "" {
		v.local = strings.FieldsFunc(strings.ToLower(m[10]), func(r rune) bool {
			return r == '.' || r == '-' || r == '_'
		})
	}
	return v, nil
}

func (v *pep440Version) isPrerelease() bool { return v.hasPre || v.hasDev }

// withoutLocal 返回去掉 local 段的副本（== 比较时 spec 无 local 需忽略候选
// 版本的 local）。
func (v *pep440Version) withoutLocal() *pep440Version {
	if len(v.local) == 0 {
		return v
	}
	c := *v
	c.local = nil
	return &c
}

// pep440Key 是把版本归一成可比较元组的中间表示。
type pep440Key struct {
	epoch    int
	release  []int
	preCat   int // 0=NegInf，1=值，2=Inf
	preKind  int
	preNum   int
	postCat  int // 0=NegInf，1=值
	postNum  int
	devCat   int // 1=值，2=Inf
	devNum   int
	localCat int // 0=NegInf，1=值
	local    []string
}

// cmpKey 对齐 packaging Version._cmpkey。
func (v *pep440Version) cmpKey() pep440Key {
	k := pep440Key{epoch: v.epoch, release: v.release}
	switch {
	case !v.hasPre && !v.hasPost && v.hasDev:
		k.preCat = 0 // 纯 dev 版本排在所有 pre 之前
	case !v.hasPre:
		k.preCat = 2
	default:
		k.preCat = 1
		k.preKind, k.preNum = v.preKind, v.preNum
	}
	if v.hasPost {
		k.postCat, k.postNum = 1, v.postNum
	}
	if v.hasDev {
		k.devCat, k.devNum = 1, v.devNum
	} else {
		k.devCat = 2
	}
	if len(v.local) > 0 {
		k.localCat, k.local = 1, v.local
	}
	return k
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func cmpRelease(a, b []int) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(a) {
			ai = a[i]
		}
		if i < len(b) {
			bi = b[i]
		}
		if c := cmpInt(ai, bi); c != 0 {
			return c
		}
	}
	return 0
}

func cmpCatNum(aCat, aNum, bCat, bNum int) int {
	if aCat != bCat {
		return cmpInt(aCat, bCat)
	}
	if aCat != 1 {
		return 0
	}
	return cmpInt(aNum, bNum)
}

func parseLocalNum(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// cmpLocal 按 PEP 440 比较 local 段：数字段 > 字母段；同类按值/字典序；
// 共同前缀相等时短段更小。
func cmpLocal(aCat int, a []string, bCat int, b []string) int {
	if aCat != bCat {
		return cmpInt(aCat, bCat)
	}
	if aCat != 1 {
		return 0
	}
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if i >= len(a) {
			return -1
		}
		if i >= len(b) {
			return 1
		}
		ai, aok := parseLocalNum(a[i])
		bi, bok := parseLocalNum(b[i])
		switch {
		case aok && bok:
			if c := cmpInt(ai, bi); c != 0 {
				return c
			}
		case aok && !bok:
			return 1
		case !aok && bok:
			return -1
		default:
			if c := strings.Compare(a[i], b[i]); c != 0 {
				return c
			}
		}
	}
	return 0
}

// pep440Compare 返回 -1/0/1。
func pep440Compare(a, b *pep440Version) int {
	ka, kb := a.cmpKey(), b.cmpKey()
	if c := cmpInt(ka.epoch, kb.epoch); c != 0 {
		return c
	}
	if c := cmpRelease(ka.release, kb.release); c != 0 {
		return c
	}
	if ka.preCat != kb.preCat {
		return cmpInt(ka.preCat, kb.preCat)
	}
	if ka.preCat == 1 {
		if c := cmpInt(ka.preKind, kb.preKind); c != 0 {
			return c
		}
		if c := cmpInt(ka.preNum, kb.preNum); c != 0 {
			return c
		}
	}
	if c := cmpCatNum(ka.postCat, ka.postNum, kb.postCat, kb.postNum); c != 0 {
		return c
	}
	if c := cmpCatNum(ka.devCat, ka.devNum, kb.devCat, kb.devNum); c != 0 {
		return c
	}
	return cmpLocal(ka.localCat, ka.local, kb.localCat, kb.local)
}

func samePEP440Base(a, b *pep440Version) bool {
	return a.epoch == b.epoch && cmpRelease(a.release, b.release) == 0
}

// releasePrefixMatch 报告 a 是否以 prefix 开头（逐段相等，a 可更长）。
func releasePrefixMatch(a, prefix []int) bool {
	if len(a) < len(prefix) {
		return false
	}
	for i := range prefix {
		if a[i] != prefix[i] {
			return false
		}
	}
	return true
}

// pep440Specifier 是单个比较子句。
type pep440Specifier struct {
	op       string
	ver      *pep440Version
	arg      string // === 的原始比较串
	wildcard bool   // == / != 的 .* 形式
}

var pep440Ops = []string{"===", "~=", "==", "!=", "<=", ">=", "<", ">"}

// parsePEP440SpecifierSet 解析逗号分隔的 specifier 集合（AND）。
func parsePEP440SpecifierSet(spec string) ([]pep440Specifier, error) {
	parts := strings.Split(spec, ",")
	out := make([]pep440Specifier, 0, len(parts))
	for _, raw := range parts {
		item := strings.TrimSpace(raw)
		if item == "" {
			continue
		}
		op := ""
		for _, cand := range pep440Ops {
			if strings.HasPrefix(item, cand) {
				op = cand
				break
			}
		}
		if op == "" {
			return nil, fmt.Errorf("缺少比较运算符: %q", item)
		}
		val := strings.TrimSpace(item[len(op):])
		if val == "" {
			return nil, fmt.Errorf("运算符 %s 缺少版本: %q", op, item)
		}
		if op == "===" {
			out = append(out, pep440Specifier{op: op, arg: strings.ToLower(val)})
			continue
		}
		wildcard := false
		if strings.HasSuffix(val, ".*") {
			if op != "==" && op != "!=" {
				return nil, fmt.Errorf("通配符仅支持 == / !=: %q", item)
			}
			wildcard = true
			val = strings.TrimSuffix(val, ".*")
		}
		v, err := parsePEP440(val)
		if err != nil {
			return nil, err
		}
		if op == "~=" && len(v.release) < 2 {
			return nil, fmt.Errorf("~= 至少需要两个版本段: %q", item)
		}
		out = append(out, pep440Specifier{op: op, ver: v, wildcard: wildcard})
	}
	return out, nil
}

func (s pep440Specifier) equal(v *pep440Version) bool {
	if s.wildcard {
		return releasePrefixMatch(v.release, s.ver.release)
	}
	cand := v
	if len(s.ver.local) == 0 {
		cand = v.withoutLocal()
	}
	return pep440Compare(cand, s.ver) == 0
}

func (s pep440Specifier) matches(v *pep440Version) bool {
	switch s.op {
	case "===":
		return strings.ToLower(strings.TrimSpace(v.raw)) == s.arg
	case "==":
		return s.equal(v)
	case "!=":
		return !s.equal(v)
	case "<=":
		return pep440Compare(v, s.ver) <= 0
	case ">=":
		return pep440Compare(v, s.ver) >= 0
	case "<":
		if pep440Compare(v, s.ver) >= 0 {
			return false
		}
		// <V 不匹配与 V 同 base 的 pre-release（V 自身非 pre-release 时）。
		if !s.ver.isPrerelease() && v.isPrerelease() && samePEP440Base(v, s.ver) {
			return false
		}
		return true
	case ">":
		if pep440Compare(v, s.ver) <= 0 {
			return false
		}
		// >V 不匹配与 V 同 base 的 post-release（V 自身非 post 时）与 local 版本。
		if !s.ver.hasPost && v.hasPost && samePEP440Base(v, s.ver) {
			return false
		}
		if len(v.local) > 0 && samePEP440Base(v, s.ver) {
			return false
		}
		return true
	case "~=":
		if pep440Compare(v, s.ver) < 0 {
			return false
		}
		return releasePrefixMatch(v.release, s.ver.release[:len(s.ver.release)-1])
	}
	return false
}

func pep440SetContains(set []pep440Specifier, v *pep440Version) bool {
	for _, s := range set {
		if !s.matches(v) {
			return false
		}
	}
	return true
}

// CheckAstrbotVersionCompatibility 校验插件 metadata 的 astrbot_version（PEP
// 440 specifier）是否被当前 AstrBot 版本满足。空 spec 视为兼容；语法非法或
// 不满足返回带说明的错误（对齐 Python _validate_astrbot_version_specifier）。
func CheckAstrbotVersionCompatibility(spec string) error {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return nil
	}
	set, err := parsePEP440SpecifierSet(trimmed)
	if err != nil {
		return fmt.Errorf("astrbot_version %q 非法（请使用 PEP 440 范围，如 >=4.16,<5）: %w", trimmed, err)
	}
	cur, err := parsePEP440(version.PythonVersion)
	if err != nil {
		return fmt.Errorf("当前 AstrBot 版本 %q 非法，无法校验插件版本范围: %w", version.PythonVersion, err)
	}
	if !pep440SetContains(set, cur) {
		return fmt.Errorf("AstrBot %s 不满足插件 astrbot_version: %s", version.PythonVersion, trimmed)
	}
	return nil
}
