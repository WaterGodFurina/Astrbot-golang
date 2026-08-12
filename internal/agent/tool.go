// Package agent implements the LLM agent tool system.
// Ported from astrbot/core/agent/tool.py and astrbot/core/provider/func_tool_manager.py
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("Agent")

// FunctionTool represents a callable tool that LLMs can invoke.
type FunctionTool struct {
	Name              string                                                                      `json:"name"`
	Description       string                                                                      `json:"description"`
	Parameters        map[string]interface{}                                                      `json:"parameters"`
	Handler           func(ctx context.Context, args map[string]interface{}) (interface{}, error) `json:"-"`
	Active            bool                                                                        `json:"active"`
	HandlerModulePath string                                                                      `json:"handler_module_path,omitempty"`
}

// NewFunctionTool creates a tool.
func NewFunctionTool(name, desc string, params map[string]interface{}) *FunctionTool {
	return &FunctionTool{
		Name:        name,
		Description: desc,
		Parameters:  params,
		Active:      true,
	}
}

// Call invokes the tool handler.
func (t *FunctionTool) Call(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	if t.Handler == nil {
		return nil, fmt.Errorf("tool %s has no handler", t.Name)
	}
	logger.Debug("Tool call: %s", t.Name)
	return t.Handler(ctx, args)
}

// ToOpenAISchema converts the tool to OpenAI function schema.
func (t *FunctionTool) ToOpenAISchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		},
	}
}

// ToAnthropicSchema converts the tool to Anthropic tool schema.
func (t *FunctionTool) ToAnthropicSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":         t.Name,
		"description":  t.Description,
		"input_schema": t.Parameters,
	}
}

// ToGoogleSchema converts the tool to Google GenAI schema.
func (t *FunctionTool) ToGoogleSchema() map[string]interface{} {
	return map[string]interface{}{
		"name":        t.Name,
		"description": t.Description,
		"parameters":  t.Parameters,
	}
}

// ToolSet is a collection of tools.
type ToolSet struct {
	mu    sync.RWMutex
	tools map[string]*FunctionTool // name → tool (last wins)
}

// NewToolSet creates an empty tool set.
func NewToolSet() *ToolSet {
	return &ToolSet{tools: make(map[string]*FunctionTool)}
}

// AddTool adds or replaces a tool (last wins for same name).
func (ts *ToolSet) AddTool(tool *FunctionTool) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.tools[tool.Name] = tool
}

// RemoveTool removes a tool by name.
func (ts *ToolSet) RemoveTool(name string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.tools, name)
}

// Get returns a tool by name.
func (ts *ToolSet) Get(name string) *FunctionTool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return ts.tools[name]
}

// All returns all tools.
func (ts *ToolSet) All() []*FunctionTool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	result := make([]*FunctionTool, 0, len(ts.tools))
	for _, t := range ts.tools {
		result = append(result, t)
	}
	return result
}

// Empty returns true if no tools.
func (ts *ToolSet) Empty() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.tools) == 0
}

// OpenAISchema returns all tools in OpenAI format.
func (ts *ToolSet) OpenAISchema() []map[string]interface{} {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(ts.tools))
	for _, t := range ts.tools {
		if t.Active {
			result = append(result, t.ToOpenAISchema())
		}
	}
	return result
}

// AnthropicSchema returns all tools in Anthropic format.
func (ts *ToolSet) AnthropicSchema() []map[string]interface{} {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(ts.tools))
	for _, t := range ts.tools {
		if t.Active {
			result = append(result, t.ToAnthropicSchema())
		}
	}
	return result
}

// GoogleSchema returns all tools in Google GenAI format.
func (ts *ToolSet) GoogleSchema() map[string]interface{} {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	functions := make([]map[string]interface{}, 0, len(ts.tools))
	for _, t := range ts.tools {
		if t.Active {
			functions = append(functions, t.ToGoogleSchema())
		}
	}
	return map[string]interface{}{"function_declarations": functions}
}

// FunctionToolManager manages all LLM function tools.
// Ported from astrbot/core/provider/func_tool_manager.py
type FunctionToolManager struct {
	mu       sync.RWMutex
	funcList []*FunctionTool
}

// NewFunctionToolManager creates a manager.
func NewFunctionToolManager() *FunctionToolManager {
	return &FunctionToolManager{}
}

// Empty returns true if no tools are registered.
func (m *FunctionToolManager) Empty() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.funcList) == 0
}

// AddFunc registers a function tool.
func (m *FunctionToolManager) AddFunc(name, desc string, params map[string]interface{}, handler func(ctx context.Context, args map[string]interface{}) (interface{}, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Remove existing tool with same name
	for i, f := range m.funcList {
		if f.Name == name {
			m.funcList = append(m.funcList[:i], m.funcList[i+1:]...)
			break
		}
	}
	m.funcList = append(m.funcList, &FunctionTool{
		Name:        name,
		Description: desc,
		Parameters:  params,
		Handler:     handler,
		Active:      true,
	})
}

// RemoveFunc removes a tool by name.
func (m *FunctionToolManager) RemoveFunc(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, f := range m.funcList {
		if f.Name == name {
			m.funcList = append(m.funcList[:i], m.funcList[i+1:]...)
			break
		}
	}
}

// GetFunc returns a tool by name.
func (m *FunctionToolManager) GetFunc(name string) *FunctionTool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Search in reverse (last added wins)
	for i := len(m.funcList) - 1; i >= 0; i-- {
		if m.funcList[i].Name == name && m.funcList[i].Active {
			return m.funcList[i]
		}
	}
	// Fallback: last matching tool (even if inactive)
	for i := len(m.funcList) - 1; i >= 0; i-- {
		if m.funcList[i].Name == name {
			return m.funcList[i]
		}
	}
	return nil
}

// GetFullToolSet returns a ToolSet with all active tools.
func (m *FunctionToolManager) GetFullToolSet() *ToolSet {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ts := NewToolSet()
	for _, f := range m.funcList {
		ts.AddTool(f)
	}
	return ts
}

// GetOpenAISchema returns all active tools in OpenAI format.
func (m *FunctionToolManager) GetOpenAISchema() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(m.funcList))
	for _, f := range m.funcList {
		if f.Active {
			result = append(result, f.ToOpenAISchema())
		}
	}
	return result
}

// GetAnthropicSchema returns all active tools in Anthropic format.
func (m *FunctionToolManager) GetAnthropicSchema() []map[string]interface{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]map[string]interface{}, 0, len(m.funcList))
	for _, f := range m.funcList {
		if f.Active {
			result = append(result, f.ToAnthropicSchema())
		}
	}
	return result
}

// DeactivateTool deactivates a tool.
func (m *FunctionToolManager) DeactivateTool(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.funcList {
		if f.Name == name {
			f.Active = false
			return true
		}
	}
	return false
}

// ActivateTool activates a tool.
func (m *FunctionToolManager) ActivateTool(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, f := range m.funcList {
		if f.Name == name {
			f.Active = true
			return true
		}
	}
	return false
}

// AllTools returns all registered tools.
func (m *FunctionToolManager) AllTools() []*FunctionTool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*FunctionTool, len(m.funcList))
	copy(result, m.funcList)
	return result
}

// ToJSON serializes the tool list for API responses.
func (m *FunctionToolManager) ToJSON() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, _ := json.Marshal(m.funcList)
	return string(data)
}
