package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
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

// TestInjectAuthFieldsPreservesTOTP: 配置保存整体替换 dashboard 时必须保留
// TOTP 段，否则任意一次配置保存即清掉双因素认证（H-24）。
func TestInjectAuthFieldsPreservesTOTP(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	secret, _, _, err := s.auth.SetupTOTP()
	if err != nil {
		t.Fatal(err)
	}

	dash := map[string]interface{}{"port": 8080}
	s.injectAuthFields(dash)

	totp, ok := dash["totp"].(map[string]interface{})
	if !ok {
		t.Fatalf("injectAuthFields must re-assert the totp section, got %#v", dash["totp"])
	}
	if enable, _ := totp["enable"].(bool); !enable {
		t.Fatalf("totp enable must be true, got %#v", totp)
	}
	if got, _ := totp["secret"].(string); got != secret {
		t.Fatalf("totp secret mismatch: got %q, want %q", got, secret)
	}
	if rh, _ := totp["recovery_code_hash"].(string); rh == "" {
		t.Fatal("recovery_code_hash must be persisted")
	}
}

// TestInjectAuthFieldsTotpDisabledClearsSection: TOTP 被禁用后回填的 totp 段
// 必须反映禁用状态（enable=false、secret 清空），不得残留旧密钥。
func TestInjectAuthFieldsTotpDisabledClearsSection(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "cmd_config.json"))
	defer s.Stop()

	s.auth.SetupTOTP()
	s.auth.DisableTOTP()

	dash := map[string]interface{}{}
	s.injectAuthFields(dash)
	totp, ok := dash["totp"].(map[string]interface{})
	if !ok {
		t.Fatal("totp section must still be re-asserted after disable")
	}
	if enable, _ := totp["enable"].(bool); enable {
		t.Fatalf("totp enable must be false after disable, got %#v", totp)
	}
	if got, _ := totp["secret"].(string); got != "" {
		t.Fatalf("totp secret must be cleared after disable, got %q", got)
	}
}

// TestConfigSavePreservesTOTP: 模拟 WebUI 保存 dashboard 配置（不携带 totp），
// 验证 totp 段被 injectAuthFields 回填进 ConfigManager 并落盘，同时客户端
// 可见的 config 快照不泄露 totp（H-24）。
func TestConfigSavePreservesTOTP(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(0, filepath.Join(dir, "cmd_config.json"))
	defer s.Stop()

	cm := config.NewConfigManager()
	cfg := config.NewConfig(filepath.Join(dir, "config.json"), nil)
	cm.Register("default", cfg)
	s.configMgr = cm

	secret, _, _, err := s.auth.SetupTOTP()
	if err != nil {
		t.Fatal(err)
	}

	// WebUI 保存 dashboard 时不携带 totp 字段。
	if err := s.setConfigData("dashboard", map[string]interface{}{"port": 8080}); err != nil {
		t.Fatal(err)
	}

	// ConfigManager 内存中 totp 段必须保留。
	all := cfg.All()
	dash, _ := all["dashboard"].(map[string]interface{})
	totp, ok := dash["totp"].(map[string]interface{})
	if !ok {
		t.Fatal("dashboard save must preserve the totp section")
	}
	if got, _ := totp["secret"].(string); got != secret {
		t.Fatalf("totp secret not preserved: got %q, want %q", got, secret)
	}

	// 落盘文件同样保留 totp。
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	if d, ok := onDisk["dashboard"].(map[string]interface{}); ok {
		if tp, ok := d["totp"].(map[string]interface{}); !ok || tp["secret"] != secret {
			t.Fatal("on-disk config must preserve the totp section")
		}
	} else {
		t.Fatal("on-disk config missing dashboard.totp")
	}

	// 客户端可见的 config 快照不得泄露 totp。
	if snap := s.getConfigSnapshot(); snap != nil {
		if sd, ok := snap["dashboard"].(map[string]interface{}); ok {
			if _, leaked := sd["totp"]; leaked {
				t.Fatal("config snapshot must not expose the totp section")
			}
		}
	}
}

// TestAuthPersistViaConfigManager: auth 的持久化必须走 ConfigManager（M-08），
// 与 ConfigManager.Save 共享同一内存快照与写锁，避免并发互相覆盖丢失更新。
func TestAuthPersistViaConfigManager(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(0, filepath.Join(dir, "cmd_config.json"))
	defer s.Stop()

	cm := config.NewConfigManager()
	cfg := config.NewConfig(filepath.Join(dir, "config.json"), nil)
	cm.Register("default", cfg)
	s.configMgr = cm
	s.auth.SetConfigManager(cm)

	// 先保存一个与 dashboard 无关的键，模拟 ConfigManager 侧已有数据。
	if err := cfg.Set("language", "en"); err != nil {
		t.Fatal(err)
	}

	// auth 侧写入 TOTP 与密码，必须反映到 ConfigManager 内存快照与磁盘。
	if _, _, _, err := s.auth.SetupTOTP(); err != nil {
		t.Fatal(err)
	}
	s.auth.SetPassword("NewPass@2026")

	all := cfg.All()
	dash, _ := all["dashboard"].(map[string]interface{})
	if got, _ := dash["jwt_secret"].(string); got == "" {
		t.Fatal("auth persist via ConfigManager must carry jwt_secret into the snapshot")
	}
	if _, ok := dash["totp"].(map[string]interface{}); !ok {
		t.Fatal("auth persist via ConfigManager must carry totp into the snapshot")
	}
	if got, _ := dash["pbkdf2_password"].(string); got == "" {
		t.Fatal("auth persist via ConfigManager must carry pbkdf2_password into the snapshot")
	}
	// 无关键必须保留：auth 与 ConfigManager 共用同一快照，不互相覆盖。
	if got := cfg.GetString("language"); got != "en" {
		t.Fatalf("language lost after auth persist: %q", got)
	}

	// 落盘文件同样包含全部 auth 段。
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	d, ok := onDisk["dashboard"].(map[string]interface{})
	if !ok {
		t.Fatal("on-disk config missing dashboard section")
	}
	if _, ok := d["totp"].(map[string]interface{}); !ok {
		t.Fatal("on-disk config missing dashboard.totp after auth persist")
	}
	if got, _ := d["jwt_secret"].(string); got == "" {
		t.Fatal("on-disk config missing dashboard.jwt_secret after auth persist")
	}
}
