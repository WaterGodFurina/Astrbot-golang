// Package dashboard - API handler implementations.
// Ported from astrbot/dashboard/api/ route modules.
package dashboard

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/conversation"
	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
	"github.com/WaterGodFurina/Astrbot-golang/internal/knowledgebase"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
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
		redactDashboardSecrets(cfg)
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
		// Preserve persisted dashboard auth fields (pbkdf2_password,
		// jwt_secret, ...) so a WebUI save round-trip does not drop them.
		// The password hash, plaintext password and JWT signing secret are
		// intentionally excluded: the client never needs them, and they are
		// re-asserted by injectAuthFields on save.
		dash := map[string]interface{}{}
		if pd, ok := persisted["dashboard"].(map[string]interface{}); ok {
			for k, v := range pd {
				switch k {
				case "password", "pbkdf2_password", "jwt_secret":
					continue
				}
				dash[k] = v
			}
		}
		dash["username"] = s.auth.Username()
		dash["host"] = "0.0.0.0"
		dash["port"] = s.port
		cfg["dashboard"] = dash
	}
	return cfg
}

// redactDashboardSecrets strips the dashboard auth secrets (plaintext password,
// PBKDF2 hash, JWT signing secret) from a config map before it is returned to
// the client. The client never needs them; on save, injectAuthFields re-asserts
// them from the password manager so round-trips are unaffected.
func redactDashboardSecrets(cfg map[string]interface{}) {
	dash, ok := cfg["dashboard"].(map[string]interface{})
	if !ok {
		return
	}
	delete(dash, "password")
	delete(dash, "pbkdf2_password")
	delete(dash, "jwt_secret")
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
func (s *Server) getSystemMetadata() *config.OrderedJSON {
	metadata := metadataFromJSON()
	for _, k := range metadata.Keys() {
		if k != "system_group" {
			metadata.Delete(k)
		}
	}
	return metadata
}

// getProfileMetadata returns the metadata for the config-profiles page.
// Ported from astrbot/dashboard/services/config_service.py get_profile_schema
// (CONFIG_METADATA_3: ai/platform/plugin/ext groups + platform adapter templates).
func (s *Server) getProfileMetadata() *config.OrderedJSON {
	metadata := metadataFromJSON()
	for _, k := range metadata.Keys() {
		if k != "ai_group" && k != "platform_group" && k != "plugin_group" && k != "ext_group" {
			metadata.Delete(k)
		}
	}
	s.injectPlatformSection(metadata)
	return metadata
}

// getConfigMetadata returns the full metadata for system-config/runtime.
func (s *Server) getConfigMetadata() *config.OrderedJSON {
	metadata := metadataFromJSON()
	s.injectPlatformSection(metadata)
	providerGroup := config.NewOrderedJSON()
	providerGroup.Set("name", "provider_group.name")
	providerSettings := config.NewOrderedJSON()
	providerSettings.Set("description", "provider_group.provider_settings.description")
	providerSettings.Set("type", "object")
	settingsItems := config.NewOrderedJSON()
	for _, item := range []struct {
		key   string
		typ   string
		desc  string
		extra map[string]interface{}
	}{
		{"provider_settings.enable", "bool", "provider_group.provider_settings.enable.description", nil},
		{"provider_settings.default_provider_id", "string", "provider_group.provider_settings.default_provider_id.description", nil},
		{"provider_settings.wake_prefix", "string", "provider_group.provider_settings.wake_prefix.description", nil},
		{"provider_settings.prompt_prefix", "string", "provider_group.provider_settings.prompt_prefix.description", nil},
		{"provider_settings.identifier", "bool", "provider_group.provider_settings.identifier.description", nil},
		{"provider_settings.display_reasoning_text", "bool", "provider_group.provider_settings.display_reasoning_text.description", nil},
		{"provider_settings.max_context_length", "int", "provider_group.provider_settings.max_context_length.description", nil},
		{"provider_settings.dequeue_context_length", "int", "provider_group.provider_settings.dequeue_context_length.description", nil},
		{"provider_settings.request_max_retries", "int", "provider_group.provider_settings.request_max_retries.description", nil},
		{"provider_settings.web_search", "bool", "provider_group.provider_settings.web_search.description", nil},
		{"provider_settings.streaming_response", "bool", "provider_group.provider_settings.streaming_response.description", nil},
	} {
		field := config.NewOrderedJSON()
		field.Set("description", item.desc)
		field.Set("type", item.typ)
		for k, v := range item.extra {
			field.Set(k, v)
		}
		settingsItems.Set(item.key, field)
	}
	providerSettings.Set("items", settingsItems)
	providerSection := config.NewOrderedJSON()
	providerSection.Set("description", "大语言模型提供方")
	providerSection.Set("type", "list")
	providerSection.Set("config_template", s.getProviderTemplates())
	providerSection.Set("items", s.getProviderItems())
	metadataGroup := config.NewOrderedJSON()
	metadataGroup.Set("provider_settings", providerSettings)
	metadataGroup.Set("provider", providerSection)
	providerGroup.Set("metadata", metadataGroup)
	metadata.Set("provider_group", providerGroup)
	return metadata
}

// injectPlatformSection adds the Go-supported platform adapter templates to the
// platform_group metadata (mirrors Python's platform_registry injection).
func (s *Server) injectPlatformSection(metadata *config.OrderedJSON) {
	platformSection := map[string]interface{}{
		"description": "消息平台适配器",
		"type":        "list",
		"config_template": map[string]interface{}{
			"QQ 官方机器人(Websocket, 推荐)": map[string]interface{}{
				"id": "default", "type": "qq_official", "enable": true,
				"appid": "", "secret": "",
				"enable_group_c2c": true, "enable_guild_direct_message": true,
			},
			"QQ 官方机器人(Webhook)": map[string]interface{}{
				"id": "default", "type": "qq_official_webhook", "enable": true,
				"appid": "", "secret": "",
				"is_sandbox": false, "unified_webhook_mode": true,
				"webhook_uuid": "", "callback_server_host": "0.0.0.0", "port": 6196,
			},
			"OneBot v11": map[string]interface{}{
				"id": "default", "type": "aiocqhttp", "enable": true,
				"ws_reverse_host": "0.0.0.0", "ws_reverse_port": 6199, "ws_reverse_token": "",
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
			"飞书(Lark)": map[string]interface{}{
				"id": "lark", "type": "lark", "enable": true,
				"app_id": "", "app_secret": "",
				"domain":                  "https://open.feishu.cn",
				"lark_connection_mode":    "socket",
				"webhook_uuid":            "",
				"lark_encrypt_key":        "",
				"lark_verification_token": "",
			},
			"Discord": map[string]interface{}{
				"id": "discord", "type": "discord", "enable": true,
				"discord_token":              "",
				"discord_proxy":              "",
				"discord_command_register":   true,
				"discord_activity_name":      "",
				"discord_allow_bot_messages": false,
			},
			"KOOK": map[string]interface{}{
				"id": "kook", "type": "kook", "enable": true,
				"kook_bot_token":                "",
				"kook_reconnect_delay":          1,
				"kook_max_reconnect_delay":      60,
				"kook_max_retry_delay":          60,
				"kook_heartbeat_interval":       30,
				"kook_heartbeat_timeout":        6,
				"kook_max_heartbeat_failures":   3,
				"kook_max_consecutive_failures": 5,
			},
			"钉钉(DingTalk)": map[string]interface{}{
				"id": "dingtalk", "type": "dingtalk", "enable": true,
				"client_id":        "",
				"client_secret":    "",
				"card_template_id": "",
			},
			"微信开放平台(Weixin OC)": map[string]interface{}{
				"id": "weixin_oc", "type": "weixin_oc", "enable": true,
				"weixin_oc_base_url":              "https://ilinkai.weixin.qq.com",
				"weixin_oc_cdn_base_url":          "https://cdn.wx.qq.com",
				"weixin_oc_bot_type":              "3",
				"weixin_oc_qr_poll_interval":      2,
				"weixin_oc_long_poll_timeout_ms":  35000,
				"weixin_oc_api_timeout_ms":        120000,
			},
			"微信公众号": map[string]interface{}{
				"id": "weixin_official_account", "type": "weixin_official_account", "enable": true,
				"appid":               "",
				"secret":              "",
				"token":               "",
				"encoding_aes_key":    "",
				"api_base_url":        "https://api.weixin.qq.com/cgi-bin/",
				"unified_webhook_mode": true,
				"webhook_uuid":        "",
				"callback_server_host": "0.0.0.0",
				"port":                6194,
				"active_send_mode":    false,
			},
			"Satori": map[string]interface{}{
				"id": "satori", "type": "satori", "enable": true,
				"satori_api_base_url":       "http://localhost:5140/satori/v1",
				"satori_endpoint":           "ws://localhost:5140/satori/v1/events",
				"satori_token":              "",
				"satori_auto_reconnect":     true,
				"satori_heartbeat_interval": 10,
				"satori_reconnect_delay":    5,
			},
			"Line": map[string]interface{}{
				"id": "line", "type": "line", "enable": true,
				"channel_access_token": "",
				"channel_secret":       "",
				"unified_webhook_mode": true,
				"webhook_uuid":         "",
			},
			"Slack": map[string]interface{}{
				"id": "slack", "type": "slack", "enable": true,
				"bot_token":             "",
				"app_token":             "",
				"signing_secret":        "",
				"slack_connection_mode": "socket", // webhook, socket
				"unified_webhook_mode":  true,
				"webhook_uuid":          "",
				"slack_webhook_host":    "0.0.0.0",
				"slack_webhook_port":    6197,
				"slack_webhook_path":    "/astrbot-slack-webhook/callback",
			},
			"Misskey": map[string]interface{}{
				"id": "misskey", "type": "misskey", "enable": true,
				"misskey_instance_url":             "https://misskey.example",
				"misskey_token":                    "",
				"max_message_length":               3000,
				"misskey_default_visibility":       "public",
				"misskey_local_only":               false,
				"misskey_enable_chat":              true,
				"misskey_enable_file_upload":       true,
				"misskey_upload_concurrency":       3,
				"misskey_upload_folder":            "",
				"misskey_allow_insecure_downloads": false,
				"misskey_download_timeout":         15,
				"misskey_download_chunk_size":      65536,
				"misskey_max_download_bytes":       0,
			},
			"Mattermost": map[string]interface{}{
				"id": "mattermost", "type": "mattermost", "enable": true,
				"mattermost_url":             "https://chat.example.com",
				"mattermost_bot_token":       "",
				"mattermost_reconnect_delay": 5.0,
			},
			"企业微信应用 & 微信客服": map[string]interface{}{
				"id": "wecom", "type": "wecom", "enable": true,
				"corpid":               "",
				"secret":               "",
				"token":                "",
				"encoding_aes_key":     "",
				"kf_name":              "",
				"api_base_url":         "https://qyapi.weixin.qq.com/cgi-bin/",
				"unified_webhook_mode": true,
				"webhook_uuid":         "",
				"callback_server_host": "0.0.0.0",
				"port":                 6195,
			},
			"企业微信 (智能机器人)": map[string]interface{}{
				"id":                                     "wecom_ai_bot",
				"type":                                   "wecom_ai_bot",
				"hint":                                   "如果发现字段有异常，请重新创建",
				"enable":                                 true,
				"wecom_ai_bot_connection_mode":           "long_connection", // long_connection, webhook
				"wecom_ai_bot_name":                      "",
				"wecomaibot_ws_bot_id":                   "",
				"wecomaibot_ws_secret":                   "",
				"wecomaibot_token":                       "",
				"wecomaibot_encoding_aes_key":            "",
				"wecomaibot_init_respond_text":           "",
				"wecomaibot_friend_message_welcome_text": "",
				"msg_push_webhook_url":                   "",
				"only_use_webhook_url_to_send":           false,
				"wecomaibot_ws_url":                      "wss://openws.work.weixin.qq.com",
				"wecomaibot_heartbeat_interval":          30,
				"unified_webhook_mode":                   true,
				"webhook_uuid":                           "",
				"callback_server_host":                   "0.0.0.0",
				"port":                                   6198,
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
			"app_id": map[string]interface{}{
				"description": "app_id",
				"type":        "string",
				"hint":        "必填项。飞书开放平台的 App ID。",
			},
			"app_secret": map[string]interface{}{
				"description": "app_secret",
				"type":        "string",
				"hint":        "必填项。飞书开放平台的 App Secret。",
			},
			"domain": map[string]interface{}{
				"description": "开放平台域名",
				"type":        "string",
				"hint":        "默认 https://open.feishu.cn，Lark 国际版填 https://open.larksuite.com。",
			},
			"lark_connection_mode": map[string]interface{}{
				"description": "订阅方式",
				"type":        "string",
				"options":     []string{"socket", "webhook"},
				"labels":      []string{"长连接模式", "推送至服务器模式"},
			},
			"webhook_uuid": map[string]interface{}{
				"description": "Webhook UUID",
				"type":        "string",
				"hint":        "推送至服务器模式下的回调标识。回调地址为 /api/v1/webhooks/platforms/{webhook_uuid}。",
			},
			"lark_encrypt_key": map[string]interface{}{
				"description": "Encrypt Key",
				"type":        "string",
				"hint":        "用于解密飞书回调数据的加密密钥",
			},
			"lark_verification_token": map[string]interface{}{
				"description": "Verification Token",
				"type":        "string",
				"hint":        "用于验证飞书回调请求的令牌",
			},
			"discord_token": map[string]interface{}{
				"description": "Discord Bot Token",
				"type":        "string",
				"hint":        "在此处填入你的 Discord Bot Token",
			},
			"discord_proxy": map[string]interface{}{
				"description": "Discord 代理地址",
				"type":        "string",
				"hint":        "可选的代理地址：http://ip:port",
			},
			"discord_command_register": map[string]interface{}{
				"description": "注册 Discord 指令",
				"type":        "bool",
				"hint":        "启用后，自动将插件指令注册为 Discord 斜杠指令",
			},
			"discord_activity_name": map[string]interface{}{
				"description": "Discord 活动名称",
				"type":        "string",
				"hint":        "可选的 Discord 活动名称。留空则不设置活动。",
			},
			"discord_allow_bot_messages": map[string]interface{}{
				"description": "允许接收机器人消息",
				"type":        "bool",
				"hint":        "启用后，AstrBot 将接收来自其他 Discord 机器人的消息。适用于机器人间通信场景（如消息转发）。默认关闭。",
			},
			"mattermost_url": map[string]interface{}{
				"description": "Mattermost URL",
				"type":        "string",
				"hint":        "Mattermost 服务地址，例如 https://chat.example.com。",
			},
			"mattermost_bot_token": map[string]interface{}{
				"description": "Mattermost Bot Token",
				"type":        "string",
				"hint":        "在 Mattermost 中创建 Bot 账户后生成的访问令牌。",
			},
			"mattermost_reconnect_delay": map[string]interface{}{
				"description": "Mattermost 重连延迟",
				"type":        "float",
				"hint":        "WebSocket 断开后的重连等待时间，单位为秒。默认 5 秒。",
			},
			"misskey_instance_url": map[string]interface{}{
				"description": "Misskey 实例 URL",
				"type":        "string",
				"hint":        "例如 https://misskey.example，填写 Bot 账号所在的 Misskey 实例地址",
			},
			"misskey_token": map[string]interface{}{
				"description": "Misskey Access Token",
				"type":        "string",
				"hint":        "连接服务设置生成的 API 鉴权访问令牌（Access token）",
			},
			"max_message_length": map[string]interface{}{
				"description": "最大消息长度",
				"type":        "int",
				"hint":        "发帖时文本的最大长度，超出部分将被截断并追加省略号。默认 3000。",
			},
			"misskey_default_visibility": map[string]interface{}{
				"description": "默认帖子可见性",
				"type":        "string",
				"options":     []string{"public", "home", "followers"},
				"hint":        "机器人发帖时的默认可见性设置。public：公开，home：主页时间线，followers：仅关注者。",
			},
			"misskey_local_only": map[string]interface{}{
				"description": "仅限本站（不参与联合）",
				"type":        "bool",
				"hint":        "启用后，机器人发出的帖子将仅在本实例可见，不会联合到其他实例",
			},
			"misskey_enable_chat": map[string]interface{}{
				"description": "启用聊天消息响应",
				"type":        "bool",
				"hint":        "启用后，机器人将会监听和响应私信聊天消息",
			},
			"misskey_enable_file_upload": map[string]interface{}{
				"description": "启用文件上传到 Misskey",
				"type":        "bool",
				"hint":        "启用后，适配器会尝试将消息链中的文件上传到 Misskey。URL 文件会先尝试服务器端上传，异步上传失败时会回退到下载后本地上传。",
			},
			"misskey_allow_insecure_downloads": map[string]interface{}{
				"description": "允许不安全下载（禁用 SSL 验证）",
				"type":        "bool",
				"hint":        "当远端服务器存在证书问题导致无法正常下载时，自动禁用 SSL 验证作为回退方案。适用于某些图床的证书配置问题。启用有安全风险，仅在必要时使用。",
			},
			"misskey_download_timeout": map[string]interface{}{
				"description": "远端下载超时时间（秒）",
				"type":        "int",
				"hint":        "下载远程文件时的超时时间（秒），用于异步上传回退到本地上传的场景。",
			},
			"misskey_download_chunk_size": map[string]interface{}{
				"description": "流式下载分块大小（字节）",
				"type":        "int",
				"hint":        "流式下载和计算 MD5 时使用的每次读取字节数，过小会增加开销，过大会占用内存。",
			},
			"misskey_max_download_bytes": map[string]interface{}{
				"description": "最大允许下载字节数（超出则中止）",
				"type":        "int",
				"hint":        "如果希望限制下载文件的最大大小以防止 OOM，请填写最大字节数；留空或 null 表示不限制。",
			},
			"misskey_upload_concurrency": map[string]interface{}{
				"description": "并发上传限制",
				"type":        "int",
				"hint":        "同时进行的文件上传任务上限（整数，默认 3）。",
			},
			"misskey_upload_folder": map[string]interface{}{
				"description": "上传到网盘的目标文件夹 ID",
				"type":        "string",
				"hint":        "可选：填写 Misskey 网盘中目标文件夹的 ID，上传的文件将放置到该文件夹内。留空则使用账号网盘根目录。",
			},
			"channel_access_token": map[string]interface{}{
				"description": "LINE Channel Access Token",
				"type":        "string",
				"hint":        "LINE Messaging API 的 channel access token。",
			},
			"channel_secret": map[string]interface{}{
				"description": "LINE Channel Secret",
				"type":        "string",
				"hint":        "用于校验 LINE Webhook 签名。",
			},
			"bot_token": map[string]interface{}{
				"description": "Bot Token",
				"type":        "string",
				"hint":        "Slack Bot User OAuth Token（xoxb- 开头）。",
			},
			"app_token": map[string]interface{}{
				"description": "App Token",
				"type":        "string",
				"hint":        "Slack App-Level Token（xapp- 开头），Socket Mode 必需。",
			},
			"signing_secret": map[string]interface{}{
				"description": "Signing Secret",
				"type":        "string",
				"hint":        "用于校验 Slack Webhook 签名，Webhook Mode 必需。",
			},
			"slack_connection_mode": map[string]interface{}{
				"description": "Slack Connection Mode",
				"type":        "string",
				"options":     []string{"webhook", "socket"},
				"hint":        "The connection mode for Slack. `webhook` uses a webhook server, `socket` uses Slack's Socket Mode.",
			},
			"slack_webhook_host": map[string]interface{}{
				"description": "Slack Webhook Host",
				"type":        "string",
				"hint":        "Only valid when Slack connection mode is `webhook`.",
				"condition": map[string]interface{}{
					"slack_connection_mode": "webhook",
					"unified_webhook_mode":  false,
				},
			},
			"slack_webhook_port": map[string]interface{}{
				"description": "Slack Webhook Port",
				"type":        "int",
				"hint":        "Only valid when Slack connection mode is `webhook`.",
				"condition": map[string]interface{}{
					"slack_connection_mode": "webhook",
					"unified_webhook_mode":  false,
				},
			},
			"slack_webhook_path": map[string]interface{}{
				"description": "Slack Webhook Path",
				"type":        "string",
				"hint":        "Only valid when Slack connection mode is `webhook`.",
				"condition": map[string]interface{}{
					"slack_connection_mode": "webhook",
					"unified_webhook_mode":  false,
				},
			},
			"corpid": map[string]interface{}{
				"description": "企业 ID",
				"type":        "string",
				"hint":        "必填项。企业微信管理后台「我的企业」中的企业 ID（CorpID）。",
			},
			"token": map[string]interface{}{
				"description": "回调 Token",
				"type":        "string",
				"hint":        "必填项。企业微信应用回调配置中的 Token。",
			},
			"encoding_aes_key": map[string]interface{}{
				"description": "EncodingAESKey",
				"type":        "string",
				"hint":        "必填项。企业微信应用回调配置中的 EncodingAESKey，用于消息加解密。",
			},
			"kf_name": map[string]interface{}{
				"description": "微信客服名称",
				"type":        "string",
				"hint":        "可选。填写后启用微信客服模式，使用客服帐号接收与发送消息。",
			},
			"api_base_url": map[string]interface{}{
				"description": "企业微信 API 基础地址",
				"type":        "string",
				"hint":        "默认 https://qyapi.weixin.qq.com/cgi-bin/。",
			},
			"callback_server_host": map[string]interface{}{
				"description": "回调服务器主机",
				"type":        "string",
				"hint":        "回调服务器主机。统一 Webhook 模式下无需填写。",
				"condition": map[string]interface{}{
					"unified_webhook_mode": false,
				},
			},
			"port": map[string]interface{}{
				"description": "回调服务器端口",
				"type":        "int",
				"hint":        "回调服务器端口。统一 Webhook 模式下无需填写。",
				"condition": map[string]interface{}{
					"unified_webhook_mode": false,
				},
			},
			"wecom_ai_bot_name": map[string]interface{}{
				"description": "企业微信智能机器人的名字",
				"type":        "string",
				"hint":        "请务必填写正确，否则无法使用一些指令。",
			},
			"wecom_ai_bot_connection_mode": map[string]interface{}{
				"description": "企业微信智能机器人连接模式",
				"type":        "string",
				"options":     []string{"webhook", "long_connection"},
				"labels":      []string{"Webhook 回调", "长连接"},
				"hint":        "Webhook 回调模式需要配置 Token/EncodingAESKey。长连接模式需要配置 BotID/Secret。",
			},
			"wecomaibot_init_respond_text": map[string]interface{}{
				"description": "企业微信智能机器人初始响应文本",
				"type":        "string",
				"hint":        "当机器人收到消息时，首先回复的文本内容。留空则不设置。",
			},
			"wecomaibot_friend_message_welcome_text": map[string]interface{}{
				"description": "企业微信智能机器人私聊欢迎语",
				"type":        "string",
				"hint":        "当用户当天进入智能机器人单聊会话，回复欢迎语，留空则不回复。",
			},
			"wecomaibot_token": map[string]interface{}{
				"description": "企业微信智能机器人 Token",
				"type":        "string",
				"hint":        "用于 Webhook 回调模式的身份验证。",
				"condition": map[string]interface{}{
					"wecom_ai_bot_connection_mode": "webhook",
				},
			},
			"wecomaibot_encoding_aes_key": map[string]interface{}{
				"description": "企业微信智能机器人 EncodingAESKey",
				"type":        "string",
				"hint":        "用于 Webhook 回调模式的消息加密解密。",
				"condition": map[string]interface{}{
					"wecom_ai_bot_connection_mode": "webhook",
				},
			},
			"msg_push_webhook_url": map[string]interface{}{
				"description": "企业微信消息推送 Webhook URL",
				"type":        "string",
				"hint":        "用于 send_by_session 主动消息推送。格式示例: https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx",
			},
			"only_use_webhook_url_to_send": map[string]interface{}{
				"description": "仅使用 Webhook 发送消息",
				"type":        "bool",
				"hint":        "启用后，企业微信智能机器人的所有回复都改为通过消息推送 Webhook 发送。消息推送 Webhook 支持更多的消息类型（如图片、文件等）。",
			},
			"wecomaibot_ws_bot_id": map[string]interface{}{
				"description": "长连接 BotID",
				"type":        "string",
				"hint":        "企业微信智能机器人长连接模式凭证 BotID。",
				"condition": map[string]interface{}{
					"wecom_ai_bot_connection_mode": "long_connection",
				},
			},
			"wecomaibot_ws_secret": map[string]interface{}{
				"description": "长连接 Secret",
				"type":        "string",
				"hint":        "企业微信智能机器人长连接模式凭证 Secret。",
				"condition": map[string]interface{}{
					"wecom_ai_bot_connection_mode": "long_connection",
				},
			},
			"wecomaibot_ws_url": map[string]interface{}{
				"description": "长连接 WebSocket 地址",
				"type":        "string",
				"invisible":   true,
				"hint":        "默认值为 wss://openws.work.weixin.qq.com，一般无需修改。",
				"condition": map[string]interface{}{
					"wecom_ai_bot_connection_mode": "long_connection",
				},
			},
			"wecomaibot_heartbeat_interval": map[string]interface{}{
				"description": "长连接心跳间隔",
				"type":        "int",
				"invisible":   true,
				"hint":        "长连接模式心跳间隔（秒），建议 30 秒。",
				"condition": map[string]interface{}{
					"wecom_ai_bot_connection_mode": "long_connection",
				},
			},
			"satori_api_base_url": map[string]interface{}{
				"description": "Satori API 终结点",
				"type":        "string",
				"hint":        "Satori API 的基础地址。",
			},
			"satori_endpoint": map[string]interface{}{
				"description": "Satori WebSocket 终结点",
				"type":        "string",
				"hint":        "Satori 事件的 WebSocket 端点。",
			},
			"satori_token": map[string]interface{}{
				"description": "Satori 令牌",
				"type":        "string",
				"hint":        "用于 Satori API 身份验证的令牌。",
			},
			"satori_auto_reconnect": map[string]interface{}{
				"description": "启用自动重连",
				"type":        "bool",
				"hint":        "断开连接时是否自动重新连接 WebSocket。",
			},
			"satori_heartbeat_interval": map[string]interface{}{
				"description": "Satori 心跳间隔",
				"type":        "int",
				"hint":        "发送心跳消息的间隔（秒）。",
			},
			"satori_reconnect_delay": map[string]interface{}{
				"description": "Satori 重连延迟",
				"type":        "int",
				"hint":        "尝试重新连接前的延迟时间（秒）。",
			},
			"kook_bot_token": map[string]interface{}{
				"description": "机器人 Token",
				"type":        "string",
				"hint":        "必填项。从 KOOK 开发者平台获取的机器人 Token。",
			},
			"kook_reconnect_delay": map[string]interface{}{
				"description": "重连延迟",
				"type":        "int",
				"hint":        "重连延迟时间（秒），使用指数退避策略。",
			},
			"kook_max_reconnect_delay": map[string]interface{}{
				"description": "最大重连延迟",
				"type":        "int",
				"hint":        "重连延迟的最大值（秒）。",
			},
			"kook_max_retry_delay": map[string]interface{}{
				"description": "最大重试延迟",
				"type":        "int",
				"hint":        "重试的最大延迟时间（秒）。",
			},
			"kook_heartbeat_interval": map[string]interface{}{
				"description": "心跳间隔",
				"type":        "int",
				"hint":        "心跳检测间隔时间（秒）。",
			},
			"kook_heartbeat_timeout": map[string]interface{}{
				"description": "心跳超时时间",
				"type":        "int",
				"hint":        "心跳检测超时时间（秒）。",
			},
			"kook_max_heartbeat_failures": map[string]interface{}{
				"description": "最大心跳失败次数",
				"type":        "int",
				"hint":        "允许的最大心跳失败次数，超过后断开连接。",
			},
			"kook_max_consecutive_failures": map[string]interface{}{
				"description": "最大连续失败次数",
				"type":        "int",
				"hint":        "允许的最大连续失败次数，超过后停止重试。",
			},
			"client_id": map[string]interface{}{
				"description": "Client ID",
				"type":        "string",
				"hint":        "必填项。钉钉开放平台应用的 Client ID（AppKey）。",
			},
			"client_secret": map[string]interface{}{
				"description": "Client Secret",
				"type":        "string",
				"hint":        "必填项。钉钉开放平台应用的 Client Secret（AppSecret）。",
			},
			"card_template_id": map[string]interface{}{
				"description": "卡片模板 ID",
				"type":        "string",
				"hint":        "可选。钉钉互动卡片模板 ID。启用后将使用互动卡片进行流式回复。",
			},
			"weixin_oc_base_url": map[string]interface{}{
				"description": "开放平台基础地址",
				"type":        "string",
				"hint":        "默认 https://ilinkai.weixin.qq.com",
			},
			"weixin_oc_cdn_base_url": map[string]interface{}{
				"description": "CDN 基础地址",
				"type":        "string",
				"hint":        "默认 https://cdn.wx.qq.com",
			},
			"weixin_oc_bot_type": map[string]interface{}{
				"description": "机器人类型",
				"type":        "string",
				"hint":        "默认 3（开放平台机器人）。",
			},
			"weixin_oc_qr_poll_interval": map[string]interface{}{
				"description": "二维码轮询间隔（秒）",
				"type":        "int",
			},
			"weixin_oc_long_poll_timeout_ms": map[string]interface{}{
				"description": "长轮询超时（毫秒）",
				"type":        "int",
			},
			"weixin_oc_api_timeout_ms": map[string]interface{}{
				"description": "API 超时（毫秒）",
				"type":        "int",
			},
			"unified_webhook_mode": map[string]interface{}{
				"description": "统一 Webhook 模式",
				"type":        "bool",
				"hint":        "启用后使用 AstrBot 统一 Webhook 入口。",
			},
			"active_send_mode": map[string]interface{}{
				"description": "主动发送模式",
				"type":        "bool",
				"hint":        "启用后回调直接调用客服消息接口回复（否则走被动回复）。",
			},
		},
	}
	if pg, ok := metadata.Get("platform_group"); ok {
		if pgMap, ok := config.GetOrderedJSON(pg); ok {
			if md, ok := pgMap.Get("metadata"); ok {
				if mdMap, ok := config.GetOrderedJSON(md); ok {
					mdMap.Set("platform", platformSection)
				}
			}
		}
	}
}

// om builds an OrderedJSON from alternating key/value pairs, preserving the
// given order (Go map literals would otherwise be alphabetized on marshal).
func om(kv ...interface{}) *config.OrderedJSON {
	o := config.NewOrderedJSON()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// getProviderItems returns the provider config field schema shared by
// /providers/schema and the system config metadata.
func (s *Server) getProviderItems() *config.OrderedJSON {
	return om(
		"id", om("description", "名称", "type", "string", "hint", "此模型提供方的唯一标识。"),
		"type", om("description", "类型", "type", "string", "invisible", true),
		"provider", om("description", "提供商", "type", "string", "invisible", true),
		"provider_type", om("description", "提供商类型", "type", "string", "invisible", true),
		"enable", om("description", "启用", "type", "bool"),
		"key", om("description", "API Key", "type", "list", "items", om("type", "string")),
		"api_base", om("description", "API Base URL", "type", "string"),
		"proxy", om("description", "代理地址", "type", "string", "hint", "留空则直连。格式示例: http://127.0.0.1:7890"),
		"timeout", om("description", "请求超时时间（秒）", "type", "int", "hint", "默认 120 秒。"),
		"model", om("description", "模型 ID", "type", "string", "hint", "模型名称，如 gpt-4o-mini, deepseek-chat。"),
		"max_context_tokens", om("description", "模型上下文窗口大小", "type", "int", "hint", "模型最大上下文 Token 大小。如果为 0，则会自动从模型元数据填充（如有）"),
		"modalities", om("description", "模型能力", "type", "list", "items", om("type", "string"),
			"options", []string{"text", "image", "audio", "tool_use"},
			"labels", []string{"文本", "图像", "音频", "工具使用"},
			"render_type", "checkbox",
			"hint", "模型支持的模态及能力。"),
		"custom_headers", om("description", "自定义请求头", "type", "dict", "items", om(), "hint", "此处添加的键值对将被合并到 HTTP 请求头中。"),
		"custom_extra_body", om("description", "自定义请求体参数", "type", "dict", "items", om(), "hint", "用于在请求时添加额外的参数，如 temperature, top_p, max_tokens, reasoning_effort 等。"),
	)
}

// getProviderTemplates returns provider config templates for the Go-supported providers.
func (s *Server) getProviderTemplates() *config.OrderedJSON {
	template := func(name, provider, providerType, apiBase string) *config.OrderedJSON {
		return om("id", name, "type", name, "provider", provider,
			"provider_type", providerType, "enable", false, "api_base", apiBase, "key", "")
	}
	return om(
		"openai", template("openai", "openai_chat_completion", "chat_completion", "https://api.openai.com/v1"),
		"openrouter", template("openrouter", "openrouter_chat_completion", "chat_completion", "https://openrouter.ai/api/v1"),
		"anthropic", template("anthropic", "anthropic_chat_completion", "chat_completion", "https://api.anthropic.com"),
		"gemini", template("gemini", "googlegenai_chat_completion", "chat_completion", "https://generativelanguage.googleapis.com/v1beta"),
		"ollama", template("ollama", "ollama_chat_completion", "chat_completion", "http://127.0.0.1:11434"),
		"dashscope", template("dashscope", "dashscope_chat_completion", "chat_completion", "https://dashscope.aliyuncs.com/compatible-mode/v1"),
		"groq", template("groq", "groq_chat_completion", "chat_completion", "https://api.groq.com/openai/v1"),
		"xai", om("id", "xai", "type", "xai_chat_completion", "provider", "xai_chat_completion",
			"provider_type", "chat_completion", "enable", false,
			"api_base", "https://api.x.ai/v1", "key", "", "xai_native_search", false),
		"zhipu", template("zhipu", "zhipu_chat_completion", "chat_completion", "https://open.bigmodel.cn/api/paas/v4"),
		"longcat", template("longcat", "longcat_chat_completion", "chat_completion", "https://api.longcat.chat/openai/v1"),
		"aihubmix", template("aihubmix", "aihubmix_chat_completion", "chat_completion", "https://aihubmix.com/v1"),
		"xiaomi", template("xiaomi", "xiaomi_chat_completion", "chat_completion", "https://api.xiaomimimo.com/v1"),
		"openai_responses", om("id", "openai_responses", "type", "openai_responses", "provider", "openai",
			"provider_type", "chat_completion", "enable", false,
			"api_base", "https://api.openai.com/v1", "key", "", "model", ""),
		"kimi_code", om("id", "kimi_code", "type", "kimi_code_chat_completion", "provider", "kimi-code",
			"provider_type", "chat_completion", "enable", false,
			"api_base", "https://api.kimi.com/coding", "key", "", "model", "kimi-for-coding"),
		// Non-chat capabilities (STT / TTS / Embedding / Rerank). The type field
		// must match the provider registered in internal/provider/sources/init.go.
		"openai_whisper", om("id", "whisper", "type", "openai_whisper", "provider", "openai",
			"provider_type", "speech_to_text", "enable", false,
			"api_key", "", "api_base", "https://api.openai.com/v1", "model", "whisper-1"),
		"openai_tts", om("id", "openai_tts", "type", "openai_tts", "provider", "openai",
			"provider_type", "text_to_speech", "enable", false,
			"api_key", "", "api_base", "https://api.openai.com/v1", "model", "tts-1",
			"voice", "alloy"),
		"azure_tts", om("id", "azure_tts", "type", "azure_tts", "provider", "microsoft",
			"provider_type", "text_to_speech", "enable", false,
			"azure_tts_subscription_key", "", "azure_tts_region", "eastus",
			"azure_tts_voice", "zh-CN-YunxiaNeural"),
		"elevenlabs_tts", om("id", "elevenlabs_tts", "type", "elevenlabs_tts_api", "provider", "elevenlabs",
			"provider_type", "text_to_speech", "enable", false,
			"api_key", "", "api_base", "https://api.elevenlabs.io/v1",
			"model", "eleven_multilingual_v2", "elevenlabs-tts-voice-id", "JBFqnCBsd6RMkjVDRZzb"),
		"fishaudio_tts", om("id", "fishaudio_tts", "type", "fishaudio_tts_api", "provider", "fishaudio",
			"provider_type", "text_to_speech", "enable", false,
			"api_key", "", "api_base", "https://api.fish-audio.cn/v1",
			"model", "s2-pro", "fishaudio-tts-character", "可莉"),
		"minimax_tts", om("id", "minimax_tts", "type", "minimax_tts_api", "provider", "minimax",
			"provider_type", "text_to_speech", "enable", false,
			"api_key", "", "api_base", "https://api.minimax.chat/v1/t2a_v2",
			"minimax-group-id", "", "minimax-voice-id", ""),
		"edge_tts", om("id", "edge_tts", "type", "edge_tts", "provider", "microsoft",
			"provider_type", "text_to_speech", "enable", false,
			"edge-tts-voice", "zh-CN-XiaoxiaoNeural"),
		"volcengine_tts", om("id", "volcengine_tts", "type", "volcengine_tts", "provider", "volcengine",
			"provider_type", "text_to_speech", "enable", false,
			"api_key", "", "appid", "", "volcengine_cluster", "",
			"volcengine_voice_type", "", "api_base", "https://openspeech.bytedance.com/api/v1/tts"),
		"gemini_tts", om("id", "gemini_tts", "type", "gemini_tts", "provider", "google",
			"provider_type", "text_to_speech", "enable", false,
			"gemini_tts_api_key", "", "gemini_tts_api_base", "https://generativelanguage.googleapis.com/v1beta",
			"gemini_tts_model", "gemini-2.5-flash-preview-tts", "gemini_tts_voice_name", "Leda"),
		"mimo_tts", om("id", "mimo_tts", "type", "mimo_tts_api", "provider", "mimo",
			"provider_type", "text_to_speech", "enable", false,
			"api_key", "", "api_base", "https://api.xiaomimimo.com/v1", "model", "mimo-v2.5-tts",
			"mimo-tts-voice", "mimo_default"),
		"mimo_stt", om("id", "mimo_stt", "type", "mimo_stt_api", "provider", "mimo",
			"provider_type", "speech_to_text", "enable", false,
			"api_key", "", "api_base", "https://api.xiaomimimo.com/v1", "model", "mimo-v2.5-asr"),
		"openai_embedding", om("id", "openai_embedding", "type", "openai_embedding", "provider", "openai",
			"provider_type", "embedding", "enable", false,
			"embedding_api_key", "", "embedding_api_base", "https://api.openai.com/v1",
			"embedding_model", "text-embedding-3-small", "embedding_dimensions", 1024),
		"gemini_embedding", om("id", "gemini_embedding", "type", "gemini_embedding", "provider", "google",
			"provider_type", "embedding", "enable", false,
			"embedding_api_key", "", "embedding_api_base", "https://generativelanguage.googleapis.com",
			"embedding_model", "gemini-embedding-exp-03-07", "embedding_dimensions", 768),
		"nvidia_embedding", om("id", "nvidia_embedding", "type", "nvidia_embedding", "provider", "nvidia",
			"provider_type", "embedding", "enable", false,
			"embedding_api_key", "", "embedding_api_base", "https://integrate.api.nvidia.com/v1",
			"embedding_model", "nvidia/llama-nemotron-embed-1b-v2", "embedding_dimensions", 1024),
		"ollama_embedding", om("id", "ollama_embedding", "type", "ollama_embedding", "provider", "ollama",
			"provider_type", "embedding", "enable", false,
			"embedding_api_base", "http://localhost:11434", "embedding_model", "nomic-embed-text",
			"embedding_dimensions", 768),
		"dashscope_embedding", om("id", "dashscope_embedding", "type", "dashscope_embedding", "provider", "dashscope",
			"provider_type", "embedding", "enable", false,
			"embedding_api_key", "", "embedding_api_base", "https://dashscope.aliyuncs.com/api/v1",
			"embedding_model", "text-embedding-v4", "embedding_dimensions", 1024),
		"tei_rerank", om("id", "tei_rerank", "type", "tei_rerank", "provider", "tei",
			"provider_type", "rerank", "enable", false,
			"rerank_api_key", "", "rerank_api_base", "http://127.0.0.1:8080", "model", ""),
		"bailian_rerank", om("id", "bailian_rerank", "type", "bailian_rerank", "provider", "dashscope",
			"provider_type", "rerank", "enable", false,
			"rerank_api_key", "", "rerank_api_base", "https://dashscope.aliyuncs.com/api/v1/services/rerank/text-rerank/text-rerank",
			"rerank_model", "qwen3-rerank"),
		"nvidia_rerank", om("id", "nvidia_rerank", "type", "nvidia_rerank", "provider", "nvidia",
			"provider_type", "rerank", "enable", false,
			"nvidia_rerank_api_key", "", "nvidia_rerank_api_base", "https://ai.api.nvidia.com/v1/retrieval",
			"nvidia_rerank_model", "nv-rerank-qa-mistral-4b:1"),
		"vllm_rerank", om("id", "vllm_rerank", "type", "vllm_rerank", "provider", "vllm",
			"provider_type", "rerank", "enable", false,
			"rerank_api_base", "http://127.0.0.1:8000", "rerank_api_suffix", "/v1/rerank",
			"rerank_model", "BAAI/bge-reranker-base"),
		"xinference_rerank", om("id", "xinference_rerank", "type", "xinference_rerank", "provider", "xinference",
			"provider_type", "rerank", "enable", false,
			"rerank_api_base", "http://127.0.0.1:8000", "rerank_model", "BAAI/bge-reranker-base"),
		// Agent runner backends (remote HTTP APIs). Mirrors the Python
		// templates; the type values match the agent_runner_type config options
		// so a created source can be selected as an agent runner provider.
		"dify", om("id", "dify_app_default", "type", "dify", "provider", "dify",
			"provider_type", "agent_runner", "enable", true,
			"dify_api_type", "chat", "dify_api_key", "",
			"dify_api_base", "https://api.dify.ai/v1",
			"dify_workflow_output_key", "astrbot_wf_output",
			"dify_query_input_key", "astrbot_text_query",
			"variables", om(), "timeout", 60, "proxy", ""),
		"coze", om("id", "coze", "type", "coze", "provider", "coze",
			"provider_type", "agent_runner", "enable", true,
			"coze_api_key", "", "bot_id", "", "coze_api_base", "https://api.coze.cn",
			"timeout", 60, "proxy", ""),
		"dashscope_agent", om("id", "dashscope", "type", "dashscope", "provider", "dashscope",
			"provider_type", "agent_runner", "enable", true,
			"dashscope_app_type", "agent", "dashscope_api_key", "", "dashscope_app_id", "",
			"rag_options", om("pipeline_ids", []interface{}{}, "file_ids", []interface{}{}, "output_reference", false),
			"variables", om(), "timeout", 60, "proxy", ""),
		"deerflow", om("id", "deerflow", "type", "deerflow", "provider", "deerflow",
			"provider_type", "agent_runner", "enable", true,
			"deerflow_api_base", "http://127.0.0.1:2026", "deerflow_api_key", "",
			"deerflow_auth_header", "", "deerflow_assistant_id", "lead_agent",
			"deerflow_model_name", "", "deerflow_thinking_enabled", false,
			"deerflow_plan_mode", false, "deerflow_subagent_enabled", false,
			"deerflow_max_concurrent_subagents", 3, "deerflow_recursion_limit", 1000,
			"timeout", 300),
	)
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

// capabilityToProviderType maps the WebUI's capability names to the
// provider_type stored in config, mirroring Python's
// CAPABILITY_TO_PROVIDER_TYPE. Unknown values pass through unchanged.
func capabilityToProviderType(capability string) string {
	switch strings.ToLower(strings.TrimSpace(capability)) {
	case "chat", "chat_completion":
		return "chat_completion"
	case "agent", "agent_runner":
		return "agent_runner"
	case "stt", "speech_to_text":
		return "speech_to_text"
	case "tts", "text_to_speech":
		return "text_to_speech"
	case "embedding":
		return "embedding"
	case "rerank":
		return "rerank"
	}
	return capability
}

// providerNameToProviderType derives a capability type from a provider name
// (e.g. "openai_chat_completion" -> "chat_completion"). Used to backfill
// provider_type on legacy providers that lack the field.
func providerNameToProviderType(providerName string) string {
	name := strings.ToLower(strings.TrimSpace(providerName))
	switch {
	case strings.Contains(name, "speech_to_text"), strings.Contains(name, "stt"), strings.Contains(name, "whisper"):
		return "speech_to_text"
	case strings.Contains(name, "text_to_speech"), strings.Contains(name, "tts"):
		return "text_to_speech"
	case strings.Contains(name, "embedding"):
		return "embedding"
	case strings.Contains(name, "rerank"):
		return "rerank"
	case strings.Contains(name, "dify"), strings.Contains(name, "coze"), strings.Contains(name, "deerflow"), strings.Contains(name, "dashscope"), strings.Contains(name, "agent_runner"), strings.Contains(name, "agent"):
		return "agent_runner"
	case strings.Contains(name, "chat_completion"), strings.Contains(name, "chat"):
		return "chat_completion"
	}
	return ""
}

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
		// Backfill provider_type for providers created before the field was
		// stored (or via flows that omit it), so the capability filter and the
		// WebUI provider selector work. Derive from the linked source first,
		// then from the provider name.
		sources := s.getProviderSources()
		sourceTypeByID := map[string]string{}
		for _, src := range sources {
			if sm, ok := src.(map[string]interface{}); ok {
				if sid, _ := sm["id"].(string); sid != "" {
					if pt, _ := sm["provider_type"].(string); pt != "" {
						sourceTypeByID[sid] = pt
					}
				}
			}
		}
		for _, p := range providers {
			pm, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if pt, _ := pm["provider_type"].(string); pt == "" {
				derived := ""
				if sid, _ := pm["provider_source_id"].(string); sid != "" {
					derived = sourceTypeByID[sid]
				}
				if derived == "" {
					providerName, _ := pm["provider"].(string)
					derived = providerNameToProviderType(providerName)
				}
				if derived != "" {
					pm["provider_type"] = derived
				}
			}
		}
		// The WebUI requests providers by capability (e.g. capability=embedding
		// for the knowledge-base vector model selector). Filter by provider_type
		// so the same provider is not returned for every capability.
		capability := strings.TrimSpace(r.URL.Query().Get("capability"))
		if capability != "" {
			ptype := capabilityToProviderType(capability)
			filtered := make([]interface{}, 0, len(providers))
			for _, p := range providers {
				pm, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				if pm["provider_type"] != ptype {
					continue
				}
				filtered = append(filtered, pm)
			}
			providers = filtered
		}
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

// deleteProviderByID removes a provider config by id and unregisters its
// runtime instance.
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
	if err := s.setConfigData("provider", next); err != nil {
		return err
	}
	s.unregisterProvider(id)
	s.cleanProviderSettingsRefs(id)
	return nil
}

// cleanProviderSettingsRefs clears provider_settings references that point at a
// deleted provider id (default_provider_id, provider_pool, default_*_provider_id).
func (s *Server) cleanProviderSettingsRefs(id string) {
	cfg := s.getConfigSnapshot()
	ps, _ := cfg["provider_settings"].(map[string]interface{})
	if ps == nil {
		return
	}
	changed := false
	refKeys := []string{
		"default_provider_id",
		"default_stt_provider_id",
		"default_tts_provider_id",
		"default_embedding_provider_id",
		"default_rerank_provider_id",
		"default_image_caption_provider_id",
		"llm_compress_provider_id",
		"coze_agent_runner_provider_id",
		"dify_agent_runner_provider_id",
		"dashscope_agent_runner_provider_id",
		"deerflow_agent_runner_provider_id",
	}
	for _, k := range refKeys {
		if v, _ := ps[k].(string); v == id {
			ps[k] = ""
			changed = true
		}
	}
	if pool, _ := ps["provider_pool"].([]interface{}); len(pool) > 0 {
		next := make([]interface{}, 0, len(pool))
		for _, v := range pool {
			if s2, _ := v.(string); s2 == id {
				changed = true
				continue
			}
			next = append(next, v)
		}
		if len(next) != len(pool) {
			ps["provider_pool"] = next
		}
	}
	if changed {
		if err := s.setConfigData("provider_settings", ps); err != nil {
			logger.I18nWarn("清理插件 %s 的提供商引用失败: %v", id, err)
		}
	}
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
		// Bot stats: list the enabled platform instances with their metadata
		// (mirrors Python BotConfigService.get_bot_stats ->
		// PlatformManager.get_all_stats). The CronJob "未来计划" page filters
		// meta.support_proactive_message to pick delivery targets.
		statsList := make([]interface{}, 0, 4)
		for _, b := range s.getBotList() {
			pc, ok := b.(map[string]interface{})
			if !ok {
				continue
			}
			enabled, _ := pc["enable"].(bool)
			id, _ := pc["id"].(string)
			ptype, _ := pc["type"].(string)
			if id == "" {
				id = ptype
			}
			meta := map[string]interface{}{
				"id":                         id,
				"name":                       ptype,
				"display_name":               platformDisplayName(ptype),
				"support_streaming_message":  true,
				"support_proactive_message":  true,
			}
			statsList = append(statsList, map[string]interface{}{
				"id":              id,
				"type":            ptype,
				"display_name":    platformDisplayName(ptype),
				"status":          map[bool]string{true: "running", false: "stopped"}[enabled],
				"error_count":     0,
				"last_error":      nil,
				"unified_webhook": false,
				"meta":            meta,
			})
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"platforms": statsList,
			"summary": map[string]interface{}{
				"total":        len(statsList),
				"running":      countEnabledBots(s.getBotList()),
				"error":        0,
				"total_errors": 0,
			},
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
		pluginID := r.URL.Query().Get("plugin_id")
		switch r.Method {
		case http.MethodDelete:
			// WebUI uninstall sends DELETE with a {delete_config, delete_data} body.
			var body struct {
				DeleteConfig bool `json:"delete_config"`
				DeleteData   bool `json:"delete_data"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.pluginUninstall(pluginID, body.DeleteConfig, body.DeleteData)
			writeJSON(w, http.StatusOK, apiOKMsg("插件已卸载", map[string]interface{}{}))
		case http.MethodPost:
			s.pluginUninstall(pluginID, true, true)
			writeJSON(w, http.StatusOK, apiOKMsg("插件已卸载", map[string]interface{}{}))
		default:
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
			if r.Method == http.MethodPost || r.Method == http.MethodPut {
				var body struct {
					PluginID string                 `json:"plugin_id"`
					Config   map[string]interface{} `json:"config"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				if pluginID == "" {
					pluginID = body.PluginID
				}
				s.pluginSaveConfig(pluginID, body.Config)
				writeJSON(w, http.StatusOK, apiOKMsg("插件配置已保存", map[string]interface{}{}))
			} else {
				writeJSON(w, http.StatusOK, apiOK(s.pluginConfigPayload(pluginID)))
			}
		}
	case "market":
		market, err := s.fetchPluginMarket(
			r.URL.Query().Get("custom_registry"),
			strings.EqualFold(r.URL.Query().Get("force_refresh"), "true"),
		)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("获取插件市场失败: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(market))
	case "page":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "readme":
		s.handlePluginDocs(w, r, s.subPluginMgr.Readme)
	case "config-files":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"files": []interface{}{},
		}))
	case "changelog":
		s.handlePluginDocs(w, r, s.subPluginMgr.Changelog)
	case "update":
		s.handlePluginUpdate(w, r, parts[1:])
	case "install":
		s.handlePluginInstall(w, r, parts)
	case "validate":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"valid": true,
		}))
	case "version-support":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"supported": true,
		}))
	case "log-level":
		if r.Method != http.MethodPut {
			writeJSON(w, http.StatusOK, apiError("仅支持 PUT 请求"))
			return
		}
		var body struct {
			Level string `json:"level"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		level := strings.ToUpper(strings.TrimSpace(body.Level))
		switch level {
		case "DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL":
		default:
			writeJSON(w, http.StatusOK, apiError("无效的日志级别: "+body.Level))
			return
		}
		log.GetDefault().SetLevel(log.ParseLevel(level))
		_ = s.setConfigData("log_level", level)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"log_level": level}))
	default:
		// /api/v1/plugins/{plugin_id}, /{plugin_id}/config, /{plugin_id}/source,
		// /{plugin_id}/update and /{plugin_id}/reload
		pluginID := sub
		if len(parts) > 1 {
			switch parts[1] {
			case "log-level":
				// 前端将 plugin_id 作为占位，实际设置全局日志级别
				if r.Method != http.MethodPut {
					writeJSON(w, http.StatusOK, apiError("仅支持 PUT 请求"))
					return
				}
				var body struct {
					Level string `json:"level"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				level := strings.ToUpper(strings.TrimSpace(body.Level))
				switch level {
				case "DEBUG", "INFO", "WARNING", "ERROR", "CRITICAL":
				default:
					writeJSON(w, http.StatusOK, apiError("无效的日志级别: "+body.Level))
					return
				}
				log.GetDefault().SetLevel(log.ParseLevel(level))
				_ = s.setConfigData("log_level", level)
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"log_level": level}))
				return
			case "config":
				if r.Method == http.MethodPost || r.Method == http.MethodPut {
					var body struct {
						Config map[string]interface{} `json:"config"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					s.pluginSaveConfig(pluginID, body.Config)
					writeJSON(w, http.StatusOK, apiOKMsg("插件配置已保存", map[string]interface{}{}))
				} else {
					writeJSON(w, http.StatusOK, apiOK(s.pluginConfigPayload(pluginID)))
				}
				return
			case "source":
				s.handlePluginBindSource(w, r, pluginID)
				return
			case "update":
				s.handlePluginUpdate(w, r, []string{pluginID})
				return
			case "reload":
				s.pluginReload(pluginID)
				writeJSON(w, http.StatusOK, apiOKMsg("插件已重载", map[string]interface{}{}))
				return
			case "readme":
				s.handlePluginDocs(w, r, s.subPluginMgr.Readme)
				return
			case "changelog":
				s.handlePluginDocs(w, r, s.subPluginMgr.Changelog)
				return
			}
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, apiOK(s.pluginByID(pluginID)))
		case http.MethodDelete, http.MethodPost:
			s.pluginUninstall(pluginID, false, false)
			writeJSON(w, http.StatusOK, apiOKMsg("插件已卸载", map[string]interface{}{}))
		default:
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		}
	}
}

// handlePluginInstall installs a plugin via the subprocess runtime from a git
// URL, archive URL, or uploaded file. When the static scan finds risky imports
// and ignore_risk is not set, it returns code=plugin_risk with the offending
// code locations so the WebUI can prompt the user (ignore & continue / cancel).
func (s *Server) handlePluginInstall(w http.ResponseWriter, r *http.Request, parts []string) {
	if s.subPluginMgr == nil {
		writeJSON(w, http.StatusOK, apiError("插件子系统未初始化"))
		return
	}

	// GET /api/v1/plugins/install/progress?install_id=... returns live progress
	// so the WebUI can poll while the install request is in flight.
	if len(parts) > 1 && parts[1] == "progress" {
		writeJSON(w, http.StatusOK, apiOK(s.getInstallProgress(r.URL.Query().Get("install_id"))))
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, apiError("method not allowed"))
		return
	}

	var source, id, installID string
	var ignoreRisk bool
	var ccChoice string
	var installMethod, registryURL, registryName, marketPluginID, repo, downloadURL string

	method := "url"
	if len(parts) > 1 {
		method = parts[1]
	}

	switch method {
	case "url", "git", "github":
		var body struct {
			URL            string `json:"url"`
			IgnoreRisk     bool   `json:"ignore_risk"`
			CCChoice       string `json:"cc_choice"`
			InstallID      string `json:"install_id"`
			InstallMethod  string `json:"install_method"`
			RegistryURL    string `json:"registry_url"`
			RegistryName   string `json:"registry_name"`
			MarketPluginID string `json:"market_plugin_id"`
			Repo           string `json:"repo"`
			DownloadURL    string `json:"download_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		source = strings.TrimSpace(body.URL)
		ignoreRisk = body.IgnoreRisk
		ccChoice = strings.TrimSpace(body.CCChoice)
		installID = body.InstallID
		installMethod = body.InstallMethod
		registryURL = body.RegistryURL
		registryName = body.RegistryName
		marketPluginID = body.MarketPluginID
		repo = body.Repo
		downloadURL = body.DownloadURL
	case "upload":
		file, fh, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("未收到插件文件: "+err.Error()))
			return
		}
		defer file.Close()
		tmp, err := os.MkdirTemp("", "astrbot-plugin-upload-*")
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("创建临时目录失败"))
			return
		}
		defer os.RemoveAll(tmp)
		// Preserve the extension so the archive is recognized as .zip/.tar.gz.
		archive := filepath.Join(tmp, filepath.Base(fh.Filename))
		out, err := os.Create(archive)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("保存上传文件失败"))
			return
		}
		if _, err := io.Copy(out, file); err != nil {
			out.Close()
			writeJSON(w, http.StatusOK, apiError("保存上传文件失败: "+err.Error()))
			return
		}
		out.Close()
		source = archive
		id = idFromSource(fh.Filename)
		ignoreRisk = r.FormValue("ignore_risk") == "true"
		ccChoice = strings.TrimSpace(r.FormValue("cc_choice"))
		installID = r.FormValue("install_id")
		installMethod = r.FormValue("install_method")
		registryURL = r.FormValue("registry_url")
		registryName = r.FormValue("registry_name")
		marketPluginID = r.FormValue("market_plugin_id")
		repo = r.FormValue("repo")
		downloadURL = r.FormValue("download_url")
	default:
		writeJSON(w, http.StatusOK, apiError("未知安装方式: "+method))
		return
	}

	if source == "" {
		writeJSON(w, http.StatusOK, apiError("插件地址不能为空"))
		return
	}
	if id == "" {
		id = idFromSource(source)
	}

	// For market/repository installs the url IS the repository; default the
	// persisted repo so the WebUI can offer reinstall / change-source.
	if repo == "" && (installMethod == "market" || installMethod == "repository") {
		repo = source
	}

	s.setInstallProgress(installID, &installStatus{Status: "installing", Percent: 0, Text: "准备安装…"})
	// On success, report done; prompt branches override the state themselves
	// (risk / cgo compiler wait for user input) and mark noDone so the defer
	// does not clobber the "waiting" state shown while the dialog is open.
	done := true
	defer func() {
		if done {
			s.setInstallProgress(installID, &installStatus{Status: "done", Percent: 100, Text: "安装完成"})
		}
	}()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	inst, err := s.subPluginMgr.InstallFromSource(ctx, id, source, plugin.InstallOptions{
		IgnoreRisk:     ignoreRisk,
		CCChoice:       ccChoice,
		Progress:       s.installProgressCallback(installID),
		Stage:          s.installStageCallback(installID),
		InstallMethod:  installMethod,
		RegistryURL:    registryURL,
		RegistryName:   registryName,
		MarketPluginID: marketPluginID,
		Repo:           repo,
		DownloadURL:    downloadURL,
	})
	if err != nil {
		var riskErr *plugin.RiskError
		if errors.As(err, &riskErr) {
			done = false
			s.setInstallProgress(installID, &installStatus{Status: "installing", Text: "检测到风险代码，等待确认…"})
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":  "error",
				"code":    "plugin_risk",
				"message": fmt.Sprintf("插件源码包含 %d 处风险代码", len(riskErr.Findings)),
				"data": map[string]interface{}{
					"risks": riskErr.Findings,
				},
			})
			return
		}
		var ccErr *plugin.CCompilerPromptError
		if errors.As(err, &ccErr) {
			done = false
			s.setInstallProgress(installID, &installStatus{Status: "installing", Text: "需要选择 C 编译器…"})
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"status":  "error",
				"code":    "c_compiler_prompt",
				"message": ccErr.Error(),
				"data": map[string]interface{}{
					"kind":        string(ccErr.Kind),
					"has_gcc":     ccErr.HasGCC,
					"gcc_path":    ccErr.GCCPath,
					"gcc_xx_path": ccErr.GCCXXPath,
					"gcc_version": ccErr.GCCVersion,
				},
			})
			return
		}
		s.setInstallProgress(installID, &installStatus{Status: "error", Text: err.Error()})
		done = false
		writeJSON(w, http.StatusOK, apiError("插件安装失败: "+err.Error()))
		return
	}

	s.notifyPluginsChanged()
	writeJSON(w, http.StatusOK, apiOKMsg("插件安装成功", map[string]interface{}{
		"name":    inst.Name,
		"version": inst.Version,
	}))
}

// handlePluginUpdate updates/reinstalls plugins. The batch route
// POST /api/v1/plugins/update accepts {plugin_id} (single) or {names: [...]}
// (batch); the single route POST /api/v1/plugins/{plugin_id}/update passes the
// id directly. Each plugin is reinstalled from its persisted install source.
func (s *Server) handlePluginUpdate(w http.ResponseWriter, r *http.Request, parts []string) {
	if s.subPluginMgr == nil {
		writeJSON(w, http.StatusOK, apiError("插件子系统未初始化"))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, apiError("method not allowed"))
		return
	}

	ids := []string{}
	ccChoice := ""
	if len(parts) == 1 {
		ids = append(ids, parts[0])
	} else {
		var body struct {
			PluginID string   `json:"plugin_id"`
			Names    []string `json:"names"`
			CCChoice string   `json:"cc_choice"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.PluginID != "" {
			ids = append(ids, body.PluginID)
		} else {
			ids = body.Names
		}
		ccChoice = strings.TrimSpace(body.CCChoice)
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, apiError("缺少要更新的插件"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	type result struct {
		Name    string `json:"name"`
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	results := make([]result, 0, len(ids))
	for _, id := range ids {
		pid, name, ok := s.resolveSubprocessPlugin(id)
		if !ok {
			results = append(results, result{Name: id, Status: "error", Message: "插件不存在或非子进程插件"})
			continue
		}
		inst, err := s.subPluginMgr.ReinstallSource(ctx, pid, plugin.InstallOptions{
			Progress: s.installProgressCallback(""),
			CCChoice: ccChoice,
		})
		if err != nil {
			var riskErr *plugin.RiskError
			if errors.As(err, &riskErr) {
				results = append(results, result{Name: name, Status: "error", Message: "源码包含风险代码"})
			} else {
				results = append(results, result{Name: name, Status: "error", Message: err.Error()})
			}
			continue
		}
		results = append(results, result{Name: inst.Name, Status: "ok", Message: "更新成功"})
	}

	s.notifyPluginsChanged()
	if len(ids) == 1 && len(results) == 1 && results[0].Status == "error" {
		writeJSON(w, http.StatusOK, apiError("插件更新失败: "+results[0].Message))
		return
	}
	failed := 0
	for _, r := range results {
		if r.Status != "ok" {
			failed++
		}
	}
	msg := "更新完成"
	if failed > 0 {
		msg = fmt.Sprintf("更新完成，其中 %d/%d 个插件失败", failed, len(results))
	}
	writeJSON(w, http.StatusOK, apiOKMsg(msg, map[string]interface{}{
		"results": results,
	}))
}

// handlePluginBindSource binds an installed plugin to a marketplace/repository
// source (POST /api/v1/plugins/{plugin_id}/source), persisting the install
// source so reinstall/update resolves from the new registry.
func (s *Server) handlePluginBindSource(w http.ResponseWriter, r *http.Request, pluginID string) {
	if s.subPluginMgr == nil {
		writeJSON(w, http.StatusOK, apiError("插件子系统未初始化"))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusOK, apiError("method not allowed"))
		return
	}

	var body struct {
		InstallMethod  string `json:"install_method"`
		RegistryURL    string `json:"registry_url"`
		RegistryName   string `json:"registry_name"`
		MarketPluginID string `json:"market_plugin_id"`
		Repo           string `json:"repo"`
		DownloadURL    string `json:"download_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	pid, _, ok := s.resolveSubprocessPlugin(pluginID)
	if !ok {
		writeJSON(w, http.StatusOK, apiError("插件不存在或非子进程插件"))
		return
	}
	method := strings.ToLower(strings.TrimSpace(body.InstallMethod))
	if method != "" && method != "market" && method != "repository" {
		writeJSON(w, http.StatusOK, apiError("不支持的插件源类型: "+method))
		return
	}
	if err := s.subPluginMgr.BindSource(pid, method, body.RegistryURL, body.RegistryName,
		body.MarketPluginID, body.Repo, body.DownloadURL); err != nil {
		writeJSON(w, http.StatusOK, apiError("绑定插件源失败: "+err.Error()))
		return
	}
	s.notifyPluginsChanged()
	writeJSON(w, http.StatusOK, apiOKMsg("插件源已更新", map[string]interface{}{}))
}

// handlePluginDocs serves plugin README/CHANGELOG content
// (GET /api/v1/plugins/{id}/readme, /changelog and the query variants) from
// the subprocess runtime's cached plugin docs.
func (s *Server) handlePluginDocs(w http.ResponseWriter, r *http.Request, subprocess func(string) string) {
	pluginID := r.URL.Query().Get("plugin_id")
	if pluginID == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == "plugins" && i+1 < len(parts) {
				pluginID = parts[i+1]
				break
			}
		}
	}
	if pluginID == "" {
		writeJSON(w, http.StatusOK, apiError("缺少插件 ID"))
		return
	}

	if s.subPluginMgr != nil && subprocess != nil {
		content := subprocess(pluginID)
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"content": content,
		}))
		return
	}

	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"content": "",
	}))
}

// idFromSource derives a stable plugin id from an install source.
func idFromSource(source string) string {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		if u, err := url.Parse(source); err == nil {
			seg := strings.Trim(u.Path, "/")
			if i := strings.LastIndex(seg, "/"); i >= 0 {
				seg = seg[i+1:]
			}
			seg = strings.TrimSuffix(seg, ".git")
			if seg != "" {
				return seg
			}
		}
	}
	base := strings.TrimSuffix(filepath.Base(source), ".zip")
	base = strings.TrimSuffix(base, ".tar.gz")
	base = strings.TrimSuffix(base, ".tgz")
	if base != "" && base != "." && base != "/" {
		return base
	}
	return "plugin"
}

// ── Knowledge base handlers ──────────────────────────────────

func (s *Server) handleKB(w http.ResponseWriter, r *http.Request, parts []string) {
	// RESTful style: /knowledge-bases (GET list, POST create) and
	// /knowledge-bases/{kb_id} (GET/PUT/DELETE). Also accept the legacy
	// /knowledge_base/list|create|update|delete|by-id sub-paths.
	method := r.Method
	if len(parts) > 0 && parts[0] == "tasks" {
		// GET /knowledge-bases/tasks/{task_id} — WebUI polls upload progress.
		if len(parts) > 1 {
			taskID := parts[1]
			task := s.getKBTask(taskID)
			if task == nil {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"status": "unknown",
				}))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"status": task.Status,
				"result": map[string]interface{}{
					"success_count": task.SuccessCount,
					"failed_count":  task.FailedCount,
					"error":         task.Error,
				},
				"progress": map[string]interface{}{
					"stage":      task.Stage,
					"file_index": task.FileIndex,
					"current":    task.Current,
					"total":      task.Total,
				},
			}))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"tasks": []interface{}{},
		}))
		return
	}
	// Sub-path form: /knowledge_base/<verb>?kb_id=...
	if len(parts) > 0 {
		switch parts[0] {
		case "list":
			s.writeKBList(w, r)
			return
		case "create":
			if method == http.MethodPost {
				var body map[string]interface{}
				_ = json.NewDecoder(r.Body).Decode(&body)
				kb, err := s.createKB(body)
				if err != nil {
					writeJSON(w, http.StatusOK, apiError("创建知识库失败: "+err.Error()))
					return
				}
				writeJSON(w, http.StatusOK, apiOKMsg("创建知识库成功", kb))
				return
			}
		case "update":
			kbID := r.URL.Query().Get("kb_id")
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if kbID == "" {
				if v, _ := body["kb_id"].(string); v != "" {
					kbID = v
				}
			}
			if kbID == "" {
				writeJSON(w, http.StatusOK, apiError("缺少参数 kb_id"))
				return
			}
			kb, err := s.updateKB(kbID, body)
			if err != nil {
				writeJSON(w, http.StatusOK, apiError("更新知识库失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("更新知识库成功", kb))
			return
		case "delete":
			kbID := r.URL.Query().Get("kb_id")
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if kbID == "" {
				if v, _ := body["kb_id"].(string); v != "" {
					kbID = v
				}
			}
			if kbID == "" {
				writeJSON(w, http.StatusOK, apiError("缺少参数 kb_id"))
				return
			}
			if err := s.deleteKB(kbID); err != nil {
				writeJSON(w, http.StatusOK, apiError("删除知识库失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("删除知识库成功", map[string]interface{}{}))
			return
		case "by-id":
			kbID := r.URL.Query().Get("kb_id")
			if kbID == "" {
				writeJSON(w, http.StatusOK, apiError("缺少参数 kb_id"))
				return
			}
			kb := s.getKBByID(kbID)
			if kb == nil {
				writeJSON(w, http.StatusOK, apiError("知识库不存在"))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(kb))
			return
		case "documents":
			kbID := r.URL.Query().Get("kb_id")
			s.handleKBDocuments(w, r, kbID, parts[1:])
			return
		case "chunks":
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"chunks": []interface{}{},
			}))
			return
		case "retrieve":
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"results": []interface{}{},
				"total":   0,
				"query":   "",
			}))
			return
		}
		// Fall through: parts[0] is a kb_id in the RESTful path form.
		kbID := parts[0]
		// Nested sub-resources: /knowledge-bases/{kb_id}/<resource>
		if len(parts) > 1 {
			switch parts[1] {
			case "documents":
				s.handleKBDocuments(w, r, kbID, parts[2:])
				return
			case "chunks":
				// List chunks from the SQLite index (list source of truth).
				docID := r.URL.Query().Get("doc_id")
				chunks, err := s.database.ListKBChunks(kbID, docID)
				items := []interface{}{}
				if err == nil {
					for _, c := range chunks {
						items = append(items, map[string]interface{}{
							"chunk_id":    c.ChunkID,
							"doc_id":      c.DocID,
							"doc_name":    c.DocName,
							"content":     c.Content,
							"chunk_index": c.ChunkIdx,
						})
					}
				}
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"items":     items,
					"page":      1,
					"page_size": len(items),
					"total":     len(items),
				}))
				return
			case "stats":
				docCount, _ := s.kbDocCounts(kbID)
				chunkCount, _ := s.database.CountKBChunks(kbID, "")
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"document_count": docCount,
					"chunk_count":    chunkCount,
				}))
				return
			case "retrieve":
				var body map[string]interface{}
				_ = json.NewDecoder(r.Body).Decode(&body)
				query, _ := body["query"].(string)
				if strings.TrimSpace(query) == "" {
					query = r.URL.Query().Get("query")
				}
				if query == "" {
					writeJSON(w, http.StatusOK, apiError("缺少查询内容"))
					return
				}
				topK := 5
				if v, ok := body["top_k"].(float64); ok && int(v) > 0 {
					topK = int(v)
				}
				results, err := s.kbRetrieve(kbID, query, topK)
				out := []interface{}{}
				if err == nil {
					for _, r := range results {
						out = append(out, map[string]interface{}{
							"chunk_id": r.ChunkID,
							"doc_id":   r.DocID,
							"doc_name": r.DocName,
							"score":    r.Score,
							"content":  r.Content,
						})
					}
				}
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"results": out,
					"total":   len(out),
					"query":   query,
				}))
				return
			default:
				writeJSON(w, http.StatusOK, apiError("未知的知识库子资源: "+parts[1]))
				return
			}
		}
		if method == http.MethodGet {
			kb := s.getKBByID(kbID)
			if kb == nil {
				writeJSON(w, http.StatusOK, apiError("知识库不存在"))
				return
			}
			writeJSON(w, http.StatusOK, apiOK(kb))
			return
		}
		if method == http.MethodPut {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			kb, err := s.updateKB(kbID, body)
			if err != nil {
				writeJSON(w, http.StatusOK, apiError("更新知识库失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("更新知识库成功", kb))
			return
		}
		if method == http.MethodDelete {
			if err := s.deleteKB(kbID); err != nil {
				writeJSON(w, http.StatusOK, apiError("删除知识库失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("删除知识库成功", map[string]interface{}{}))
			return
		}
		writeJSON(w, http.StatusOK, apiError("不支持的请求方法: "+method))
		return
	}

	// No sub-path: GET → list, POST → create.
	if method == http.MethodPost {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		kb, err := s.createKB(body)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("创建知识库失败: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOKMsg("创建知识库成功", kb))
		return
	}
	s.writeKBList(w, r)
}

// writeKBList writes the paginated KB list in the WebUI's expected shape
// {items, page, page_size, total}.
func (s *Server) writeKBList(w http.ResponseWriter, r *http.Request) {
	page := 1
	pageSize := 20
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	var all []interface{}
	if s.database != nil {
		rows, err := s.database.ListKBs()
		if err == nil {
			all = make([]interface{}, 0, len(rows))
			for i := range rows {
				all = append(all, s.kbRowToMap(&rows[i]))
			}
		}
	}
	total := len(all)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	items := all[start:end]
	if items == nil {
		items = []interface{}{}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"items":     items,
		"page":      page,
		"page_size": pageSize,
		"total":     total,
	}))
}

// handleKBDocuments is a stub for document endpoints (uploads / listing).
// kbUploadTask tracks a knowledge-base document upload task so the WebUI can
// poll its progress. The Go runtime saves uploaded files directly (it does not
// yet perform chunking/embedding), so a task is marked completed immediately
// after the files are persisted.
type kbUploadTask struct {
	TaskID       string
	Status       string // "completed" | "failed"
	Stage        string
	FileIndex    int
	Current      int
	Total        int
	SuccessCount int
	FailedCount  int
	Error        string
}

// getKBTask returns the stored upload task, or nil.
func (s *Server) getKBTask(taskID string) *kbUploadTask {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.kbTasks[taskID]
}

// recordKBTask stores a knowledge-base upload task state.
func (s *Server) recordKBTask(t *kbUploadTask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kbTasks[t.TaskID] = t
}

// handleKBDocuments handles document endpoints: list (GET), upload (POST
// multipart), and URL import. kbID comes from the RESTful path
// /knowledge-bases/{kb_id}/documents; for the legacy sub-path form the caller
// passes it via parts[0].
func (s *Server) handleKBDocuments(w http.ResponseWriter, r *http.Request, kbID string, parts []string) {
	if len(parts) > 0 && parts[0] == "import-url" {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		url, _ := body["url"].(string)
		if url == "" || (!strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://")) {
			writeJSON(w, http.StatusOK, apiError("缺少或无效的参数 url"))
			return
		}
		chunkSize := 512
		chunkOverlap := 50
		if v, ok := body["chunk_size"].(float64); ok && v > 0 {
			chunkSize = int(v)
		}
		if v, ok := body["chunk_overlap"].(float64); ok && v >= 0 {
			chunkOverlap = int(v)
		}

		// Download the remote content with a bounded timeout and size limit.
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("创建下载请求失败: "+err.Error()))
			return
		}
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("下载文档失败: "+err.Error()))
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			writeJSON(w, http.StatusOK, apiError(fmt.Sprintf("下载文档失败: HTTP %d", resp.StatusCode)))
			return
		}
		content, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			writeJSON(w, http.StatusOK, apiError("读取文档内容失败: "+err.Error()))
			return
		}
		if len(content) == 0 {
			writeJSON(w, http.StatusOK, apiError("下载的文档内容为空"))
			return
		}

		// Save under the KB documents directory like the multipart upload path.
		dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeJSON(w, http.StatusOK, apiError("创建知识库数据目录失败: "+err.Error()))
			return
		}
		name := filepath.Base(strings.TrimRight(url, "/"))
		if name == "" || name == "." || name == "/" {
			name = "imported"
		}
		dst := filepath.Join(dir, sanitizePath(name))
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			writeJSON(w, http.StatusOK, apiError("保存文档失败: "+err.Error()))
			return
		}
		mod := time.Now().UnixNano()
		docID := fmt.Sprintf("doc_%d_%s", mod, name)
		taskID := fmt.Sprintf("task_%d", time.Now().UnixNano())
		s.recordKBTask(&kbUploadTask{
			TaskID: taskID,
			Status: "processing",
			Stage:  "chunking",
			Total:  100,
		})

		// Index asynchronously (chunk → embed → dual write), same as uploads.
		go func() {
			if _, err := s.indexKBFile(kbID, docID, name, content, chunkSize, chunkOverlap); err != nil {
				s.recordKBTask(&kbUploadTask{
					TaskID:       taskID,
					Status:       "completed",
					Stage:        "embedding_failed",
					Current:      100,
					Total:        100,
					SuccessCount: 0,
					FailedCount:  1,
					Error:        err.Error(),
				})
				return
			}
			s.recordKBTask(&kbUploadTask{
				TaskID:       taskID,
				Status:       "completed",
				Stage:        "completed",
				Current:      100,
				Total:        100,
				SuccessCount: 1,
				FailedCount:  0,
			})
		}()

		writeJSON(w, http.StatusOK, apiOKMsg("文档导入任务已创建", map[string]interface{}{
			"task_id":    taskID,
			"file_count": 1,
			"documents": []map[string]interface{}{{
				"doc_id":    docID,
				"doc_name":  name,
				"file_size": len(content),
			}},
		}))
		return
	}
	// GET /knowledge-bases/{kb_id}/documents/{document_id} — single doc detail.
	if len(parts) > 0 && (r.Method == http.MethodGet || r.Method == http.MethodDelete) {
		docID := parts[0]
		if r.Method == http.MethodDelete {
			// Delete nanovec vectors first, then SQLite chunk rows, then the
			// on-disk file.
			_ = s.kbDeleteDoc(kbID, docID)
			dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
			if doc := s.kbDocumentByID(kbID, docID); doc != nil {
				_ = os.Remove(filepath.Join(dir, sanitizePath(anyStr(doc["doc_name"]))))
			}
			writeJSON(w, http.StatusOK, apiOKMsg("文档已删除", map[string]interface{}{}))
			return
		}
		if doc := s.kbDocumentByID(kbID, docID); doc != nil {
			writeJSON(w, http.StatusOK, apiOK(doc))
			return
		}
		writeJSON(w, http.StatusOK, apiError("文档不存在"))
		return
	}
	if r.Method == http.MethodPost {
		// Multipart upload: save the files under the KB's data directory, then
		// index them asynchronously (chunk → embed → SQLite + nanovec dual
		// write). The WebUI polls the returned task for progress.
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeJSON(w, http.StatusOK, apiError("解析上传文件失败: "+err.Error()))
			return
		}
		chunkSize := 512
		chunkOverlap := 50
		if v := r.FormValue("chunk_size"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				chunkSize = n
			}
		}
		if v := r.FormValue("chunk_overlap"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				chunkOverlap = n
			}
		}

		dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeJSON(w, http.StatusOK, apiError("创建知识库数据目录失败: "+err.Error()))
			return
		}
		var docs []struct {
			DocID string
			Name  string
			Path  string
		}
		for key, files := range r.MultipartForm.File {
			if key != "file" && !strings.HasPrefix(key, "file") && key != "files[]" {
				continue
			}
			for _, fh := range files {
				src, err := fh.Open()
				if err != nil {
					continue
				}
				name := filepath.Base(fh.Filename)
				if name == "" || name == "." {
					name = "document"
				}
				dst := filepath.Join(dir, sanitizePath(name))
				out, err := os.Create(dst)
				if err != nil {
					src.Close()
					continue
				}
				if _, err := io.Copy(out, src); err != nil {
					out.Close()
					src.Close()
					continue
				}
				out.Close()
				src.Close()
				info, _ := os.Stat(dst)
				mod := int64(0)
				if info != nil {
					mod = info.ModTime().UnixNano()
				}
				docID := fmt.Sprintf("doc_%d_%s", mod, name)
				docs = append(docs, struct {
					DocID string
					Name  string
					Path  string
				}{DocID: docID, Name: name, Path: dst})
			}
		}
		if len(docs) == 0 {
			writeJSON(w, http.StatusOK, apiError("缺少文件"))
			return
		}
		taskID := fmt.Sprintf("task_%d", time.Now().UnixNano())
		s.recordKBTask(&kbUploadTask{
			TaskID: taskID,
			Status: "processing",
			Stage:  "chunking",
			Total:  len(docs) * 100,
		})

		saved := make([]map[string]interface{}, 0, len(docs))
		for _, d := range docs {
			saved = append(saved, map[string]interface{}{
				"doc_id":   d.DocID,
				"doc_name": d.Name,
				"file_size": func() int64 {
					if fi, err := os.Stat(d.Path); err == nil {
						return fi.Size()
					}
					return 0
				}(),
			})
		}

		// Index asynchronously: chunk each file, embed, dual-write.
		go func() {
			success, failed := 0, 0
			total := len(docs)
			for i, d := range docs {
				s.recordKBTask(&kbUploadTask{
					TaskID:       taskID,
					Status:       "processing",
					Stage:        "chunking",
					FileIndex:    i,
					Current:      i * 100,
					Total:        total * 100,
					SuccessCount: success,
					FailedCount:  failed,
				})
				content, err := os.ReadFile(d.Path)
				if err != nil {
					failed++
					continue
				}
				if _, err := s.indexKBFile(kbID, d.DocID, d.Name, content, chunkSize, chunkOverlap); err != nil {
					// The file is saved; SQLite records may exist. Report failure
					// for the vector index but keep the doc listed.
					failed++
					s.recordKBTask(&kbUploadTask{
						TaskID:       taskID,
						Status:       "processing",
						Stage:        "embedding_failed",
						FileIndex:    i,
						Current:      (i + 1) * 100,
						Total:        total * 100,
						SuccessCount: success,
						FailedCount:  failed,
						Error:        err.Error(),
					})
					continue
				}
				success++
			}
			status := "completed"
			if failed > 0 {
				status = "completed"
			}
			s.recordKBTask(&kbUploadTask{
				TaskID:       taskID,
				Status:       status,
				Stage:        "completed",
				Current:      total * 100,
				Total:        total * 100,
				SuccessCount: success,
				FailedCount:  failed,
			})
		}()

		writeJSON(w, http.StatusOK, apiOKMsg("文档上传成功，正在后台分块", map[string]interface{}{
			"task_id":    taskID,
			"file_count": len(docs),
			"documents":  saved,
		}))
		return
	}
	// GET: list documents stored under the KB data directory.
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	items := []interface{}{}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, _ := e.Info()
			size := int64(0)
			mod := int64(0)
			modTime := time.Time{}
			if info != nil {
				size = info.Size()
				mod = info.ModTime().UnixNano()
				modTime = info.ModTime()
			}
			docID := fmt.Sprintf("doc_%d_%s", mod, e.Name())
			chunkCount := 0
			if s.database != nil {
				if n, err := s.database.CountKBChunks(kbID, docID); err == nil {
					chunkCount = n
				}
			}
			items = append(items, map[string]interface{}{
				"doc_id":      docID,
				"doc_name":    e.Name(),
				"file_type":   strings.TrimPrefix(filepath.Ext(e.Name()), "."),
				"file_size":   size,
				"created_at":  modTime.Format(time.RFC3339),
				"chunk_count": chunkCount,
			})
		}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"items":     items,
		"page":      1,
		"page_size": len(items),
		"total":     len(items),
	}))
}

// kbDocumentByID finds a document file in a KB's data directory by its doc_id.
// The doc_id encodes "<modtime>_<filename>"; the file name is the part after
// the first underscore so re-listing and detail both resolve to the same doc.
func (s *Server) kbDocumentByID(kbID, docID string) map[string]interface{} {
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, _ := e.Info()
		mod := int64(0)
		if info != nil {
			mod = info.ModTime().UnixNano()
		}
		candidate := fmt.Sprintf("doc_%d_%s", mod, e.Name())
		if candidate != docID {
			continue
		}
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		content, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		chunkCount := 0
		if s.database != nil {
			if n, err := s.database.CountKBChunks(kbID, candidate); err == nil {
				chunkCount = n
			}
		}
		created := ""
		if info != nil {
			created = info.ModTime().Format(time.RFC3339)
		}
		return map[string]interface{}{
			"doc_id":      candidate,
			"doc_name":    e.Name(),
			"file_type":   strings.TrimPrefix(filepath.Ext(e.Name()), "."),
			"file_size":   size,
			"content":     string(content),
			"chunk_count": chunkCount,
			"created_at":  created,
		}
	}
	return nil
}

// anyStr extracts a string from an interface value.
func anyStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// getKBByID returns a single knowledge base as a serializable map.
func (s *Server) getKBByID(kbID string) map[string]interface{} {
	if s.database == nil {
		return nil
	}
	row, err := s.database.GetKB(kbID)
	if err != nil {
		return nil
	}
	return s.kbRowToMap(row)
}

// createKB creates a knowledge base: validates the embedding provider, stores
// it in the in-memory manager and persists it to SQLite.
func (s *Server) createKB(body map[string]interface{}) (map[string]interface{}, error) {
	kbName, _ := body["kb_name"].(string)
	kbName = strings.TrimSpace(kbName)
	if kbName == "" {
		return nil, fmt.Errorf("知识库名称不能为空")
	}
	embeddingProviderID, _ := body["embedding_provider_id"].(string)
	if strings.TrimSpace(embeddingProviderID) == "" {
		return nil, fmt.Errorf("缺少参数 embedding_provider_id")
	}
	rerankProviderID, _ := body["rerank_provider_id"].(string)

	// Validate the embedding provider exists and is an embedding provider
	// (checked against the config's provider_type, since runtime providers are
	// created lazily during pipeline runs).
	embeddingOK := false
	cfg := s.getConfigSnapshot()
	if providers, ok := cfg["provider"].([]interface{}); ok {
		for _, p := range providers {
			m, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if pid, _ := m["id"].(string); pid == embeddingProviderID {
				ptype, _ := m["provider_type"].(string)
				embeddingOK = ptype == "embedding"
				break
			}
		}
	}
	if !embeddingOK {
		return nil, fmt.Errorf("嵌入模型不存在或类型错误: %s", embeddingProviderID)
	}

	// In-memory manager (mirrors Python's kb_manager.create_kb).
	row := db.KBRow{
		KBID:                fmt.Sprintf("kb_%d", time.Now().UnixNano()),
		KBName:              kbName,
		Description:         strString(body, "description"),
		Emoji:               strString(body, "emoji"),
		EmbeddingProviderID: embeddingProviderID,
		RerankProviderID:    rerankProviderID,
		ChunkSize:           strInt(body, "chunk_size", 512),
		ChunkOverlap:        strInt(body, "chunk_overlap", 50),
		TopKDense:           strInt(body, "top_k_dense", 50),
		TopKSparse:          strInt(body, "top_k_sparse", 50),
		TopMFinal:           strInt(body, "top_m_final", 5),
	}
	if row.Emoji == "" {
		row.Emoji = "📚"
	}

	if err := s.database.CreateKB(row); err != nil {
		return nil, fmt.Errorf("持久化知识库失败: %w", err)
	}
	kb := s.kbRowToMap(&row)
	kb["created_at"] = time.Now().Format(time.RFC3339)
	kb["updated_at"] = time.Now().Format(time.RFC3339)
	return kb, nil
}

// updateKB updates an existing knowledge base.
func (s *Server) updateKB(kbID string, body map[string]interface{}) (map[string]interface{}, error) {
	if s.database == nil {
		return nil, fmt.Errorf("数据库不可用")
	}
	existing, err := s.database.GetKB(kbID)
	if err != nil {
		return nil, fmt.Errorf("知识库不存在")
	}
	// Only update provided fields (aligns with Python's update_keys logic).
	if v, ok := body["kb_name"].(string); ok && v != "" {
		existing.KBName = v
	}
	if v, ok := body["description"].(string); ok {
		existing.Description = v
	}
	if v, ok := body["emoji"].(string); ok && v != "" {
		existing.Emoji = v
	}
	if v, ok := body["embedding_provider_id"].(string); ok && v != "" {
		existing.EmbeddingProviderID = v
	}
	if v, ok := body["rerank_provider_id"].(string); ok {
		existing.RerankProviderID = v
	}
	if v, ok := body["chunk_size"].(float64); ok && v > 0 {
		existing.ChunkSize = int(v)
	}
	if v, ok := body["chunk_overlap"].(float64); ok && v >= 0 {
		existing.ChunkOverlap = int(v)
	}
	if v, ok := body["top_k_dense"].(float64); ok && v > 0 {
		existing.TopKDense = int(v)
	}
	if v, ok := body["top_k_sparse"].(float64); ok && v > 0 {
		existing.TopKSparse = int(v)
	}
	if v, ok := body["top_m_final"].(float64); ok && v > 0 {
		existing.TopMFinal = int(v)
	}
	if err := s.database.UpdateKB(kbID, *existing); err != nil {
		return nil, fmt.Errorf("持久化知识库失败: %w", err)
	}
	return s.kbRowToMap(existing), nil
}

// deleteKB removes a knowledge base.
func (s *Server) deleteKB(kbID string) error {
	if s.database == nil {
		return fmt.Errorf("数据库不可用")
	}
	// 级联清理 1：先删除该 KB 的全部分块行（knowledge_base_chunks），
	// 避免只删 knowledge_bases 行后留下孤儿分块。
	if err := s.database.DeleteKBChunks(kbID, ""); err != nil {
		return fmt.Errorf("清理知识库分块失败: %w", err)
	}
	if err := s.database.DeleteKB(kbID); err != nil {
		return err
	}
	// 级联清理 2：删除磁盘数据目录 data/knowledge_bases/<id>/，
	// 其中 documents/ 与 nanovec 的 .store/.idx（.vec.db）文件一并移除。
	kbDir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID))
	if err := os.RemoveAll(kbDir); err != nil {
		return fmt.Errorf("删除知识库数据目录失败: %w", err)
	}
	// 级联清理 3：释放该 KB 的向量写锁（kbVecMu 中的常驻 Mutex），
	// 防止每 KB 一把锁随删除积累导致内存泄漏。
	kbVecMu.Delete(kbID)
	// Remove from in-memory manager too.
	if km, ok := s.kbMgr.(interface {
		DeleteKB(kbID string) bool
	}); ok {
		km.DeleteKB(kbID)
	}
	return nil
}

// kbRowToMap serializes a KB row for the WebUI.
// kbDataDir returns the runtime data directory (parent of the config file, i.e.
// the project's data/ dir).
func (s *Server) kbDataDir() string {
	if s.dataDir != "" {
		return s.dataDir
	}
	return "data"
}

// sanitizePath makes a path segment safe for use in file names.
func sanitizePath(p string) string {
	p = filepath.Base(strings.ReplaceAll(p, "\\", "/"))
	var b strings.Builder
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r == ' ', r >= 0x4e00 && r <= 0x9fff:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// kbRowToMap serializes a KB row for the WebUI, including the document/chunk
// counts the KB list page shows (computed from the KB's data directory).
func (s *Server) kbRowToMap(row *db.KBRow) map[string]interface{} {
	if row == nil {
		return nil
	}
	docCount, chunkCount := s.kbDocCounts(row.KBID)
	return map[string]interface{}{
		"kb_id":                 row.KBID,
		"kb_name":               row.KBName,
		"description":           row.Description,
		"emoji":                 row.Emoji,
		"embedding_provider_id": row.EmbeddingProviderID,
		"rerank_provider_id":    row.RerankProviderID,
		"chunk_size":            row.ChunkSize,
		"chunk_overlap":         row.ChunkOverlap,
		"top_k_dense":           row.TopKDense,
		"top_k_sparse":          row.TopKSparse,
		"top_m_final":           row.TopMFinal,
		"doc_count":             docCount,
		"chunk_count":           chunkCount,
		"created_at":            row.CreatedAt.In(time.Local).Format(time.RFC3339),
		"updated_at":            row.UpdatedAt.In(time.Local).Format(time.RFC3339),
	}
}

// kbDocCounts returns the number of documents stored on disk and the number of
// indexed chunks (from the SQLite list source of truth).
func (s *Server) kbDocCounts(kbID string) (docCount, chunkCount int) {
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				docCount++
			}
		}
	}
	if s.database != nil {
		if n, err := s.database.CountKBChunks(kbID, ""); err == nil {
			chunkCount = n
		}
	}
	return docCount, chunkCount
}

// strString reads a string field from a request body map.
func strString(m map[string]interface{}, key string) string {
	v, _ := m[key].(string)
	return v
}

// strInt reads an int field from a request body map (JSON numbers decode as
// float64).
func strInt(m map[string]interface{}, key string, def int) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return def
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
			s.batchUpdateSessionProvider(w, r)
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "rules":
		s.handleSessionRules(w, r, parts)
	case "service":
		if r.Method == http.MethodPatch {
			s.batchUpdateSessionService(w, r)
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	case "batch-delete":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"deleted": 0,
		}))
	case "session-groups":
		s.handleSessionGroups(w, r, parts)
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

// conversationManager returns the conversation manager or nil.
func (s *Server) conversationManager() *conversation.Manager {
	if cm, ok := s.conversationMgr.(*conversation.Manager); ok {
		return cm
	}
	return nil
}

// sessionGroupsPreference is where the session-group map is stored in the
// preferences table (mirrors Python sp.get_async("unknown","unknown",
// "session_groups")).
const sessionGroupsPreference = "session_groups"

// getSessionGroups loads the {group_id: {name, umos}} map.
func (s *Server) getSessionGroups() map[string]map[string]interface{} {
	if s.database == nil {
		return map[string]map[string]interface{}{}
	}
	val, found, _ := s.database.GetPreference("unknown", "unknown", sessionGroupsPreference)
	if !found || val == "" {
		return map[string]map[string]interface{}{}
	}
	var groups map[string]map[string]interface{}
	if json.Unmarshal([]byte(val), &groups) != nil || groups == nil {
		return map[string]map[string]interface{}{}
	}
	return groups
}

func (s *Server) saveSessionGroups(groups map[string]map[string]interface{}) error {
	if s.database == nil {
		return errors.New("数据库不可用")
	}
	data, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	return s.database.SetPreference("unknown", "unknown", sessionGroupsPreference, string(data))
}

func (s *Server) sessionGroupMap(g map[string]interface{}, id string) map[string]interface{} {
	name, _ := g["name"].(string)
	umos, _ := g["umos"].([]interface{})
	return map[string]interface{}{
		"id":        id,
		"name":      name,
		"umos":      umos,
		"umo_count": len(umos),
	}
}

// handleSessionGroups implements session-group CRUD:
// GET /session-groups, POST /session-groups, PUT/DELETE /session-groups/{id}.
func (s *Server) handleSessionGroups(w http.ResponseWriter, r *http.Request, parts []string) {
	groupID := ""
	if len(parts) > 1 {
		groupID = parts[1]
	}
	switch r.Method {
	case http.MethodGet:
		groups := s.getSessionGroups()
		list := make([]interface{}, 0, len(groups))
		for id, g := range groups {
			list = append(list, s.sessionGroupMap(g, id))
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"groups": list}))
	case http.MethodPost:
		var body struct {
			Name string   `json:"name"`
			Umos []string `json:"umos"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.TrimSpace(body.Name) == "" {
			writeJSON(w, http.StatusOK, apiError("分组名称不能为空"))
			return
		}
		groups := s.getSessionGroups()
		id := strings.ToLower(generateRandomToken(4))
		umos := make([]interface{}, 0, len(body.Umos))
		for _, u := range body.Umos {
			umos = append(umos, u)
		}
		groups[id] = map[string]interface{}{"name": body.Name, "umos": umos}
		if err := s.saveSessionGroups(groups); err != nil {
			writeJSON(w, http.StatusOK, apiError("保存分组失败: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": fmt.Sprintf("分组 '%s' 创建成功", body.Name),
			"group":   s.sessionGroupMap(groups[id], id),
		}))
	case http.MethodPut:
		if groupID == "" {
			writeJSON(w, http.StatusOK, apiError("分组 ID 不能为空"))
			return
		}
		var body struct {
			Name string   `json:"name"`
			Umos []string `json:"umos"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		groups := s.getSessionGroups()
		g, ok := groups[groupID]
		if !ok {
			writeJSON(w, http.StatusOK, apiError(fmt.Sprintf("分组 '%s' 不存在", groupID)))
			return
		}
		if strings.TrimSpace(body.Name) != "" {
			g["name"] = body.Name
		}
		if body.Umos != nil {
			umos := make([]interface{}, 0, len(body.Umos))
			for _, u := range body.Umos {
				umos = append(umos, u)
			}
			g["umos"] = umos
		}
		if err := s.saveSessionGroups(groups); err != nil {
			writeJSON(w, http.StatusOK, apiError("保存分组失败: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": fmt.Sprintf("分组 '%s' 更新成功", g["name"]),
			"group":   s.sessionGroupMap(g, groupID),
		}))
	case http.MethodDelete:
		if groupID == "" {
			writeJSON(w, http.StatusOK, apiError("分组 ID 不能为空"))
			return
		}
		groups := s.getSessionGroups()
		if _, ok := groups[groupID]; !ok {
			writeJSON(w, http.StatusOK, apiError(fmt.Sprintf("分组 '%s' 不存在", groupID)))
			return
		}
		delete(groups, groupID)
		if err := s.saveSessionGroups(groups); err != nil {
			writeJSON(w, http.StatusOK, apiError("删除分组失败: "+err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"message": "分组已删除"}))
	default:
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
	}
}

// handleSessionRules implements GET/POST /sessions/rules and
// POST /sessions/rules/delete (list / upsert / delete).
func (s *Server) handleSessionRules(w http.ResponseWriter, r *http.Request, parts []string) {
	cm := s.conversationManager()
	if cm == nil {
		writeJSON(w, http.StatusOK, apiError("会话管理不可用"))
		return
	}
	if len(parts) > 1 && parts[1] == "delete" {
		var body struct {
			UMO     string   `json:"umo"`
			UMOs    []string `json:"umos"`
			RuleKey string   `json:"rule_key"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		umos := body.UMOs
		if body.UMO != "" {
			umos = append(umos, body.UMO)
		}
		deleted := 0
		for _, umo := range umos {
			if err := cm.DeleteSessionRule(umo, body.RuleKey); err == nil {
				deleted++
			}
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"deleted": deleted}))
		return
	}
	if r.Method == http.MethodPost {
		var body struct {
			UMO       string      `json:"umo"`
			RuleKey   string      `json:"rule_key"`
			RuleValue interface{} `json:"rule_value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusOK, apiError("无效的请求体"))
			return
		}
		if body.UMO == "" || body.RuleKey == "" {
			writeJSON(w, http.StatusOK, apiError("缺少必要参数: umo / rule_key"))
			return
		}
		if err := cm.SetSessionRule(body.UMO, body.RuleKey, body.RuleValue); err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"message": "规则 " + body.RuleKey + " 已更新",
			"umo":     body.UMO,
		}))
		return
	}
	// GET list.
	page := 1
	pageSize := 10
	search := r.URL.Query().Get("search")
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
		}
	}
	if v := r.URL.Query().Get("page_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			pageSize = n
		}
	}
	all, err := cm.ListAllSessionRules()
	if err != nil {
		writeJSON(w, http.StatusOK, apiError("读取规则失败: "+err.Error()))
		return
	}
	// The main table lists sessions that have at least one rule; all sessions
	// are available in the batch-operations area via /sessions/active-umos.
	umos := make([]string, 0, len(all))
	for umo := range all {
		umos = append(umos, umo)
	}
	sort.Strings(umos)
	infos := make(map[string]map[string]interface{}, len(umos))
	for _, umo := range umos {
		infos[umo] = conversation.BuildUMOInfo(umo)
	}
	// Search by umo, custom_name, or auto display name.
	if search != "" {
		sl := strings.ToLower(search)
		var filtered []string
		for _, umo := range umos {
			if strings.Contains(strings.ToLower(umo), sl) {
				filtered = append(filtered, umo)
				continue
			}
			if rules, ok := all[umo]; ok {
				if sc, ok := rules[conversation.RuleServiceConfig].(map[string]interface{}); ok {
					if name, ok := sc["custom_name"].(string); ok && strings.Contains(strings.ToLower(name), sl) {
						filtered = append(filtered, umo)
						continue
					}
				}
			}
			if info, ok := infos[umo]; ok {
				for _, k := range []string{"auto_name", "user_alias", "display_name"} {
					if v, ok := info[k].(string); ok && v != "" && strings.Contains(strings.ToLower(v), sl) {
						filtered = append(filtered, umo)
						break
					}
				}
			}
		}
		umos = filtered
	}
	total := len(umos)
	start := (page - 1) * pageSize
	end := start + pageSize
	if start > total {
		start = total
	}
	if end > total {
		end = total
	}
	rulesList := make([]interface{}, 0, len(umos))
	for _, umo := range umos[start:end] {
		item := conversation.BuildUMOInfo(umo)
		rules, has := all[umo]
		if !has {
			rules = map[string]interface{}{}
		}
		item["rules"] = rules
		rulesList = append(rulesList, item)
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"rules":                    rulesList,
		"total":                    total,
		"page":                     page,
		"page_size":                pageSize,
		"available_personas":       s.getAvailablePersonas(),
		"available_chat_providers": s.getAvailableChatProviders(),
		"available_stt_providers":  []interface{}{},
		"available_tts_providers":  []interface{}{},
		"available_plugins":        s.getAvailablePlugins(),
		"available_kbs":            s.getAvailableKBs(),
		"available_rule_keys":      conversation.AvailableSessionRuleKeys,
	}))
}

// followConfigValue matches the WebUI sentinel "__astrbot_follow_config__".
const followConfigValue = "__astrbot_follow_config__"

// resolveScopeUMOs resolves a batch-operation scope into concrete umos.
func (s *Server) resolveScopeUMOs(scope string, umos []string, groupID string) []string {
	cm := s.conversationManager()
	if cm == nil {
		return umos
	}
	all := cm.ActiveUMOs()
	switch scope {
	case "all":
		return all
	case "selected", "":
		if len(umos) > 0 {
			return umos
		}
		return all
	case "private":
		var out []string
		for _, umo := range all {
			if !strings.Contains(umo, ":group:") {
				out = append(out, umo)
			}
		}
		return out
	case "group":
		var out []string
		for _, umo := range all {
			if strings.Contains(umo, ":group:") {
				out = append(out, umo)
			}
		}
		return out
	case "custom_group":
		// Resolve members from the stored session group.
		groups := s.getSessionGroups()
		if groupID == "" {
			return nil
		}
		g, ok := groups[groupID]
		if !ok {
			return nil
		}
		raw, _ := g["umos"].([]interface{})
		out := make([]string, 0, len(raw))
		for _, u := range raw {
			if su, ok := u.(string); ok && su != "" {
				out = append(out, su)
			}
		}
		return out
	}
	return umos
}

// batchUpdateSessionProvider implements PATCH /sessions/provider.
func (s *Server) batchUpdateSessionProvider(w http.ResponseWriter, r *http.Request) {
	cm := s.conversationManager()
	if cm == nil {
		writeJSON(w, http.StatusOK, apiError("会话管理不可用"))
		return
	}
	var body struct {
		UMOs         []string `json:"umos"`
		Scope        string   `json:"scope"`
		GroupID      string   `json:"group_id"`
		ProviderID   string   `json:"provider_id"`
		ProviderType string   `json:"provider_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, apiError("无效的请求体"))
		return
	}
	key := conversation.RuleProviderChatCompletion
	switch body.ProviderType {
	case "speech_to_text":
		key = conversation.RuleProviderSpeechToText
	case "text_to_speech":
		key = conversation.RuleProviderTextToSpeech
	}
	umos := s.resolveScopeUMOs(body.Scope, body.UMOs, body.GroupID)
	updated := 0
	for _, umo := range umos {
		if body.ProviderID == "" || body.ProviderID == followConfigValue {
			if err := cm.DeleteSessionRule(umo, key); err == nil {
				updated++
			}
		} else if err := cm.SetSessionRule(umo, key, body.ProviderID); err == nil {
			updated++
		}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"updated": updated}))
}

// batchUpdateSessionService implements PATCH /sessions/service.
func (s *Server) batchUpdateSessionService(w http.ResponseWriter, r *http.Request) {
	cm := s.conversationManager()
	if cm == nil {
		writeJSON(w, http.StatusOK, apiError("会话管理不可用"))
		return
	}
	var body struct {
		UMOs           []string `json:"umos"`
		Scope          string   `json:"scope"`
		GroupID        string   `json:"group_id"`
		SessionEnabled *bool    `json:"session_enabled"`
		LLMEnabled     *bool    `json:"llm_enabled"`
		TTSEnabled     *bool    `json:"tts_enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusOK, apiError("无效的请求体"))
		return
	}
	umos := s.resolveScopeUMOs(body.Scope, body.UMOs, body.GroupID)
	updated := 0
	for _, umo := range umos {
		cur := cm.GetSessionRules(umo)
		cfg, _ := cur[conversation.RuleServiceConfig].(map[string]interface{})
		if cfg == nil {
			cfg = map[string]interface{}{}
		}
		changed := false
		if body.SessionEnabled != nil {
			cfg["session_enabled"] = *body.SessionEnabled
			changed = true
		}
		if body.LLMEnabled != nil {
			cfg["llm_enabled"] = *body.LLMEnabled
			changed = true
		}
		if body.TTSEnabled != nil {
			cfg["tts_enabled"] = *body.TTSEnabled
			changed = true
		}
		if !changed {
			continue
		}
		if err := cm.SetSessionRule(umo, conversation.RuleServiceConfig, cfg); err == nil {
			updated++
		}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"updated": updated}))
}

// getAvailableChatProviders serializes configured chat providers as
// {id, name, model} for the rules editor (name mirrors Python: id).
// Embedding / rerank / other non-chat provider entries are excluded.
func (s *Server) getAvailableChatProviders() []interface{} {
	cfg := s.getConfigData("default")
	providers, ok := cfg["provider"].([]interface{})
	if !ok {
		return []interface{}{}
	}
	out := make([]interface{}, 0, len(providers))
	for _, p := range providers {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		id, _ := pm["id"].(string)
		model, _ := pm["model"].(string)
		enabled, _ := pm["enable"].(bool)
		if id == "" || !enabled {
			continue
		}
		// Skip embedding / rerank / TTS / STT provider entries.
		providerType, _ := pm["provider_type"].(string)
		ptype, _ := pm["type"].(string)
		kind := strings.ToLower(providerType + " " + ptype)
		if strings.Contains(kind, "embedding") || strings.Contains(kind, "rerank") ||
			strings.Contains(kind, "tts") || strings.Contains(kind, "stt") {
			continue
		}
		out = append(out, map[string]interface{}{
			"id":    id,
			"name":  id,
			"model": model,
		})
	}
	return out
}

// getAvailablePersonas returns the persona list for the rules editor.
func (s *Server) getAvailablePersonas() []interface{} {
	out := []interface{}{}
	if s.personas == nil {
		return out
	}
	for _, p := range s.personas.listPersonas(nil) {
		name, _ := p["name"].(string)
		if name == "" {
			name, _ = p["persona_id"].(string)
		}
		if name == "" {
			continue
		}
		out = append(out, map[string]interface{}{
			"name":   name,
			"prompt": p["prompt"],
		})
	}
	return out
}

// getAvailablePlugins returns plugin display metadata for the rules editor.
func (s *Server) getAvailablePlugins() []interface{} {
	out := []interface{}{}
	spm := s.subPluginMgr
	if spm == nil {
		return out
	}
	for _, inst := range spm.List() {
		if inst.Meta == nil {
			continue
		}
		name := inst.Meta.Name
		out = append(out, map[string]interface{}{
			"name":         name,
			"display_name": name,
			"desc":         inst.Meta.Description,
		})
	}
	return out
}

// getAvailableKBs returns the knowledge-base list for the rules editor. The
// SQLite `knowledge_bases` table is the authoritative store (the in-memory
// kbMgr only holds runtime instances, which are empty at boot).
func (s *Server) getAvailableKBs() []interface{} {
	out := []interface{}{}
	if s.database != nil {
		if rows, err := s.database.ListKBs(); err == nil {
			for i := range rows {
				out = append(out, map[string]interface{}{
					"kb_id":   rows[i].KBID,
					"kb_name": rows[i].KBName,
					"emoji":   rows[i].Emoji,
				})
			}
			return out
		}
	}
	kb, ok := s.kbMgr.(*knowledgebase.Manager)
	if !ok {
		return out
	}
	for _, item := range kb.ListKBs() {
		out = append(out, map[string]interface{}{
			"kb_id":   item.KBID,
			"kb_name": item.KBName,
			"emoji":   item.Emoji,
		})
	}
	return out
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

// splitAfterMarker splits a URL path on the LAST occurrence of marker and
// returns the remaining segments. Used for routes with variable prefixes
// (/api/persona-folders vs /api/v1/persona-folders).
func splitAfterMarker(path, marker string) []string {
	idx := strings.Index(path, marker)
	if idx < 0 {
		return nil
	}
	rest := path[idx+len(marker):]
	if rest == "" {
		return []string{}
	}
	return strings.Split(rest, "/")
}

// handlePersonaFolders handles GET/POST /api/v1/persona-folders and PUT/DELETE /api/v1/persona-folders/{folder_id}.
func (s *Server) handlePersonaFolders(w http.ResponseWriter, r *http.Request) {
	if s.personas == nil {
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{}))
		return
	}
	parts := splitAfterMarker(r.URL.Path, "/persona-folders/")
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

	// Plugin LLM function tools (subprocess plugins' registered tools).
	if s.subPluginMgr != nil {
		for _, inst := range s.subPluginMgr.List() {
			if inst.Meta == nil {
				continue
			}
			for _, t := range inst.Meta.Tools {
				params := map[string]interface{}{}
				if len(t.ParamsJson) > 0 {
					_ = json.Unmarshal(t.ParamsJson, &params)
				}
				display := inst.Name
				if display == "" {
					display = "plugin"
				}
				result = append(result, map[string]interface{}{
					"name":                t.Name,
					"description":         t.Description,
					"parameters":          params,
					"active":              true,
					"origin":              "plugin",
					"origin_name":         display,
					"origin_display_name": display,
					"readonly":            false,
				})
			}
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
	logger.I18nInfo("已从 ModelScope 同步 %d 个 MCP 服务器", synced)
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
			if entry.IsTrace() {
				logs = append(logs, entry.Trace)
				continue
			}
			logs = append(logs, map[string]interface{}{
				"level":    sseLogLevel(entry.Level),
				"time":     float64(entry.Timestamp.UnixMilli()) / 1000.0,
				"data":     sseLogLine(entry),
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
			var payload map[string]interface{}
			if entry.IsTrace() {
				payload = entry.Trace
			} else {
				payload = map[string]interface{}{
					"type":     "log",
					"level":    sseLogLevel(entry.Level),
					"time":     float64(entry.Timestamp.UnixMilli()) / 1000.0,
					"data":     sseLogLine(entry),
					"category": "system",
				}
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

// sseLogAnsi returns the ANSI color prefix that opens a console log line,
// matching the WebUI's logColorAnsiMap (the loguru palette the frontend maps
// to highlight colors). The frontend's printLog matches a log line's leading
// ANSI code and applies the corresponding color.
func sseLogAnsi(level log.Level) string {
	switch level {
	case log.LevelDebug:
		return "\x1b[1;36m"
	case log.LevelInfo:
		return "\x1b[1;34m"
	case log.LevelWarn:
		return "\x1b[1;33m"
	case log.LevelError:
		return "\x1b[31m"
	case log.LevelCritical:
		return "\x1b[1;31m"
	default:
		return "\x1b[1;34m"
	}
}

// sseLogLine builds a console log line with the level ANSI prefix so the
// WebUI console highlights it (mirrors the Python loguru output format).
func sseLogLine(entry log.LogEntry) string {
	return fmt.Sprintf("%s[%s] [%s] %s",
		sseLogAnsi(entry.Level),
		entry.Timestamp.Format("2006-01-02 15:04:05.000"),
		sseLogLevel(entry.Level),
		entry.Message)
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
		if r.Method == http.MethodPost {
			// The WebUI chat page sends a message here and expects an SSE
			// stream back (mirrors Python's _send_chat).
			s.handleChatSend(w, r)
			return
		}
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
		cm, ok := s.configMgr.(*config.ConfigManager)
		if !ok || cm == nil {
			writeJSON(w, http.StatusOK, apiError("配置管理器不可用"))
			return
		}
		cfg := cm.Get("default")
		if cfg == nil {
			writeJSON(w, http.StatusOK, apiError("默认配置不存在"))
			return
		}
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			var body map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
				writeJSON(w, http.StatusOK, apiError("无效的请求体"))
				return
			}
			// Preserve router_system_prompt when the client omits it.
			if _, has := body["router_system_prompt"]; !has {
				if all := cfg.All(); all != nil {
					if cur, ok := all["subagent_orchestrator"].(map[string]interface{}); ok {
						if sp, ok := cur["router_system_prompt"].(string); ok {
							body["router_system_prompt"] = sp
						}
					}
				}
			}
			if err := cfg.Set("subagent_orchestrator", body); err != nil {
				writeJSON(w, http.StatusOK, apiError("保存子代理配置失败: "+err.Error()))
				return
			}
			if err := cfg.Save(); err != nil {
				writeJSON(w, http.StatusOK, apiError("保存子代理配置失败: "+err.Error()))
				return
			}
			writeJSON(w, http.StatusOK, apiOKMsg("子代理配置已保存", body))
			return
		}
		all := cfg.All()
		subCfg, _ := all["subagent_orchestrator"].(map[string]interface{})
		if subCfg == nil {
			subCfg = map[string]interface{}{
				"main_enable":                 false,
				"remove_main_duplicate_tools": false,
				"router_system_prompt":        "You are a task router...",
				"agents":                      []interface{}{},
			}
		}
		writeJSON(w, http.StatusOK, apiOK(subCfg))
	case "available-tools":
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"tools": []interface{}{}}))
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
				// Inherit the capability type from the source so the WebUI's
				// capability filter (e.g. capability=chat -> chat_completion)
				// can find the provider. Without this, providers created via
				// "add model" show up with no provider_type and the config
				// page reports "暂无可用的提供商".
				if _, ok := body.Config["provider_type"]; !ok || body.Config["provider_type"] == "" {
					if pt, ok := source["provider_type"].(string); ok && pt != "" {
						body.Config["provider_type"] = pt
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
	case "weixin_oc":
		result, err := s.weixinOCRegistration(action, body.PlatformConfig, body.RegistrationCode)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(result))
	case "lark":
		domain := ""
		if v, ok := body.PlatformConfig["domain"].(string); ok {
			domain = v
		}
		deviceCode := body.RegistrationCode
		if deviceCode == "" {
			deviceCode = body.TaskID
		}
		result, err := s.larkRegistration(action, domain, deviceCode)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(result))
	case "dingtalk":
		deviceCode := body.RegistrationCode
		if deviceCode == "" {
			deviceCode = body.TaskID
		}
		result, err := s.dingtalkRegistration(action, deviceCode)
		if err != nil {
			writeJSON(w, http.StatusOK, apiError(err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, apiOK(result))
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
	// 校验 skillName：仅允许单段目录名，拒绝空名、"."、含路径分隔符或
	// ".." 的名字，防止 skillName 携带 ../ 使 root 越出 data/skills/ 目录。
	if skillName == "" || skillName == "." || skillName == ".." ||
		strings.ContainsAny(skillName, `/\\`) || strings.Contains(skillName, "..") {
		return "", fmt.Errorf("非法技能名")
	}
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
		cfg := s.getConfigData("default")
		traceEnabled := true
		if cfg != nil {
			if v, ok := cfg["trace_enable"].(bool); ok {
				traceEnabled = v
			}
		}
		if r.Method == http.MethodPut {
			var body struct {
				TraceEnable *bool `json:"trace_enable"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.TraceEnable != nil {
				if err := s.setConfigData("trace_enable", *body.TraceEnable); err != nil {
					writeJSON(w, http.StatusInternalServerError, apiError("保存失败: "+err.Error()))
					return
				}
				traceEnabled = *body.TraceEnable
				if traceEnabled {
					logger.I18nInfo("追踪(trace)记录已开启")
				} else {
					logger.I18nInfo("追踪(trace)记录已关闭")
				}
			}
		}
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"trace_enable": traceEnabled,
		}))
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
		if s.restartFunc != nil {
			// 异步触发重启，先返回成功响应，避免阻塞当前请求
			go s.restartFunc()
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "重启中...",
			}))
		} else {
			writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
				"message": "重启功能不可用",
			}))
		}
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

// platformDisplayName returns a human-friendly name for a platform type.
func platformDisplayName(ptype string) string {
	names := map[string]string{
		"aiocqhttp":               "OneBot v11",
		"qq_official":             "QQ 官方机器人",
		"qq_official_webhook":     "QQ 官方机器人(Webhook)",
		"lark":                    "飞书(Lark)",
		"dingtalk":                "钉钉(DingTalk)",
		"discord":                 "Discord",
		"telegram":                "Telegram",
		"slack":                   "Slack",
		"kook":                    "KOOK",
		"satori":                  "Satori",
		"misskey":                 "Misskey",
		"line":                    "LINE",
		"mattermost":              "Mattermost",
		"wecom":                   "企业微信",
		"wecom_ai_bot":            "企业微信智能机器人",
		"weixin_oc":               "微信开放平台",
		"weixin_official_account": "微信公众号",
		"webchat":                 "WebChat",
	}
	if n, ok := names[ptype]; ok {
		return n
	}
	return ptype
}

// countEnabledBots returns how many configured platforms are enabled.
func countEnabledBots(platforms []interface{}) int {
	n := 0
	for _, b := range platforms {
		if pc, ok := b.(map[string]interface{}); ok {
			if enabled, ok := pc["enable"].(bool); ok && enabled {
				n++
			}
		}
	}
	return n
}
