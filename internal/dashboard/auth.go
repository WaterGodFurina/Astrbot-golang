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
	"os"
	"strconv"
	"strings"
	"sync"

	"crypto/sha256"
	"github.com/AstrBotDevs/AstrBot/internal/log"
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

// saveToConfig writes the password hash to the config JSON file.
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

	// Also store plaintext password for backward compat (Python stores it too)
	if plaintextPassword != "" {
		dash["password"] = plaintextPassword
	}

	data, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile(pm.configPath, data, 0644)
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
	_ = os.WriteFile(pm.configPath, data, 0644)
	authLogger.Info("Initialized random JWT secret for dashboard.")
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

// Login verifies a password and returns a JWT token.
func (pm *PasswordManager) Login(password string) (string, error) {
	if !pm.VerifyPassword(password) {
		return "", fmt.Errorf("invalid password")
	}
	// Simple token generation (in production, use proper JWT)
	token := generateRandomToken(32)
	pm.mu.Lock()
	if pm.tokens == nil {
		pm.tokens = make(map[string]bool)
	}
	pm.tokens[token] = true
	pm.mu.Unlock()
	return token, nil
}

// Logout invalidates a token.
func (pm *PasswordManager) Logout(token string) {
	pm.mu.Lock()
	delete(pm.tokens, token)
	pm.mu.Unlock()
}

// IsAuthenticated checks if a token is valid.
func (pm *PasswordManager) IsAuthenticated(token string) bool {
	if token == "" {
		return false
	}
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.tokens[token]
}

// RegisterToken registers a session token.
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
