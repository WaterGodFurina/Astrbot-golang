// Package agent - response types for LLM agent execution.
// Ported from astrbot/core/agent/response.py
package agent

// AgentStats tracks statistics during agent execution.
type AgentStats struct {
	IterationCount int  `json:"iteration_count"`
	ToolCallCount  int  `json:"tool_call_count"`
	InputTokens    int  `json:"input_tokens"`
	OutputTokens   int  `json:"output_tokens"`
	Aborted        bool `json:"aborted"`
}

// NewAgentStats creates a stats tracker.
func NewAgentStats() *AgentStats {
	return &AgentStats{}
}

// AddTokens adds token counts.
func (s *AgentStats) AddTokens(input, output int) {
	s.InputTokens += input
	s.OutputTokens += output
}

// IncrementIteration increments the iteration counter.
func (s *AgentStats) IncrementIteration() {
	s.IterationCount++
}

// IncrementToolCall increments the tool call counter.
func (s *AgentStats) IncrementToolCall() {
	s.ToolCallCount++
}

// ToMap converts stats to a map for persistence.
func (s *AgentStats) ToMap() map[string]interface{} {
	return map[string]interface{}{
		"iteration_count": s.IterationCount,
		"tool_call_count": s.ToolCallCount,
		"input_tokens":    s.InputTokens,
		"output_tokens":   s.OutputTokens,
		"aborted":         s.Aborted,
	}
}

// AgentRunStatus describes the outcome of an agent run.
type AgentRunStatus string

const (
	StatusCompleted AgentRunStatus = "completed"
	StatusAborted   AgentRunStatus = "aborted"
	StatusError     AgentRunStatus = "error"
)
