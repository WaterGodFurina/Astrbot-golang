// Package t2i implements text-to-image conversion.
// Ported from astrbot/core/utils/t2i/
//
// In the Python version, this uses HTML templates + browser rendering (playwright)
// to convert text to images. In Go, we provide the framework but use a simpler
// approach: HTML template rendering via standard library.
package t2i

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sync"
)

// Renderer converts text to images using HTML templates.
type Renderer struct {
	mu        sync.RWMutex
	templates map[string]*template.Template
	tmplDir   string
	enabled   bool
}

// NewRenderer creates a T2I renderer.
func NewRenderer(tmplDir string) *Renderer {
	r := &Renderer{
		templates: make(map[string]*template.Template),
		tmplDir:   tmplDir,
		enabled:   true,
	}
	// Try to load default template
	if err := r.LoadTemplate("default", "default.html"); err != nil {
		logger.Debug("t2i default template not loaded: %v", err)
	}
	return r
}

// LoadTemplate loads an HTML template by name.
func (r *Renderer) LoadTemplate(name, filename string) error {
	path := filepath.Join(r.tmplDir, filename)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tmpl, err := template.New(name).Parse(string(data))
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.templates[name] = tmpl
	r.mu.Unlock()
	return nil
}

// Render renders text as HTML using a template.
func (r *Renderer) Render(text, templateName string) (string, error) {
	r.mu.RLock()
	tmpl, ok := r.templates[templateName]
	if !ok {
		tmpl, ok = r.templates["default"]
	}
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("template %s not found and no default template", templateName)
	}

	var buf bytes.Buffer
	data := struct{ Content string }{Content: text}
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// IsEnabled returns true if T2I is enabled.
func (r *Renderer) IsEnabled() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.enabled
}

// SetEnabled enables/disables T2I.
func (r *Renderer) SetEnabled(enabled bool) {
	r.mu.Lock()
	r.enabled = enabled
	r.mu.Unlock()
}
