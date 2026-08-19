// Package dashboard - provider source storage and model listing.
package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// getProviderSources returns all provider sources from the default config.
func (s *Server) getProviderSources() []interface{} {
	cfg := s.getConfigSnapshot()
	sources, _ := cfg["provider_sources"].([]interface{})
	return sources
}

// getProviderSourceByID returns a provider source by id.
func (s *Server) getProviderSourceByID(id string) map[string]interface{} {
	for _, src := range s.getProviderSources() {
		if m, ok := src.(map[string]interface{}); ok {
			if sid, _ := m["id"].(string); sid == id {
				return m
			}
		}
	}
	return map[string]interface{}{}
}

// upsertProviderSource inserts or replaces a provider source in the default config.
func (s *Server) upsertProviderSource(config map[string]interface{}) error {
	if config == nil {
		config = map[string]interface{}{}
	}
	id, _ := config["id"].(string)
	if id == "" {
		return fmt.Errorf("provider source config must have an 'id' field")
	}
	cfg := s.getConfigSnapshot()
	sources, _ := cfg["provider_sources"].([]interface{})
	replaced := false
	for i, src := range sources {
		if m, ok := src.(map[string]interface{}); ok {
			if sid, _ := m["id"].(string); sid == id {
				sources[i] = config
				replaced = true
				break
			}
		}
	}
	if !replaced {
		sources = append(sources, config)
	}
	return s.setConfigData("provider_sources", sources)
}

// deleteProviderSource removes a provider source by id. Providers built on
// this source are cascaded away too (mirrors Python's delete_provider_source,
// which calls provider_manager.delete_provider(provider_source_id=...)), so no
// orphan providers reference a deleted source.
func (s *Server) deleteProviderSource(id string) error {
	cfg := s.getConfigSnapshot()
	sources, _ := cfg["provider_sources"].([]interface{})
	next := make([]interface{}, 0, len(sources))
	found := false
	for _, src := range sources {
		if m, ok := src.(map[string]interface{}); ok {
			if sid, _ := m["id"].(string); sid == id {
				found = true
				continue
			}
		}
		next = append(next, src)
	}
	if !found {
		return fmt.Errorf("provider source %s not found", id)
	}
	if err := s.setConfigData("provider_sources", next); err != nil {
		return err
	}

	// Cascade: drop providers whose provider_source_id points at the deleted
	// source and unregister their runtime instances.
	providers, _ := cfg["provider"].([]interface{})
	kept := make([]interface{}, 0, len(providers))
	removed := 0
	for _, p := range providers {
		if m, ok := p.(map[string]interface{}); ok {
			if sid, _ := m["provider_source_id"].(string); sid == id {
				if pid, _ := m["id"].(string); pid != "" {
					s.unregisterProvider(pid)
				}
				removed++
				continue
			}
		}
		kept = append(kept, p)
	}
	if removed > 0 {
		if err := s.setConfigData("provider", kept); err != nil {
			return err
		}
	}
	return nil
}

// fetchProviderSourceModels calls the provider API to list models for a source.
// Ported from astrbot/dashboard/services/config_service.py list_provider_source_models.
func (s *Server) fetchProviderSourceModels(sourceID string) ([]string, map[string]interface{}, error) {
	source := s.getProviderSourceByID(sourceID)
	if len(source) == 0 {
		return nil, nil, fmt.Errorf("provider source %s not found", sourceID)
	}
	providerType, _ := source["type"].(string)
	if providerType == "" {
		return nil, nil, fmt.Errorf("provider source missing type")
	}
	apiBase, _ := source["api_base"].(string)
	keys, _ := source["key"].([]interface{})
	apiKey := ""
	if len(keys) > 0 {
		apiKey, _ = keys[0].(string)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var models []string

	switch providerType {
	case "ollama":
		list, err := s.fetchOllamaModels(client, apiBase)
		if err != nil {
			return nil, nil, err
		}
		models = list
	case "anthropic":
		list, err := s.fetchOpenAICompatModels(client, strings.TrimSuffix(apiBase, "/")+"/v1/models", "x-api-key", apiKey, "anthropic-version", "2023-06-01")
		if err != nil {
			return nil, nil, err
		}
		models = list
	case "gemini":
		list, err := s.fetchOpenAICompatModels(client, strings.TrimSuffix(apiBase, "/")+"/models", "x-goog-api-key", apiKey, "", "")
		if err != nil {
			return nil, nil, err
		}
		models = list
	default:
		// OpenAI-compatible providers
		list, err := s.fetchOpenAICompatModels(client, strings.TrimSuffix(apiBase, "/")+"/models", "Authorization", "Bearer "+apiKey, "", "")
		if err != nil {
			return nil, nil, err
		}
		models = list
	}

	return models, map[string]interface{}{}, nil
}

// fetchOpenAICompatModels lists models via an OpenAI-compatible /models endpoint.
// URL 出站前过 SSRF 校验（拒绝内网/回环地址）。
func (s *Server) fetchOpenAICompatModels(client *http.Client, url, authHeader, authValue, extraHeader, extraValue string) ([]string, error) {
	if err := validateOutboundURL(url); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if authValue != "" {
		req.Header.Set(authHeader, authValue)
	}
	if extraHeader != "" {
		req.Header.Set(extraHeader, extraValue)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取模型列表失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("获取模型列表失败 (%d): %s", resp.StatusCode, string(body))
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %v", err)
	}
	models := make([]string, 0, len(payload.Data))
	for _, m := range payload.Data {
		if m.ID != "" {
			models = append(models, m.ID)
		}
	}
	return models, nil
}

// fetchOllamaModels lists models via the Ollama /api/tags endpoint.
// URL 出站前过 SSRF 校验（拒绝内网/回环地址）。
func (s *Server) fetchOllamaModels(client *http.Client, apiBase string) ([]string, error) {
	url := strings.TrimSuffix(apiBase, "/") + "/api/tags"
	if err := validateOutboundURL(url); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("获取模型列表失败: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("获取模型列表失败 (%d): %s", resp.StatusCode, string(body))
	}
	var payload struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析模型列表失败: %v", err)
	}
	models := make([]string, 0, len(payload.Models))
	for _, m := range payload.Models {
		if m.Name != "" {
			models = append(models, m.Name)
		}
	}
	return models, nil
}
