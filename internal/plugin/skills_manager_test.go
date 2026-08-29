package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/skills"
)

// newSkillManagerForTest 在临时目录构造一个含 SKILL.md 的 SkillManager，
// 验证 skills RPC 背后的 ListSkills / SetSkillActive / DeleteSkill 宿主行为。
func newSkillManagerForTest(t *testing.T) (*skills.SkillManager, string) {
	t.Helper()
	base := t.TempDir()
	sm := skills.NewSkillManager("", "", base)
	skillDir := filepath.Join(base, "skills", "weather")
	if err := os.MkdirAll(skillDir, 0750); err != nil {
		t.Fatalf("mkdir skill: %v", err)
	}
	md := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(md, []byte("---\nname: weather\ndescription: 查询天气\n---\n# Weather\n查天气。\n"), 0640); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	return sm, skillDir
}

// TestSkillsManagerListActiveDelete 覆盖宿主 skills 能力（skills RPC 数据源）：
// 发现 SKILL.md → 列出 → 禁用 → 数据落地 → 删除。
func TestSkillsManagerListActiveDelete(t *testing.T) {
	sm, skillDir := newSkillManagerForTest(t)

	list := sm.ListSkillsInfo()
	if len(list) == 0 {
		t.Fatalf("ListSkillsInfo: want >=1 skill, got 0")
	}
	found := false
	for _, s := range list {
		if s["name"] == "weather" && s["active"] == true {
			found = true
		}
	}
	if !found {
		t.Fatalf("skill 'weather' not found active in %v", list)
	}

	// 禁用
	if err := sm.SetSkillActive("weather", false); err != nil {
		t.Fatalf("SetSkillActive(false): %v", err)
	}
	after := sm.ListSkills(false, "")
	ok := false
	for _, s := range after {
		if s.Name == "weather" && !s.Active {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("skill 'weather' should be inactive after SetSkillActive(false)")
	}

	// active_only 过滤
	activeOnly := sm.ListSkills(true, "local")
	for _, s := range activeOnly {
		if s.Name == "weather" {
			t.Fatalf("inactive skill should not appear in active-only list")
		}
	}

	// 非法名被拒（对齐 validateSkillName）
	if err := sm.SetSkillActive("..", true); err == nil {
		t.Fatalf("SetSkillActive('..') should be rejected")
	}
	if err := sm.DeleteSkill("../evil"); err == nil {
		t.Fatalf("DeleteSkill('../evil') should be rejected")
	}

	// 删除
	if err := sm.DeleteSkill("weather"); err != nil {
		t.Fatalf("DeleteSkill: %v", err)
	}
	if _, err := os.Stat(skillDir); !os.IsNotExist(err) {
		t.Fatalf("skill dir should be removed, stat err = %v", err)
	}
	final := sm.ListSkills(false, "")
	for _, s := range final {
		if s.Name == "weather" {
			t.Fatalf("skill 'weather' should be gone after DeleteSkill")
		}
	}
}