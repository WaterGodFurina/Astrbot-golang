// Package skills implements the skill management system.
// Ported from astrbot/core/skills/skill_manager.py
//
// Skills are reusable instruction bundles stored as SKILL.md files.
// Each skill has a name, description, and optional scripts/assets.
//
// Skill sources:
//   - local_only:  SKILL.md in data/skills/<name>/
//   - plugin:      SKILL.md inside a .so plugin's skills/ dir
//   - sandbox_only: discovered from sandbox runtime, cached locally
//   - workspace:   request-scoped skills from a session workspace
//   - both:        exists both locally and in sandbox
package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Skills")

const (
	SkillsConfigFilename         = "skills.json"
	SandboxSkillsCacheFilename   = "sandbox_skills_cache.json"
	SandboxWorkspaceRoot         = "/workspace"
	SandboxSkillsRoot            = "skills"
	WorkspaceSkillsRoot          = "skills"
	WorkspaceSkillFrontmatterMax = 64 * 1024
	SandboxCacheVersion          = 1
)

var skillNameRe = regexp.MustCompile(`^[\w.\-]+$`)

// SourceType classifies where a skill comes from.
type SourceType string

const (
	SourceLocalOnly   SourceType = "local_only"
	SourcePlugin      SourceType = "plugin"
	SourceSandboxOnly SourceType = "sandbox_only"
	SourceWorkspace   SourceType = "workspace"
	SourceBoth        SourceType = "both"
)

// SkillInfo describes a single skill.
type SkillInfo struct {
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Path          string     `json:"path"`
	Active        bool       `json:"active"`
	SourceType    SourceType `json:"source_type"`
	SourceLabel   string     `json:"source_label"`
	LocalExists   bool       `json:"local_exists"`
	SandboxExists bool       `json:"sandbox_exists"`
	PluginName    string     `json:"plugin_name"`
	Readonly      bool       `json:"readonly"`
	Preset        bool       `json:"preset"`
}

// SandboxCacheEntry represents one cached sandbox skill.
type SandboxCacheEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path"`
}

// SandboxCache is the on-disk cache structure.
type SandboxCache struct {
	Version   int                 `json:"version"`
	Skills    []SandboxCacheEntry `json:"skills"`
	UpdatedAt string              `json:"updated_at,omitempty"`
}

// skillsConfig is the on-disk skills.json structure.
type skillsConfig struct {
	Skills map[string]map[string]interface{} `json:"skills"`
}

// SkillManager manages skill discovery, activation, and deletion.
type SkillManager struct {
	mu                     sync.RWMutex
	skillsRoot             string
	pluginsRoot            string
	configPath             string
	sandboxSkillsCachePath string
}

// NewSkillManager creates a manager.
func NewSkillManager(skillsRoot, pluginsRoot, dataDir string) *SkillManager {
	if skillsRoot == "" {
		skillsRoot = filepath.Join(dataDir, "skills")
	}
	if pluginsRoot == "" {
		pluginsRoot = filepath.Join(dataDir, "plugins")
	}
	_ = os.MkdirAll(skillsRoot, 0750)
	return &SkillManager{
		skillsRoot:             skillsRoot,
		pluginsRoot:            pluginsRoot,
		configPath:             filepath.Join(dataDir, SkillsConfigFilename),
		sandboxSkillsCachePath: filepath.Join(dataDir, SandboxSkillsCacheFilename),
	}
}

// DefaultSandboxSkillPath returns the default sandbox path for a skill.
func DefaultSandboxSkillPath(name string) string {
	return fmt.Sprintf("%s/%s/%s/SKILL.md", SandboxWorkspaceRoot, SandboxSkillsRoot, name)
}

// NormalizeCachedSandboxSkillPath validates and normalizes a cached path.
// If invalid, returns the default path.
func NormalizeCachedSandboxSkillPath(name, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return DefaultSandboxSkillPath(name)
	}
	path = strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(path, "/")
	for _, p := range parts {
		if p == ".." {
			return DefaultSandboxSkillPath(name)
		}
	}
	if !strings.HasSuffix(path, "SKILL.md") {
		return DefaultSandboxSkillPath(name)
	}
	dirParts := strings.Split(strings.TrimSuffix(path, "/SKILL.md"), "/")
	if dirParts[len(dirParts)-1] != name {
		return DefaultSandboxSkillPath(name)
	}
	return path
}

// ParseFrontmatterDescription extracts the description from YAML frontmatter.
func ParseFrontmatterDescription(text string) string {
	if !strings.HasPrefix(text, "---") {
		return ""
	}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return ""
	}
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			endIdx = i
			break
		}
	}
	if endIdx == -1 {
		return ""
	}
	frontmatter := strings.Join(lines[1:endIdx], "\n")
	// Simple YAML parsing for "description: value"
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "description:") {
			val := strings.TrimSpace(strings.TrimPrefix(line, "description:"))
			val = strings.Trim(val, `"'`)
			return val
		}
	}
	return ""
}

// NormalizeSkillMarkdownPath finds SKILL.md (or legacy skill.md) in a dir.
func NormalizeSkillMarkdownPath(skillDir string) string {
	entries, err := os.ReadDir(skillDir)
	if err != nil {
		return ""
	}
	hasCanonical := false
	hasLegacy := false
	for _, e := range entries {
		if e.Name() == "SKILL.md" {
			hasCanonical = true
		}
		if e.Name() == "skill.md" {
			hasLegacy = true
		}
	}
	if hasCanonical {
		return filepath.Join(skillDir, "SKILL.md")
	}
	if hasLegacy {
		// Rename legacy to canonical
		legacy := filepath.Join(skillDir, "skill.md")
		canonical := filepath.Join(skillDir, "SKILL.md")
		if err := os.Rename(legacy, canonical); err != nil {
			return legacy
		}
		return canonical
	}
	return ""
}

// ListSkills returns all discovered skills.
// showSandboxPath semantics mirror Python's list_skills: it only affects path
// rendering inside the "sandbox" runtime view (true → sandbox-cached path /
// default sandbox path; false → local filesystem path). sandbox_only skills
// always carry the sandbox path.
func (sm *SkillManager) ListSkills(activeOnly bool, runtime string) []*SkillInfo {
	return sm.ListSkillsView(activeOnly, runtime, true)
}

// ListSkillsView is ListSkills with explicit showSandboxPath control.
func (sm *SkillManager) ListSkillsView(activeOnly bool, runtime string, showSandboxPath bool) []*SkillInfo {
	// 只在读取配置快照期间持读锁；发现/扫描基于本地副本进行，末尾的自动补全
	// 保存改在写锁下合并写回（见 persistSkillConfigs），避免读锁下执行磁盘写
	// 与 SetSkillActive/DeleteSkill 互相覆盖。
	sm.mu.RLock()
	cfg := sm.loadConfig()
	skillConfigs := cfg.Skills
	sm.mu.RUnlock()

	modified := false
	skillsByName := make(map[string]*SkillInfo)

	// Load sandbox cache
	sandboxPaths, sandboxDescs := sm.loadSandboxCacheMaps()

	// Scan local skills root
	entries, err := os.ReadDir(sm.skillsRoot)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			name := entry.Name()
			skillMd := NormalizeSkillMarkdownPath(filepath.Join(sm.skillsRoot, name))
			if skillMd == "" {
				continue
			}
			active := true
			if sc, ok := skillConfigs[name]; ok {
				if a, ok := sc["active"].(bool); ok {
					active = a
				}
			}
			if _, ok := skillConfigs[name]; !ok {
				skillConfigs[name] = map[string]interface{}{"active": active}
				modified = true
			}
			if activeOnly && !active {
				continue
			}
			desc := ""
			// #nosec G304 -- skillMd is a filepath.Join under the local skills
			// root or plugin skills dir; entry names come from os.ReadDir of
			// those roots, so the path stays contained.
			if content, err := os.ReadFile(skillMd); err == nil {
				desc = ParseFrontmatterDescription(string(content))
			}
			// 存在性判定用缓存键（对齐 Python `name in sandbox_cached_descriptions`），
			// 描述为空的缓存条目同样算 sandbox 已存在。
			_, cached := sandboxPaths[name]
			sandboxExists := runtime == "sandbox" && cached
			sourceType := SourceLocalOnly
			sourceLabel := "local"
			if sandboxExists {
				sourceType = SourceBoth
				sourceLabel = "synced"
			}
			pathStr := skillMd
			if runtime == "sandbox" && showSandboxPath {
				if p, ok := sandboxPaths[name]; ok {
					pathStr = p
				} else {
					pathStr = DefaultSandboxSkillPath(name)
				}
			}
			pathStr = strings.ReplaceAll(pathStr, "\\", "/")
			skillsByName[name] = &SkillInfo{
				Name:          name,
				Description:   desc,
				Path:          pathStr,
				Active:        active,
				SourceType:    sourceType,
				SourceLabel:   sourceLabel,
				LocalExists:   true,
				SandboxExists: sandboxExists,
			}
		}
	}

	// Scan plugin skills
	sm.scanPluginSkills(skillsByName, skillConfigs, &modified, activeOnly, runtime, showSandboxPath, sandboxPaths, sandboxDescs)

	// Scan sandbox-only skills from cache
	if runtime == "sandbox" {
		cache := sm.loadSandboxCache()
		for _, item := range cache.Skills {
			name := strings.TrimSpace(item.Name)
			if name == "" || skillsByName[name] != nil || !skillNameRe.MatchString(name) {
				continue
			}
			active := true
			if sc, ok := skillConfigs[name]; ok {
				if a, ok := sc["active"].(bool); ok {
					active = a
				}
			}
			if _, ok := skillConfigs[name]; !ok {
				skillConfigs[name] = map[string]interface{}{"active": active}
				modified = true
			}
			if activeOnly && !active {
				continue
			}
			desc := sandboxDescs[name]
			pathStr := sandboxPaths[name]
			if pathStr == "" {
				pathStr = DefaultSandboxSkillPath(name)
			}
			skillsByName[name] = &SkillInfo{
				Name:          name,
				Description:   desc,
				Path:          strings.ReplaceAll(pathStr, "\\", "/"),
				Active:        active,
				SourceType:    SourceSandboxOnly,
				SourceLabel:   "sandbox_preset",
				LocalExists:   false,
				SandboxExists: true,
			}
		}
	}

	if modified {
		sm.persistSkillConfigs(skillConfigs)
	}

	result := make([]*SkillInfo, 0, len(skillsByName))
	for _, name := range sortedKeys(skillsByName) {
		result = append(result, skillsByName[name])
	}
	logger.Debug("Discovered %d skills (runtime=%s)", len(result), runtime)
	return result
}

// ListSkillsInfo returns skills as serializable maps.
func (sm *SkillManager) ListSkillsInfo() []map[string]interface{} {
	skills := sm.ListSkills(false, "local")
	result := make([]map[string]interface{}, 0, len(skills))
	for _, s := range skills {
		result = append(result, map[string]interface{}{
			"name":           s.Name,
			"description":    s.Description,
			"path":           s.Path,
			"active":         s.Active,
			"source_type":    s.SourceType,
			"source_label":   s.SourceLabel,
			"local_exists":   s.LocalExists,
			"sandbox_exists": s.SandboxExists,
			"plugin_name":    s.PluginName,
			"readonly":       s.Readonly,
			"preset":         s.Preset,
		})
	}
	return result
}

// validateSkillName rejects names that cannot be used as a single directory
// segment under skillsRoot: empty, ".", "..", or anything containing path
// separators / illegal characters (aligned with skillNameRe and the safe-join
// checks used at the other skill path entry points).
func validateSkillName(name string) error {
	if name == "" || name == "." || name == ".." || !skillNameRe.MatchString(name) {
		return fmt.Errorf("非法技能名")
	}
	return nil
}

// SetSkillActive enables/disables a skill. 写路径持写锁，与 ListSkills 的自动
// 补全保存互斥，避免并发读写 skills.json 互相覆盖。
func (sm *SkillManager) SetSkillActive(name string, active bool) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if sm.IsSandboxOnlySkill(name) {
		return fmt.Errorf("sandbox preset skill cannot be enabled/disabled from local skill management")
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cfg := sm.loadConfig()
	if cfg.Skills == nil {
		cfg.Skills = make(map[string]map[string]interface{})
	}
	cfg.Skills[name] = map[string]interface{}{"active": active}
	return sm.saveConfig(cfg)
}

// IsSandboxOnlySkill checks if a skill exists only in sandbox cache.
func (sm *SkillManager) IsSandboxOnlySkill(name string) bool {
	if validateSkillName(name) != nil {
		return false
	}
	skillMd := NormalizeSkillMarkdownPath(filepath.Join(sm.skillsRoot, name))
	if skillMd != "" {
		return false
	}
	cache := sm.loadSandboxCache()
	for _, item := range cache.Skills {
		if strings.TrimSpace(item.Name) == name {
			return true
		}
	}
	return false
}

// DeleteSkill removes a local skill. 写路径持写锁，防止与并发
// ListSkills/SetSkillActive 的配置保存互相覆盖。
func (sm *SkillManager) DeleteSkill(name string) error {
	if err := validateSkillName(name); err != nil {
		return err
	}
	if sm.IsSandboxOnlySkill(name) {
		return fmt.Errorf("sandbox preset skill cannot be deleted from local skill management")
	}
	sm.mu.Lock()
	defer sm.mu.Unlock()
	skillDir := filepath.Join(sm.skillsRoot, name)
	if _, err := os.Stat(skillDir); err == nil {
		if err := os.RemoveAll(skillDir); err != nil {
			return err
		}
	}
	sm.removeFromSandboxCache(name)
	cfg := sm.loadConfig()
	if _, ok := cfg.Skills[name]; ok {
		delete(cfg.Skills, name)
		return sm.saveConfig(cfg)
	}
	return nil
}

// SetSandboxSkillsCache persists sandbox skill metadata.
func (sm *SkillManager) SetSandboxSkillsCache(skills []SandboxCacheEntry) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	deduped := make(map[string]SandboxCacheEntry)
	for _, item := range skills {
		name := strings.TrimSpace(item.Name)
		if name == "" || !skillNameRe.MatchString(name) {
			continue
		}
		item.Name = name
		item.Path = NormalizeCachedSandboxSkillPath(name, item.Path)
		deduped[name] = item
	}
	cache := SandboxCache{
		Version: SandboxCacheVersion,
		Skills:  make([]SandboxCacheEntry, 0, len(deduped)),
	}
	for _, name := range sortedCacheKeys(deduped) {
		cache.Skills = append(cache.Skills, deduped[name])
	}
	cache.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	sm.saveSandboxCache(cache)
	logger.Debug("Updated sandbox skills cache: %d entries", len(cache.Skills))
}

// GetSandboxSkillsCacheStatus returns cache stats.
func (sm *SkillManager) GetSandboxSkillsCacheStatus() map[string]interface{} {
	cache := sm.loadSandboxCache()
	count := len(cache.Skills)
	_, err := os.Stat(sm.sandboxSkillsCachePath)
	exists := err == nil
	return map[string]interface{}{
		"exists":     exists,
		"ready":      count > 0,
		"count":      count,
		"updated_at": cache.UpdatedAt,
	}
}

// --- prompt sanitization (ported from skill_manager.py build_skills_prompt) ---
// Regexes for sanitizing paths/names used in prompt examples — only allow safe
// characters to prevent prompt injection via crafted skill paths/descriptions.
var (
	safePathRe         = regexp.MustCompile(`[^\w./ ,()'\-]`)
	windowsDrivePathRe = regexp.MustCompile(`^[A-Za-z]:(?:/|\\)`)
	windowsUNCPathRe   = regexp.MustCompile(`^(//|\\+)[^/\\]+[/\\][^/\\]+`)
	controlCharsRe     = regexp.MustCompile(`[\x00-\x1F\x7F]`)
	placeholderSkillMd = "<skills_root>/<skill_name>/SKILL.md"
)

// isWindowsPromptPath reports whether path is a Windows drive/UNC path (only
// meaningful on Windows hosts, mirroring os.name == "nt" in Python).
func isWindowsPromptPath(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return windowsDrivePathRe.MatchString(path) || windowsUNCPathRe.MatchString(path)
}

// sanitizePromptPathForPrompt sanitizes a skill path rendered into the prompt.
func sanitizePromptPathForPrompt(path string) string {
	if path == "" {
		return ""
	}
	if windowsDrivePathRe.MatchString(path) || windowsUNCPathRe.MatchString(path) {
		path = strings.ReplaceAll(path, "\\", "/")
	}
	drivePrefix := ""
	if windowsDrivePathRe.MatchString(path) {
		drivePrefix = path[:2]
		path = path[2:]
	}
	path = strings.ReplaceAll(path, "`", "")
	path = controlCharsRe.ReplaceAllString(path, "")
	path = safePathRe.ReplaceAllString(path, "")
	return drivePrefix + path
}

// sanitizePromptDescription sanitizes a skill description rendered into the
// prompt: backticks stripped, control chars collapsed to spaces, whitespace
// squashed.
func sanitizePromptDescription(description string) string {
	description = strings.ReplaceAll(description, "`", "")
	description = controlCharsRe.ReplaceAllString(description, " ")
	return strings.Join(strings.Fields(description), " ")
}

// sanitizeSkillDisplayName renders the skill name, replacing invalid names
// (not matching ^[\w.-]+$) with a placeholder.
func sanitizeSkillDisplayName(name string) string {
	if skillNameRe.MatchString(name) {
		return name
	}
	return "<invalid_skill_name>"
}

// buildSkillReadCommandExample builds the example shell command used in the
// "Mandatory grounding" rule.
func buildSkillReadCommandExample(path string) string {
	if path == placeholderSkillMd {
		return "cat " + path
	}
	command, pathArg := "cat", path
	if isWindowsPromptPath(path) {
		command = "type"
		pathArg = `"` + filepath.FromSlash(path) + `"`
	}
	return command + " " + pathArg
}

// BuildSkillsPrompt generates the skills section for the system prompt.
// Ported 1:1 from Python's build_skills_prompt: only name/description are
// shown upfront; the LLM must read the SKILL.md before execution
// (progressive disclosure). Untrusted fields (name/description/path) are
// sanitized against prompt injection.
func BuildSkillsPrompt(list []*SkillInfo) string {
	if len(list) == 0 {
		return ""
	}
	var lines []string
	examplePath := ""
	for _, s := range list {
		displayName := sanitizeSkillDisplayName(s.Name)

		description := s.Description
		if s.SourceType == SourceSandboxOnly || s.SourceType == SourceWorkspace {
			description = sanitizePromptDescription(description)
			if description == "" {
				description = "Read SKILL.md for details."
			}
		} else if description == "" {
			description = "No description"
		}

		renderedPath := sanitizePromptPathForPrompt(s.Path)
		if renderedPath == "" {
			if s.SourceType == SourceSandboxOnly {
				renderedPath = DefaultSandboxSkillPath(s.Name)
			} else {
				renderedPath = placeholderSkillMd
			}
		}

		lines = append(lines, fmt.Sprintf("- **%s**: %s\n  File: `%s`", displayName, description, renderedPath))
		if examplePath == "" {
			examplePath = renderedPath
		}
	}
	skillsBlock := strings.Join(lines, "\n")
	// Sanitize example_path — it may originate from sandbox cache (untrusted).
	if examplePath != placeholderSkillMd {
		examplePath = sanitizePromptPathForPrompt(examplePath)
		if examplePath == "" {
			examplePath = placeholderSkillMd
		}
	}
	exampleCommand := buildSkillReadCommandExample(examplePath)

	return "## Skills\n\n" +
		"You have specialized skills — reusable instruction bundles stored " +
		"in `SKILL.md` files. Each skill has a **name** and a **description** " +
		"that tells you what it does and when to use it.\n\n" +
		"### Available skills\n\n" +
		skillsBlock + "\n\n" +
		"### Skill rules\n\n" +
		"1. **Discovery** — The list above is the complete skill inventory " +
		"for this session. Full instructions are in the referenced " +
		"`SKILL.md` file.\n" +
		"2. **When to trigger** — Use a skill if the user names it " +
		"explicitly, or if the task clearly matches the skill's description. " +
		"*Never silently skip a matching skill* — either use it or briefly " +
		"explain why you chose not to.\n" +
		"3. **Mandatory grounding** — Before executing any skill you MUST " +
		"first read its `SKILL.md` by running a shell command compatible " +
		"with the current runtime shell and using the **absolute path** " +
		"shown above (e.g. `" + exampleCommand + "`). " +
		"Never rely on memory or assumptions about a skill's content.\n" +
		"4. **Progressive disclosure** — Load only what is directly " +
		"referenced from `SKILL.md`:\n" +
		"   - If `scripts/` exist, prefer running or patching them over " +
		"rewriting code from scratch.\n" +
		"   - If `assets/` or templates exist, reuse them.\n" +
		"   - Do NOT bulk-load every file in the skill directory.\n" +
		"5. **Coordination** — When multiple skills apply, pick the minimal " +
		"set needed. Announce which skill(s) you are using and why " +
		"(one short line). Prefer `astrbot_*` tools when running skill " +
		"scripts.\n" +
		"6. **Context hygiene** — Avoid deep reference chasing; open only " +
		"files that are directly linked from `SKILL.md`.\n" +
		"7. **Failure handling** — If a skill cannot be applied, state the " +
		"issue clearly and continue with the best alternative.\n"
}

// ListWorkspaceSkills lists request-scoped skills from a session workspace
// (ported from Python SkillManager.list_workspace_skills). Only directories
// containing a canonical SKILL.md under <workspace_root>/skills are returned;
// skill names must match ^[\w.-]+$; paths are resolve-checked against the
// workspace skills root to prevent symlink escape. An empty workspaceRoot or
// a missing skills dir yields an empty list.
func (sm *SkillManager) ListWorkspaceSkills(workspaceRoot string) []*SkillInfo {
	if workspaceRoot == "" {
		return nil
	}
	rawWorkspaceRoot := filepath.Clean(workspaceRoot)
	skillsRoot := filepath.Join(rawWorkspaceRoot, WorkspaceSkillsRoot)
	if st, err := os.Stat(skillsRoot); err != nil || !st.IsDir() {
		return nil
	}

	// resolve 防逃逸：EvalSymlinks 等价 Python Path.resolve(strict=True)。
	resolvedWorkspaceRoot, err := filepath.EvalSymlinks(rawWorkspaceRoot)
	if err != nil {
		return nil
	}
	resolvedSkillsRoot, err := filepath.EvalSymlinks(skillsRoot)
	if err != nil {
		return nil
	}
	if !withinDir(resolvedWorkspaceRoot, resolvedSkillsRoot) {
		return nil
	}

	entries, err := os.ReadDir(resolvedSkillsRoot)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	var out []*SkillInfo
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		if !skillNameRe.MatchString(skillName) {
			continue
		}
		skillDir := filepath.Join(resolvedSkillsRoot, skillName)
		if _, err := os.ReadDir(skillDir); err != nil {
			continue
		}
		skillMd := filepath.Join(skillDir, "SKILL.md")
		if st, err := os.Stat(skillMd); err != nil || st.IsDir() {
			continue
		}
		resolvedSkillMd, err := filepath.EvalSymlinks(skillMd)
		if err != nil {
			continue
		}
		if !withinDir(resolvedSkillsRoot, resolvedSkillMd) {
			continue
		}
		// #nosec G304 -- resolvedSkillMd 已通过 EvalSymlinks + withinDir 校验，
		// 严格位于工作区 skills 根内。
		content, err := os.ReadFile(resolvedSkillMd)
		if err != nil {
			continue
		}
		if len(content) > WorkspaceSkillFrontmatterMax {
			content = content[:WorkspaceSkillFrontmatterMax]
		}
		out = append(out, &SkillInfo{
			Name:        skillName,
			Description: ParseFrontmatterDescription(string(content)),
			Path:        filepath.ToSlash(resolvedSkillMd),
			Active:      true,
			SourceType:  SourceWorkspace,
			SourceLabel: "workspace",
			LocalExists: true,
			Readonly:    true,
		})
	}
	return out
}

// withinDir reports whether path is dir itself or located inside dir
// (lexical+realpath containment, mirroring Python's is_relative_to checks).
func withinDir(dir, path string) bool {
	if dir == path {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// --- internal helpers ---

// persistSkillConfigs merges discovered-skill defaults into skills.json under
// the write lock. 重读当前配置后再合并，避免覆盖并发 SetSkillActive/DeleteSkill
// 已写入的其他技能条目。
func (sm *SkillManager) persistSkillConfigs(updates map[string]map[string]interface{}) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	cfg := sm.loadConfig()
	if cfg.Skills == nil {
		cfg.Skills = make(map[string]map[string]interface{})
	}
	for name, sc := range updates {
		cfg.Skills[name] = sc
	}
	_ = sm.saveConfig(cfg)
}

func (sm *SkillManager) loadConfig() skillsConfig {
	data, err := os.ReadFile(sm.configPath)
	if err != nil {
		return skillsConfig{Skills: make(map[string]map[string]interface{})}
	}
	var cfg skillsConfig
	if err := json.Unmarshal(data, &cfg); err != nil || cfg.Skills == nil {
		return skillsConfig{Skills: make(map[string]map[string]interface{})}
	}
	return cfg
}

// atomicWriteFile writes data to path atomically: temp file + fsync + rename,
// so a crash mid-write can never leave a truncated file at path.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	committed = true
	return nil
}

// saveConfig writes skills.json atomically: 先写临时文件并 fsync，再 os.Rename
// 替换，避免并发写/崩溃截断导致配置丢失（与 plugin manifest 的 Save 一致）。
func (sm *SkillManager) saveConfig(cfg skillsConfig) error {
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	return atomicWriteFile(sm.configPath, data, 0o600)
}

func (sm *SkillManager) loadSandboxCache() SandboxCache {
	data, err := os.ReadFile(sm.sandboxSkillsCachePath)
	if err != nil {
		return SandboxCache{Version: SandboxCacheVersion, Skills: []SandboxCacheEntry{}}
	}
	var cache SandboxCache
	if err := json.Unmarshal(data, &cache); err != nil {
		logger.Warn("sandbox skills cache %s is corrupted, falling back to empty: %v", sm.sandboxSkillsCachePath, err)
		return SandboxCache{Version: SandboxCacheVersion, Skills: []SandboxCacheEntry{}}
	}
	if cache.Skills == nil {
		cache.Skills = []SandboxCacheEntry{}
	}
	return cache
}

func (sm *SkillManager) saveSandboxCache(cache SandboxCache) {
	cache.Version = SandboxCacheVersion
	cache.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	data, _ := json.MarshalIndent(cache, "", "  ")
	// 原子写（temp+fsync+rename），与 saveConfig 同策略，避免崩溃截断后
	// loadSandboxCache 静默回退空缓存。
	if err := atomicWriteFile(sm.sandboxSkillsCachePath, data, 0600); err != nil {
		logger.Debug("write sandbox skills cache failed: %v", err)
	}
}

func (sm *SkillManager) loadSandboxCacheMaps() (map[string]string, map[string]string) {
	cache := sm.loadSandboxCache()
	paths := make(map[string]string)
	descs := make(map[string]string)
	for _, item := range cache.Skills {
		name := strings.TrimSpace(item.Name)
		if name == "" || !skillNameRe.MatchString(name) {
			continue
		}
		descs[name] = item.Description
		paths[name] = NormalizeCachedSandboxSkillPath(name, item.Path)
	}
	return paths, descs
}

func (sm *SkillManager) scanPluginSkills(
	skillsByName map[string]*SkillInfo,
	skillConfigs map[string]map[string]interface{},
	modified *bool,
	activeOnly bool,
	runtime string,
	showSandboxPath bool,
	sandboxPaths, sandboxDescs map[string]string,
) {
	entries, err := os.ReadDir(sm.pluginsRoot)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pluginName := entry.Name()
		skillsDir := filepath.Join(sm.pluginsRoot, pluginName, "skills")
		skillEntries, err := os.ReadDir(skillsDir)
		if err != nil {
			continue
		}
		for _, se := range skillEntries {
			if !se.IsDir() {
				continue
			}
			skillName := se.Name()
			if !skillNameRe.MatchString(skillName) {
				continue
			}
			if skillsByName[skillName] != nil {
				continue
			}
			skillDir := filepath.Join(skillsDir, skillName)
			skillMd := NormalizeSkillMarkdownPath(skillDir)
			if skillMd == "" {
				continue
			}
			active := true
			if sc, ok := skillConfigs[skillName]; ok {
				if a, ok := sc["active"].(bool); ok {
					active = a
				}
			}
			if _, ok := skillConfigs[skillName]; !ok {
				skillConfigs[skillName] = map[string]interface{}{"active": active}
				*modified = true
			}
			if activeOnly && !active {
				continue
			}
			desc := ""
			// #nosec G304 -- skillMd is a filepath.Join under the plugin skills
			// dir; entry names come from os.ReadDir of that root, so the path
			// stays contained.
			if content, err := os.ReadFile(skillMd); err == nil {
				desc = ParseFrontmatterDescription(string(content))
			}
			// 存在性判定用缓存键（对齐 Python），空描述条目同样算已存在。
			_, cached := sandboxPaths[skillName]
			sandboxExists := runtime == "sandbox" && cached
			pathStr := skillMd
			if runtime == "sandbox" && showSandboxPath {
				if p, ok := sandboxPaths[skillName]; ok {
					pathStr = p
				} else {
					pathStr = DefaultSandboxSkillPath(skillName)
				}
			}
			skillsByName[skillName] = &SkillInfo{
				Name:          skillName,
				Description:   desc,
				Path:          strings.ReplaceAll(pathStr, "\\", "/"),
				Active:        active,
				SourceType:    SourcePlugin,
				SourceLabel:   pluginName,
				LocalExists:   true,
				SandboxExists: sandboxExists,
				PluginName:    pluginName,
				Readonly:      true,
			}
		}
	}
}

func (sm *SkillManager) removeFromSandboxCache(name string) {
	cache := sm.loadSandboxCache()
	filtered := make([]SandboxCacheEntry, 0, len(cache.Skills))
	for _, item := range cache.Skills {
		if strings.TrimSpace(item.Name) != name {
			filtered = append(filtered, item)
		}
	}
	if len(filtered) != len(cache.Skills) {
		cache.Skills = filtered
		sm.saveSandboxCache(cache)
	}
}

func sortedKeys(m map[string]*SkillInfo) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedCacheKeys(m map[string]SandboxCacheEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
