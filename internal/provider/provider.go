// Package provider implements LLM provider interfaces and management.
// Ported from astrbot/core/provider/provider.py and manager.py
package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Provider")

// ProviderType identifies the category of a provider.
type ProviderType string

const (
	ProviderTypeChat      ProviderType = "chat_completion"
	ProviderTypeSTT       ProviderType = "speech_to_text"
	ProviderTypeTTS       ProviderType = "text_to_speech"
	ProviderTypeEmbedding ProviderType = "embedding"
	ProviderTypeRerank    ProviderType = "rerank"
)

// AbstractProvider is the base interface all providers implement.
type AbstractProvider interface {
	// Meta returns provider metadata.
	Meta() ProviderMeta
	// SetModel sets the current model name.
	SetModel(model string)
	// GetModel returns the current model name.
	GetModel() string
	// Test verifies the provider is working.
	Test(ctx context.Context) error
}

// ChatProvider handles LLM text chat.
type ChatProvider interface {
	AbstractProvider
	// TextChat sends a chat request and returns a response.
	TextChat(ctx context.Context, req *ProviderRequest) (*LLMResponse, error)
	// TextChatStream sends a chat request and returns a streaming response channel.
	TextChatStream(ctx context.Context, req *ProviderRequest) (<-chan *LLMResponse, error)
}

// STTProvider handles speech-to-text.
type STTProvider interface {
	AbstractProvider
	GetText(ctx context.Context, audioURL string) (string, error)
}

// TTSProvider handles text-to-speech.
type TTSProvider interface {
	AbstractProvider
	GetAudio(ctx context.Context, text string) (string, error)
	SupportStream() bool
}

// EmbeddingProvider handles text embeddings.
type EmbeddingProvider interface {
	AbstractProvider
	GetEmbedding(ctx context.Context, text string) ([]float32, error)
	GetEmbeddings(ctx context.Context, texts []string) ([][]float32, error)
	GetDim() int
}

// RerankProvider handles document reranking.
type RerankProvider interface {
	AbstractProvider
	Rerank(ctx context.Context, query string, documents []string, topN int) ([]*RerankResult, error)
}

// BaseProvider provides common provider state.
type BaseProvider struct {
	mu               sync.RWMutex
	modelName        string
	providerConfig   map[string]interface{}
	providerSettings map[string]interface{}
	capability       ProviderCapabilityType
}

// SetCapability records the provider capability reported by Meta(). Defaults
// to CapChatCompletion when unset (legacy chat sources).
func (b *BaseProvider) SetCapability(cap ProviderCapabilityType) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.capability = cap
}

// SetModel sets the current model.
func (b *BaseProvider) SetModel(model string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.modelName = model
}

// GetModel returns the current model.
func (b *BaseProvider) GetModel() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.modelName
}

// Config returns the provider config.
func (b *BaseProvider) Config() map[string]interface{} {
	return b.providerConfig
}

// Settings returns the provider settings.
func (b *BaseProvider) Settings() map[string]interface{} {
	return b.providerSettings
}

// Meta returns provider metadata.
func (b *BaseProvider) Meta() ProviderMeta {
	id, _ := b.providerConfig["id"].(string)
	if id == "" {
		id = "default"
	}
	typeName, _ := b.providerConfig["type"].(string)
	capability := b.capability
	if capability == "" {
		capability = CapChatCompletion
	}
	return ProviderMeta{
		ID:           id,
		Model:        b.GetModel(),
		Type:         typeName,
		ProviderType: capability,
	}
}

// Test verifies the provider. Override in implementations.
func (b *BaseProvider) Test(ctx context.Context) error {
	return fmt.Errorf("test not implemented for this provider")
}

// NewBaseProvider creates a base provider.
func NewBaseProvider(config, settings map[string]interface{}) BaseProvider {
	model, _ := config["model"].(string)
	return BaseProvider{
		modelName:        model,
		providerConfig:   config,
		providerSettings: settings,
	}
}

// ProviderManager manages all registered providers.
type ProviderManager struct {
	mu         sync.RWMutex
	providers  map[string]AbstractProvider
	chatProvID string // default chat provider ID
	sttProvID  string // default STT provider ID
	ttsProvID  string // default TTS provider ID
	embProvID  string // default embedding provider ID
	rerankID   string // default rerank provider ID
}

// NewProviderManager creates a manager.
func NewProviderManager() *ProviderManager {
	return &ProviderManager{providers: make(map[string]AbstractProvider)}
}

// Register adds a provider.
//
// 保留待用：当前生产代码无调用点（运行时 provider 初始化走 sources/ 的
// RegisterProvider 工厂注册），仅测试与未来注册路径使用，勿删除。
func (pm *ProviderManager) Register(id string, p AbstractProvider) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.providers[id] = p
	logger.Info("Registered provider: %s (type=%s, model=%s)", id, p.Meta().Type, p.GetModel())
}

// Unregister removes a provider.
func (pm *ProviderManager) Unregister(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.providers, id)
}

// Get returns a provider by ID.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) Get(id string) AbstractProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	return pm.providers[id]
}

// GetChatProvider returns the default chat provider.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) GetChatProvider() ChatProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.chatProvID != "" {
		if p, ok := pm.providers[pm.chatProvID].(ChatProvider); ok {
			return p
		}
	}
	// Fallback: find first chat provider
	for _, p := range pm.providers {
		if cp, ok := p.(ChatProvider); ok {
			return cp
		}
	}
	return nil
}

// GetSTTProvider returns the default STT provider.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) GetSTTProvider() STTProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.sttProvID != "" {
		if p, ok := pm.providers[pm.sttProvID].(STTProvider); ok {
			return p
		}
	}
	for _, p := range pm.providers {
		if sp, ok := p.(STTProvider); ok {
			return sp
		}
	}
	return nil
}

// GetTTSProvider returns the default TTS provider.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) GetTTSProvider() TTSProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.ttsProvID != "" {
		if p, ok := pm.providers[pm.ttsProvID].(TTSProvider); ok {
			return p
		}
	}
	for _, p := range pm.providers {
		if tp, ok := p.(TTSProvider); ok {
			return tp
		}
	}
	return nil
}

// GetEmbeddingProvider returns the default embedding provider.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) GetEmbeddingProvider() EmbeddingProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.embProvID != "" {
		if p, ok := pm.providers[pm.embProvID].(EmbeddingProvider); ok {
			return p
		}
	}
	for _, p := range pm.providers {
		if ep, ok := p.(EmbeddingProvider); ok {
			return ep
		}
	}
	return nil
}

// GetRerankProvider returns the default rerank provider.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) GetRerankProvider() RerankProvider {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	if pm.rerankID != "" {
		if p, ok := pm.providers[pm.rerankID].(RerankProvider); ok {
			return p
		}
	}
	for _, p := range pm.providers {
		if rp, ok := p.(RerankProvider); ok {
			return rp
		}
	}
	return nil
}

// SetDefaultChatProvider sets the default chat provider ID.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) SetDefaultChatProvider(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.chatProvID = id
}

// SetDefaultSTTProvider sets the default STT provider ID.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) SetDefaultSTTProvider(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.sttProvID = id
}

// SetDefaultTTSProvider sets the default TTS provider ID.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) SetDefaultTTSProvider(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.ttsProvID = id
}

// SetDefaultEmbeddingProvider sets the default embedding provider ID.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) SetDefaultEmbeddingProvider(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.embProvID = id
}

// SetDefaultRerankProvider sets the default rerank provider ID.
//
// 保留待用：当前生产代码无调用点，仅测试使用，勿删除。
func (pm *ProviderManager) SetDefaultRerankProvider(id string) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.rerankID = id
}

// All returns all provider IDs.
func (pm *ProviderManager) All() []string {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	ids := make([]string, 0, len(pm.providers))
	for id := range pm.providers {
		ids = append(ids, id)
	}
	return ids
}

// Close cleans up all providers.
func (pm *ProviderManager) Close() {
	// Nothing to do for now; individual providers may implement io.Closer
}
