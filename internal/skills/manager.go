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
func (sm *SkillManager) ListSkills(activeOnly bool, runtime string) []*SkillInfo {
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
			sandboxExists := runtime == "sandbox" && sandboxDescs[name] != ""
			sourceType := SourceLocalOnly
			sourceLabel := "local"
			if sandboxExists {
				sourceType = SourceBoth
				sourceLabel = "synced"
			}
			pathStr := skillMd
			if runtime == "sandbox" {
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
	sm.scanPluginSkills(skillsByName, skillConfigs, &modified, activeOnly, runtime, sandboxPaths, sandboxDescs)

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

// BuildSkillsPrompt generates the skills section for the system prompt.
func BuildSkillsPrompt(skills []*SkillInfo) string {
	if len(skills) == 0 {
		return ""
	}
	var lines []string
	lines = append(lines, "## Skills\n\n")
	lines = append(lines, "You have specialized skills — reusable instruction bundles stored in `SKILL.md` files.\n\n")
	lines = append(lines, "### Available skills\n\n")
	for _, s := range skills {
		desc := s.Description
		if desc == "" {
			desc = "No description"
		}
		lines = append(lines, fmt.Sprintf("- **%s**: %s\n  File: `%s`\n", s.Name, desc, s.Path))
	}
	return strings.Join(lines, "")
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

// saveConfig writes skills.json atomically: 先写临时文件并 fsync，再 os.Rename
// 替换，避免并发写/崩溃截断导致配置丢失（与 plugin manifest 的 Save 一致）。
func (sm *SkillManager) saveConfig(cfg skillsConfig) error {
	data, err := json.MarshalIndent(cfg, "", "    ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(sm.configPath)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(sm.configPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			if err := os.Remove(tmpName); err != nil {
				logger.Debug("cleanup temp config %s failed: %v", tmpName, err)
			}
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
	if err := os.Rename(tmpName, sm.configPath); err != nil {
		return err
	}
	committed = true
	return nil
}

func (sm *SkillManager) loadSandboxCache() SandboxCache {
	data, err := os.ReadFile(sm.sandboxSkillsCachePath)
	if err != nil {
		return SandboxCache{Version: SandboxCacheVersion, Skills: []SandboxCacheEntry{}}
	}
	var cache SandboxCache
	if err := json.Unmarshal(data, &cache); err != nil {
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
	// #nosec G306 -- cache file stores non-sensitive skill metadata; 0600 keeps
	// other local users from reading it.
	if err := os.WriteFile(sm.sandboxSkillsCachePath, data, 0600); err != nil {
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
			sandboxExists := runtime == "sandbox" && sandboxDescs[skillName] != ""
			pathStr := skillMd
			if runtime == "sandbox" {
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
