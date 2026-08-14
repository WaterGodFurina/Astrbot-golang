package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
)

// SubAgent is one configured subagent (from subagent_orchestrator.agents).
type SubAgent struct {
	Name              string
	PersonaID         string
	PublicDescription string
	SystemPrompt      string
	ProviderID        string
	Enabled           bool
}

// loadSubAgents reads subagent_orchestrator from the config map. The second
// return value is main_enable (whether handoff tools should be injected).
func loadSubAgents(cfg map[string]interface{}) ([]*SubAgent, bool) {
	raw, ok := cfg["subagent_orchestrator"].(map[string]interface{})
	if !ok {
		return nil, false
	}
	mainEnable, _ := raw["main_enable"].(bool)
	agentsRaw, _ := raw["agents"].([]interface{})
	var agents []*SubAgent
	for _, item := range agentsRaw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		personaID, _ := m["persona_id"].(string)
		publicDesc, _ := m["public_description"].(string)
		systemPrompt, _ := m["system_prompt"].(string)
		providerID, _ := m["provider_id"].(string)
		enabled := true
		if v, ok := m["enabled"].(bool); ok {
			enabled = v
		}
		agents = append(agents, &SubAgent{
			Name:              name,
			PersonaID:         personaID,
			PublicDescription: strings.TrimSpace(publicDesc),
			SystemPrompt:      strings.TrimSpace(systemPrompt),
			ProviderID:        strings.TrimSpace(providerID),
			Enabled:           enabled,
		})
	}
	return agents, mainEnable
}

// subAgentToolSchemas builds OpenAI transfer_to_<name> tool schemas for the
// enabled subagents (mirrors Python HandoffTool). Non-legal names are rewritten
// with pluginToolSafeName so the provider does not reject the whole request.
func subAgentToolSchemas(agents []*SubAgent) []map[string]interface{} {
	var out []map[string]interface{}
	for _, a := range agents {
		if !a.Enabled {
			continue
		}
		desc := a.PublicDescription
		if desc == "" {
			desc = "Delegate tasks to " + a.Name + " agent to handle the request."
		}
		out = append(out, map[string]interface{}{
			"type": "function",
			"function": map[string]interface{}{
				"name":        subAgentToolName(a.Name),
				"description": desc,
				"parameters": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"input": map[string]interface{}{
							"type":        "string",
							"description": "The input to be handed off to the agent. This should be a clear and concise request or task.",
						},
						"background_task": map[string]interface{}{
							"type":        "boolean",
							"description": "Defaults to false. Set to true if the task may take noticeable time or involves external tools.",
						},
					},
					"required": []interface{}{"input"},
				},
			},
		})
	}
	return out
}

// subAgentToolName returns the provider-safe tool name for a subagent. Legal
// names keep their "transfer_to_" form; illegal ones are sanitized via
// pluginToolSafeName so the OpenAI schema (^[a-zA-Z0-9_-]+$) stays valid.
func subAgentToolName(name string) string {
	return "transfer_to_" + pluginToolSafeName(name)
}

// findSubAgentByName resolves a tool name (possibly sanitized) back to the
// original subagent. Exact match wins; otherwise the sanitized form is matched.
func (s *ProcessStage) findSubAgentByName(toolName string) *SubAgent {
	agentName := strings.TrimPrefix(toolName, "transfer_to_")
	for _, a := range s.subAgents {
		if a.Name == agentName {
			return a
		}
	}
	for _, a := range s.subAgents {
		if pluginToolSafeName(a.Name) == agentName {
			return a
		}
	}
	return nil
}

// findProviderByID returns the provider config map whose id matches.
func findProviderByID(cfg map[string]interface{}, id string) map[string]interface{} {
	providers, _ := cfg["provider"].([]interface{})
	for _, p := range providers {
		pm, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		if pid, _ := pm["id"].(string); pid == id {
			return pm
		}
	}
	return nil
}

// applyKnowledgeBase retrieves knowledge-base content for the prompt and
// appends it as reference context (mirrors Python _apply_kb, non-agentic mode).
func (s *ProcessStage) applyKnowledgeBase(event *core.Event, prompt string) string {
	if s.kbRetriever == nil {
		return prompt
	}
	umo := event.UnifiedMsgOrigin()
	contextText, err := s.kbRetriever(umo, prompt)
	if err != nil {
		logger.I18nWarn("知识库检索失败: %v", err)
		return prompt
	}
	if strings.TrimSpace(contextText) == "" {
		return prompt
	}
	return prompt + "\n\n[Related Knowledge Base Results]:\n" + contextText
}

// executeSubAgent handles a transfer_to_<name> call: it runs a fresh LLM round
// with the subagent's persona (and optional provider override) and returns the
// subagent's reply as the tool result.
func (s *ProcessStage) executeSubAgent(event *core.Event, name string, args map[string]interface{}) (string, bool) {
	agent := s.findSubAgentByName(name)
	if agent == nil {
		return "", false
	}
	agentName := agent.Name
	input, _ := args["input"].(string)
	if input == "" {
		if v, ok := args["request"].(string); ok {
			input = v
		}
	}
	if strings.TrimSpace(input) == "" {
		return "子代理需要一个 input 参数来描述要执行的任务。", true
	}

	systemPrompt := agent.SystemPrompt
	if s.personaPrompt != nil && agent.PersonaID != "" {
		if p := s.personaPrompt(event.UnifiedMsgOrigin(), agent.PersonaID); p != "" {
			systemPrompt = p
		}
	}
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = "你是子代理 " + agentName + "。请认真完成用户交给你的任务。"
	}

	providerCfg, providerSettings, err := s.resolveProvider()
	if err != nil {
		return "子代理 " + agentName + " 执行失败: " + err.Error(), true
	}
	if agent.ProviderID != "" {
		if pc := findProviderByID(s.config, agent.ProviderID); pc != nil {
			providerCfg = pc
		}
	}
	providerType, _ := providerCfg["type"].(string)
	if providerType == "" {
		providerType, _ = providerCfg["provider"].(string)
	}
	if providerType == "" {
		return "子代理 " + agentName + " 的模型提供商配置缺少 type 字段。", true
	}
	mergedCfg := mergeProviderSource(providerCfg, s.config["provider_sources"])
	inst, err := provider.CreateProvider(providerType, mergedCfg, providerSettings)
	if err != nil {
		return "子代理 " + agentName + " 初始化模型提供商失败: " + err.Error(), true
	}
	chat, ok := inst.(provider.ChatProvider)
	if !ok {
		return "子代理 " + agentName + " 的提供商不支持聊天能力。", true
	}

	llmCtx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	req := &provider.ProviderRequest{
		Prompt:       input,
		SessionID:    event.UnifiedMsgOrigin() + ":subagent:" + agentName,
		SystemPrompt: systemPrompt,
		Conversation: s.convMgr,
		// Subagents run with an isolated context (no main-conversation history).
		Contexts: nil,
	}
	resp, err := chat.TextChat(llmCtx, req)
	if err != nil {
		return fmt.Sprintf("子代理 %s 执行失败: %s", agentName, err.Error()), true
	}
	return resp.CompletionText, true
}
