// Package sources - Xiaomi OpenAI-compatible chat provider.
// Ported from astrbot/core/provider/sources/xiaomi_source.py
package sources

import (
	"context"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// xiaomiModels is the built-in fallback model list when the API is unreachable.
var xiaomiModels = []string{
	"mimo-v2.5-pro",
	"mimo-v2.5",
	"mimo-v2-pro",
	"mimo-v2-omni",
	"mimo-v2-flash",
}

// XiaomiSource is an OpenAI-compatible chat provider for Xiaomi (MiMo).
type XiaomiSource struct {
	*OpenAISource
}

// NewXiaomiSource creates a Xiaomi provider with api_base defaulting to
// https://api.xiaomimimo.com/v1 and model defaulting to mimo-v2.5.
func NewXiaomiSource(config, settings map[string]interface{}) *XiaomiSource {
	if configString(config, "api_base", "") == "" {
		config["api_base"] = "https://api.xiaomimimo.com/v1"
	}
	if configString(config, "model", "") == "" {
		config["model"] = "mimo-v2.5"
	}
	return &XiaomiSource{OpenAISource: NewOpenAISource(config, settings)}
}

// GetModels returns the known Xiaomi models, falling back to the built-in list
// when the API is unreachable or returns nothing.
func (s *XiaomiSource) GetModels(ctx context.Context) ([]string, error) {
	models, err := s.OpenAISource.GetModels(ctx)
	if err != nil || len(models) == 0 {
		return append([]string(nil), xiaomiModels...), nil
	}
	return models, nil
}

var _ provider.ChatProvider = (*XiaomiSource)(nil)
