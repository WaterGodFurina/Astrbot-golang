// Package i18n implements internationalization support.
// Ported from astrbot/core/i18n/
package i18n

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// Translator manages translations for multiple locales.
type Translator struct {
	mu       sync.RWMutex
	locales  map[string]map[string]string
	current  string
	fallback string
}

// NewTranslator creates a translator.
func NewTranslator(defaultLocale string) *Translator {
	return &Translator{
		locales:  make(map[string]map[string]string),
		current:  defaultLocale,
		fallback: "en_US",
	}
}

// LoadLocale loads translations from a JSON file.
func (t *Translator) LoadLocale(locale, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var translations map[string]string
	if err := json.Unmarshal(data, &translations); err != nil {
		return err
	}
	t.mu.Lock()
	if t.locales[locale] == nil {
		t.locales[locale] = make(map[string]string)
	}
	for k, v := range translations {
		t.locales[locale][k] = v
	}
	t.mu.Unlock()
	return nil
}

// LoadLocaleFromDir loads all locale JSON files from a directory.
func (t *Translator) LoadLocaleFromDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		locale := entry.Name()[:len(entry.Name())-5]
		if err := t.LoadLocale(locale, filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("load locale %s: %w", locale, err)
		}
	}
	return nil
}

// SetLocale sets the current locale.
func (t *Translator) SetLocale(locale string) {
	t.mu.Lock()
	t.current = locale
	t.mu.Unlock()
}

// Get returns the translation for the given key.
func (t *Translator) Get(key string, args ...interface{}) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if translations, ok := t.locales[t.current]; ok {
		if text, ok := translations[key]; ok {
			if len(args) > 0 {
				return fmt.Sprintf(text, args...)
			}
			return text
		}
	}
	if t.fallback != "" && t.fallback != t.current {
		if translations, ok := t.locales[t.fallback]; ok {
			if text, ok := translations[key]; ok {
				if len(args) > 0 {
					return fmt.Sprintf(text, args...)
				}
				return text
			}
		}
	}
	return key
}

// AvailableLocales returns all loaded locale names.
func (t *Translator) AvailableLocales() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	locales := make([]string, 0, len(t.locales))
	for locale := range t.locales {
		locales = append(locales, locale)
	}
	return locales
}

var defaultTranslator = NewTranslator("en_US")

func Default() *Translator { return defaultTranslator }

func Get(key string, args ...interface{}) string {
	return defaultTranslator.Get(key, args...)
}

func SetLocale(locale string) {
	defaultTranslator.SetLocale(locale)
}
