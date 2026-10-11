package pipeline

// 回归测试：第三方 agent runner（dify/coze/dashscope/deerflow）在未配置任何
// 本地 chat provider（provider 列表为空或仅含遗留 agent_runner 类型条目）时
// 必须可达（对齐 py third_party.py：第三方 runner 完全不依赖 chat provider）；
// 本地 runner 的 errNoAvailableProvider 行为保持不变；resolveProvider 的
// "首个 enabled provider" 回退排除 provider_type == "agent_runner" 条目。

import (
	"errors"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

// legacyAgentRunnerProvider 构造一个遗留 agent_runner 类型 provider 条目
// （迁移前的旧配置形态，CreateProvider 不认识该类型）。
func legacyAgentRunnerProvider(id string) map[string]interface{} {
	return map[string]interface{}{
		"id":            id,
		"enable":        true,
		"provider_type": "agent_runner",
		"type":          "dify",
		"dify_api_key":  "k",
	}
}

func TestAgentRunnerType(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]interface{}
		want string
	}{
		{"missing", map[string]interface{}{}, "local"},
		{"empty runner_type", map[string]interface{}{"agent_runner": map[string]interface{}{}}, "local"},
		{"local", map[string]interface{}{"agent_runner": map[string]interface{}{"runner_type": "local"}}, "local"},
		{"dify", map[string]interface{}{"agent_runner": map[string]interface{}{"runner_type": "dify"}}, "dify"},
		{"coze", map[string]interface{}{"agent_runner": map[string]interface{}{"runner_type": "coze"}}, "coze"},
		{"dashscope", map[string]interface{}{"agent_runner": map[string]interface{}{"runner_type": "dashscope"}}, "dashscope"},
		{"deerflow", map[string]interface{}{"agent_runner": map[string]interface{}{"runner_type": "deerflow"}}, "deerflow"},
	}
	for _, tc := range cases {
		if got := agentRunnerType(tc.cfg); got != tc.want {
			t.Errorf("%s: agentRunnerType = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestIsThirdPartyAgentRunner(t *testing.T) {
	for _, rt := range []string{"dify", "coze", "dashscope", "deerflow"} {
		if !isThirdPartyAgentRunner(rt) {
			t.Errorf("isThirdPartyAgentRunner(%q) = false, want true", rt)
		}
	}
	for _, rt := range []string{"local", "", "unknown"} {
		if isThirdPartyAgentRunner(rt) {
			t.Errorf("isThirdPartyAgentRunner(%q) = true, want false", rt)
		}
	}
}

// TestResolveProviderSkipsAgentRunnerEntries：首个 enabled provider 回退不得
// 选中遗留 agent_runner 类型条目。
func TestResolveProviderSkipsAgentRunnerEntries(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider": []interface{}{
			legacyAgentRunnerProvider("legacy-runner"),
			map[string]interface{}{"id": "chat1", "type": "openai_chat_completion", "enable": true},
		},
	})
	pc, _, err := s.resolveProvider()
	if err != nil {
		t.Fatalf("resolveProvider: %v", err)
	}
	if id, _ := pc["id"].(string); id != "chat1" {
		t.Fatalf("resolveProvider picked %q, want chat1 (agent_runner entry must be skipped)", id)
	}
}

// TestResolveProviderAgentRunnerOnlyStillFails：provider 列表仅含 agent_runner
// 类型条目时仍返回 errNoAvailableProvider（本地 runner 行为不变）。
func TestResolveProviderAgentRunnerOnlyStillFails(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider": []interface{}{legacyAgentRunnerProvider("legacy-runner")},
	})
	_, _, err := s.resolveProvider()
	if !errors.Is(err, errNoAvailableProvider) {
		t.Fatalf("resolveProvider err = %v, want errNoAvailableProvider", err)
	}
}

// TestCallLLMAgentDifyWithoutChatProvider：runner_type=dify 且无任何 chat
// provider（空列表 / 仅 agent_runner 条目）时，callLLMAgent 必须进入 dify
// 分支（这里以 runDifyAgent 被调用、报出 Dify 配置错误为准，而非
// errNoAvailableProvider / provider type 失败）。
func TestCallLLMAgentDifyWithoutChatProvider(t *testing.T) {
	providerLists := [][]interface{}{
		nil,
		{},
		{legacyAgentRunnerProvider("legacy-runner")},
	}
	for _, providers := range providerLists {
		cfg := map[string]interface{}{
			"provider": providers,
			"agent_runner": map[string]interface{}{
				"runner_type": "dify",
				"config":      map[string]interface{}{},
			},
		}
		s := testProcessStageWithConfig(t, cfg)
		event := &core.Event{Source: core.EventSource{Platform: "webchat", ConvID: "c1"}, MessageStr: "hello"}
		err := s.callLLMAgent(t.Context(), event)
		if err == nil {
			t.Fatal("callLLMAgent should fail with empty dify config (missing api key), not succeed")
		}
		if errors.Is(err, errNoAvailableProvider) {
			t.Fatalf("providers=%v: callLLMAgent failed with errNoAvailableProvider; third-party runner must be reachable without chat providers", providers)
		}
		if event.Result != nil && len(event.Result.Chain) > 0 {
			t.Fatalf("providers=%v: unexpected failure reply written: %v", providers, event.Result.Chain)
		}
	}
}

// TestCallLLMAgentLocalWithoutChatProviderUnchanged：本地 runner 无 provider
// 时仍以 errNoAvailableProvider 失败且平台侧静默（原有行为）。
func TestCallLLMAgentLocalWithoutChatProviderUnchanged(t *testing.T) {
	s := testProcessStageWithConfig(t, map[string]interface{}{
		"provider": []interface{}{legacyAgentRunnerProvider("legacy-runner")},
	})
	event := &core.Event{Source: core.EventSource{Platform: "webchat", ConvID: "c1"}, MessageStr: "hello"}
	err := s.callLLMAgent(t.Context(), event)
	if !errors.Is(err, errNoAvailableProvider) {
		t.Fatalf("local runner err = %v, want errNoAvailableProvider", err)
	}
	if event.Result != nil && len(event.Result.Chain) > 0 {
		t.Fatalf("local runner must stay silent (no reply chain), got %v", event.Result.Chain)
	}
}
