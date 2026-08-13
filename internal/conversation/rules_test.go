package conversation

import (
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
)

func TestSessionRulesCRUD(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "rules.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	m := NewManager(database)

	umo := "qq:group:123"
	if err := m.SetSessionRule(umo, RuleProviderChatCompletion, "openai/gpt-4"); err != nil {
		t.Fatalf("set provider rule: %v", err)
	}
	if err := m.SetSessionRule(umo, RuleServiceConfig, map[string]interface{}{"custom_name": "我的会话"}); err != nil {
		t.Fatalf("set service rule: %v", err)
	}

	rules := m.GetSessionRules(umo)
	if rules[RuleProviderChatCompletion] != "openai/gpt-4" {
		t.Fatalf("provider rule = %v", rules[RuleProviderChatCompletion])
	}
	sc, ok := rules[RuleServiceConfig].(map[string]interface{})
	if !ok || sc["custom_name"] != "我的会话" {
		t.Fatalf("service rule = %v", rules[RuleServiceConfig])
	}

	all, err := m.ListAllSessionRules()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 || all[umo][RuleProviderChatCompletion] != "openai/gpt-4" {
		t.Fatalf("list = %v", all)
	}

	// Invalid rule key rejected.
	if err := m.SetSessionRule(umo, "hack_key", "x"); err == nil {
		t.Fatal("invalid rule key should be rejected")
	}

	if err := m.DeleteSessionRule(umo, RuleProviderChatCompletion); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got := m.GetSessionRules(umo); got[RuleProviderChatCompletion] != nil {
		t.Fatalf("rule should be deleted: %v", got)
	}
}

func TestDeleteAllSessionRules(t *testing.T) {
	database, err := db.New(filepath.Join(t.TempDir(), "rules2.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	m := NewManager(database)

	umo := "qq:group:9"
	_ = m.SetSessionRule(umo, RuleProviderChatCompletion, "openai/gpt-4")
	_ = m.SetSessionRule(umo, RuleServiceConfig, map[string]interface{}{"custom_name": "x"})

	// Empty key deletes all rules of the session.
	if err := m.DeleteSessionRule(umo, ""); err != nil {
		t.Fatalf("delete all: %v", err)
	}
	if got := m.GetSessionRules(umo); len(got) != 0 {
		t.Fatalf("expected no rules after delete-all, got %v", got)
	}
}
