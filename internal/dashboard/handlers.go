// Package dashboard - API handler implementations.
// Ported from astrbot/dashboard/api/ route modules.
package dashboard

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/AstrBotDevs/AstrBot/internal/log"
	"github.com/AstrBotDevs/AstrBot/internal/provider"
	"github.com/AstrBotDevs/AstrBot/internal/star"
)

// ── Auth handlers ────────────────────────────────────────────
// (handleAuth, handleLogin, handleLogout, handleCheck, handleSetupStatus,
//  handleSetup, handleAccountEdit are in server.go)

// ── System config handlers ──────────────────────────────────

func (s *Server) handleSystemConfig(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "get":
		if r.Method == http.MethodPut {
			// Frontend sends the config object directly (DynamicConfig),
			// not wrapped in {"config": ...}.
			var raw json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&raw)
			var body struct {
				Config map[string]interface{} `json:"config"`
			}
			var direct map[string]interface{}
			if err := json.Unmarshal(raw, &direct); err == nil && direct != nil {
				if inner, ok := direct["config"].(map[string]interface{}); ok && len(direct) == 1 {
					body.Config = inner
				} else {
					body.Config = direct
				}
			}
			if body.Config != nil {
				if err := s.setConfigDataAll(body.Config); err != nil {
					writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
					return
				}
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "保存成功",
			}))
			return
		}
		cfg := s.getConfigData("default")
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"config":   cfg,
			"metadata": s.getSystemMetadata(),
		}))
	case "schema":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"config":   s.getConfigSnapshot(),
			"metadata": s.getProfileMetadata(),
		}))
	case "runtime":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"config":                     s.getConfigSnapshot(),
			"metadata":                   s.getConfigMetadata(),
			"platform_i18n_translations": map[string]interface{}{},
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// getConfigSnapshot returns the current config as a map.
// Based on the Python DEFAULT_CONFIG, deep-merged with persisted values.
func (s *Server) getConfigSnapshot() map[string]interface{} {
	cfg := defaultConfigFromJSON()
	persisted := s.getConfigData("default")
	deepMerge(cfg, persisted)
	// Normalize wake_prefix: persisted legacy configs may store "" (string).
	if wp, ok := cfg["wake_prefix"]; ok {
		switch v := wp.(type) {
		case string:
			if v == "" {
				cfg["wake_prefix"] = []interface{}{"/"}
			} else {
				cfg["wake_prefix"] = []interface{}{v}
			}
		case nil:
			cfg["wake_prefix"] = []interface{}{"/"}
		}
	}
	if s.auth != nil {
		cfg["dashboard"] = map[string]interface{}{
			"username": s.auth.Username(),
			"host":     "0.0.0.0",
			"port":     s.port,
		}
	}
	return cfg
}

// deepMerge recursively overlays src values onto dst; dst keys win at leaf level.
func deepMerge(dst, src map[string]interface{}) {
	for k, v := range src {
		if sm, ok := v.(map[string]interface{}); ok {
			if dm, ok := dst[k].(map[string]interface{}); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// getConfigMetadata returns the config metadata (templates) consumed by the WebUI.
// Ported from astrbot/core/config/default.py CONFIG_METADATA_2 (subset for Go-supported platforms).
// getSystemMetadata returns the metadata for the system-config page (system_group only).
// Ported from astrbot/dashboard/services/config_service.py get_system_schema.
func (s *Server) getSystemMetadata() map[string]interface{} {
	metadata := metadataFromJSON()
	for g := range metadata {
		if g != "system_group" {
			delete(metadata, g)
		}
	}
	return metadata
}

// getProfileMetadata returns the metadata for the config-profiles page.
// Ported from astrbot/dashboard/services/config_service.py get_profile_schema
// (CONFIG_METADATA_3: ai/platform/plugin/ext groups + platform adapter templates).
func (s *Server) getProfileMetadata() map[string]interface{} {
	metadata := metadataFromJSON()
	for g := range metadata {
		if g != "ai_group" && g != "platform_group" && g != "plugin_group" && g != "ext_group" {
			delete(metadata, g)
		}
	}
	s.injectPlatformSection(metadata)
	return metadata
}

// getConfigMetadata returns the full metadata for system-config/runtime.
func (s *Server) getConfigMetadata() map[string]interface{} {
	metadata := metadataFromJSON()
	s.injectPlatformSection(metadata)
	providerGroup := map[string]interface{}{
		"name": "provider_group.name",
		"metadata": map[string]interface{}{
			"provider_settings": map[string]interface{}{
				"description": "provider_group.provider_settings.description",
				"type":        "object",
				"items": map[string]interface{}{
					"provider_settings.enable": map[string]interface{}{
						"description": "provider_group.provider_settings.enable.description",
						"type":        "bool",
					},
					"provider_settings.default_provider_id": map[string]interface{}{
						"description": "provider_group.provider_settings.default_provider_id.description",
						"type":        "string",
					},
					"provider_settings.wake_prefix": map[string]interface{}{
						"description": "provider_group.provider_settings.wake_prefix.description",
						"type":        "string",
					},
					"provider_settings.prompt_prefix": map[string]interface{}{
						"description": "provider_group.provider_settings.prompt_prefix.description",
						"type":        "string",
					},
					"provider_settings.identifier": map[string]interface{}{
						"description": "provider_group.provider_settings.identifier.description",
						"type":        "bool",
					},
					"provider_settings.display_reasoning_text": map[string]interface{}{
						"description": "provider_group.provider_settings.display_reasoning_text.description",
						"type":        "bool",
					},
					"provider_settings.max_context_length": map[string]interface{}{
						"description": "provider_group.provider_settings.max_context_length.description",
						"type":        "int",
					},
					"provider_settings.dequeue_context_length": map[string]interface{}{
						"description": "provider_group.provider_settings.dequeue_context_length.description",
						"type":        "int",
					},
					"provider_settings.request_max_retries": map[string]interface{}{
						"description": "provider_group.provider_settings.request_max_retries.description",
						"type":        "int",
					},
					"provider_settings.web_search": map[string]interface{}{
						"description": "provider_group.provider_settings.web_search.description",
						"type":        "bool",
					},
					"provider_settings.streaming_response": map[string]interface{}{
						"description": "provider_group.provider_settings.streaming_response.description",
						"type":        "bool",
					},
				},
			},
			"provider": map[string]interface{}{
				"description":     "大语言模型提供方",
				"type":            "list",
				"config_template": s.getProviderTemplates(),
				"items":           s.getProviderItems(),
			},
		},
	}
	metadata["provider_group"] = providerGroup
	return metadata
}

// injectPlatformSection adds the Go-supported platform adapter templates to the
// platform_group metadata (mirrors Python's platform_registry injection).
func (s *Server) injectPlatformSection(metadata map[string]interface{}) {
	platformSection := map[string]interface{}{
		"description": "消息平台适配器",
		"type":        "list",
		"config_template": map[string]interface{}{
			"QQ 官方机器人(Websocket, 推荐)": map[string]interface{}{
				"id": "default", "type": "qq_official", "enable": true,
				"appid": "", "secret": "",
				"enable_group_c2c": true, "enable_guild_direct_message": true,
			},
			"OneBot v11": map[string]interface{}{
				"id": "default", "type": "aiocqhttp", "enable": true,
				"ws_reverse_host": "0.0.0.0", "ws_reverse_port": 6199, "ws_reverse_token": "",
			},
			"WebChat": map[string]interface{}{
				"id": "default", "type": "webchat", "enable": false,
			},
			"Telegram": map[string]interface{}{
				"id": "telegram", "type": "telegram", "enable": true,
				"telegram_token":                     "your_bot_token",
				"start_message":                      "Hello, I'm AstrBot!",
				"telegram_api_base_url":              "https://api.telegram.org/bot",
				"telegram_file_base_url":             "https://api.telegram.org/file/bot",
				"telegram_command_register":          true,
				"telegram_command_auto_refresh":      true,
				"telegram_command_register_interval": 300,
				"telegram_polling_restart_delay":     5.0,
			},
		},
		"items": map[string]interface{}{
			"id": map[string]interface{}{
				"description": "机器人名称",
				"type":        "string",
				"hint":        "机器人名称",
			},
			"type": map[string]interface{}{
				"description": "适配器类型",
				"type":        "string",
				"invisible":   true,
			},
			"enable": map[string]interface{}{
				"description": "启用",
				"type":        "bool",
				"hint":        "是否启用该适配器。未启用的适配器对应的消息平台将不会接收到消息。",
			},
			"ws_reverse_host": map[string]interface{}{
				"description": "反向 Websocket 主机",
				"type":        "string",
				"hint":        "AstrBot 将作为服务器端。",
			},
			"ws_reverse_port": map[string]interface{}{
				"description": "反向 Websocket 端口",
				"type":        "int",
			},
			"ws_reverse_token": map[string]interface{}{
				"description": "反向 Websocket Token",
				"type":        "string",
				"hint":        "反向 Websocket Token。未设置则不启用 Token 验证。",
			},
			"appid": map[string]interface{}{
				"description": "appid",
				"type":        "string",
				"hint":        "必填项。当前消息平台的 AppID。如何获取请参考对应平台接入文档。",
			},
			"secret": map[string]interface{}{
				"description": "secret",
				"type":        "string",
				"hint":        "必填项。",
			},
			"enable_group_c2c": map[string]interface{}{
				"description": "启用消息列表单聊",
				"type":        "bool",
				"hint":        "启用后，机器人可以接收到 QQ 消息列表中的私聊消息。",
			},
			"enable_guild_direct_message": map[string]interface{}{
				"description": "启用频道私聊",
				"type":        "bool",
				"hint":        "启用后，机器人可以接收到频道的私聊消息。",
			},
			"is_sandbox": map[string]interface{}{
				"description": "沙箱模式",
				"type":        "bool",
			},
			"telegram_token": map[string]interface{}{
				"description": "Bot Token",
				"type":        "string",
				"hint":        "如果你的网络环境为中国大陆，请在 `其他配置` 处设置代理或更改 api_base。",
			},
			"start_message": map[string]interface{}{
				"description": "开始消息",
				"type":        "string",
			},
			"telegram_api_base_url": map[string]interface{}{
				"description": "Telegram API 基础地址",
				"type":        "string",
				"hint":        "默认 https://api.telegram.org/bot，中国大陆环境建议配置代理。",
			},
			"telegram_file_base_url": map[string]interface{}{
				"description": "Telegram 文件基础地址",
				"type":        "string",
				"hint":        "默认 https://api.telegram.org/file/bot",
			},
			"telegram_command_register": map[string]interface{}{
				"description": "Telegram 命令注册",
				"type":        "bool",
				"hint":        "启用后，AstrBot 将会自动注册 Telegram 命令。",
			},
			"telegram_command_auto_refresh": map[string]interface{}{
				"description": "Telegram 命令自动刷新",
				"type":        "bool",
				"hint":        "启用后，AstrBot 将会在运行时自动刷新 Telegram 命令。(单独设置此项无效)",
			},
			"telegram_command_register_interval": map[string]interface{}{
				"description": "Telegram 命令自动刷新间隔",
				"type":        "int",
				"hint":        "Telegram 命令自动刷新间隔，单位为秒。",
			},
			"telegram_polling_restart_delay": map[string]interface{}{
				"description": "Telegram 轮询重启延迟",
				"type":        "float",
				"hint":        "当轮询意外结束尝试自动重启时的延迟时间，单位为秒。默认为 5s。",
			},
		},
	}
	if pg, ok := metadata["platform_group"].(map[string]interface{}); ok {
		if md, ok := pg["metadata"].(map[string]interface{}); ok {
			md["platform"] = platformSection
		}
	}
}

// getProviderItems returns the provider config field schema shared by
// /providers/schema and the system config metadata.
func (s *Server) getProviderItems() map[string]interface{} {
	return map[string]interface{}{
		"id": map[string]interface{}{
			"description": "名称",
			"type":        "string",
			"hint":        "此模型提供方的唯一标识。",
		},
		"type": map[string]interface{}{
			"description": "类型",
			"type":        "string",
			"invisible":   true,
		},
		"provider": map[string]interface{}{
			"description": "提供商",
			"type":        "string",
			"invisible":   true,
		},
		"provider_type": map[string]interface{}{
			"description": "提供商类型",
			"type":        "string",
			"invisible":   true,
		},
		"enable": map[string]interface{}{
			"description": "启用",
			"type":        "bool",
		},
		"key": map[string]interface{}{
			"description": "API Key",
			"type":        "list",
			"items":       map[string]interface{}{"type": "string"},
		},
		"api_base": map[string]interface{}{
			"description": "API Base URL",
			"type":        "string",
		},
		"proxy": map[string]interface{}{
			"description": "代理地址",
			"type":        "string",
			"hint":        "留空则直连。格式示例: http://127.0.0.1:7890",
		},
		"timeout": map[string]interface{}{
			"description": "请求超时时间（秒）",
			"type":        "int",
			"hint":        "默认 120 秒。",
		},
		"model": map[string]interface{}{
			"description": "模型 ID",
			"type":        "string",
			"hint":        "模型名称，如 gpt-4o-mini, deepseek-chat。",
		},
		"max_context_tokens": map[string]interface{}{
			"description": "模型上下文窗口大小",
			"type":        "int",
			"hint":        "模型最大上下文 Token 大小。如果为 0，则会自动从模型元数据填充（如有）",
		},
		"modalities": map[string]interface{}{
			"description": "模型能力",
			"type":        "list",
			"items":       map[string]interface{}{"type": "string"},
			"options":     []string{"text", "image", "audio", "tool_use"},
			"labels":      []string{"文本", "图像", "音频", "工具使用"},
			"render_type": "checkbox",
			"hint":        "模型支持的模态及能力。",
		},
		"custom_headers": map[string]interface{}{
			"description": "自定义请求头",
			"type":        "dict",
			"items":       map[string]interface{}{},
			"hint":        "此处添加的键值对将被合并到 HTTP 请求头中。",
		},
		"custom_extra_body": map[string]interface{}{
			"description": "自定义请求体参数",
			"type":        "dict",
			"items":       map[string]interface{}{},
			"hint":        "用于在请求时添加额外的参数，如 temperature, top_p, max_tokens, reasoning_effort 等。",
		},
	}
}

// getProviderTemplates returns provider config templates for the Go-supported providers.
func (s *Server) getProviderTemplates() map[string]interface{} {
	template := func(name, providerType, apiBase string) map[string]interface{} {
		return map[string]interface{}{
			"id":            name,
			"type":          name,
			"provider":      providerType,
			"provider_type": "chat_completion",
			"enable":        false,
			"api_base":      apiBase,
			"key":           "",
		}
	}
	return map[string]interface{}{
		"openai":     template("openai", "openai_chat_completion", "https://api.openai.com/v1"),
		"openrouter": template("openrouter", "openrouter_chat_completion", "https://openrouter.ai/api/v1"),
		"anthropic":  template("anthropic", "anthropic_chat_completion", "https://api.anthropic.com"),
		"gemini":     template("gemini", "googlegenai_chat_completion", "https://generativelanguage.googleapis.com/v1beta"),
		"ollama":     template("ollama", "ollama_chat_completion", "http://127.0.0.1:11434"),
		"dashscope":  template("dashscope", "dashscope_chat_completion", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
	}
}

// ── Config handlers (legacy /api/config/) ───────────────────

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "get":
		writeJSON(w, http.StatusOK, apiOK(s.getConfigSnapshot()))
	case "set", "update":
		// Accept config updates (stub — would need to merge into config file)
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "config updated",
		}))
	case "reload":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "config reloaded",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Provider handlers ────────────────────────────────────────

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		if r.Method == http.MethodPost {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["config"] == nil {
				body["config"] = map[string]interface{}{}
			}
			config, _ := body["config"].(map[string]interface{})
			if err := s.upsertProvider(config); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "保存成功",
			}))
			return
		}
		cfg := s.getConfigSnapshot()
		providers, _ := cfg["provider"].([]interface{})
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"providers":      providers,
			"model_metadata": map[string]interface{}{},
		}))
	case "schema":
		cfg := s.getConfigSnapshot()
		providers, _ := cfg["provider"].([]interface{})
		providerTemplates := s.getProviderTemplates()
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"config_schema": map[string]interface{}{
				"provider": map[string]interface{}{
					"description":     "大语言模型提供方",
					"type":            "list",
					"config_template": providerTemplates,
					"items":           s.getProviderItems(),
				},
			},
			"providers":        providers,
			"provider_sources": s.getProviderSources(),
			"model_metadata":   map[string]interface{}{},
		}))
	case "test":
		if r.Method == http.MethodPost {
			var body struct {
				ProviderID string `json:"provider_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, http.StatusOK, apiOK(s.testProvider(body.ProviderID)))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "by-id":
		providerID := r.URL.Query().Get("provider_id")
		switch r.Method {
		case http.MethodGet:
			merged := r.URL.Query().Get("merged") == "true"
			p := s.getProviderByID(providerID)
			if merged && len(p) > 0 {
				p = s.mergeProviderSource(p)
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"provider": p,
				"merged":   merged,
			}))
		case http.MethodPut:
			var body struct {
				ProviderID string                 `json:"provider_id"`
				Config     map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ProviderID != "" {
				providerID = body.ProviderID
			}
			if body.Config == nil {
				body.Config = map[string]interface{}{}
			}
			body.Config["id"] = providerID
			if err := s.upsertProvider(body.Config); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "保存成功",
			}))
		case http.MethodDelete:
			if err := s.deleteProviderByID(providerID); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("删除失败"))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "删除成功",
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "enabled":
		var body struct {
			ProviderID string `json:"provider_id"`
			Enabled    bool   `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		writeJSON(w, http.StatusOK, apiOK(s.setProviderEnabled(body.ProviderID, body.Enabled)))
	case "models":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"models": []interface{}{},
		}))
	case "embedding-dimension":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"dimension": 0,
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// mergeProviderSource merges the provider source config into a provider config.
// Ported from astrbot/core/provider/manager.py get_merged_provider_config:
// provider config wins, id stays as the provider's id.
func (s *Server) mergeProviderSource(pc map[string]interface{}) map[string]interface{} {
	sourceID, _ := pc["provider_source_id"].(string)
	if sourceID == "" {
		return pc
	}
	source := s.getProviderSourceByID(sourceID)
	if len(source) == 0 {
		return pc
	}
	merged := map[string]interface{}{}
	for k, v := range source {
		merged[k] = v
	}
	for k, v := range pc {
		merged[k] = v
	}
	return merged
}

// testProvider verifies a provider by sending a test chat request.
// Ported from astrbot/dashboard/services/config_service.py test_provider.
func (s *Server) testProvider(providerID string) map[string]interface{} {
	result := map[string]interface{}{
		"id":     providerID,
		"model":  "",
		"type":   "",
		"name":   providerID,
		"status": "unavailable",
		"error":  nil,
	}
	cfg := s.getConfigSnapshot()
	providers, _ := cfg["provider"].([]interface{})
	var target map[string]interface{}
	for _, p := range providers {
		if m, ok := p.(map[string]interface{}); ok {
			if pid, _ := m["id"].(string); pid == providerID {
				target = m
				break
			}
		}
	}
	if target == nil {
		result["error"] = fmt.Sprintf("Provider %s not found", providerID)
		return result
	}
	merged := s.mergeProviderSource(target)
	providerType, _ := merged["type"].(string)
	if providerType == "" {
		providerType, _ = merged["provider"].(string)
	}
	model, _ := merged["model"].(string)
	result["model"] = model
	result["type"] = providerType

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	inst, err := provider.CreateProvider(providerType, merged, map[string]interface{}{})
	if err != nil {
		result["error"] = err.Error()
		return result
	}
	if err := inst.Test(ctx); err != nil {
		result["error"] = err.Error()
		return result
	}
	result["status"] = "available"
	return result
}

// upsertProvider inserts or replaces a provider config in the default config and persists it.
func (s *Server) upsertProvider(config map[string]interface{}) error {
	id, _ := config["id"].(string)
	if id == "" {
		return fmt.Errorf("缺少提供商 ID")
	}
	cfg := s.getConfigSnapshot()
	providers, _ := cfg["provider"].([]interface{})
	replaced := false
	for i, p := range providers {
		if m, ok := p.(map[string]interface{}); ok {
			if pid, _ := m["id"].(string); pid == id {
				providers[i] = config
				replaced = true
				break
			}
		}
	}
	if !replaced {
		providers = append(providers, config)
	}
	return s.setConfigData("provider", providers)
}

// getProviderByID returns a provider config by id.
func (s *Server) getProviderByID(id string) map[string]interface{} {
	cfg := s.getConfigSnapshot()
	providers, _ := cfg["provider"].([]interface{})
	for _, p := range providers {
		if m, ok := p.(map[string]interface{}); ok {
			if pid, _ := m["id"].(string); pid == id {
				return m
			}
		}
	}
	return map[string]interface{}{}
}

// setProviderEnabled toggles a provider's enable flag.
func (s *Server) setProviderEnabled(id string, enabled bool) map[string]interface{} {
	cfg := s.getConfigSnapshot()
	providers, _ := cfg["provider"].([]interface{})
	for i, p := range providers {
		if m, ok := p.(map[string]interface{}); ok {
			if pid, _ := m["id"].(string); pid == id {
				providers[i].(map[string]interface{})["enable"] = enabled
				if err := s.setConfigData("provider", providers); err != nil {
					return map[string]interface{}{"message": "保存失败: " + err.Error()}
				}
				return map[string]interface{}{"message": "更新成功"}
			}
		}
	}
	return map[string]interface{}{"message": "未找到对应提供商"}
}

// deleteProviderByID removes a provider config by id.
func (s *Server) deleteProviderByID(id string) error {
	cfg := s.getConfigSnapshot()
	providers, _ := cfg["provider"].([]interface{})
	next := make([]interface{}, 0, len(providers))
	for _, p := range providers {
		if m, ok := p.(map[string]interface{}); ok {
			if pid, _ := m["id"].(string); pid == id {
				continue
			}
		}
		next = append(next, p)
	}
	return s.setConfigData("provider", next)
}

// ── Platform / Bot handlers ─────────────────────────────────

func (s *Server) handleBots(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		if r.Method == http.MethodPost && sub == "" {
			s.createBot(w, r)
			return
		}
		bots := s.getBotList()
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"bots": bots,
		}))
	case "by-id":
		switch r.Method {
		case http.MethodGet:
			botID := r.URL.Query().Get("bot_id")
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"bot": s.getBotByID(botID),
			}))
		case http.MethodPut:
			var body struct {
				BotID  string                 `json:"bot_id"`
				Config map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Config == nil {
				body.Config = map[string]interface{}{}
			}
			if body.BotID != "" {
				body.Config["id"] = body.BotID
			}
			if id, _ := body.Config["id"].(string); id != "" {
				if err := s.upsertBot(body.Config); err != nil {
					writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
					return
				}
				s.notifyPlatformsChanged()
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "保存成功",
			}))
		case http.MethodDelete:
			botID := r.URL.Query().Get("bot_id")
			s.deleteBotByID(botID)
			s.notifyPlatformsChanged()
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "删除成功",
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "enabled":
		if r.Method == http.MethodPatch {
			s.setBotEnabled(w, r)
		} else {
			bots := s.getBotList()
			enabled := make([]interface{}, 0)
			for _, b := range bots {
				if m, ok := b.(map[string]interface{}); ok {
					if enable, ok := m["enable"].(bool); ok && enable {
						enabled = append(enabled, b)
					}
				}
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"bots": enabled,
			}))
		}
	case "stats":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"platforms": []interface{}{},
		}))
	case "create":
		if r.Method == http.MethodPost {
			s.createBot(w, r)
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "set-enabled", "set_enabled":
		s.setBotEnabled(w, r)
	case "delete":
		s.deleteBot(w, r)
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// createBot handles POST /api/v1/bots (create or update by id).
func (s *Server) createBot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string                 `json:"id"`
		Type    string                 `json:"type"`
		Enabled *bool                  `json:"enabled"`
		Config  map[string]interface{} `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("invalid JSON: "+err.Error()))
		return
	}
	if body.Config == nil {
		body.Config = map[string]interface{}{}
	}
	botID := body.ID
	if botID == "" {
		botID, _ = body.Config["id"].(string)
	}
	if botID == "" {
		writeJSON(w, http.StatusBadRequest, apiError("缺少机器人 ID"))
		return
	}
	body.Config["id"] = botID
	if body.Type != "" {
		body.Config["type"] = body.Type
	}
	if body.Enabled != nil {
		body.Config["enable"] = *body.Enabled
	}
	if _, ok := body.Config["enable"]; !ok {
		body.Config["enable"] = true
	}

	if err := s.upsertBot(body.Config); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
		return
	}
	s.notifyPlatformsChanged()
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"id":      botID,
		"message": "保存成功",
	}))
}

// notifyPlatformsChanged triggers the platform reload callback if registered.
func (s *Server) notifyPlatformsChanged() {
	if s.onPlatformsChanged != nil {
		s.onPlatformsChanged()
	}
}

// upsertBot inserts or replaces a platform config in the default config and persists it.
func (s *Server) upsertBot(config map[string]interface{}) error {
	botID, _ := config["id"].(string)
	if botID == "" {
		return fmt.Errorf("缺少机器人 ID")
	}
	platforms := s.getBotList()
	replaced := false
	for i, p := range platforms {
		if m, ok := p.(map[string]interface{}); ok {
			if id, _ := m["id"].(string); id == botID {
				platforms[i] = config
				replaced = true
				break
			}
		}
	}
	if !replaced {
		platforms = append(platforms, config)
	}
	return s.setConfigData("platform", platforms)
}

// setBotEnabled handles PATCH /api/v1/bots/set-enabled.
func (s *Server) setBotEnabled(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BotID   string `json:"bot_id"`
		Enabled bool   `json:"enabled"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	platforms := s.getBotList()
	for i, p := range platforms {
		if m, ok := p.(map[string]interface{}); ok {
			if id, _ := m["id"].(string); id == body.BotID {
				platforms[i].(map[string]interface{})["enable"] = body.Enabled
				if err := s.setConfigData("platform", platforms); err != nil {
					writeJSON(w, http.StatusInternalServerError, apiError("保存失败"))
					return
				}
				s.notifyPlatformsChanged()
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "更新成功",
				}))
				return
			}
		}
	}
	writeJSON(w, http.StatusOK, apiError("未找到对应机器人"))
}

// deleteBot handles DELETE /api/v1/bots/delete.
func (s *Server) deleteBot(w http.ResponseWriter, r *http.Request) {
	botID := r.URL.Query().Get("bot_id")
	s.deleteBotByID(botID)
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"message": "删除成功",
	}))
}

// deleteBotByID removes a platform config by id and persists the change.
func (s *Server) deleteBotByID(botID string) {
	platforms := s.getBotList()
	next := make([]interface{}, 0, len(platforms))
	for _, p := range platforms {
		if m, ok := p.(map[string]interface{}); ok {
			if id, _ := m["id"].(string); id == botID {
				continue
			}
		}
		next = append(next, p)
	}
	_ = s.setConfigData("platform", next)
}

// getBotByID returns a single bot config by id.
func (s *Server) getBotByID(botID string) map[string]interface{} {
	for _, p := range s.getBotList() {
		if m, ok := p.(map[string]interface{}); ok {
			if id, _ := m["id"].(string); id == botID {
				return m
			}
		}
	}
	return map[string]interface{}{}
}

// ── Plugin handlers ──────────────────────────────────────────

func (s *Server) handlePlugins(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		plugins := s.getPluginList()
		writeJSON(w, http.StatusOK, apiOK(plugins))
	case "by-id":
		if r.Method == http.MethodPost {
			pluginID := r.URL.Query().Get("plugin_id")
			s.pluginUninstall(pluginID, true)
			writeJSON(w, http.StatusOK, apiOKMsg("插件已卸载", map[string]interface{}{}))
		} else {
			pluginID := r.URL.Query().Get("plugin_id")
			writeJSON(w, http.StatusOK, apiOK(s.pluginByID(pluginID)))
		}
	case "enabled":
		var body struct {
			PluginID string `json:"plugin_id"`
			Enabled  bool   `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.pluginSetEnabled(body.PluginID, body.Enabled)
		writeJSON(w, http.StatusOK, apiOKMsg("插件状态已更新", map[string]interface{}{}))
	case "failed":
		writeJSON(w, http.StatusOK, apiOK(s.pluginFailed()))
	case "reload":
		var body struct {
			PluginID string `json:"plugin_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.pluginReload(body.PluginID)
		writeJSON(w, http.StatusOK, apiOKMsg("插件已重载", map[string]interface{}{}))
	case "config":
		if len(parts) > 1 && parts[1] == "schema" {
			pluginID := r.URL.Query().Get("plugin_id")
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"config_schema": s.pluginConfigSchema(pluginID),
			}))
		} else {
			pluginID := r.URL.Query().Get("plugin_id")
			if r.Method == http.MethodPost {
				var body struct {
					Config map[string]interface{} `json:"config"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				s.pluginSaveConfig(pluginID, body.Config)
				writeJSON(w, http.StatusOK, apiOKMsg("插件配置已保存", map[string]interface{}{}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"config": s.pluginLoadConfig(pluginID),
				}))
			}
		}
	case "market":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"market": []interface{}{},
		}))
	case "page":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "readme":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"content": "",
		}))
	case "config-files":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"files": []interface{}{},
		}))
	case "changelog":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"content": "",
		}))
	case "update":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "plugin update not implemented",
		}))
	case "install":
		if len(parts) > 1 {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "plugin install via " + parts[1] + " not implemented",
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "plugin install not implemented",
			}))
		}
	case "validate":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"valid": true,
		}))
	case "version-support":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"supported": true,
		}))
	default:
		// /api/v1/plugins/{plugin_id} and /api/v1/plugins/{plugin_id}/config
		pluginID := sub
		if len(parts) > 1 && parts[1] == "config" {
			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				var body struct {
					Config map[string]interface{} `json:"config"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				s.pluginSaveConfig(pluginID, body.Config)
				writeJSON(w, http.StatusOK, apiOKMsg("插件配置已保存", map[string]interface{}{}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"config": s.pluginLoadConfig(pluginID),
				}))
			}
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, apiOK(s.pluginByID(pluginID)))
		case http.MethodDelete, http.MethodPost:
			s.pluginUninstall(pluginID, false)
			writeJSON(w, http.StatusOK, apiOKMsg("插件已卸载", map[string]interface{}{}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	}
}

// ── Knowledge base handlers ──────────────────────────────────

func (s *Server) handleKB(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		kbs := s.getKBList()
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"kbs": kbs,
		}))
	case "tasks":
		if len(parts) > 1 {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"status":   "pending",
				"progress": 0,
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"tasks": []interface{}{},
			}))
		}
	case "by-id":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "create":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "update":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "delete":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "documents":
		if len(parts) > 1 {
			if parts[1] == "import-url" {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"documents": []interface{}{},
			}))
		}
	case "chunks":
		if len(parts) > 1 {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"chunks": []interface{}{},
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"chunks": []interface{}{},
			}))
		}
	case "retrieve":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"results": []interface{}{},
			"total":   0,
			"query":   "",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Sessions / Conversations handlers ───────────────────────

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		convs := s.getConversationList()
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"sessions": convs,
		}))
	case "active-umos":
		writeJSON(w, http.StatusOK, apiOK(s.getActiveUMOs()))
	case "provider":
		if r.Method == http.MethodPatch {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "rules":
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		} else if len(parts) > 1 && parts[1] == "delete" {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"deleted": 0,
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"rules":                    []interface{}{},
				"total":                    0,
				"available_personas":       []interface{}{},
				"available_chat_providers": s.getProviderList(),
				"available_stt_providers":  []interface{}{},
				"available_tts_providers":  []interface{}{},
				"available_plugins":        []interface{}{},
				"available_kbs":            []interface{}{},
			}))
		}
	case "service":
		if r.Method == http.MethodPatch {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "batch-delete":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"deleted": 0,
		}))
	case "export":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"data": []interface{}{},
		}))
	case "by-id":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "update":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "delete":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Persona handlers ─────────────────────────────────────────

func (s *Server) handlePersonas(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	if s.personas == nil {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		return
	}

	// /api/v1/persona-folders[/{folder_id}]
	if sub == "" && len(parts) > 0 && strings.HasPrefix(r.URL.Path, "/api/v1/persona-folders") {
		s.handlePersonaFolders(w, r)
		return
	}

	switch sub {
	case "", "list":
		switch r.Method {
		case http.MethodPost:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if err := s.personas.upsertPersona(body); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
				return
			}
			id, _ := body["persona_id"].(string)
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"id": id,
			}))
		default:
			folderID := r.URL.Query().Get("folder_id")
			var fid *string
			if r.URL.Query().Has("folder_id") {
				fid = &folderID
			}
			writeJSON(w, http.StatusOK, apiOK(s.personas.listPersonas(fid)))
		}
	case "by-id":
		personaID := r.URL.Query().Get("persona_id")
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"persona": s.personas.getPersona(personaID),
			}))
		case http.MethodPut:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if pid, ok := body["persona_id"].(string); ok && pid != "" {
				personaID = pid
			}
			body["persona_id"] = personaID
			if err := s.personas.upsertPersona(body); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "保存成功",
			}))
		case http.MethodDelete:
			if err := s.personas.deletePersona(personaID); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("删除失败"))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "删除成功",
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "move":
		var body struct {
			PersonaID string `json:"persona_id"`
			FolderID  string `json:"folder_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = s.personas.movePersona(body.PersonaID, body.FolderID)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "移动成功",
		}))
	case "reorder":
		var body struct {
			Items []map[string]interface{} `json:"items"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = s.personas.reorder(body.Items)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "更新成功",
		}))
	case "tree":
		writeJSON(w, http.StatusOK, apiOK(s.personas.tree()))
	case "create":
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := s.personas.upsertPersona(body); err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
			return
		}
		id, _ := body["persona_id"].(string)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"id": id,
		}))
	case "folders":
		s.handlePersonaFolders(w, r)
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// handlePersonaFolders handles GET/POST /api/v1/persona-folders and PUT/DELETE /api/v1/persona-folders/{folder_id}.
func (s *Server) handlePersonaFolders(w http.ResponseWriter, r *http.Request) {
	if s.personas == nil {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/persona-folders/"), "/")
	if len(parts) > 0 && parts[0] != "" {
		folderID := parts[0]
		switch r.Method {
		case http.MethodPut:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["folder_id"] = folderID
			if err := s.personas.upsertFolder(body); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"id": folderID,
			}))
		case http.MethodDelete:
			if err := s.personas.deleteFolder(folderID); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("删除失败"))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "删除成功",
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
		return
	}

	switch r.Method {
	case http.MethodPost:
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if err := s.personas.upsertFolder(body); err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"folder": body,
		}))
	default:
		parentID := r.URL.Query().Get("parent_id")
		var pid *string
		if r.URL.Query().Has("parent_id") {
			pid = &parentID
		}
		writeJSON(w, http.StatusOK, apiOK(s.personas.listFolders(pid)))
	}
}

// ── Tools / Skills handlers ──────────────────────────────────

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	// PATCH /api/v1/tools/{tool_id}/enabled|permission
	if len(parts) > 1 && r.Method == http.MethodPatch {
		s.updateTool(w, r, parts[0], parts[1])
		return
	}
	switch sub {
	case "", "list":
		writeJSON(w, http.StatusOK, apiOK(s.listTools()))
	case "by-name":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "archive":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "tool archive download not implemented",
		}))
	case "batch":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "file":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"content": "",
		}))
	case "files":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"files": []interface{}{},
		}))
	case "by-id":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// builtinToolNames lists the AstrBot built-in tools (readonly entries).
func builtinToolNames() []string {
	return []string{
		"web_search_tavily", "web_search_baidu", "web_search_bocha",
		"web_search_brave", "web_search_exa", "web_search_firecrawl",
		"tavily_extract_web_page", "firecrawl_extract_web_page", "exa_get_contents",
		"send_message_to_user", "get_group_message_history", "future_task",
		"astr_kb_search",
	}
}

// listTools returns the tool list for the dashboard tools panel.
// Built-in tools are readonly; MCP server tools come from the MCP store.
func (s *Server) listTools() []interface{} {
	cfg := s.getConfigSnapshot()
	permissions, _ := cfg["tool_permissions"].(map[string]interface{})
	if permissions == nil {
		permissions = map[string]interface{}{}
	}

	result := []interface{}{}
	for _, name := range builtinToolNames() {
		result = append(result, map[string]interface{}{
			"name":                    name,
			"description":             "AstrBot 内置工具",
			"parameters":              map[string]interface{}{},
			"active":                  true,
			"origin":                  "builtin",
			"origin_name":             "AstrBot Core",
			"origin_display_name":     "AstrBot Core",
			"readonly":                true,
			"builtin_config_statuses": []interface{}{},
			"builtin_config_tags":     []interface{}{},
		})
	}

	if s.mcp != nil {
		for _, server := range s.mcp.list() {
			name, _ := server["name"].(string)
			active, _ := server["active"].(bool)
			if name == "" {
				continue
			}
			perm := "member"
			configured := false
			if rec, ok := permissions[name].(map[string]interface{}); ok {
				if p, ok := rec["permission"].(string); ok && p != "" {
					perm = p
					configured = true
				}
			}
			result = append(result, map[string]interface{}{
				"name":                  name,
				"description":           "MCP 服务器工具（" + name + "）",
				"parameters":            map[string]interface{}{},
				"active":                active,
				"origin":                "mcp",
				"origin_name":           name,
				"origin_display_name":   name,
				"readonly":              false,
				"permission":            perm,
				"permission_configured": configured,
			})
		}
	}
	return result
}

// updateTool handles PATCH /api/v1/tools/{tool_id}/enabled|permission.
func (s *Server) updateTool(w http.ResponseWriter, r *http.Request, toolID, action string) {
	switch action {
	case "enabled":
		var body struct {
			Enabled bool `json:"enabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// Built-in tools are readonly; MCP tools map to their server active flag.
		for _, name := range builtinToolNames() {
			if name == toolID {
				writeJSON(w, http.StatusOK, apiError("内置工具不可禁用"))
				return
			}
		}
		if s.mcp != nil {
			if err := s.mcp.setEnabled(toolID, body.Enabled); err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
		}
		writeJSON(w, http.StatusOK, apiOKMsg("工具状态已更新", map[string]interface{}{}))
	case "permission":
		var body struct {
			Permission string `json:"permission"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Permission != "admin" && body.Permission != "member" {
			writeJSON(w, http.StatusOK, apiError("权限类型必须为 admin 或 member"))
			return
		}
		cfg := s.getConfigSnapshot()
		permissions, _ := cfg["tool_permissions"].(map[string]interface{})
		if permissions == nil {
			permissions = map[string]interface{}{}
		}
		permissions[toolID] = map[string]interface{}{"permission": body.Permission}
		_ = s.setConfigData("tool_permissions", permissions)
		writeJSON(w, http.StatusOK, apiOKMsg("工具权限已更新", map[string]interface{}{}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── MCP handlers ──────────────────────────────────────────────

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	if s.mcp == nil {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		return
	}
	// /api/v1/mcp/servers/{server_name}[/enabled|/test]
	if sub == "servers" && len(parts) > 1 {
		// Reserved keywords take precedence over the {server_name} path param.
		switch parts[1] {
		case "by-name":
			s.mcpByID(w, r)
			return
		case "enabled":
			var body struct {
				ServerName string `json:"server_name"`
				Enabled    bool   `json:"enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if err := s.mcp.setEnabled(body.ServerName, body.Enabled); err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("MCP server 状态已更新", map[string]interface{}{}))
			return
		case "test":
			var body struct {
				ServerName string `json:"server_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.testMCPServer(w, r, body.ServerName)
			return
		}
		serverName := parts[1]
		if len(parts) > 2 {
			switch parts[2] {
			case "enabled":
				var body struct {
					ServerName string `json:"server_name"`
					Enabled    bool   `json:"enabled"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if body.ServerName != "" {
					serverName = body.ServerName
				}
				if err := s.mcp.setEnabled(serverName, body.Enabled); err != nil {
					writeJSON(w, http.StatusOK, apiError(err.Error()))
					return
				}
				writeJSON(w, http.StatusOK, apiOKMsg("MCP server 状态已更新", map[string]interface{}{}))
			case "test":
				s.testMCPServer(w, r, serverName)
			default:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			}
			return
		}
		switch r.Method {
		case http.MethodPut, http.MethodPatch:
			var body struct {
				ServerName string                 `json:"server_name"`
				Config     map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ServerName != "" {
				serverName = body.ServerName
			}
			if body.Config == nil {
				body.Config = map[string]interface{}{}
			}
			body.Config["name"] = serverName
			if err := s.mcp.upsert(serverName, body.Config); err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("MCP server 已更新", map[string]interface{}{}))
		case http.MethodDelete:
			if err := s.mcp.delete(serverName); err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("MCP server 已删除", map[string]interface{}{}))
		default:
			writeJSON(w, http.StatusOK, apiOK(s.mcp.get(serverName)))
		}
		return
	}
	switch sub {
	case "servers":
		if len(parts) > 1 {
			// by-name / enabled / test are handled before the main switch
			// (reserved keywords take precedence over {server_name}).
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		} else if r.Method == http.MethodPost {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			name, _ := body["name"].(string)
			if name == "" {
				writeJSON(w, http.StatusOK, apiError("Server name cannot be empty"))
				return
			}
			if err := s.mcp.upsert(name, body); err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("MCP server 已添加", map[string]interface{}{}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(s.mcp.list()))
		}
	case "providers":
		if len(parts) > 1 && parts[1] == "modelscope" && len(parts) > 2 && parts[2] == "sync" {
			var body struct {
				AccessToken string `json:"access_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			count, err := s.syncModelScopeMCPServers(body.AccessToken)
			if err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg(
				fmt.Sprintf("同步成功，共同步 %d 个 MCP 服务器", count),
				map[string]interface{}{"synced": count},
			))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// mcpByID handles PUT/DELETE/GET /api/v1/mcp/servers/by-name.
func (s *Server) mcpByID(w http.ResponseWriter, r *http.Request) {
	serverName := r.URL.Query().Get("server_name")
	var body struct {
		ServerName string                 `json:"server_name"`
		Config     map[string]interface{} `json:"config"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.ServerName != "" {
		serverName = body.ServerName
	}
	switch r.Method {
	case http.MethodPut:
		if body.Config == nil {
			body.Config = map[string]interface{}{}
		}
		body.Config["name"] = serverName
		if err := s.mcp.upsert(serverName, body.Config); err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOKMsg("MCP server 已更新", map[string]interface{}{}))
	case http.MethodDelete:
		if err := s.mcp.delete(serverName); err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOKMsg("MCP server 已删除", map[string]interface{}{}))
	default:
		writeJSON(w, http.StatusOK, apiOK(s.mcp.get(serverName)))
	}
}

// syncModelScopeMCPServers syncs MCP servers from the ModelScope platform.
// Ported from astrbot/core/provider/func_tool_manager.py sync_modelscope_mcp_servers.
func (s *Server) syncModelScopeMCPServers(accessToken string) (int, error) {
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return 0, fmt.Errorf("缺少 ModelScope access token")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet,
		"https://www.modelscope.cn/openapi/v1/mcp/servers/operational", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("网络连接错误: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("ModelScope API 请求失败: HTTP %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var payload struct {
		Data struct {
			McpServerList []struct {
				Name            string `json:"name"`
				OperationalURLs []struct {
					URL string `json:"url"`
				} `json:"operational_urls"`
			} `json:"mcp_server_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, fmt.Errorf("解析 ModelScope 响应失败: %v", err)
	}
	synced := 0
	for _, server := range payload.Data.McpServerList {
		name := strings.TrimSpace(server.Name)
		if name == "" || len(server.OperationalURLs) == 0 {
			continue
		}
		serverURL := strings.TrimSpace(server.OperationalURLs[0].URL)
		if serverURL == "" {
			continue
		}
		cfg := map[string]interface{}{
			"url":       serverURL,
			"transport": "sse",
			"active":    true,
			"provider":  "modelscope",
			"name":      name,
		}
		if err := s.mcp.upsert(name, cfg); err != nil {
			continue
		}
		synced++
	}
	if synced == 0 {
		return 0, nil
	}
	logger.Info("Synced %d MCP server(s) from ModelScope", synced)
	return synced, nil
}

// (stdio: check command exists; sse/http: try a HEAD/GET request).
func (s *Server) testMCPServer(w http.ResponseWriter, r *http.Request, serverName string) {
	cfg := s.mcp.get(serverName)
	if len(cfg) == 0 {
		writeJSON(w, http.StatusOK, apiError("Server does not exist"))
		return
	}
	result := map[string]interface{}{
		"name":    serverName,
		"success": true,
		"error":   nil,
	}
	transport, _ := cfg["transport"].(string)
	if transport == "" {
		transport, _ = cfg["type"].(string)
	}
	switch transport {
	case "stdio":
		command, _ := cfg["command"].(string)
		if command == "" {
			result["success"] = false
			result["error"] = "MCP stdio server 缺少 command"
		}
	case "sse", "streamable_http", "http", "":
		url, _ := cfg["url"].(string)
		if url == "" {
			result["success"] = false
			result["error"] = "MCP server 缺少 url"
		} else {
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Get(url)
			if err != nil {
				result["success"] = false
				result["error"] = err.Error()
			} else {
				resp.Body.Close()
			}
		}
	default:
		result["success"] = false
		result["error"] = "不支持的 transport: " + transport
	}
	writeJSON(w, http.StatusOK, apiOK(result))
}

// ── Logs handlers ────────────────────────────────────────────

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "history":
		entries := log.GetDefault().History()
		logs := make([]interface{}, 0, len(entries))
		for _, entry := range entries {
			logs = append(logs, map[string]interface{}{
				"level":    sseLogLevel(entry.Level),
				"time":     float64(entry.Timestamp.UnixMilli()) / 1000.0,
				"data":     fmt.Sprintf("[%s] [%s] %s", entry.Timestamp.Format("2006-01-02 15:04:05.000"), sseLogLevel(entry.Level), entry.Message),
				"category": "system",
			})
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"logs": logs,
		}))
	case "live":
		s.handleLogStream(w, r)
	default:
		writeJSON(w, http.StatusOK, apiOK([]string{}))
	}
}

// handleLogStream streams log entries over SSE (GET /api/v1/logs/live).
// Ported from astrbot/dashboard/api/logs.py live_logs + LogService.stream_log_events.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	ch := log.GetDefault().Subscribe(200)
	defer log.GetDefault().Unsubscribe(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case entry, more := <-ch:
			if !more {
				return
			}
			payload := map[string]interface{}{
				"type":     "log",
				"level":    sseLogLevel(entry.Level),
				"time":     float64(entry.Timestamp.UnixMilli()) / 1000.0,
				"data":     fmt.Sprintf("[%s] [%s] %s", entry.Timestamp.Format("2006-01-02 15:04:05.000"), sseLogLevel(entry.Level), entry.Message),
				"category": "system",
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "id: %d\ndata: %s\n\n", entry.Timestamp.UnixMilli(), data)
			flusher.Flush()
		}
	}
}

// sseLogLevel maps internal log levels to the WebUI's level names.
func sseLogLevel(level log.Level) string {
	switch level {
	case log.LevelDebug:
		return "DEBUG"
	case log.LevelInfo:
		return "INFO"
	case log.LevelWarn:
		return "WARNING"
	case log.LevelError:
		return "ERROR"
	case log.LevelCritical:
		return "CRITICAL"
	default:
		return "INFO"
	}
}

// ── Backups handlers ─────────────────────────────────────────

func (s *Server) handleBackups(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"items":     []interface{}{},
			"total":     0,
			"page":      1,
			"page_size": 20,
		}))
	case "tasks":
		if len(parts) > 1 {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"status":   "pending",
				"progress": 0,
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK([]interface{}{}))
		}
	case "upload":
		if len(parts) > 1 {
			switch parts[1] {
			case "init":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"task_id": "",
				}))
			case "chunk":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "chunk uploaded",
				}))
			case "complete":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "upload complete",
				}))
			case "abort":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "upload aborted",
				}))
			default:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "upload endpoint",
				}))
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "upload endpoint",
			}))
		}
	default:
		if len(parts) > 0 {
			if len(parts) > 1 {
				switch parts[1] {
				case "check":
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"valid": true,
					}))
				case "import":
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "import started",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			} else {
				switch r.Method {
				case http.MethodPatch:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "renamed",
					}))
				case http.MethodDelete:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "deleted",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"backups": []interface{}{},
			}))
		}
	}
}

// ── Cron handlers ────────────────────────────────────────────

func (s *Server) handleCron(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "jobs", "":
		if len(parts) > 1 {
			jobID := parts[1]
			if len(parts) > 2 {
				switch parts[2] {
				case "run":
					if err := s.cronRunJob(jobID); err != nil {
						writeJSON(w, http.StatusBadRequest, apiError("执行任务失败: "+err.Error()))
						return
					}
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "job " + jobID + " executed",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			} else {
				switch r.Method {
				case http.MethodPatch:
					var body map[string]interface{}
					_ = json.NewDecoder(r.Body).Decode(&body)
					updated, errMsg := s.cronUpdateJob(jobID, body)
					if errMsg != "" {
						writeJSON(w, http.StatusNotFound, apiOK(map[string]interface{}{
							"message": errMsg,
						}))
						return
					}
					updated["message"] = "job updated"
					writeJSON(w, http.StatusOK, apiOK(updated))
				case http.MethodDelete:
					if s.cronDeleteJob(jobID) {
						writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
							"message": "job deleted",
						}))
					} else {
						writeJSON(w, http.StatusNotFound, apiOK(map[string]interface{}{
							"message": "job not found",
						}))
					}
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			if r.Method == http.MethodPost {
				var body map[string]interface{}
				_ = json.NewDecoder(r.Body).Decode(&body)
				created, errMsg := s.cronCreateJob(body)
				if errMsg != "" {
					writeJSON(w, http.StatusBadRequest, apiOK(map[string]interface{}{
						"message": errMsg,
					}))
					return
				}
				writeJSON(w, http.StatusOK, apiOK(created))
			} else {
				jobs := s.getCronJobs()
				writeJSON(w, http.StatusOK, apiOK(jobs))
			}
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "endpoint not yet implemented: " + sub,
		}))
	}
}

// ── Chat handlers ────────────────────────────────────────────

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"sessions": s.chat.listSessions(),
		}))
	case "sessions":
		s.handleChatSessions(w, r, parts[1:])
	case "threads":
		if len(parts) > 1 {
			threadID := parts[1]
			if len(parts) > 2 && parts[2] == "messages" {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "message sent to thread " + threadID,
				}))
			} else {
				switch r.Method {
				case http.MethodGet:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"id": threadID,
					}))
				case http.MethodDelete:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "thread " + threadID + " deleted",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			if r.Method == http.MethodPost {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"id": "",
				}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"threads": []interface{}{},
				}))
			}
		}
	case "runs":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"runs": []interface{}{},
		}))
	case "projects":
		if len(parts) > 1 {
			projectID := parts[1]
			if len(parts) > 2 {
				switch parts[2] {
				case "sessions":
					if len(parts) > 3 {
						sessionID := parts[3]
						if r.Method == http.MethodPost {
							writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
								"message": "session " + sessionID + " added to project " + projectID,
							}))
						} else if r.Method == http.MethodDelete {
							writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
								"message": "session " + sessionID + " removed from project " + projectID,
							}))
						} else {
							writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
						}
					} else {
						writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
							"sessions": []interface{}{},
						}))
					}
				case "workspace":
					if len(parts) > 3 {
						switch parts[3] {
						case "files":
							writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
								"path":    "",
								"entries": []interface{}{},
							}))
						case "file":
							if len(parts) > 4 && parts[4] == "download" {
								writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
									"content": "",
								}))
							} else {
								writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
									"content": "",
								}))
							}
						default:
							writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
						}
					} else {
						writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
					}
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			} else {
				switch r.Method {
				case http.MethodGet:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"id": projectID,
					}))
				case http.MethodPatch:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "project " + projectID + " updated",
					}))
				case http.MethodDelete:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "project " + projectID + " deleted",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			if r.Method == http.MethodPost {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"id": "",
				}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"projects": []interface{}{},
				}))
			}
		}
	case "send":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "message sent",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// handleChatSessions handles /api/v1/chat/sessions[...] endpoints.
func (s *Server) handleChatSessions(w http.ResponseWriter, r *http.Request, rest []string) {
	if s.chat == nil {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		return
	}
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, apiOK(s.chat.listSessions()))
		case http.MethodPost:
			platformID := r.URL.Query().Get("platform_id")
			session, err := s.chat.createSession(platformID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("创建会话失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"session_id":  session.SessionID,
				"platform_id": session.PlatformID,
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(s.chat.listSessions()))
		}
		return
	}

	sub := rest[0]
	switch sub {
	case "new":
		session, err := s.chat.createSession("")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, apiError("创建会话失败"))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"session_id":  session.SessionID,
			"platform_id": session.PlatformID,
		}))
	case "batch-delete":
		var body struct {
			SessionIDs []string `json:"session_ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		deleted := s.chat.deleteSessions(body.SessionIDs)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"deleted_count": deleted,
			"failed_count":  0,
			"failed_items":  []interface{}{},
		}))
	default:
		sessionID := sub
		if len(rest) > 1 {
			switch rest[1] {
			case "messages":
				if len(rest) > 2 {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "message updated",
					}))
				} else {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"messages": []interface{}{},
					}))
				}
				return
			case "stop":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "session stopped",
				}))
				return
			default:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				return
			}
		}
		switch r.Method {
		case http.MethodGet:
			detail := s.chat.sessionDetail(sessionID)
			if detail == nil {
				writeJSON(w, http.StatusNotFound, apiError("会话不存在"))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(detail))
		case http.MethodPatch:
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = s.chat.updateSession(sessionID, body)
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "session updated",
			}))
		case http.MethodDelete:
			s.chat.deleteSessions([]string{sessionID})
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "session deleted",
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	}
}

// ── Update handlers ──────────────────────────────────────────

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "check":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"version":        "4.27.2-go",
			"latest_version": "4.27.2-go",
			"has_update":     false,
		}))
	case "releases":
		writeJSON(w, http.StatusOK, apiOK([]interface{}{}))
	case "progress":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"progress": 0,
			"status":   "idle",
		}))
	case "do", "core", "dashboard":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "update not supported in Go version",
		}))
	case "pip-install":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "pip install not applicable in Go version",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Subagents handlers ──────────────────────────────────────

func (s *Server) handleSubagents(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "config":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "available-tools":
		writeJSON(w, http.StatusOK, apiOK([]interface{}{}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Files handlers ──────────────────────────────────────────

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"files": []interface{}{},
		}))
	case "content":
		if len(parts) > 1 {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"content": "",
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"content": "",
			}))
		}
	case "tokens":
		if len(parts) > 1 {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"tokens": []interface{}{},
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"tokens": []interface{}{},
			}))
		}
	case "upload":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "file uploaded",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Commands handlers ───────────────────────────────────────

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	// PATCH /api/v1/commands/{handler_full_name}
	if r.Method == http.MethodPatch && sub != "" && sub != "by-id" {
		s.updateCommand(w, r, sub)
		return
	}
	switch sub {
	case "", "list":
		items := s.listCommandDescriptors()
		total := len(items)
		disabled := 0
		conflicts := 0
		for _, item := range items {
			if enabled, ok := item["enabled"].(bool); ok && !enabled {
				disabled++
			}
			if hasConflict, ok := item["has_conflict"].(bool); ok && hasConflict {
				conflicts++
			}
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"items": items,
			"summary": map[string]interface{}{
				"total":     total,
				"disabled":  disabled,
				"conflicts": conflicts,
			},
			"wake_prefix": s.getWakePrefix(),
		}))
	case "conflicts":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"conflicts": []interface{}{},
		}))
	case "by-id":
		if r.Method == http.MethodPatch {
			commandID := r.URL.Query().Get("command_id")
			if commandID == "" {
				commandID = r.URL.Query().Get("handler_full_name")
			}
			s.updateCommand(w, r, commandID)
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// getWakePrefix returns the current wake prefix list.
func (s *Server) getWakePrefix() []interface{} {
	cfg := s.getConfigSnapshot()
	if wp, ok := cfg["wake_prefix"].([]interface{}); ok {
		return wp
	}
	return []interface{}{"/"}
}

// listCommandDescriptors collects command descriptors for the management UI.
func (s *Server) listCommandDescriptors() []map[string]interface{} {
	sm, ok := s.starMgr.(*star.Manager)
	if !ok || sm == nil {
		return []map[string]interface{}{}
	}
	descriptors := star.CollectCommandDescriptors(sm.Handlers())
	// Apply persisted configs (enabled / renamed command / permission)
	cfg := s.getConfigSnapshot()
	records, _ := cfg["command_configs"].(map[string]interface{})

	result := make([]map[string]interface{}, 0, len(descriptors))
	for _, d := range descriptors {
		item := descriptorToDict(d)
		if records != nil {
			if rec, ok := records[d.HandlerFullName].(map[string]interface{}); ok {
				if enabled, ok := rec["enabled"].(bool); ok {
					item["enabled"] = enabled
				}
				if cmd, ok := rec["effective_command"].(string); ok && cmd != "" {
					item["effective_command"] = cmd
				}
				if perm, ok := rec["permission"].(string); ok && perm != "" {
					item["permission"] = perm
				}
			}
		}
		result = append(result, item)
	}
	return result
}

// descriptorToDict converts a star descriptor to the dashboard dict format.
func descriptorToDict(d *star.CommandDescriptor) map[string]interface{} {
	aliases := d.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	return map[string]interface{}{
		"handler_full_name":   d.HandlerFullName,
		"handler_name":        d.HandlerName,
		"plugin":              d.PluginName,
		"plugin_display_name": d.PluginName,
		"module_path":         d.ModulePath,
		"description":         d.Description,
		"type":                d.CommandType,
		"parent_signature":    d.ParentSignature,
		"original_command":    d.OriginalCommand,
		"current_fragment":    d.CurrentFragment,
		"effective_command":   d.EffectiveCommand,
		"aliases":             aliases,
		"permission":          d.Permission,
		"enabled":             d.Enabled,
		"is_group":            d.IsGroup,
		"has_conflict":        d.HasConflict,
		"reserved":            d.Reserved,
		"sub_commands":        []interface{}{},
	}
}

// updateCommand handles PATCH /api/v1/commands/{id} for enabled/alias/permission.
func (s *Server) updateCommand(w http.ResponseWriter, r *http.Request, handlerFullName string) {
	var body struct {
		Enabled         *bool    `json:"enabled"`
		Alias           *string  `json:"alias"`
		Aliases         []string `json:"aliases"`
		PermissionGroup *string  `json:"permission_group"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	sm, ok := s.starMgr.(*star.Manager)
	if !ok || sm == nil {
		writeJSON(w, http.StatusOK, apiError("指令管理器不可用"))
		return
	}
	handler := sm.Handlers().Get(handlerFullName)
	if handler == nil {
		writeJSON(w, http.StatusOK, apiError("指定的处理函数不存在或不是指令"))
		return
	}

	// Load persisted config
	cfg := s.getConfigSnapshot()
	records, _ := cfg["command_configs"].(map[string]interface{})
	if records == nil {
		records = map[string]interface{}{}
	}
	rec, _ := records[handlerFullName].(map[string]interface{})
	if rec == nil {
		rec = map[string]interface{}{}
	}

	if body.Enabled != nil {
		handler.Enabled = *body.Enabled
		rec["enabled"] = *body.Enabled
	}
	if body.Alias != nil && strings.TrimSpace(*body.Alias) != "" {
		desc, err := star.RenameCommand(sm.Handlers(), handlerFullName, *body.Alias)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		rec["effective_command"] = desc.EffectiveCommand
	}
	if body.PermissionGroup != nil {
		perm := strings.TrimSpace(*body.PermissionGroup)
		if perm != "admin" && perm != "member" {
			writeJSON(w, http.StatusOK, apiError("权限类型必须为 admin 或 member"))
			return
		}
		star.SetHandlerPermission(handler, perm)
		rec["permission"] = perm
	}
	records[handlerFullName] = rec
	_ = s.setConfigData("command_configs", records)

	writeJSON(w, http.StatusOK, apiOK(s.findCommandPayload(sm, handlerFullName)))
}

// findCommandPayload returns the updated descriptor dict for a command.
func (s *Server) findCommandPayload(sm *star.Manager, handlerFullName string) map[string]interface{} {
	for _, d := range star.CollectCommandDescriptors(sm.Handlers()) {
		if d.HandlerFullName == handlerFullName {
			return descriptorToDict(d)
		}
	}
	return map[string]interface{}{}
}

// ── System handlers ──────────────────────────────────────────

func (s *Server) handleProviderSources(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		if r.Method == http.MethodPost {
			var body struct {
				Config map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if err := s.upsertProviderSource(body.Config); err != nil {
				writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "更新 provider source 成功",
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"provider_sources": s.getProviderSources(),
			}))
		}
	case "by-id":
		sourceID := r.URL.Query().Get("source_id")
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"provider_source": s.getProviderSourceByID(sourceID),
			}))
		case http.MethodPut:
			var body struct {
				SourceID string                 `json:"source_id"`
				Config   map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.SourceID != "" {
				sourceID = body.SourceID
			}
			if body.Config == nil {
				body.Config = map[string]interface{}{}
			}
			if id, _ := body.Config["id"].(string); id == "" {
				body.Config["id"] = sourceID
			}
			if err := s.upsertProviderSource(body.Config); err != nil {
				writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "更新 provider source 成功",
			}))
		case http.MethodDelete:
			if err := s.deleteProviderSource(sourceID); err != nil {
				writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "删除 provider source 成功",
			}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "models":
		sourceID := r.URL.Query().Get("source_id")
		models, metadata, err := s.fetchProviderSourceModels(sourceID)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"models":             models,
			"provider_source_id": sourceID,
			"model_metadata":     metadata,
		}))
	case "providers":
		if r.Method == http.MethodPost {
			var body struct {
				SourceID string                 `json:"source_id"`
				Config   map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Config == nil {
				body.Config = map[string]interface{}{}
			}
			if body.SourceID != "" {
				body.Config["provider_source_id"] = body.SourceID
				// Inherit type/provider from the source if missing
				source := s.getProviderSourceByID(body.SourceID)
				if _, ok := body.Config["type"]; !ok || body.Config["type"] == "" {
					if t, ok := source["type"].(string); ok && t != "" {
						body.Config["type"] = t
					}
				}
				if _, ok := body.Config["provider"]; !ok || body.Config["provider"] == "" {
					if p, ok := source["provider"].(string); ok && p != "" {
						body.Config["provider"] = p
					}
				}
			}
			if err := s.upsertProvider(body.Config); err != nil {
				writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "保存成功",
			}))
		} else {
			sourceID := r.URL.Query().Get("source_id")
			cfg := s.getConfigSnapshot()
			providers, _ := cfg["provider"].([]interface{})
			filtered := make([]interface{}, 0)
			for _, p := range providers {
				if m, ok := p.(map[string]interface{}); ok {
					if sid, _ := m["provider_source_id"].(string); sourceID == "" || sid == sourceID {
						filtered = append(filtered, p)
					}
				}
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"providers":      filtered,
				"model_metadata": map[string]interface{}{},
			}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

func (s *Server) handleBotTypes(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	// /api/v1/bot-types/{bot_type}/registration
	if len(parts) > 1 && parts[1] == "registration" && r.Method == http.MethodPost {
		s.handleBotRegistration(w, r, parts[0])
		return
	}
	switch sub {
	case "", "list":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"bot_types": s.listBotTypes(),
		}))
	case "by-id":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "registration":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "bot registration created",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// listBotTypes returns the supported platform types (mirrors Python's
// BotConfigService.list_bot_types / platform_registry). Proactive message
// support drives the future-task delivery dialog in the WebUI.
func (s *Server) listBotTypes() []interface{} {
	return []interface{}{
		map[string]interface{}{
			"type":                      "qq_official",
			"id":                        "qq_official",
			"description":               "QQ 官方机器人（Websocket）适配器",
			"display_name":              "QQ 官方机器人",
			"support_streaming_message": true,
			"support_proactive_message": true,
			"default_config": map[string]interface{}{
				"id": "default", "type": "qq_official", "enable": true,
				"appid": "", "secret": "",
				"enable_group_c2c": true, "enable_guild_direct_message": true,
			},
		},
		map[string]interface{}{
			"type":                      "aiocqhttp",
			"id":                        "aiocqhttp",
			"description":               "OneBot v11 适配器（反向 WebSocket）",
			"display_name":              "OneBot v11",
			"support_streaming_message": false,
			"support_proactive_message": true,
			"default_config": map[string]interface{}{
				"id": "default", "type": "aiocqhttp", "enable": true,
				"ws_reverse_host": "0.0.0.0", "ws_reverse_port": 6199, "ws_reverse_token": "",
			},
		},
		map[string]interface{}{
			"type":                      "webchat",
			"id":                        "webchat",
			"description":               "内置 WebChat 适配器",
			"display_name":              "WebChat",
			"support_streaming_message": true,
			"support_proactive_message": true,
			"default_config": map[string]interface{}{
				"id": "default", "type": "webchat", "enable": false,
			},
		},
		map[string]interface{}{
			"type":                      "telegram",
			"id":                        "telegram",
			"description":               "Telegram Bot 适配器",
			"display_name":              "Telegram",
			"support_streaming_message": true,
			"support_proactive_message": true,
			"default_config": map[string]interface{}{
				"id": "default", "type": "telegram", "enable": true,
				"telegram_token": "your_bot_token",
			},
		},
	}
}

// handleBotRegistration handles POST /api/v1/bot-types/{bot_type}/registration.
// Ported from astrbot/dashboard/services/platform_service.py handle_platform_registration.
func (s *Server) handleBotRegistration(w http.ResponseWriter, r *http.Request, botType string) {
	var body struct {
		Action           string                 `json:"action"`
		PlatformConfig   map[string]interface{} `json:"platform_config"`
		RegistrationCode string                 `json:"registration_code"`
		TaskID           string                 `json:"task_id"`
		BindKey          string                 `json:"bind_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.PlatformConfig == nil {
		body.PlatformConfig = map[string]interface{}{}
	}
	action := strings.ToLower(strings.TrimSpace(body.Action))

	switch botType {
	case "qq_official", "qq_official_webhook":
		result, err := s.qqOfficialRegistration(action, body.PlatformConfig, body.TaskID, body.BindKey)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(result))
	case "lark", "weixin_oc", "dingtalk":
		writeJSON(w, http.StatusOK, apiError(botType+" 一键注册尚未实现"))
	default:
		writeJSON(w, http.StatusOK, apiError("Unsupported platform registration: "+botType))
	}
}

func (s *Server) handlePluginSources(w http.ResponseWriter, r *http.Request, parts []string) {
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"sources": []interface{}{},
	}))
}

func (s *Server) handleConfigProfiles(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		if r.Method == http.MethodPost {
			var body struct {
				Name   string                 `json:"name"`
				Config map[string]interface{} `json:"config"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			confID := "default"
			if body.Config != nil {
				_ = s.setConfigDataAll(body.Config)
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"conf_id": confID,
			}))
		} else {
			now := time.Now().Format("2006-01-02T15:04:05.000Z")
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"info_list": []map[string]interface{}{
					{
						"id":         "default",
						"name":       "default",
						"updated_at": now,
					},
				},
			}))
		}
	case "schema":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"config_schema": map[string]interface{}{
				"platform": map[string]interface{}{
					"description":     "消息平台适配器",
					"type":            "list",
					"config_template": map[string]interface{}{},
				},
			},
		}))
	default:
		if len(parts) > 0 {
			profileID := parts[0]
			switch r.Method {
			case http.MethodGet:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"id":       profileID,
					"name":     profileID,
					"config":   s.getConfigSnapshot(),
					"metadata": s.getProfileMetadata(),
				}))
			case http.MethodPut, http.MethodPatch:
				// Frontend sends the config object directly (DynamicConfig).
				var raw json.RawMessage
				_ = json.NewDecoder(r.Body).Decode(&raw)
				var direct map[string]interface{}
				if err := json.Unmarshal(raw, &direct); err == nil && direct != nil {
					if inner, ok := direct["config"].(map[string]interface{}); ok && len(direct) == 1 {
						direct = inner
					}
					if err := s.setConfigDataAll(direct); err != nil {
						writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
						return
					}
				}
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "profile " + profileID + " updated",
				}))
			case http.MethodDelete:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "profile " + profileID + " deleted",
				}))
			default:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	}
}

func (s *Server) handleConfigRoutes(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		if r.Method == http.MethodPut {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "config routes updated",
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"routing": map[string]interface{}{},
			}))
		}
	case "by-umo":
		if len(parts) > 1 {
			umo := parts[1]
			if r.Method == http.MethodPut {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"umo":     umo,
					"message": "route updated",
				}))
			} else if r.Method == http.MethodDelete {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "route deleted",
				}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

func (s *Server) handleAPIKeys(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		if r.Method == http.MethodPost {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"key": map[string]interface{}{
					"id":         "",
					"name":       "",
					"key":        "",
					"created_at": "",
				},
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"keys": []interface{}{},
			}))
		}
	case "by-id":
		if len(parts) > 1 {
			keyID := parts[1]
			if len(parts) > 2 && parts[2] == "revoke" {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "API key " + keyID + " revoked",
				}))
			} else {
				switch r.Method {
				case http.MethodDelete:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "API key " + keyID + " deleted",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

func (s *Server) handleT2I(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "templates":
		if len(parts) > 1 {
			templateName := parts[1]
			if templateName == "active" {
				if r.Method == http.MethodPut {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "active template updated",
					}))
				} else {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"name": "",
					}))
				}
			} else if templateName == "default" && len(parts) > 2 && parts[2] == "reset" {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "default template reset",
				}))
			} else {
				switch r.Method {
				case http.MethodGet:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"name": templateName,
					}))
				case http.MethodPut:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "template " + templateName + " updated",
					}))
				case http.MethodDelete:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "template " + templateName + " deleted",
					}))
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			if r.Method == http.MethodPost {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"name": "",
				}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"templates": []interface{}{},
				}))
			}
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		skills := s.getSkillList()
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"skills": skills,
		}))
	case "batch":
		s.uploadSkillsBatch(w, r)
	case "by-name":
		skillName := r.URL.Query().Get("skill_name")
		if r.Method == http.MethodPatch {
			var body struct {
				SkillName string `json:"skill_name"`
				Enabled   *bool  `json:"enabled"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.SkillName != "" {
				skillName = body.SkillName
			}
			if body.Enabled != nil {
				if err := s.skillSetActive(skillName, *body.Enabled); err != nil {
					writeJSON(w, http.StatusOK, apiError(err.Error()))
					return
				}
				writeJSON(w, http.StatusOK, apiOKMsg("技能状态已更新", map[string]interface{}{}))
			} else {
				writeJSON(w, http.StatusOK, apiOKMsg("技能已更新", map[string]interface{}{}))
			}
		} else if r.Method == http.MethodDelete {
			if err := s.skillDelete(skillName); err != nil {
				writeJSON(w, http.StatusOK, apiError(err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("技能已删除", map[string]interface{}{}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "files":
		s.listSkillFiles(w, r)
	case "file":
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			s.updateSkillFile(w, r)
		} else {
			s.getSkillFile(w, r)
		}
	case "archive":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "skill archive download not implemented",
		}))
	case "neo":
		if len(parts) > 1 {
			switch parts[1] {
			case "candidates":
				if len(parts) > 2 && parts[2] == "delete" {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "candidate deleted",
					}))
				} else {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"candidates": []interface{}{},
					}))
				}
			case "evaluate":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "skill evaluated",
				}))
			case "promote":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "skill promoted",
				}))
			case "releases":
				if len(parts) > 2 && parts[2] == "delete" {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"message": "release deleted",
					}))
				} else {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
						"releases": []interface{}{},
					}))
				}
			case "rollback":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "skill rolled back",
				}))
			case "sync":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"message": "skills synced",
				}))
			case "payload":
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"payload": "",
				}))
			default:
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// skillFileEditable reports whether a skill file may be edited in the WebUI.
func skillFileEditable(name string) bool {
	editableSuffixes := map[string]bool{
		".css": true, ".html": true, ".ini": true, ".js": true,
		".json": true, ".md": true, ".py": true, ".sh": true,
		".toml": true, ".ts": true, ".txt": true, ".yaml": true, ".yml": true,
	}
	editableNames := map[string]bool{"Dockerfile": true, "Makefile": true}
	if editableNames[name] {
		return true
	}
	ext := strings.ToLower(filepath.Ext(name))
	return editableSuffixes[ext]
}

// skillFilePath resolves a file path inside data/skills/<name>, guarding
// against path traversal.
func skillFilePath(skillName, relPath string) (string, error) {
	root, err := filepath.Abs(filepath.Join("data", "skills", skillName))
	if err != nil {
		return "", err
	}
	target := filepath.Join(root, filepath.Clean("/"+relPath))
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(abs, root+string(os.PathSeparator)) && abs != root {
		return "", fmt.Errorf("非法路径")
	}
	return abs, nil
}

// listSkillFiles implements GET /api/v1/skills/files.
func (s *Server) listSkillFiles(w http.ResponseWriter, r *http.Request) {
	skillName := r.URL.Query().Get("skill_name")
	relPath := r.URL.Query().Get("path")
	target, err := skillFilePath(skillName, relPath)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError(err.Error()))
		return
	}
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		writeJSON(w, http.StatusOK, apiError("目录不存在"))
		return
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError(err.Error()))
		return
	}
	result := []interface{}{}
	for _, e := range entries {
		epath := filepath.Join(target, e.Name())
		estat, _ := os.Stat(epath)
		isDir := e.IsDir()
		size := int64(0)
		editable := false
		if !isDir && estat != nil {
			size = estat.Size()
			editable = skillFileEditable(e.Name()) && size <= 512*1024
		}
		result = append(result, map[string]interface{}{
			"name":     e.Name(),
			"path":     filepath.Join(relPath, e.Name()),
			"type":     map[bool]string{true: "directory"}[isDir] + map[bool]string{false: "file"}[isDir],
			"size":     size,
			"editable": editable,
		})
	}
	// directories first, then by name
	sort.SliceStable(result, func(i, j int) bool {
		a := result[i].(map[string]interface{})
		b := result[j].(map[string]interface{})
		at, _ := a["type"].(string)
		bt, _ := b["type"].(string)
		if at != bt {
			return at == "directory"
		}
		return a["name"].(string) < b["name"].(string)
	})
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"name":    skillName,
		"path":    relPath,
		"entries": result,
	}))
}

// getSkillFile implements GET /api/v1/skills/file.
func (s *Server) getSkillFile(w http.ResponseWriter, r *http.Request) {
	skillName := r.URL.Query().Get("skill_name")
	relPath := r.URL.Query().Get("path")
	if relPath == "" {
		relPath = "SKILL.md"
	}
	target, err := skillFilePath(skillName, relPath)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError(err.Error()))
		return
	}
	info, err := os.Stat(target)
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusOK, apiError("文件不存在"))
		return
	}
	if !skillFileEditable(filepath.Base(target)) {
		writeJSON(w, http.StatusOK, apiError("Unsupported file type"))
		return
	}
	if info.Size() > 512*1024 {
		writeJSON(w, http.StatusOK, apiError("File is too large"))
		return
	}
	content, err := os.ReadFile(target)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"name":     skillName,
		"path":     relPath,
		"content":  string(content),
		"size":     info.Size(),
		"editable": true,
	}))
}

// updateSkillFile implements POST /api/v1/skills/file.
func (s *Server) updateSkillFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SkillName string `json:"skill_name"`
		Path      string `json:"path"`
		Content   string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Path == "" {
		body.Path = "SKILL.md"
	}
	if len([]byte(body.Content)) > 512*1024 {
		writeJSON(w, http.StatusOK, apiError("File content is too large"))
		return
	}
	target, err := skillFilePath(body.SkillName, body.Path)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError(err.Error()))
		return
	}
	if !skillFileEditable(filepath.Base(target)) {
		writeJSON(w, http.StatusOK, apiError("Unsupported file type"))
		return
	}
	if err := os.WriteFile(target, []byte(body.Content), 0644); err != nil {
		writeJSON(w, http.StatusOK, apiError(err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, apiOKMsg("文件已保存", map[string]interface{}{}))
}

// uploadSkillsBatch handles POST /api/v1/skills/batch (multipart "files").
// Each file is a .zip skill package containing a SKILL.md; it is extracted
// into data/skills/<name>/ (mirrors astrbot skills_service.batch_upload_skills).
func (s *Server) uploadSkillsBatch(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusOK, apiError("解析上传失败: "+err.Error()))
		return
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		writeJSON(w, http.StatusOK, apiError("No files provided"))
		return
	}

	succeeded := []interface{}{}
	failed := []interface{}{}
	skipped := []interface{}{}
	skillsRoot := "data/skills"

	for _, fh := range files {
		filename := filepath.Base(fh.Filename)
		if !strings.HasSuffix(strings.ToLower(filename), ".zip") {
			failed = append(failed, map[string]interface{}{"filename": filename, "error": "Only .zip files are supported"})
			continue
		}
		src, err := fh.Open()
		if err != nil {
			failed = append(failed, map[string]interface{}{"filename": filename, "error": err.Error()})
			continue
		}
		skillName, installErr := installSkillFromZip(src, fh.Size, skillsRoot, strings.TrimSuffix(filename, ".zip"))
		src.Close()
		if installErr != nil {
			if strings.Contains(installErr.Error(), "already exists") {
				skipped = append(skipped, map[string]interface{}{
					"filename": filename,
					"name":     strings.TrimSuffix(filename, ".zip"),
					"error":    "Skill already exists.",
				})
			} else {
				failed = append(failed, map[string]interface{}{"filename": filename, "error": installErr.Error()})
			}
			continue
		}
		succeeded = append(succeeded, map[string]interface{}{"filename": filename, "name": skillName})
	}

	writeJSON(w, http.StatusOK, apiOKMsg("上传成功", map[string]interface{}{
		"succeeded": succeeded,
		"failed":    failed,
		"skipped":   skipped,
	}))
}

// installSkillFromZip extracts a skill zip into skillsRoot/<name>/.
// The zip must contain a SKILL.md (either at the top level or under one
// subdirectory). Returns the installed skill name.
func installSkillFromZip(src io.ReaderAt, size int64, skillsRoot, nameHint string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "skill-upload-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	zr, err := zip.NewReader(src, size)
	if err != nil {
		return "", fmt.Errorf("invalid zip: %v", err)
	}
	for _, f := range zr.File {
		dest := filepath.Join(tmpDir, filepath.Clean(f.Name))
		if !strings.HasPrefix(dest, tmpDir+string(os.PathSeparator)) {
			return "", fmt.Errorf("illegal path in zip: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			_ = os.MkdirAll(dest, 0755)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(dest), 0755)
		rc, err := f.Open()
		if err != nil {
			return "", err
		}
		out, err := os.Create(dest)
		if err != nil {
			rc.Close()
			return "", err
		}
		_, _ = io.Copy(out, rc)
		out.Close()
		rc.Close()
	}

	// Locate SKILL.md
	skillDir := ""
	err = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.EqualFold(info.Name(), "SKILL.md") {
			skillDir = filepath.Dir(path)
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if skillDir == "" {
		return "", fmt.Errorf("zip 中未找到 SKILL.md")
	}

	// Skill name: prefer SKILL.md frontmatter "name", then the containing
	// directory (when SKILL.md sits in a sub-folder), then the zip file name.
	name := skillFrontmatterName(filepath.Join(skillDir, "SKILL.md"))
	if name == "" {
		name = filepath.Base(skillDir)
	}
	if name == "." || name == string(os.PathSeparator) || name == "" {
		name = nameHint
	}
	if name == "" {
		return "", fmt.Errorf("无法确定技能名称")
	}
	name = sanitizeSkillDirName(name)

	dest := filepath.Join(skillsRoot, name)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("Skill already exists")
	}
	_ = os.MkdirAll(skillsRoot, 0755)
	if err := os.Rename(skillDir, dest); err != nil {
		if err2 := copyDir(skillDir, dest); err2 != nil {
			return "", err2
		}
	}
	return name, nil
}

// copyDir recursively copies a directory.
func copyDir(src, dst string) error {
	_ = os.MkdirAll(dst, 0755)
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "list":
		page := 1
		pageSize := 20
		if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 0 {
			page = p
		}
		if ps, err := strconv.Atoi(r.URL.Query().Get("page_size")); err == nil && ps > 0 {
			pageSize = ps
		}
		if pageSize > 100 {
			pageSize = 100
		}
		all := s.getConversationList()
		total := len(all)
		totalPages := (total + pageSize - 1) / pageSize
		if totalPages < 1 {
			totalPages = 1
		}
		start := (page - 1) * pageSize
		end := start + pageSize
		if start > total {
			start = total
		}
		if end > total {
			end = total
		}
		var items []interface{}
		if start <= total {
			items = all[start:end]
		} else {
			items = []interface{}{}
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"conversations": items,
			"pagination": map[string]interface{}{
				"page":        page,
				"page_size":   pageSize,
				"total":       total,
				"total_pages": totalPages,
			},
		}))
	case "by-id":
		if len(parts) > 1 {
			convID := parts[1]
			if len(parts) > 2 && parts[2] == "messages" {
				if r.Method == http.MethodPut {
					var body struct {
						History []map[string]interface{} `json:"history"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if s.conversationUpdateByCID(convID, map[string]interface{}{"history": body.History}) {
						writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
							"message": "conversation " + convID + " messages updated",
						}))
					} else {
						writeJSON(w, http.StatusNotFound, apiOK(map[string]interface{}{
							"message": "conversation not found",
						}))
					}
				} else {
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			} else {
				switch r.Method {
				case http.MethodGet:
					detail := s.getConversationDetail(convID)
					if detail == nil {
						writeJSON(w, http.StatusNotFound, apiOK(map[string]interface{}{
							"message": "conversation not found",
						}))
						return
					}
					writeJSON(w, http.StatusOK, apiOK(detail))
				case http.MethodPatch:
					var body map[string]interface{}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if s.conversationUpdateByCID(convID, body) {
						writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
							"message": "conversation " + convID + " updated",
						}))
					} else {
						writeJSON(w, http.StatusNotFound, apiOK(map[string]interface{}{
							"message": "conversation not found",
						}))
					}
				case http.MethodDelete:
					if s.conversationDeleteByCID(convID) {
						writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
							"message": "conversation " + convID + " deleted",
						}))
					} else {
						writeJSON(w, http.StatusNotFound, apiOK(map[string]interface{}{
							"message": "conversation not found",
						}))
					}
				default:
					writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
				}
			}
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	case "batch-delete":
		var body struct {
			ConversationIDs []string `json:"conversation_ids"`
			UserID          string   `json:"user_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		deleted := 0
		for _, id := range body.ConversationIDs {
			if s.conversationDeleteByCID(id) {
				deleted++
			}
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"deleted": deleted,
		}))
	case "export":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"data": []interface{}{},
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── Trace handlers ──────────────────────────────────────────

func (s *Server) handleTrace(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "", "settings":
		if r.Method == http.MethodPut {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"settings": map[string]interface{}{
					"enabled": false,
					"level":   "info",
				},
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"settings": map[string]interface{}{
					"enabled": false,
					"level":   "info",
				},
			}))
		}
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// ── System handlers ──────────────────────────────────────────

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request, parts []string) {
	sub := ""
	if len(parts) > 0 {
		sub = parts[0]
	}
	switch sub {
	case "restart":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "restart not supported in Go version",
		}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// skillFrontmatterName extracts the "name" field from a SKILL.md frontmatter.
func skillFrontmatterName(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	inFrontmatter := false
	for _, raw := range lines {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "---") {
			if !inFrontmatter {
				inFrontmatter = true
				continue
			}
			break
		}
		if inFrontmatter && strings.HasPrefix(line, "name:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		}
	}
	return ""
}

// sanitizeSkillDirName keeps skill directory names filesystem-safe.
func sanitizeSkillDirName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.', r > 127:
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}
