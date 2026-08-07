// Package dashboard - authentication and password management.
// Ported from astrbot/core/utils/auth_password.py and astrbot/dashboard/services/auth_service.py
package dashboard

import (
        "crypto/rand"
        "crypto/subtle"
        "encoding/json"
        "fmt"
        "os"
        "strings"
        "sync"

        "github.com/AstrBotDevs/AstrBot/internal/log"
        "golang.org/x/crypto/pbkdf2"
        "crypto/sha256"
)

const (
        PasswordLength = 16
        Username      = "astrbot"
)

var authLogger = log.GetDefault().WithComponent("Auth")

// PasswordManager handles dashboard password generation, hashing, and verification.
type PasswordManager struct {
        mu               sync.RWMutex
        configPath       string
        username         string
        hashedPassword   string // PBKDF2 hash (hex)
        passwordChangeRequired bool
        jwtSecret        string
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
        pm.mu.Lock()
        pm.hashedPassword = hashPBKDF2(password)
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

        dash["username"] = pm.username
        dash["pbkdf2_password"] = pm.hashedPassword
        dash["password_change_required"] = true
        dash["password_storage_upgraded"] = true

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

// VerifyPassword checks if the given plaintext password matches the stored hash.
func (pm *PasswordManager) VerifyPassword(password string) bool {
        pm.mu.RLock()
        defer pm.mu.RUnlock()
        if pm.hashedPassword == "" {
                return false
        }
        hashed := hashPBKDF2(password)
        return subtle.ConstantTimeCompare([]byte(hashed), []byte(pm.hashedPassword)) == 1
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

// hashPBKDF2 hashes a password using PBKDF2-SHA256.
func hashPBKDF2(password string) string {
        salt := []byte("astrbot_salt_v1")
        iterations := 100000
        dk := pbkdf2.Key([]byte(password), salt, iterations, 32, sha256.New)
        return fmt.Sprintf("pbkdf2_sha256$%d$%s$%x", iterations, "astrbot_salt_v1", dk)
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
