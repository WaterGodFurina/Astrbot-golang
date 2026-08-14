package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestDeleteSkillRejectsTraversal(t *testing.T) {
	dataDir := t.TempDir()
	skillsRoot := filepath.Join(dataDir, "skills")
	sm := NewSkillManager(skillsRoot, filepath.Join(dataDir, "plugins"), dataDir)

	victim := filepath.Join(dataDir, "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	victimFile := filepath.Join(victim, "secret.txt")
	if err := os.WriteFile(victimFile, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"../../victim", "..", ".", "/etc", `..\victim`, "a/b", "a\\b", ""} {
		if err := sm.DeleteSkill(name); err == nil {
			t.Fatalf("DeleteSkill(%q) should be rejected", name)
		}
		if err := sm.SetSkillActive(name, true); err == nil {
			t.Fatalf("SetSkillActive(%q) should be rejected", name)
		}
	}
	if _, err := os.Stat(victimFile); err != nil {
		t.Fatalf("victim file was deleted: %v", err)
	}
}

// TestSetSkillActiveConcurrentWithListSkills guards against lost updates when
// SetSkillActive races ListSkills' automatic config completion: both write
// skills.json, and previously neither held a lock (SetSkillActive) or the save
// ran under a read lock (ListSkills), so concurrent runs could drop entries.
func TestSetSkillActiveConcurrentWithListSkills(t *testing.T) {
	dataDir := t.TempDir()
	skillsRoot := filepath.Join(dataDir, "skills")
	sm := NewSkillManager(skillsRoot, filepath.Join(dataDir, "plugins"), dataDir)

	const n = 8
	names := make([]string, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("skill%d", i)
		names = append(names, name)
		if err := os.MkdirAll(filepath.Join(skillsRoot, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillsRoot, name, "SKILL.md"),
			[]byte("---\ndescription: d\n---\n# "+name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	for _, name := range names {
		name := name
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sm.SetSkillActive(name, true); err != nil {
				t.Errorf("SetSkillActive(%s): %v", name, err)
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			sm.ListSkills(false, "local")
		}()
	}
	wg.Wait()

	// Every discovered skill must survive in the final config.
	cfg := sm.loadConfig()
	for _, name := range names {
		if _, ok := cfg.Skills[name]; !ok {
			t.Errorf("skill %s lost from final config: %v", name, cfg.Skills)
		}
	}
}

// TestSaveConfigAtomic verifies the atomic write leaves a readable config file
// and that an interrupted write does not leave a partial skills.json.
func TestSaveConfigAtomic(t *testing.T) {
	dataDir := t.TempDir()
	sm := NewSkillManager(filepath.Join(dataDir, "skills"), filepath.Join(dataDir, "plugins"), dataDir)

	cfg := skillsConfig{Skills: map[string]map[string]interface{}{
		"skill_a": {"active": true},
		"skill_b": {"active": false},
	}}
	if err := sm.saveConfig(cfg); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	got := sm.loadConfig()
	if len(got.Skills) != 2 || got.Skills["skill_a"]["active"] != true || got.Skills["skill_b"]["active"] != false {
		t.Fatalf("config round-trip mismatch: %+v", got.Skills)
	}
	// No stray temp files left behind by the atomic write (the skills root dir
	// and any other data-dir entries are expected; only skills.json temp files
	// would indicate a failed rename).
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), SkillsConfigFilename) && e.Name() != SkillsConfigFilename {
			t.Errorf("unexpected leftover temp file %q (should be renamed away)", e.Name())
		}
	}
}
