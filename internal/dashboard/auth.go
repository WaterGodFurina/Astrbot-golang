// Package dashboard - authentication and password management.
// Ported from astrbot/core/utils/auth_password.py and astrbot/dashboard/services/auth_service.py
package dashboard

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"crypto/sha256"

	"github.com/golang-jwt/jwt/v5"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"golang.org/x/crypto/pbkdf2"
)

const (
	PasswordLength = 16
	Username       = "astrbot"
)

var authLogger = log.GetDefault().WithComponent("Auth")

// PasswordManager handles dashboard password generation, hashing, and verification.
type PasswordManager struct {
	mu                     sync.RWMutex
	configPath             string
	username               string
	hashedPassword         string // PBKDF2 hash (hex)
	plainPassword          string // plaintext password (temporary storage)
	passwordChangeRequired bool
	jwtSecret              string
	tokens                 map[string]bool // active session tokens
	revoked                map[string]bool // JWT jti blacklist (in-memory)
}

// NewPasswordManager creates a password manager, generating a random password
// on first run (or when --reset-password is used).
func NewPasswordManager(configPath string) *PasswordManager {
	pm := &PasswordManager{
		configPath: configPath,
		username:   Username,
	}

	// Check if we need to generate a new password
	needGenerate := false

	// Check for --reset-password flag via env
	if os.Getenv("ASTRBOT_RESET_DASHBOARD_PASSWORD") == "1" {
		needGenerate = true
		os.Unsetenv("ASTRBOT_RESET_DASHBOARD_PASSWORD")
	}

	// Load existing config to check if password exists
	if data, err := os.ReadFile(configPath); err == nil {
		var cfg map[string]interface{}
		if json.Unmarshal(data, &cfg) == nil {
			if dash, ok := cfg["dashboard"].(map[string]interface{}); ok {
				if user, ok := dash["username"].(string); ok && user != "" {
					pm.username = user
				}
				if hashed, ok := dash["pbkdf2_password"].(string); ok && hashed != "" {
					pm.hashedPassword = hashed
				}
				if plain, ok := dash["password"].(string); ok && plain != "" {
					pm.plainPassword = plain
				}
				if pm.hashedPassword == "" {
					// No PBKDF2 password set, need to generate
					needGenerate = true
				}
			} else {
				needGenerate = true
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

	return pm
}

// generateAndStorePassword creates a random password, hashes it, stores the hash,
// and prints the plaintext to the console for the user.
func (pm *PasswordManager) generateAndStorePassword() {
	password := generateRandomPassword()
	hashed := hashPBKDF2(password)
	pm.mu.Lock()
	pm.hashedPassword = hashed
	pm.passwordChangeRequired = true
	pm.mu.Unlock()

	// Save to config file
	pm.saveToConfig(password)

	// Print credentials to console
	authLogger.Info("")
	authLogger.Info("========================================")
	authLogger.Info("  AstrBot Dashboard Initial Credentials")
	authLogger.Info("========================================")
	authLogger.Info("  Username: %s", pm.username)
	authLogger.Info("  Password: %s", password)
	authLogger.Info("  >>> Change it after logging in <<<")
	authLogger.Info("========================================")
	authLogger.Info("")
}

// saveToConfig writes the password hash to the config JSON file. 明文密码不再
// 落盘（仅保留 pbkdf2_password 哈希）；旧配置中的明文 password 字段由
// NewPasswordManager 读取，用于向后兼容校验。
func (pm *PasswordManager) saveToConfig(plaintextPassword string) {
	cfg := make(map[string]interface{})

	// Load existing config
	if data, err := os.ReadFile(pm.configPath); err == nil {
		json.Unmarshal(data, &cfg)
	}

	dash, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		dash = make(map[string]interface{})
		cfg["dashboard"] = dash
	}

	pm.mu.RLock()
	dash["username"] = pm.username
	dash["pbkdf2_password"] = pm.hashedPassword
	dash["password_change_required"] = pm.passwordChangeRequired
	dash["password_storage_upgraded"] = true
	pm.mu.RUnlock()

	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = writeConfigFile(pm.configPath, data)
}

// ensureJWTSecret generates a random JWT secret if none exists.
func (pm *PasswordManager) ensureJWTSecret() {
	cfg := make(map[string]interface{})
	if data, err := os.ReadFile(pm.configPath); err == nil {
		json.Unmarshal(data, &cfg)
	}

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

	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = writeConfigFile(pm.configPath, data)
	authLogger.Info("Initialized random JWT secret for dashboard.")
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
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// VerifyPassword checks if the given plaintext password matches the stored credential.
// Prefers the plaintext password field (temporary storage); falls back to
// PBKDF2 (Python-generated) and MD5 (legacy) formats.
func (pm *PasswordManager) VerifyPassword(password string) bool {
	pm.mu.RLock()
	plain := pm.plainPassword
	stored := pm.hashedPassword
	pm.mu.RUnlock()

	if plain != "" {
		return subtle.ConstantTimeCompare([]byte(password), []byte(plain)) == 1
	}

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

// generateRandomToken creates a random hex token of the given byte length.
func generateRandomToken(byteLen int) string {
	b := make([]byte, byteLen)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// hashPBKDF2 hashes a password using PBKDF2-HMAC-SHA256 with a random salt.
// Format: pbkdf2_sha256$600000$<32_hex_chars_salt>$<64_hex_chars_digest>
// Compatible with Python AstrBot's auth_password.py hash_dashboard_password().
func hashPBKDF2(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	saltHex := hex.EncodeToString(salt)
	iterations := 600000
	dk := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)
	return fmt.Sprintf("pbkdf2_sha256$%d$%s$%x", iterations, saltHex, dk)
}

// md5Hash returns the MD5 hex digest of a password.
func md5Hash(password string) string {
	h := md5.Sum([]byte(password))
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

// randomChoice picks a random byte from the given charset.
func randomChoice(charset string) byte {
	b := make([]byte, 1)
	rand.Read(b)
	return charset[int(b[0])%len(charset)]
}

// shuffleBytes shuffles a byte slice in-place using crypto/rand.
func shuffleBytes(b []byte) {
	for i := len(b) - 1; i > 0; i-- {
		j := make([]byte, 1)
		rand.Read(j)
		idx := int(j[0]) % (i + 1)
		b[i], b[idx] = b[idx], b[i]
	}
}

// PrintStartupBanner prints the dashboard URL and credentials summary.
func (pm *PasswordManager) PrintStartupBanner(port int, ipAddrs []string) {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString(" ========================================\n")
	sb.WriteString(fmt.Sprintf("  AstrBot Go - Dashboard Ready\n"))
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

// Login verifies a password and returns a signed session token.
func (pm *PasswordManager) Login(password string) (string, error) {
	if !pm.VerifyPassword(password) {
		return "", fmt.Errorf("invalid password")
	}
	return pm.IssueToken(pm.Username())
}

// Logout invalidates a token. Signed JWTs are revoked by jti blacklist (kept
// in memory, cleared on restart); legacy in-memory tokens are dropped.
func (pm *PasswordManager) Logout(token string) {
	// 解析 jti 需要读 JWTSecret（RLock），不能在持有写锁时调用（同一
	// goroutine 写锁内再读锁会死锁），故先解析再上锁写黑名单。
	_, jti, ok := pm.verifyJWT(token)
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.tokens, token)
	if ok && jti != "" {
		if pm.revoked == nil {
			pm.revoked = make(map[string]bool)
		}
		pm.revoked[jti] = true
	}
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
func (pm *PasswordManager) SetUsername(username string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if username != "" {
		pm.username = username
	}
}

// SetPassword updates the dashboard password (re-hashes).
func (pm *PasswordManager) SetPassword(password string) {
	if password == "" {
		return
	}
	// Hash the new password and update in-memory state FIRST
	newHash := hashPBKDF2(password)
	pm.mu.Lock()
	pm.hashedPassword = newHash
	pm.plainPassword = password
	pm.passwordChangeRequired = false
	pm.mu.Unlock()
	// Now persist to config file (uses pm.hashedPassword which is already updated)
	pm.saveToConfig(password)
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
			if idx := strings.IndexByte(xff, ','); idx >= 0 {
				return strings.TrimSpace(xff[:idx])
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
