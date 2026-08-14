package kook

// KookConfig 对应 Python kook_config.py 的 KookConfig 数据类。
// 配置字段名与 Python 完全一致 (kook_bot_token 等)。
type KookConfig struct {
	// 基础配置
	Token  string
	Enable bool
	ID     string

	// 重连配置
	ReconnectDelay    int // 重连延迟基数(秒), 指数退避
	MaxReconnectDelay int // 最大重连延迟(秒)
	MaxRetryDelay     int // 最大重试延迟(秒)

	// 心跳配置
	HeartbeatInterval    int // 心跳间隔(秒)
	HeartbeatTimeout     int // 心跳超时时间(秒)
	MaxHeartbeatFailures int // 最大心跳失败次数

	// 失败处理
	MaxConsecutiveFailures int // 最大连续失败次数
}

// defaultKookConfig 提供与 Python KookConfig 一致的默认值。
var defaultKookConfig = KookConfig{
	Enable:                 false,
	ID:                     "kook",
	ReconnectDelay:         1,
	MaxReconnectDelay:      60,
	MaxRetryDelay:          60,
	HeartbeatInterval:      30,
	HeartbeatTimeout:       6,
	MaxHeartbeatFailures:   3,
	MaxConsecutiveFailures: 5,
}

// getInt 从配置字典中读取 int 字段, 缺失或类型不符时返回默认值。
func getInt(config map[string]interface{}, key string, def int) int {
	if v, ok := config[key].(int); ok {
		return v
	}
	// 配置经 JSON 解析后可能是 float64, 做一次兼容转换
	if v, ok := config[key].(float64); ok {
		return int(v)
	}
	return def
}

// getBool 从配置字典中读取 bool 字段, 缺失或类型不符时返回默认值。
func getBool(config map[string]interface{}, key string, def bool) bool {
	if v, ok := config[key].(bool); ok {
		return v
	}
	return def
}

// fromConfig 从平台实例配置字典创建 KookConfig (对应 Python 的 from_dict)。
func (c *KookConfig) fromConfig(config map[string]interface{}) {
	// 适配器 id 是不可改的
	c.ID = "kook"
	c.Enable = getBool(config, "enable", defaultKookConfig.Enable)
	c.Token, _ = config["kook_bot_token"].(string)
	c.ReconnectDelay = getInt(config, "kook_reconnect_delay", defaultKookConfig.ReconnectDelay)
	c.MaxReconnectDelay = getInt(config, "kook_max_reconnect_delay", defaultKookConfig.MaxReconnectDelay)
	c.MaxRetryDelay = getInt(config, "kook_max_retry_delay", defaultKookConfig.MaxRetryDelay)
	c.HeartbeatInterval = getInt(config, "kook_heartbeat_interval", defaultKookConfig.HeartbeatInterval)
	c.HeartbeatTimeout = getInt(config, "kook_heartbeat_timeout", defaultKookConfig.HeartbeatTimeout)
	c.MaxHeartbeatFailures = getInt(config, "kook_max_heartbeat_failures", defaultKookConfig.MaxHeartbeatFailures)
	c.MaxConsecutiveFailures = getInt(config, "kook_max_consecutive_failures", defaultKookConfig.MaxConsecutiveFailures)
}
