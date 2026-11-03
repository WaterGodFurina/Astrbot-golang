package sources

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

func init() {
	// Register the Decision providers (type name matches the Python
	// register_provider_adapter("system_one_decision", ...) decorator, 4.29.4).
	provider.RegisterProvider("system_one_decision", func(config, settings map[string]interface{}) (provider.AbstractProvider, error) {
		return NewSystemOneDecisionSource(config, settings)
	})
}
