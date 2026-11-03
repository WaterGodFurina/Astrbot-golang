// Package platform - portable button callback payload helpers.
// Ported from astrbot/core/platform/button_interaction.py
package platform

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/blake2s"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ButtonCallbackPrefix marks an AstrBot-hosted button callback payload.
const ButtonCallbackPrefix = "astrbot:"

// ButtonActionExpiredMessage is sent to the user when a callback has expired.
const ButtonActionExpiredMessage = "按钮已过期，请重新发起操作。 / This button has expired. Please start again."

// ButtonActionExpiredError indicates an expired callback that should receive an
// expiry notice (aligned with Python ButtonActionExpiredError).
type ButtonActionExpiredError struct{}

func (ButtonActionExpiredError) Error() string { return "AstrBot button action has expired." }

// ButtonCallbackStore persists callback payloads behind compact tokens so
// clicks survive a host restart. Implemented by internal/db.Database.
type ButtonCallbackStore interface {
	// SaveButtonCallback upserts the payload for the given token.
	SaveButtonCallback(token, payload string) error
	// GetButtonCallback loads the payload for the given token. found=false
	// means the token is unknown.
	GetButtonCallback(token string) (payload string, found bool, err error)
}

// buttonCallbackEntry is a resolved callback registration.
type buttonCallbackEntry struct {
	actionID  string
	data      message.JSONValue
	expiresAt *float64
}

// buttonCallbackRegistry stores callback data behind compact, platform-safe
// tokens. Ported from Python _ButtonCallbackRegistry.
type buttonCallbackRegistry struct {
	mu    sync.RWMutex
	cache map[string]buttonCallbackEntry
	store ButtonCallbackStore
}

var buttonRegistry = &buttonCallbackRegistry{cache: make(map[string]buttonCallbackEntry)}

// ConfigureButtonCallbackStore enables persistent callback lookup using
// AstrBot's database (aligned with Python configure_button_callback_registry).
func ConfigureButtonCallbackStore(store ButtonCallbackStore) {
	buttonRegistry.mu.Lock()
	buttonRegistry.store = store
	buttonRegistry.mu.Unlock()
}

// register stores one callback payload and returns its compact token.
func (r *buttonCallbackRegistry) register(actionID string, data message.JSONValue, expiresAt *float64) (string, error) {
	payload := map[string]interface{}{"i": actionID, "d": data}
	if expiresAt != nil {
		if math.IsInf(*expiresAt, 0) || math.IsNaN(*expiresAt) {
			return "", fmt.Errorf("Button callback expiry must be finite.")
		}
		payload["e"] = *expiresAt
	}
	// json.Marshal emits compact output with sorted map keys and rejects
	// NaN/Inf, matching Python json.dumps(..., sort_keys=True,
	// separators=(",", ":"), allow_nan=False).
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("Button callback data must be JSON-compatible.")
	}

	hasher, _ := blake2s.New128(nil)
	hasher.Write(encoded)
	token := hex.EncodeToString(hasher.Sum(nil))

	r.mu.Lock()
	r.cache[token] = buttonCallbackEntry{
		actionID:  actionID,
		data:      deepCopyJSONValue(data),
		expiresAt: cloneFloatPtr(expiresAt),
	}
	store := r.store
	r.mu.Unlock()

	if store != nil {
		if err := store.SaveButtonCallback(token, string(encoded)); err != nil {
			return "", err
		}
	}
	return token, nil
}

// resolve returns the registered action identifier and callback data for a
// token, loading it from persistent storage when it is not cached.
func (r *buttonCallbackRegistry) resolve(token string, allowExpired bool) (string, message.JSONValue, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.cache[token]
	if !ok {
		payload := ""
		if r.store != nil {
			p, found, err := r.store.GetButtonCallback(token)
			if err != nil {
				return "", nil, err
			}
			if found {
				payload = p
			}
		}
		if payload == "" {
			return "", nil, fmt.Errorf("Unknown AstrBot button callback token.")
		}
		decoded := map[string]interface{}{}
		if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
			return "", nil, fmt.Errorf("Invalid stored button callback payload.")
		}
		actionID, ok := decoded["i"].(string)
		if !ok {
			return "", nil, fmt.Errorf("Invalid stored button callback payload.")
		}
		var expiresAt *float64
		if rawExp, present := decoded["e"]; present && rawExp != nil {
			exp, ok := rawExp.(float64)
			if !ok || math.IsInf(exp, 0) || math.IsNaN(exp) {
				return "", nil, fmt.Errorf("Invalid stored button callback expiry.")
			}
			expiresAt = &exp
		}
		entry = buttonCallbackEntry{actionID: actionID, data: decoded["d"], expiresAt: expiresAt}
		r.cache[token] = entry
	}

	if !allowExpired && entry.expiresAt != nil && nowUnix() >= *entry.expiresAt {
		// Retain expiry metadata so repeated clicks also receive a notice.
		return "", nil, ButtonActionExpiredError{}
	}
	return entry.actionID, deepCopyJSONValue(entry.data), nil
}

// EncodeButtonCallback encodes a callback action as a compact opaque token.
// Ported from Python encode_button_callback.
func EncodeButtonCallback(actionID string, data message.JSONValue, expiresAt *float64) (string, error) {
	if actionID == "" {
		return "", fmt.Errorf("Button action_id cannot be empty.")
	}
	token, err := buttonRegistry.register(actionID, data, expiresAt)
	if err != nil {
		return "", err
	}
	return ButtonCallbackPrefix + token, nil
}

// DecodeButtonCallback resolves an AstrBot button callback token. When
// allowExpired is true, routing metadata is returned even for an expired
// callback (so the caller can send an expiry notice).
// Ported from Python decode_button_callback.
func DecodeButtonCallback(payload string, allowExpired bool) (string, message.JSONValue, error) {
	if !strings.HasPrefix(payload, ButtonCallbackPrefix) {
		return "", nil, fmt.Errorf("Not an AstrBot button callback payload.")
	}
	token := strings.TrimPrefix(payload, ButtonCallbackPrefix)
	if len(token) != 32 || !isLowerHexToken(token) {
		return "", nil, fmt.Errorf("Invalid AstrBot button callback token.")
	}
	return buttonRegistry.resolve(token, allowExpired)
}

func isLowerHexToken(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func nowUnix() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func cloneFloatPtr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// deepCopyJSONValue recursively copies a JSON-compatible value so callers never
// share mutable map/slice storage (aligned with Python copy.deepcopy).
func deepCopyJSONValue(v message.JSONValue) message.JSONValue {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			out[k] = deepCopyJSONValue(e)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = deepCopyJSONValue(e)
		}
		return out
	default:
		return v
	}
}
