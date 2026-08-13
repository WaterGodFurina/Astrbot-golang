package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFirstSetupPersistsCredentials(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")

	// Simulate first run: no config -> random password generated & stored.
	pm := NewPasswordManager(cfgPath)
	if pm.PasswordChangeRequired() != true {
		t.Fatal("first run should require a password change")
	}

	// User sets a new username + password (first-install onboarding).
	pm.SetUsername("mybot")
	pm.SetPassword("NewPass@2026")

	// The persisted config must carry username + plaintext password + hash.
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid config json: %v", err)
	}
	dash, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		t.Fatal("missing dashboard section")
	}
	if got, _ := dash["username"].(string); got != "mybot" {
		t.Fatalf("username = %q, want mybot", got)
	}
	if got, _ := dash["password"].(string); got != "NewPass@2026" {
		t.Fatalf("password = %q, want NewPass@2026", got)
	}
	if got, _ := dash["pbkdf2_password"].(string); got == "" {
		t.Fatal("pbkdf2_password should be persisted")
	}
	if upgraded, _ := dash["password_storage_upgraded"].(bool); !upgraded {
		t.Fatal("password_storage_upgraded should be true")
	}

	// A fresh manager reading the config must accept the new credentials.
	pm2 := NewPasswordManager(cfgPath)
	if pm2.Username() != "mybot" {
		t.Fatalf("reloaded username = %q", pm2.Username())
	}
	if !pm2.VerifyPassword("NewPass@2026") {
		t.Fatal("reloaded manager should verify the new password")
	}
	if pm2.PasswordChangeRequired() {
		t.Fatal("after setup, change should no longer be required")
	}
}
