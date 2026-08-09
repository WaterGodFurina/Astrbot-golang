package sources

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ttsConfigFloat returns the float64 value of key, or the fallback when absent.
func ttsConfigFloat(config map[string]interface{}, key string, fallback float64) float64 {
	switch v := config[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		var f float64
		if _, err := fmt.Sscanf(v, "%g", &f); err == nil {
			return f
		}
	}
	return fallback
}

// ttsConfigBool returns the bool value of key, or the fallback when absent.
func ttsConfigBool(config map[string]interface{}, key string, fallback bool) bool {
	switch v := config[key].(type) {
	case bool:
		return v
	case int:
		return v != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "1", "yes", "y", "on":
			return true
		case "false", "0", "no", "n", "off":
			return false
		}
	}
	return fallback
}

// ttsOptFloat parses an optional float from config. Returns ok=false when the
// key is absent, empty, or not parseable as a number.
func ttsOptFloat(config map[string]interface{}, key string) (float64, bool) {
	v, present := config[key]
	if !present {
		return 0, false
	}
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case string:
		if n == "" {
			return 0, false
		}
		if _, err := fmt.Sscanf(n, "%g", &f); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return f, true
}

// ttsRandomNonce returns a random alphanumeric string of length n.
func ttsRandomNonce(n int) string {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = chars[rand.IntN(len(chars))]
	}
	return string(b)
}

// ttsUUID returns a random UUID v4 string.
func ttsUUID() string {
	b := make([]byte, 16)
	_, _ = cryptorand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// ttsSaveAudio streams an audio response body into data/temp and returns the
// resulting file path.
func ttsSaveAudio(r io.Reader, prefix, ext string) (string, error) {
	dir := filepath.Join("data", "temp")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%d.%s", prefix, time.Now().UnixNano(), ext))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(path)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}
