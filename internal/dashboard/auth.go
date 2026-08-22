// Package dashboard - authentication and password management.
// Ported from astrbot/core/utils/auth_password.py and astrbot/dashboard/services/auth_service.py
package dashboard

import (
	"crypto/md5" // #nosec G501 -- 遗留 MD5 密码哈希的迁移兼容，见 md5Hash
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/sha256"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/pbkdf2"
)

const (
	PasswordLength = 16
	// Username 是默认/重置后的 dashboard 用户名（配置缺失或凭据被清空时使用）。
	Username = "admin"
)

var authLogger = log.GetDefault().WithComponent("Auth")

// PasswordManager handles dashboard password generation, hashing, and verification.
// 凭据只以两种形式存在：内存中的明文（生成后临时持有、打印给用户）与配置里的
// PBKDF2 哈希（dashboard.password 字段，永不写明文）。
type PasswordManager struct {
	mu                     sync.RWMutex
	configPath             string
	configMgr              *config.ConfigManager // 注入后 auth 持久化统一走 ConfigManager（M-08）
	username               string
	hashedPassword         string // PBKDF2 hash（dashboard.password 字段）
	initialPassword        string // 启动生成/重置的初始密码明文（仅内存，供 setup 校验与测试）
	passwordChangeRequired bool
	jwtSecret              string
	tokens                 map[string]bool // active session tokens
	revoked                map[string]bool // JWT jti blacklist (in-memory, persisted)
	revokedPath            string          // 持久化 jti 黑名单的 JSON 文件路径
	totpSecret             string          // TOTP 密钥（base32）
	totpEnabled            bool            // 是否启用 TOTP 双重认证
	totpRecoveryCodes      []string        // 恢复码的 SHA-256 哈希列表（不落明文）
}

// NewPasswordManager creates a password manager, generating a random password
// on first run, when the config holds no usable credential, or when
// --reset-password is used. The generated password is printed to the console
// and stored only as a PBKDF2 hash; the user must change it after logging in.
//
// dashboard.password 只允许存放哈希（PBKDF2 或遗留 MD5），绝不出现明文。空值
// 规则（均触发重置并打印新随机密码，用户名恢复默认 admin）：
//   - password 为空、username 非空：去除用户名，重置；
//   - username 为空、password 非空：清除密码，重置；
//   - 两者都为空：重置；
//   - 两者都有但用户从未设置过自己的密码（password_change_required=true，
//     初始密码只打印不落盘）：重置。
func NewPasswordManager(configPath string) *PasswordManager {
	pm := &PasswordManager{
		configPath: configPath,
		username:   Username,
	}

	// Check if we need to generate a new password
	needGenerate := false
	// 磁盘存在需要迁移的旧凭据键（明文 password 残留 / 旧字段名
	// pbkdf2_password）时，启动完成（JWT secret 就绪）后主动保存一次，
	// 让磁盘立即收敛为 password=哈希，不把明文留在磁盘上。
	migrateAuthKeys := false

	// Check for --reset-password flag via env
	if os.Getenv("ASTRBOT_RESET_DASHBOARD_PASSWORD") == "1" {
		needGenerate = true
		_ = os.Unsetenv("ASTRBOT_RESET_DASHBOARD_PASSWORD")
	}

	// Load existing config to check if password exists
	if data, err := os.ReadFile(configPath); err == nil {
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) == nil {
			if dash, ok := cfg["dashboard"].(map[string]interface{}); ok {
				// 记录磁盘上是否存在 username 键：无键时 username 保持内存默认
				// admin，但需要区分"从未设置"与"重置等待状态"（两者都表现为
				// 无键，均不重置；只有键存在但值为空才按用户空值规则处理）。
				diskHasUsername := false
				if user, ok := dash["username"].(string); ok {
					diskHasUsername = true
					pm.username = user
				}
				// 哈希统一存 dashboard.password 字段；旧版 pbkdf2_password 字段
				// 迁移兼容（写回时会被统一为 password）。
				hashed, _ := dash["password"].(string)
				// password 字段只允许是合法哈希：旧配置的明文会被当作无效
				// 凭据，绝不拿明文参与校验。
				if hashed != "" && !isPBKDF2Hash(hashed) && !isMD5Hash(hashed) {
					hashed = ""
					migrateAuthKeys = true
				}
				if hashed == "" {
					// 回退旧字段名（迁移期）。
					if legacy, _ := dash["pbkdf2_password"].(string); legacy != "" &&
						(isPBKDF2Hash(legacy) || isMD5Hash(legacy)) {
						hashed = legacy
						migrateAuthKeys = true
					}
				}
				if _, legacy := dash["pbkdf2_password"]; legacy {
					migrateAuthKeys = true
				}
				pm.hashedPassword = hashed
				if changeReq, ok := dash["password_change_required"].(bool); ok {
					pm.passwordChangeRequired = changeReq
				}
				// 空值/凭据缺失即重置（详见 NewPasswordManager 注释）。
				switch {
				case pm.hashedPassword == "" && pm.username != "":
					// password 为空：去除用户名，让用户重新设置账号密码。
					pm.username = ""
					needGenerate = true
				case pm.hashedPassword == "" && pm.username == "":
					needGenerate = true
				case !diskHasUsername || pm.username == "":
					// 磁盘无 username 键（重置后等待状态）或用户名空、密码非空：
					//  - change_required=true：等待用户设置，恢复默认 admin 供
					//    登录，不重复重置；
					//  - change_required=false：用户已设置过自己的密码但用户名
					//    异常缺失，清除密码值，让用户重新设置账号密码。
					if pm.passwordChangeRequired {
						pm.username = Username
					} else {
						pm.hashedPassword = ""
						needGenerate = true
					}
				case pm.passwordChangeRequired:
					// 初始生成的密码只打印不落盘，用户尚未改密（重启后无人
					// 知晓）——自动重置并打印新随机密码。
					needGenerate = true
				}
				// 读取 TOTP 配置：dashboard.totp.{enable,secret,recovery_code_hash}
				if totpCfg, ok := dash["totp"].(map[string]interface{}); ok {
					if enable, ok := totpCfg["enable"].(bool); ok {
						pm.totpEnabled = enable
					}
					if secret, ok := totpCfg["secret"].(string); ok && secret != "" {
						pm.totpSecret = secret
						// 存在 secret 但未显式开启时按已启用处理，保证两者一致。
						if !pm.totpEnabled {
							pm.totpEnabled = true
						}
					}
					if rh, ok := totpCfg["recovery_code_hash"].(string); ok && rh != "" {
						pm.totpRecoveryCodes = parseRecoveryCodeHashes(rh)
					}
				}
			}
		} else {
			needGenerate = true
		}
	} else {
		// Config doesn't exist, first run
		needGenerate = true
	}

	if needGenerate {
		pm.generateAndStorePassword()
	}

	// Generate or load JWT secret
	pm.ensureJWTSecret()

	// 持久化 jti 黑名单路径并加载历史撤销记录，避免进程重启后已撤销的
	// token 重新有效。
	pm.revokedPath = filepath.Join(filepath.Dir(configPath), "jwt_revoked.json")
	pm.loadRevoked()

	// 迁移清理：启动时把磁盘凭据键统一为 password=哈希（删除明文残留与
	// 旧字段名），避免明文在磁盘上长期存在。
	if migrateAuthKeys && pm.jwtSecret != "" {
		pm.saveToConfig()
	}

	return pm
}

// generateAndStorePassword creates a random password, hashes it, stores the hash,
// and prints the plaintext to the console for the user. 用户名被清空（空值重置）
// 时恢复默认 admin 用于打印与登录，但**不写入配置文件**——重置只落密码哈希，
// 用户名等用户在改密/设置流程中显式重设后才由 SetUsername 落盘。
func (pm *PasswordManager) generateAndStorePassword() {
	password := generateRandomPassword()
	hashed := hashPBKDF2(password)
	pm.mu.Lock()
	if pm.username == "" {
		pm.username = Username
	}
	pm.hashedPassword = hashed
	pm.initialPassword = password
	pm.passwordChangeRequired = true
	pm.mu.Unlock()

	// 持久化密码哈希（不含 username：重置时用户名不落盘）。
	fields := pm.dashboardAuthFields()
	delete(fields, "username")
	pm.persistDashboard(fields)

	// Print credentials to console
	authLogger.Info("")
	authLogger.Info("========================================")
	authLogger.Info("  AstrBot Dashboard Initial Credentials")
	authLogger.Info("========================================")
	authLogger.Info("  Username: %s", pm.username)
	authLogger.Info("  Password: %s", password)
	authLogger.Info("  >>> Change it after logging in <<<")
	authLogger.Info("========================================")
	authLogger.Info("  NOTE: 初始密码不落盘（仅存哈希）。若在改密前重启，将自动重置并打印新密码；也可用 --reset-password 手动重置。")
}

// saveToConfig writes the username/password hash to the config JSON file.
// dashboard.password 只存 PBKDF2 哈希，绝不写明文；旧字段名 pbkdf2_password
// 与历史明文残留由 persistDashboard 一并清除。
func (pm *PasswordManager) saveToConfig() {
	pm.persistDashboard(pm.dashboardAuthFields())
}

// SetConfigManager 注入 ConfigManager，使 auth 的持久化与 ConfigManager.Save
// 共用同一把内部锁与内存快照，避免"直读文件→改 dashboard 段→原子写回"与
// ConfigManager 保存互相覆盖（M-08）。
func (pm *PasswordManager) SetConfigManager(cm interface{}) {
	pm.mu.Lock()
	if mgr, ok := cm.(*config.ConfigManager); ok {
		pm.configMgr = mgr
	}
	pm.mu.Unlock()
}

// dashboardAuthFields 汇总 auth 需要持久化的 dashboard 字段。与
// injectAuthFields 的回填保持一致（含 jwt_secret 与 totp），确保经
// ConfigManager 保存时不会丢段（H-24 / M-08）。
// password 字段只存放 PBKDF2 哈希，绝不写明文。
func (pm *PasswordManager) dashboardAuthFields() map[string]interface{} {
	pm.mu.RLock()
	username := pm.username
	hashed := pm.hashedPassword
	changeReq := pm.passwordChangeRequired
	secret := pm.jwtSecret
	totpEnabled := pm.totpEnabled
	totpSecret := pm.totpSecret
	recoveryJSON, _ := json.Marshal(pm.totpRecoveryCodes)
	pm.mu.RUnlock()

	return map[string]interface{}{
		"username":                  username,
		"password":                  hashed,
		"password_change_required":  changeReq,
		"password_storage_upgraded": true,
		"jwt_secret":                secret,
		"totp": map[string]interface{}{
			"enable":             totpEnabled,
			"secret":             totpSecret,
			"recovery_code_hash": string(recoveryJSON),
		},
	}
}

// persistDashboard 把 dashboard 段写入配置：优先走 ConfigManager（Update 递归
// 合并 + Save，与 ConfigManager 自己的保存串行化）；无 ConfigManager（独立
// 模式/单测）时回退为直读文件→合并→原子写回。
// 升级清理：合并式 Update 不会删除旧键，这里显式移除历史遗留的明文 password、
// 旧字段名 pbkdf2_password 以及（当本次不携带 username 时）旧 username 键，
// 保证磁盘只存在 password=哈希。
func (pm *PasswordManager) persistDashboard(dash map[string]interface{}) {
	pm.mu.RLock()
	cm := pm.configMgr
	pm.mu.RUnlock()
	if cm != nil {
		if cfg := cm.Get("default"); cfg != nil {
			if err := cfg.Update(map[string]interface{}{"dashboard": dash}); err != nil {
				authLogger.Error("persistDashboard: config update: %v", err)
			}
			_ = cfg.DeleteNested("dashboard", "pbkdf2_password")
			if _, has := dash["username"]; !has {
				_ = cfg.DeleteNested("dashboard", "username")
			}
			return
		}
	}
	pm.writeDashboardDirect(dash)
}

// writeDashboardDirect 在不经过 ConfigManager 时把 dashboard 段合并写回文件
// （读取现有配置→覆盖 dashboard 字段→原子写回）。
func (pm *PasswordManager) writeDashboardDirect(dash map[string]interface{}) {
	cfg := loadConfigOrNew(pm.configPath)
	d, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		d = make(map[string]interface{})
		cfg["dashboard"] = d
	}
	for k, v := range dash {
		d[k] = v
	}
	// 升级清理：只允许 password=哈希；移除旧字段名与任何明文残留。
	delete(d, "pbkdf2_password")
	if _, has := dash["username"]; !has {
		// 本次未携带 username（重置场景）：用户名不落盘，删除旧键。
		delete(d, "username")
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		authLogger.Error("writeDashboardDirect: marshal config: %v", err)
		return
	}
	if err := writeConfigFile(pm.configPath, data); err != nil {
		authLogger.Error("writeDashboardDirect: write config: %v", err)
	}
}

// ensureJWTSecret generates a random JWT secret if none exists.
func (pm *PasswordManager) ensureJWTSecret() {
	cfg := loadConfigOrNew(pm.configPath)

	dash, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		dash = make(map[string]interface{})
		cfg["dashboard"] = dash
	}

	if secret, ok := dash["jwt_secret"].(string); ok && secret != "" {
		pm.jwtSecret = secret
		return
	}

	// Generate new JWT secret
	pm.jwtSecret = generateRandomToken(32)
	dash["jwt_secret"] = pm.jwtSecret

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		authLogger.Error("ensureJWTSecret: marshal config: %v", err)
		return
	}
	if err := writeConfigFile(pm.configPath, data); err != nil {
		authLogger.Error("ensureJWTSecret: write config: %v", err)
	}
	authLogger.Info("Initialized random JWT secret for dashboard.")
}

// loadConfigOrNew reads a JSON config file into a map. If the file is missing
// it returns an empty map; if it is corrupt it logs and backs the file up
// before returning an empty map, so a later save never silently wipes other
// configuration that it failed to parse.
func loadConfigOrNew(path string) map[string]interface{} {
	cfg := make(map[string]interface{})
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			authLogger.Error("loadConfigOrNew: read %s: %v", path, err)
		}
		return cfg
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		backup := path + ".corrupt.bak"
		authLogger.Error("loadConfigOrNew: %s has invalid JSON (%v); backing up to %s", path, err, backup)
		_ = os.WriteFile(backup, data, 0o600)
	}
	return cfg
}

// writeConfigFile 将配置以 0600 权限原子写入（temp + rename），避免明文
// 凭据/jwt_secret 以 0644 落盘被同机其他用户读取。参照 internal/config/config.go
// 的 save 写法。
func writeConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir == "" {
		dir = "."
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
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
	return os.Rename(tmpName, path)
}

// VerifyPassword checks the given plaintext password against the stored hash.
// Only hash comparison is performed (PBKDF2 authoritative, legacy MD5
// supported); the config never holds a plaintext password to fall back to.
func (pm *PasswordManager) VerifyPassword(password string) bool {
	pm.mu.RLock()
	stored := pm.hashedPassword
	pm.mu.RUnlock()

	if stored == "" {
		return false
	}

	if isMD5Hash(stored) {
		candidate := md5Hash(password)
		return subtle.ConstantTimeCompare([]byte(candidate), []byte(stored)) == 1
	}

	if isPBKDF2Hash(stored) {
		parts := strings.Split(stored, "$")
		if len(parts) != 4 {
			return false
		}
		iterations, err := strconv.Atoi(parts[1])
		if err != nil {
			return false
		}
		salt, err := hex.DecodeString(parts[2])
		if err != nil {
			return false
		}
		expectedDigest, err := hex.DecodeString(parts[3])
		if err != nil {
			return false
		}
		candidateKey := pbkdf2.Key([]byte(password), salt, iterations, len(expectedDigest), sha256.New)
		return subtle.ConstantTimeCompare(candidateKey, expectedDigest) == 1
	}

	return false
}

// Username returns the dashboard username.
func (pm *PasswordManager) Username() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.username
}

// JWTSecret returns the JWT signing secret.
func (pm *PasswordManager) JWTSecret() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.jwtSecret
}

// HashedPassword returns the PBKDF2 password hash.
func (pm *PasswordManager) HashedPassword() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.hashedPassword
}

// PasswordChangeRequired returns whether the user must change their password.
func (pm *PasswordManager) PasswordChangeRequired() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.passwordChangeRequired
}

// InitialPassword 返回启动生成/重置的初始密码明文（仅内存）。供 setup 端点
// 校验初始密码与测试使用；从未生成过时返回空串。
func (pm *PasswordManager) InitialPassword() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.initialPassword
}

// generateRandomPassword creates a strong random password.
// Ported from astrbot/core/utils/auth_password.py generate_dashboard_password()
func generateRandomPassword() string {
	const uppercase = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	const lowercase = "abcdefghijklmnopqrstuvwxyz"
	const digits = "0123456789"
	const alphabet = uppercase + lowercase + digits

	chars := make([]byte, PasswordLength)
	// Ensure at least one of each type
	chars[0] = randomChoice(uppercase)
	chars[1] = randomChoice(lowercase)
	chars[2] = randomChoice(digits)
	for i := 3; i < PasswordLength; i++ {
		chars[i] = randomChoice(alphabet)
	}
	// Shuffle
	shuffleBytes(chars)
	return string(chars)
}

// mustRandRead fills b with cryptographically secure random bytes. crypto/rand
// only fails when the OS entropy source is broken; on the auth/security paths
// that would silently weaken tokens/salts, so failing hard is preferable to
// continuing with predictable randomness.
func mustRandRead(b []byte) {
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
}

// generateRandomToken creates a random hex token of the given byte length.
func generateRandomToken(byteLen int) string {
	b := make([]byte, byteLen)
	mustRandRead(b)
	return fmt.Sprintf("%x", b)
}

// hashPBKDF2 hashes a password using PBKDF2-HMAC-SHA256 with a random salt.
// Format: pbkdf2_sha256$600000$<32_hex_chars_salt>$<64_hex_chars_digest>
// Compatible with Python AstrBot's auth_password.py hash_dashboard_password().
func hashPBKDF2(password string) string {
	salt := make([]byte, 16)
	mustRandRead(salt)
	saltHex := hex.EncodeToString(salt)
	iterations := 600000
	dk := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%x", iterations, saltHex, dk)
}

// md5Hash returns the MD5 hex digest of a password.
// #nosec G401 -- 仅用于校验历史遗留的 MD5 密码哈希（迁移兼容）；新密码一律
// 使用 PBKDF2 哈希，MD5 不作为新凭据的哈希算法。
func md5Hash(password string) string {
	h := md5.Sum([]byte(password)) // nosemgrep: go.lang.security.audit.crypto.use_of_weak_crypto.use-of-md5
	return fmt.Sprintf("%x", h)
}

// isMD5Hash checks if the stored hash is an MD5 hash (32 hex chars).
func isMD5Hash(stored string) bool {
	if len(stored) != 32 {
		return false
	}
	for _, c := range stored {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// isPBKDF2Hash checks if the stored hash is a PBKDF2 hash.
func isPBKDF2Hash(stored string) bool {
	return strings.HasPrefix(stored, "pbkdf2_sha256$")
}

// cryptoRandInt returns a uniformly distributed int in [0, n) using rejection
// sampling. Plain modulo over a random byte is biased when n does not divide
// 256, which matters for password/secret generation (bug.md 5.1).
func cryptoRandInt(n int) int {
	if n <= 0 {
		return 0
	}
	// 以 2^32 为界：limit 是小于 2^32 的最大 n 的倍数，落在 [limit, 2^32) 的
	// 样本重试，保证每个输出等概率。
	const maxUint32 = uint64(1) << 32
	limit := maxUint32 - maxUint32%uint64(n)
	var b [4]byte
	for {
		mustRandRead(b[:])
		v := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
		if v < limit {
			// #nosec G115 -- v%uint64(n) < n，而 n 为字符集长度或切片长度
			// （int 正小值），结果恒远小于 int 上限，无溢出风险。
			return int(v % uint64(n))
		}
	}
}

// randomChoice picks a random byte from the given charset (uniform, unbiased).
func randomChoice(charset string) byte {
	return charset[cryptoRandInt(len(charset))]
}

// shuffleBytes shuffles a byte slice in-place using crypto/rand (unbiased).
func shuffleBytes(b []byte) {
	for i := len(b) - 1; i > 0; i-- {
		idx := cryptoRandInt(i + 1)
		b[i], b[idx] = b[idx], b[i]
	}
}

// PrintStartupBanner prints the dashboard URL and credentials summary.
func (pm *PasswordManager) PrintStartupBanner(port int, ipAddrs []string) {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(" ========================================\n")
	sb.WriteString("  AstrBot Go - Dashboard Ready\n")
	sb.WriteString(" ========================================\n")
	sb.WriteString(fmt.Sprintf("  Username: %s\n", pm.username))
	sb.WriteString(fmt.Sprintf("  Local:   http://localhost:%d\n", port))
	for _, ip := range ipAddrs {
		sb.WriteString(fmt.Sprintf("  Network: http://%s:%d\n", ip, port))
	}
	sb.WriteString(" ========================================\n")
	authLogger.Info("%s", sb.String())
}

// tokenTTL is how long an issued session token stays valid. Kept long enough
// that a browser tab survives a few days of inactivity, short enough that a
// leaked token's blast radius is bounded.
const tokenTTL = 7 * 24 * time.Hour

// jwtClaims are the custom claims carried by dashboard session tokens. Each
// token carries a random jti so Logout can revoke it server-side (blacklist)
// instead of leaving it valid until expiry.
type jwtClaims struct {
	Username string `json:"username"`
	jwt.RegisteredClaims
}

// IssueToken returns a signed JWT for the given username. Unlike the old
// in-memory random tokens, this token stays valid across restarts (the signing
// secret is persisted in the dashboard config), so the WebUI's WebSocket chat
// transport keeps working after the server is restarted.
func (pm *PasswordManager) IssueToken(username string) (string, error) {
	now := time.Now()
	claims := jwtClaims{
		Username: username,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        generateRandomToken(16),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(tokenTTL)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(pm.JWTSecret()))
}

// verifyJWT validates a signed JWT and returns the embedded username and jti
// (empty when the token predates jti issuance).
func (pm *PasswordManager) verifyJWT(token string) (string, string, bool) {
	parsed, err := jwt.ParseWithClaims(token, &jwtClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(pm.JWTSecret()), nil
	})
	if err != nil {
		return "", "", false
	}
	claims, ok := parsed.Claims.(*jwtClaims)
	if !ok || !parsed.Valid {
		return "", "", false
	}
	return claims.Username, claims.ID, true
}

// loadRevoked 从 revokedPath 加载持久化的 jti 黑名单。文件缺失或损坏时忽略
// （损坏文件视为空黑名单），不阻断启动。
func (pm *PasswordManager) loadRevoked() {
	if pm.revokedPath == "" {
		return
	}
	data, err := os.ReadFile(pm.revokedPath)
	if err != nil {
		if !os.IsNotExist(err) {
			authLogger.Warn("loadRevoked: read %s: %v", pm.revokedPath, err)
		}
		return
	}
	var revoked []string
	if err := json.Unmarshal(data, &revoked); err != nil {
		authLogger.Warn("loadRevoked: %s has invalid JSON (%v); ignoring", pm.revokedPath, err)
		return
	}
	if len(revoked) == 0 {
		return
	}
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.revoked == nil {
		pm.revoked = make(map[string]bool, len(revoked))
	}
	for _, jti := range revoked {
		if jti != "" {
			pm.revoked[jti] = true
		}
	}
}

// saveRevoked 把内存中的 jti 黑名单原子写入 revokedPath。失败仅告警，不阻断
// 撤销流程。
func (pm *PasswordManager) saveRevoked() {
	if pm.revokedPath == "" {
		return
	}
	pm.mu.RLock()
	revoked := make([]string, 0, len(pm.revoked))
	for jti := range pm.revoked {
		revoked = append(revoked, jti)
	}
	pm.mu.RUnlock()
	data, err := json.Marshal(revoked)
	if err != nil {
		authLogger.Warn("saveRevoked: marshal: %v", err)
		return
	}
	if err := writeConfigFile(pm.revokedPath, data); err != nil {
		authLogger.Warn("saveRevoked: write %s: %v", pm.revokedPath, err)
	}
}

// Login verifies a password and returns a signed session token.
func (pm *PasswordManager) Login(password string) (string, error) {
	if !pm.VerifyPassword(password) {
		return "", fmt.Errorf("invalid password")
	}
	// 旧版无盐 MD5 哈希登录成功后自动升级为 PBKDF2 落盘（不注销会话）。
	pm.mu.RLock()
	md5Stored := isMD5Hash(pm.hashedPassword)
	pm.mu.RUnlock()
	if md5Stored {
		pm.SetPasswordKeepUsername(password)
	}
	return pm.IssueToken(pm.Username())
}

// Logout invalidates a token. Signed JWTs are revoked by jti blacklist
// (persisted so it survives restarts); legacy in-memory tokens are dropped.
func (pm *PasswordManager) Logout(token string) {
	// 解析 jti 需要读 JWTSecret（RLock），不能在持有写锁时调用（同一
	// goroutine 写锁内再读锁会死锁），故先解析再上锁写黑名单。
	_, jti, ok := pm.verifyJWT(token)
	pm.mu.Lock()
	delete(pm.tokens, token)
	if ok && jti != "" {
		if pm.revoked == nil {
			pm.revoked = make(map[string]bool)
		}
		pm.revoked[jti] = true
	}
	pm.mu.Unlock()
	pm.saveRevoked()
}

// IsAuthenticated checks if a token is valid. It accepts both signed JWTs
// (persistent across restarts, minus those revoked by Logout) and legacy
// in-memory tokens registered via RegisterToken (kept for backward
// compatibility with pre-JWT logins).
func (pm *PasswordManager) IsAuthenticated(token string) bool {
	if token == "" {
		return false
	}
	if _, jti, ok := pm.verifyJWT(token); ok {
		if jti == "" {
			return true // 无 jti 的旧 token：未列入黑名单，直接放行
		}
		pm.mu.RLock()
		defer pm.mu.RUnlock()
		return !pm.revoked[jti]
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.tokens[token]
}

// RegisterToken registers a legacy (in-memory) session token.
func (pm *PasswordManager) RegisterToken(token string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if pm.tokens == nil {
		pm.tokens = make(map[string]bool)
	}
	pm.tokens[token] = true
}

// SetUsername updates the dashboard username.
// SetUsername updates the dashboard username and persists it to config (so a
// first-time account rename survives restarts).
func (pm *PasswordManager) SetUsername(username string) {
	pm.mu.Lock()
	if username != "" {
		pm.username = username
	}
	pm.mu.Unlock()
	pm.saveToConfig()
}

// SetPassword updates the dashboard password (re-hashes) and rotates the JWT
// signing secret so every previously-issued session token becomes invalid:
// existing JWTs fail signature verification against the new secret, and legacy
// in-memory tokens are dropped. This is the standard "credential change logs
// all sessions out" behavior.
// 用户名在改密时默认填 admin：若当前用户名未设置（重置等待状态，用户名不
// 落盘），改密后自动以 admin 写入配置文件；用户在前端显式填写了其他用户名
// 时由 SetUsername 先行覆盖。
func (pm *PasswordManager) SetPassword(password string) {
	if password == "" {
		return
	}
	// Hash the new password and update in-memory state FIRST
	newHash := hashPBKDF2(password)
	pm.mu.Lock()
	if pm.username == "" {
		pm.username = Username
	}
	pm.hashedPassword = newHash
	pm.passwordChangeRequired = false
	pm.jwtSecret = generateRandomToken(32)
	pm.tokens = make(map[string]bool)
	pm.mu.Unlock()
	// Now persist to config file (uses pm.hashedPassword which is already updated).
	// saveToConfig 的 dashboardAuthFields 会带上 username（默认 admin），
	// 改密后用户名自动填入配置文件。
	pm.saveToConfig()
}

// SetPasswordKeepUsername 仅重新哈希密码并落盘（PBKDF2），不轮换 JWT secret、
// 不清会话、不改用户名——用于旧版无盐 MD5 哈希登录成功后透明升级。
func (pm *PasswordManager) SetPasswordKeepUsername(password string) {
	if password == "" {
		return
	}
	pm.mu.Lock()
	pm.hashedPassword = hashPBKDF2(password)
	pm.mu.Unlock()
	pm.saveToConfig()
}

// loginRateLimiter is a per-IP token bucket used to slow down dashboard login
// brute force. Config comes from dashboard.auth_rate_limit (enable /
// average_interval / max_burst). Buckets idle longer than loginBucketIdleTTL
// are pruned on access so the map cannot grow unboundedly.
type loginRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*loginBucket
	lastGC  time.Time
}

type loginBucket struct {
	tokens float64
	last   time.Time
}

const loginBucketIdleTTL = 10 * time.Minute

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{buckets: make(map[string]*loginBucket), lastGC: time.Now()}
}

// Allow reports whether the key may proceed. avgInterval is the token refill
// interval in seconds (1 token per avgInterval), maxBurst the bucket capacity.
func (l *loginRateLimiter) Allow(key string, avgInterval, maxBurst float64) bool {
	if avgInterval <= 0 || maxBurst <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastGC) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last) > loginBucketIdleTTL {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}

	b, ok := l.buckets[key]
	if !ok {
		b = &loginBucket{tokens: maxBurst, last: now}
		l.buckets[key] = b
	}
	refill := now.Sub(b.last).Seconds() / avgInterval
	if refill > 0 {
		b.tokens = b.tokens + refill
		if b.tokens > maxBurst {
			b.tokens = maxBurst
		}
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// clientIP extracts the client IP for rate limiting. Honors
// dashboard.trust_proxy_headers for X-Forwarded-For; otherwise uses the remote
// address host.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// 常规反代向 XFF 追加而非覆盖，最右一个非空地址是最近一跳（即
			// 客户端真实 IP，由可信反代写入）；取首段的话攻击者直接伪造
			// 第一跳即可获得独立限速桶，绕过暴力破解防护。
			hops := strings.Split(xff, ",")
			for i := len(hops) - 1; i >= 0; i-- {
				if ip := strings.TrimSpace(hops[i]); ip != "" {
					return ip
				}
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host
}

// extractAPIKey 提取请求携带的 API key（对齐 Python _extract_raw_api_key）：
// Authorization: ApiKey <key>、X-API-Key 头、?api_key= / ?key= query 参数。
// 以 "Bearer " 开头的 Authorization 属于 JWT，不算 API key。
func extractAPIKey(r *http.Request) string {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	if strings.HasPrefix(auth, "ApiKey ") {
		return strings.TrimSpace(strings.TrimPrefix(auth, "ApiKey "))
	}
	if key := strings.TrimSpace(r.URL.Query().Get("api_key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(r.URL.Query().Get("key")); key != "" {
		return key
	}
	if key := strings.TrimSpace(r.Header.Get("X-API-Key")); key != "" {
		return key
	}
	return ""
}

// apiKeyAuthorized 校验 API key 并判定端点放行：key 存在、未吊销、未过期，
// 且其 scope 满足端点规则（对齐 Python _require_api_key_scope）。systemOnly
// 端点一律拒绝 API key（Python 语义：system 不在 ALL_OPEN_API_SCOPES）。
func (s *Server) apiKeyAuthorized(rawKey string, rule endpointScope, method string) bool {
	if rule.systemOnly || s.apiKeys == nil {
		return false
	}
	rec := s.apiKeys.getByHash(hashAPIKey(rawKey))
	if rec == nil || rec.RevokedAt != "" {
		return false
	}
	if rec.ExpiresAt != "" {
		if t, err := time.Parse(time.RFC3339, rec.ExpiresAt); err == nil && t.Before(time.Now()) {
			return false
		}
	}
	if !apiKeyScopeAllowed(rec.Scopes, rule, method) {
		return false
	}
	// 校验通过后更新 last_used_at（对齐 Python touch_api_key）。
	s.apiKeys.touch(rec.KeyID)
	return true
}

// apiKeyScopeAllowed 判定 key 的 scopes 是否满足端点所需 scope（对齐 Python
// require_scope 的 scope 判定）：含 "*"、含所需 scope，或 OPEN_API_SCOPE_INCLUDES
// 隐式包含（config 包含 bot/provider）。required 为空时满足任一默认 OPEN API
// scope 即可（普通端点默认放行）。
func apiKeyScopeAllowed(scopes []string, rule endpointScope, method string) bool {
	required := rule.readScope
	if rule.writeScope != "" && method != http.MethodGet && method != http.MethodHead {
		required = rule.writeScope
	}
	if required == "" {
		for _, sc := range scopes {
			if sc == "*" || containsScope(defaultOpenAPIScopes, sc) {
				return true
			}
		}
		return false
	}
	if containsScope(scopes, "*") || containsScope(scopes, required) {
		return true
	}
	for _, sc := range scopes {
		if containsScope(openAPIScopeIncludes[sc], required) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// TOTP 双重认证
// ---------------------------------------------------------------------------

// recoveryCodeAlphabet 是恢复码的字符集（大写字母 + 数字 2-7，与前端
// AuthStageRecovery 期望的 base32 风格 [A-Z2-7] 一致）。
const recoveryCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// recoveryCodeLength 是单个恢复码的长度。
const recoveryCodeLength = 32

// TOTPEnabled 返回是否已启用 TOTP 双重认证。
func (pm *PasswordManager) TOTPEnabled() bool {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.totpEnabled && pm.totpSecret != ""
}

// TOTPSecret 返回 TOTP 密钥（base32），调用方可用它拼出 otpauth:// URL
// 并生成二维码。
func (pm *PasswordManager) TOTPSecret() string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.totpSecret
}

// TOTPConfig 返回 dashboard.totp 段应持久化的字段，格式与 saveTOTPToConfig
// 写入一致，供配置整体替换前回填，避免保存 dashboard 时丢 TOTP。
func (pm *PasswordManager) TOTPConfig() map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	recoveryJSON, _ := json.Marshal(pm.totpRecoveryCodes)
	return map[string]interface{}{
		"enable":             pm.totpEnabled,
		"secret":             pm.totpSecret,
		"recovery_code_hash": string(recoveryJSON),
	}
}

// TOTPOtpauthURL 生成 otpauth:// 二维码链接（未启用 TOTP 时返回空串）。
func (pm *PasswordManager) TOTPOtpauthURL() string {
	pm.mu.RLock()
	secret := pm.totpSecret
	username := pm.username
	pm.mu.RUnlock()
	if secret == "" {
		return ""
	}
	return otpauthURL(username, secret)
}

// otpauthURL 组装 otpauth://totp/<Issuer>:<AccountName>?secret=...&issuer=...
func otpauthURL(username, secret string) string {
	label := url.PathEscape("AstrBot:" + username)
	return fmt.Sprintf("otpauth://totp/%s?secret=%s&issuer=AstrBot", label, secret)
}

// SetupTOTP 生成并启用 TOTP：返回 base32 密钥、otpauth 二维码链接、恢复码列表，
// 并持久化到 config 的 dashboard.totp 段。调用方应把恢复码一次性展示给用户妥善保存。
func (pm *PasswordManager) SetupTOTP() (secret string, otpauthURLStr string, recoveryCodes []string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "AstrBot",
		AccountName: pm.Username(),
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("生成 TOTP 密钥失败: %w", err)
	}

	codes := generateRecoveryCodes(10)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, hashRecoveryCode(c))
	}

	pm.mu.Lock()
	pm.totpSecret = key.Secret()
	pm.totpEnabled = true
	pm.totpRecoveryCodes = hashes
	pm.mu.Unlock()

	pm.saveTOTPToConfig()
	authLogger.Info("Dashboard TOTP 双重认证已启用。")
	return key.Secret(), key.URL(), codes, nil
}

// GenerateTOTP 生成 TOTP 密钥与恢复码但**不启用**，供前端两步流程：
// 先扫码展示二维码，用户输入验证码后再 EnableTOTP 启用。
func (pm *PasswordManager) GenerateTOTP() (secret string, otpauthURLStr string, recoveryCodes []string, err error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "AstrBot",
		AccountName: pm.Username(),
	})
	if err != nil {
		return "", "", nil, fmt.Errorf("生成 TOTP 密钥失败: %w", err)
	}
	codes := generateRecoveryCodes(10)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, hashRecoveryCode(c))
	}

	pm.mu.Lock()
	pm.totpSecret = key.Secret()
	pm.totpEnabled = false // 待验证码启用
	pm.totpRecoveryCodes = hashes
	pm.mu.Unlock()
	pm.saveTOTPToConfig()
	return key.Secret(), key.URL(), codes, nil
}

// EnableTOTP 用认证器验证码启用 TOTP（需先 GenerateTOTP 生成密钥）。
func (pm *PasswordManager) EnableTOTP(code string) bool {
	pm.mu.RLock()
	secret := pm.totpSecret
	pm.mu.RUnlock()
	if secret == "" {
		return false
	}
	if !totp.Validate(code, secret) {
		return false
	}
	pm.mu.Lock()
	pm.totpEnabled = true
	pm.mu.Unlock()
	pm.saveTOTPToConfig()
	authLogger.Info("Dashboard TOTP 双重认证已启用。")
	return true
}

// RegenerateRecoveryCodes 重新生成一批恢复码并返回明文（旧恢复码作废）。
// 与 GenerateTOTP 不同，它**保留**当前 TOTP 密钥与启用状态，避免把 secret
// 换成新值导致用户验证器失配（M-07）。
func (pm *PasswordManager) RegenerateRecoveryCodes() ([]string, error) {
	pm.mu.RLock()
	enabled := pm.totpEnabled
	hasSecret := pm.totpSecret != ""
	pm.mu.RUnlock()
	if !enabled || !hasSecret {
		return nil, fmt.Errorf("TOTP 未启用")
	}
	codes := generateRecoveryCodes(10)
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		hashes = append(hashes, hashRecoveryCode(c))
	}
	pm.mu.Lock()
	pm.totpRecoveryCodes = hashes
	pm.mu.Unlock()
	pm.saveTOTPToConfig()
	authLogger.Info("Dashboard TOTP 恢复码已重新生成。")
	return codes, nil
}

// EnableTOTPNoop 在已有密钥的前提下恢复启用状态（recovery 重新生成恢复码
// 时 GenerateTOTP 会暂时置为未启用，本方法把它恢复为已启用）。
func (pm *PasswordManager) EnableTOTPNoop() {
	pm.mu.Lock()
	if pm.totpSecret != "" {
		pm.totpEnabled = true
	}
	pm.mu.Unlock()
	pm.saveTOTPToConfig()
}

// VerifyTOTP 校验 TOTP 验证码；验证码错误时回退校验恢复码（哈希比对）。
func (pm *PasswordManager) VerifyTOTP(code string) bool {
	ok, _ := pm.VerifyTOTPEx(code)
	return ok
}

// VerifyTOTPEx 校验 TOTP 验证码，返回是否通过以及是否用的是恢复码。
// 恢复码为一次性：命中后立即从列表移除并持久化，不可重复使用
// （usedRecovery=true 时调用方通常还会 DisableTOTP 关闭双重认证，对齐
// "使用恢复码登录将禁用双因素认证"）。
func (pm *PasswordManager) VerifyTOTPEx(code string) (ok, usedRecovery bool) {
	if code == "" {
		return false, false
	}
	pm.mu.RLock()
	secret := pm.totpSecret
	enabled := pm.totpEnabled
	hashes := append([]string(nil), pm.totpRecoveryCodes...)
	pm.mu.RUnlock()

	if !enabled || secret == "" {
		return false, false
	}
	if totp.Validate(code, secret) {
		return true, false
	}
	if hit := recoveryCodeHashFor(code, hashes); hit != "" {
		// 一次性消耗：从内存列表移除并持久化，防止恢复码被复用。
		pm.mu.Lock()
		for i, h := range pm.totpRecoveryCodes {
			if h == hit {
				pm.totpRecoveryCodes = append(pm.totpRecoveryCodes[:i], pm.totpRecoveryCodes[i+1:]...)
				break
			}
		}
		pm.mu.Unlock()
		pm.saveTOTPToConfig()
		return true, true
	}
	return false, false
}

// DisableTOTP 清除 TOTP 密钥与恢复码并关闭双重认证，同步更新 config。
func (pm *PasswordManager) DisableTOTP() {
	pm.mu.Lock()
	pm.totpSecret = ""
	pm.totpEnabled = false
	pm.totpRecoveryCodes = nil
	pm.mu.Unlock()

	pm.saveTOTPToConfig()
	authLogger.Info("Dashboard TOTP 双重认证已禁用。")
}

// saveTOTPToConfig 把 TOTP 相关字段写入 config 的 dashboard.totp 段，
// 恢复码仅以 SHA-256 哈希落盘，不保存明文。
func (pm *PasswordManager) saveTOTPToConfig() {
	pm.persistDashboard(pm.dashboardAuthFields())
}

// generateRecoveryCodes 生成 count 个恢复码，每个为 32 位 [A-Z2-7] 随机串，
// 风格与 generateRandomToken 一致，可安全展示给用户。
func generateRecoveryCodes(count int) []string {
	codes := make([]string, 0, count)
	for i := 0; i < count; i++ {
		var sb strings.Builder
		sb.Grow(recoveryCodeLength)
		for j := 0; j < recoveryCodeLength; j++ {
			sb.WriteByte(recoveryCodeAlphabet[cryptoRandInt(len(recoveryCodeAlphabet))])
		}
		codes = append(codes, sb.String())
	}
	return codes
}

// hashRecoveryCode 计算恢复码的 SHA-256 十六进制摘要，用于落盘存储。
func hashRecoveryCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return hex.EncodeToString(h[:])
}

// recoveryCodeHashFor 归一化恢复码（忽略大小写、空格与连字符），比对
// SHA-256 哈希是否命中任一已存哈希；命中返回对应哈希，未命中返回空串。
func recoveryCodeHashFor(code string, hashes []string) string {
	if code == "" || len(hashes) == 0 {
		return ""
	}
	normalized := strings.ToUpper(strings.TrimSpace(code))
	normalized = strings.ReplaceAll(normalized, "-", "")
	normalized = strings.ReplaceAll(normalized, " ", "")
	candidate := hashRecoveryCode(normalized)
	for _, h := range hashes {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(h)) == 1 {
			return h
		}
	}
	return ""
}

// parseRecoveryCodeHashes 解析 config 中保存的恢复码哈希：优先当作 JSON
// 字符串数组；解析失败时若为单个哈希值则按单元素列表返回。
func parseRecoveryCodeHashes(raw string) []string {
	var hashes []string
	if err := json.Unmarshal([]byte(raw), &hashes); err == nil {
		return hashes
	}
	if strings.TrimSpace(raw) != "" {
		return []string{raw}
	}
	return nil
}
