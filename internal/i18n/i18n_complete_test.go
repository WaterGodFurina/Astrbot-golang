package i18n

import (
	"encoding/json"
	"os"
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
