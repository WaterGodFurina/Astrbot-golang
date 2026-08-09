// Code generated from astrbot/core/config/default.py DEFAULT_CONFIG.
// DO NOT EDIT MANUALLY.
package dashboard

import (
	"encoding/json"

	"github.com/AstrBotDevs/AstrBot/internal/config"
)

func defaultConfigFromJSON() map[string]interface{} {
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(config.DefaultConfigJSON()), &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}
