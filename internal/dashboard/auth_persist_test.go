package dashboard

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

	// The persisted config must carry username + password hash (never
	// plaintext, never the legacy pbkdf2_password key).
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
	hash, _ := dash["password"].(string)
	if !isPBKDF2Hash(hash) {
		t.Fatalf("dashboard.password must be a PBKDF2 hash, got %q", hash)
	}
	if hash == "NewPass@2026" {
		t.Fatal("plaintext password must never be persisted")
	}
	if _, exists := dash["pbkdf2_password"]; exists {
		t.Fatal("legacy pbkdf2_password key must not be written")
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

	_, _, _, _ = s.auth.SetupTOTP()
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
	cfg := config.NewConfig(filepath.Join(dir, "config.json"))
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
	cfg := config.NewConfig(filepath.Join(dir, "config.json"))
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
	if got, _ := dash["password"].(string); !isPBKDF2Hash(got) {
		t.Fatal("auth persist via ConfigManager must carry the password hash into the snapshot")
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

func TestSetPasswordRotatesJWTSecret(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	pm := NewPasswordManager(cfgPath)

	// 设置初始密码并登录，取得旧 secret 下的会话 token。
	pm.SetPassword("OldPass@123")
	oldSecret := pm.JWTSecret()
	oldToken, err := pm.Login("OldPass@123")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if !pm.IsAuthenticated(oldToken) {
		t.Fatal("改密前签发的 token 应可认证")
	}
	// 遗留内存 token 也应在改密后失效。
	legacy := generateRandomToken(32)
	pm.RegisterToken(legacy)
	if !pm.IsAuthenticated(legacy) {
		t.Fatal("遗留内存 token 应可认证")
	}

	// 改密：轮换 secret，全部旧会话失效。
	pm.SetPassword("NewPass@456")
	if pm.JWTSecret() == oldSecret {
		t.Error("改密后 jwt_secret 应被轮换")
	}
	if pm.IsAuthenticated(oldToken) {
		t.Error("旧 secret 签发的 JWT 在轮换后应失效")
	}
	if pm.IsAuthenticated(legacy) {
		t.Error("遗留内存 token 在改密后应被清除")
	}
	if pm.VerifyPassword("OldPass@123") {
		t.Error("旧密码应校验失败")
	}
	newToken, err := pm.Login("NewPass@456")
	if err != nil {
		t.Fatalf("新密码登录: %v", err)
	}
	if !pm.IsAuthenticated(newToken) {
		t.Error("新密码签发的 token 应可认证")
	}

	// 落盘配置携带新 secret。
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("config not written: %v", err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	dash, _ := cfg["dashboard"].(map[string]interface{})
	persisted, _ := dash["jwt_secret"].(string)
	if persisted == "" || persisted == oldSecret {
		t.Errorf("落盘 jwt_secret 应为轮换后的值, got %q", persisted)
	}

	// 重启加载后语义保持一致。
	pm2 := NewPasswordManager(cfgPath)
	if pm2.JWTSecret() != pm.JWTSecret() {
		t.Error("重启后 secret 应等于轮换后的值")
	}
	if pm2.IsAuthenticated(oldToken) {
		t.Error("旧 token 重启后应保持失效")
	}
	if !pm2.IsAuthenticated(newToken) {
		t.Error("新 token 重启后应保持有效")
	}
}

// TestLegacyPlaintextAndOldFieldMigrated: 旧配置（pbkdf2_password 旧字段 +
// password 明文残留）加载时以 pbkdf2_password 迁移哈希、明文被忽略；SetPassword
// 后磁盘统一为 password=哈希，旧字段与明文全部清除（bug.md 4.2）。
func TestLegacyPlaintextAndOldFieldMigrated(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")

	// 模拟旧配置：旧字段 pbkdf2_password 存哈希，password 字段残留明文。
	legacyPlain := "LegacyPass@1"
	legacyHash := hashPBKDF2(legacyPlain)
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username":        "admin",
		"password":        legacyPlain,
		"pbkdf2_password": legacyHash,
	})

	pm := NewPasswordManager(cfgPath)
	// 哈希从旧字段迁移，明文不参与校验。
	if !pm.VerifyPassword(legacyPlain) {
		t.Fatal("hash-based verification must accept the legacy password")
	}
	if pm.Username() != "admin" {
		t.Fatalf("username = %q, want admin", pm.Username())
	}

	// 改密后：磁盘只有 password=新哈希，明文与旧字段都不存在。
	pm.SetPassword("NewSecure@2026")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	d, ok := onDisk["dashboard"].(map[string]interface{})
	if !ok {
		t.Fatal("missing dashboard section")
	}
	if h, _ := d["password"].(string); !isPBKDF2Hash(h) || h == legacyHash {
		t.Fatalf("dashboard.password must be the new PBKDF2 hash, got %q", h)
	}
	if _, exists := d["pbkdf2_password"]; exists {
		t.Fatal("legacy pbkdf2_password key must be removed on write")
	}
	if !pm.VerifyPassword("NewSecure@2026") {
		t.Fatal("new password must verify")
	}
	if pm.VerifyPassword(legacyPlain) {
		t.Fatal("legacy password must no longer verify")
	}
}

// TestVerifyPasswordUsesHashField: 校验只基于 dashboard.password 哈希；
// 配置中的任何明文（如残留旧值）不参与校验。
func TestVerifyPasswordUsesHashField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")

	correct := "RealPass@9"
	hash := hashPBKDF2(correct)
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username": "admin",
		"password": hash,
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.VerifyPassword(correct) {
		t.Fatal("password matching the hash must verify")
	}
	if pm.VerifyPassword("wrong") {
		t.Fatal("wrong password must not verify")
	}
}

// writeAuthTestConfig writes a dashboard config fragment to disk for tests.
func writeAuthTestConfig(t *testing.T, cfgPath string, dash map[string]interface{}) {
	t.Helper()
	cfg := map[string]interface{}{"dashboard": dash}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestResetWhenChangeRequired: password 哈希存在但用户从未设置过自己的密码
// （password_change_required=true，初始密码只打印不落盘）时，重启自动重置：
// 生成新随机密码（哈希替换）并保持强制改密状态。
func TestResetWhenChangeRequired(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	oldHash := hashPBKDF2("Generated@1")
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username":                 "admin",
		"password":                 oldHash,
		"password_change_required": true,
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.PasswordChangeRequired() {
		t.Fatal("reset must keep password_change_required=true")
	}
	if pm.HashedPassword() == oldHash {
		t.Fatal("reset must replace the old generated hash")
	}
	if pm.VerifyPassword("Generated@1") {
		t.Fatal("the old generated password must not verify after reset")
	}
	// 磁盘上的 password 字段是新的合法哈希（绝不写明文），旧字段被清除。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	d, _ := onDisk["dashboard"].(map[string]interface{})
	if h, _ := d["password"].(string); !isPBKDF2Hash(h) || h == oldHash {
		t.Fatalf("dashboard.password must be a fresh PBKDF2 hash, got %q", h)
	}
	if _, exists := d["pbkdf2_password"]; exists {
		t.Fatal("legacy pbkdf2_password key must be removed")
	}
}

// TestNoResetWhenUserSetPassword: 用户已通过 SetPassword 改过密码
// （password_change_required=false），重启不重置（哈希即凭据）。
func TestNoResetWhenUserSetPassword(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	userPass := "UserSet@2026"
	hash := hashPBKDF2(userPass)
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username":                 "admin",
		"password":                 hash,
		"password_change_required": false,
	})

	pm := NewPasswordManager(cfgPath)
	if pm.PasswordChangeRequired() {
		t.Fatal("must not reset when the user already set a password")
	}
	if pm.HashedPassword() != hash {
		t.Fatal("user-set hash must be kept across restarts")
	}
	if !pm.VerifyPassword(userPass) {
		t.Fatal("user-set password must keep verifying after restart")
	}
}

// readDashboardSection reads the on-disk dashboard section (helper).
func readDashboardSection(t *testing.T, cfgPath string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	d, _ := onDisk["dashboard"].(map[string]interface{})
	return d
}

// TestResetWhenPasswordEmptyUsernamePresent: password 为空、username 非空时
// 重置，且重置会把用户名去除（恢复默认 admin 用于打印，但不写入配置文件，
// 用户重新设置账号密码时由 SetUsername 落盘）。
func TestResetWhenPasswordEmptyUsernamePresent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username": "stale_user",
		"password": "",
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.PasswordChangeRequired() {
		t.Fatal("empty password must trigger a reset")
	}
	if pm.Username() != "admin" {
		t.Fatalf("stale username must be dropped on reset, got %q", pm.Username())
	}
	if pm.VerifyPassword("") {
		t.Fatal("empty password must not verify")
	}
	// 重置时用户名不落盘。
	d := readDashboardSection(t, cfgPath)
	if _, exists := d["username"]; exists {
		t.Fatalf("reset must not write a username to disk, got %q", d["username"])
	}
}

// TestResetWhenUsernameEmptyPasswordPresent: username 为空、password 非空时
// 重置（清除原密码值），用户名恢复默认 admin（仅内存/打印，不落盘）。
func TestResetWhenUsernameEmptyPasswordPresent(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	oldHash := hashPBKDF2("Orphan@1")
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username": "",
		"password": oldHash,
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.PasswordChangeRequired() {
		t.Fatal("empty username must trigger a reset")
	}
	if pm.Username() != "admin" {
		t.Fatalf("username must be restored to admin on reset, got %q", pm.Username())
	}
	if pm.HashedPassword() == oldHash {
		t.Fatal("orphan password must be cleared (replaced) on reset")
	}
	if pm.VerifyPassword("Orphan@1") {
		t.Fatal("orphan password must not verify after reset")
	}
	if _, exists := readDashboardSection(t, cfgPath)["username"]; exists {
		t.Fatal("reset must not write a username to disk")
	}
}

// TestResetWhenBothEmpty: username 与 password 都为空时重置。
func TestResetWhenBothEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username": "",
		"password": "",
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.PasswordChangeRequired() {
		t.Fatal("empty credentials must trigger a reset")
	}
	if pm.Username() != "admin" {
		t.Fatalf("username must be admin after reset, got %q", pm.Username())
	}
	if _, exists := readDashboardSection(t, cfgPath)["username"]; exists {
		t.Fatal("reset must not write a username to disk")
	}
}

// TestResetStateWaitsForSetup: 重置产物（username 不落盘 + password=哈希 +
// change_required=true）重启后必须保持等待状态：不重复重置、内存恢复 admin
// 供登录、原密码仍可校验、用户名仍未写入配置。
func TestResetStateWaitsForSetup(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	hash := hashPBKDF2("Printed@1")
	// 模拟真实重置产物：配置里只有 password=哈希 与 change_required，没有
	// username 键（重置时用户名不落盘）。
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"password":                 hash,
		"password_change_required": true,
	})

	pm := NewPasswordManager(cfgPath)
	if pm.PasswordChangeRequired() != true {
		t.Fatal("reset state must keep password_change_required=true")
	}
	if pm.Username() != "admin" {
		t.Fatalf("in-memory username must be admin for login, got %q", pm.Username())
	}
	if pm.HashedPassword() != hash {
		t.Fatal("reset state must NOT re-reset: hash must be preserved")
	}
	if !pm.VerifyPassword("Printed@1") {
		t.Fatal("the printed password must keep verifying in the waiting state")
	}
	if _, exists := readDashboardSection(t, cfgPath)["username"]; exists {
		t.Fatal("waiting state must not write a username to disk")
	}
}

// TestSetPasswordWritesDefaultUsername: 重置等待状态（用户名未落盘）下改密，
// 用户名默认填入 admin 并自动写入配置文件。
func TestSetPasswordWritesDefaultUsername(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	hash := hashPBKDF2("Printed@1")
	// 重置等待状态：磁盘无 username 键。
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"password":                 hash,
		"password_change_required": true,
	})

	pm := NewPasswordManager(cfgPath)
	// 改密前：用户名尚未写入配置。
	if _, exists := readDashboardSection(t, cfgPath)["username"]; exists {
		t.Fatal("username must not be on disk before SetPassword")
	}

	pm.SetPassword("NewSetup@2026")
	if pm.PasswordChangeRequired() {
		t.Fatal("SetPassword must clear the change-required flag")
	}
	if pm.Username() != "admin" {
		t.Fatalf("username must default to admin, got %q", pm.Username())
	}
	// 改密后：用户名自动填入配置文件。
	d := readDashboardSection(t, cfgPath)
	if got, _ := d["username"].(string); got != "admin" {
		t.Fatalf("username must be written to config after SetPassword, got %q", got)
	}
	if h, _ := d["password"].(string); !isPBKDF2Hash(h) {
		t.Fatalf("password must be a PBKDF2 hash, got %q", h)
	}
	if !pm.VerifyPassword("NewSetup@2026") {
		t.Fatal("new password must verify")
	}

	// 重启后凭据完整：admin + 新密码，不再重置。
	pm2 := NewPasswordManager(cfgPath)
	if pm2.Username() != "admin" {
		t.Fatalf("reloaded username = %q, want admin", pm2.Username())
	}
	if pm2.PasswordChangeRequired() {
		t.Fatal("after setup, change must no longer be required")
	}
	if !pm2.VerifyPassword("NewSetup@2026") {
		t.Fatal("reloaded manager must verify the new password")
	}
}

// TestMigrateLegacyPbkdf2Field: 旧版配置只写 pbkdf2_password 字段时，加载按
// 旧字段迁移哈希；任何一次凭据保存后磁盘统一为 password 字段并清除旧键。
func TestMigrateLegacyPbkdf2Field(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	userPass := "Legacy@1"
	hash := hashPBKDF2(userPass)
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username":        "admin",
		"pbkdf2_password": hash,
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.VerifyPassword(userPass) {
		t.Fatal("hash from the legacy pbkdf2_password field must verify")
	}

	// 触发一次保存（换用户名）→ 磁盘统一为 password=哈希，旧键删除。
	pm.SetUsername("newadmin")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	d, _ := onDisk["dashboard"].(map[string]interface{})
	if h, _ := d["password"].(string); h != hash {
		t.Fatalf("dashboard.password must be the migrated hash, got %q", h)
	}
	if _, exists := d["pbkdf2_password"]; exists {
		t.Fatal("legacy pbkdf2_password key must be removed after a save")
	}
}

// TestMigrateOnStartup: 旧配置（password 明文残留 + pbkdf2_password 旧字段）
// 在 NewPasswordManager 启动时立即迁移为 password=哈希，明文与旧字段不留盘。
func TestMigrateOnStartup(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	userPass := "Legacy@1"
	hash := hashPBKDF2(userPass)
	writeAuthTestConfig(t, cfgPath, map[string]interface{}{
		"username":        "astrbot",
		"password":        userPass, // 明文残留（旧版写入）
		"pbkdf2_password": hash,
	})

	pm := NewPasswordManager(cfgPath)
	if !pm.VerifyPassword(userPass) {
		t.Fatal("legacy credential must verify via the migrated hash")
	}
	if pm.Username() != "astrbot" {
		t.Fatalf("username must be preserved (no reset), got %q", pm.Username())
	}

	// 启动即迁移：磁盘立即只剩 password=哈希。
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk map[string]interface{}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatal(err)
	}
	d, _ := onDisk["dashboard"].(map[string]interface{})
	h, _ := d["password"].(string)
	if h != hash {
		t.Fatalf("dashboard.password must be the migrated hash right after startup, got %q", h)
	}
	if _, exists := d["pbkdf2_password"]; exists {
		t.Fatal("legacy pbkdf2_password must be removed at startup")
	}
}

// TestCryptoRandIntUniformity: cryptoRandInt 必须返回 [0,n) 内的均匀随机数
// （拒绝采样无模运算偏差，bug.md 5.1）。
func TestCryptoRandIntUniformity(t *testing.T) {
	if got := cryptoRandInt(1); got != 0 {
		t.Fatalf("cryptoRandInt(1) = %d, want 0", got)
	}
	if got := cryptoRandInt(0); got != 0 {
		t.Fatalf("cryptoRandInt(0) = %d, want 0", got)
	}
	// 大量样本全部落在 [0,n) 内（越界即触发重试逻辑或拒绝采样失败）。
	for _, n := range []int{2, 3, 7, 26, 32, 62, 36, recoveryCodeAlphabetLen()} {
		for i := 0; i < 2000; i++ {
			v := cryptoRandInt(n)
			if v < 0 || v >= n {
				t.Fatalf("cryptoRandInt(%d) = %d out of range", n, v)
			}
		}
	}
	// 生成随机密码与恢复码不应越界。
	pwd := generateRandomPassword()
	if len(pwd) != PasswordLength {
		t.Fatalf("password length = %d, want %d", len(pwd), PasswordLength)
	}
	codes := generateRecoveryCodes(10)
	for _, c := range codes {
		if len(c) != recoveryCodeLength {
			t.Fatalf("recovery code length = %d, want %d", len(c), recoveryCodeLength)
		}
	}
}

func recoveryCodeAlphabetLen() int { return len(recoveryCodeAlphabet) }

// TestRecoveryCodeConsumedOnce: 恢复码一次性——VerifyTOTPEx 命中后必须从列表
// 移除并持久化，同一恢复码不能二次使用（bug.md 5.2）。
func TestRecoveryCodeConsumedOnce(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "cmd_config.json")
	pm := NewPasswordManager(cfgPath)

	_, _, codes, err := pm.SetupTOTP()
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) == 0 {
		t.Fatal("no recovery codes generated")
	}

	// 首次使用（含连字符/大小写归一化）应通过且消耗。
	used := codes[0]
	normalized := strings.ToUpper(used)
	if len(normalized) > 8 {
		normalized = normalized[:4] + "-" + normalized[4:8] + "-" + normalized[8:]
	}
	ok, usedRecovery := pm.VerifyTOTPEx(normalized)
	if !ok || !usedRecovery {
		t.Fatalf("first recovery use must succeed: ok=%v usedRecovery=%v", ok, usedRecovery)
	}

	// 同一恢复码立即复用必须失败。
	if ok, _ := pm.VerifyTOTPEx(used); ok {
		t.Fatal("consumed recovery code must not verify again")
	}

	// 其余恢复码不受影响。
	if ok, usedRecovery := pm.VerifyTOTPEx(codes[1]); !ok || !usedRecovery {
		t.Fatalf("other recovery codes must still work: ok=%v used=%v", ok, usedRecovery)
	}

	// 重启加载后消耗状态持久化：已用码失效、未用码仍有效。
	pm2 := NewPasswordManager(cfgPath)
	if ok, _ := pm2.VerifyTOTPEx(codes[0]); ok {
		t.Fatal("consumed recovery code must stay invalid after reload")
	}
	if ok, _ := pm2.VerifyTOTPEx(codes[2]); !ok {
		t.Fatal("unconsumed recovery code must stay valid after reload")
	}
}
