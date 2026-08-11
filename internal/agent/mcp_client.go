// Package agent - MCP (Model Context Protocol) client.
// Ported from astrbot/core/agent/mcp_client.py
//
// This is a thin wrapper around github.com/mark3labs/mcp-go that connects to
// MCP servers via SSE, streamable HTTP, or stdio transport, lists tools, and
// calls them. The underlying library handles the full protocol (SSE handshake,
// endpoint discovery, JSON-RPC correlation, reconnection), so this file only
// adapts the library API to the shapes the pipeline expects.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// MCPClient represents a connection to an MCP server.
type MCPClient struct {
	mu          sync.Mutex
	reconnectMu sync.Mutex // 串行化 Reconnect 的 Cleanup+Connect 整段，防并发重入
	name        string
	active      bool
	config      map[string]interface{}
	cl          *client.Client
	tools       []MCPToolInfo
}

// MCPToolInfo describes a tool available on an MCP server.
type MCPToolInfo struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

// MCPToolCallResult is the result of calling an MCP tool.
type MCPToolCallResult struct {
	Content []map[string]interface{} `json:"content"`
	IsError bool                     `json:"isError"`
}

// NewMCPClient creates an MCP client.
func NewMCPClient(name string, config map[string]interface{}) *MCPClient {
	c := &MCPClient{
		name:   name,
		config: config,
		active: true,
	}
	return c
}

// Connect establishes the connection and lists tools.
func (c *MCPClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var cl *client.Client
	var err error
	if c.isStdio() {
		command, _ := c.config["command"].(string)
		if command == "" {
			return fmt.Errorf("MCP stdio server %q is missing the command", c.name)
		}
		var env []string
		if envCfg, ok := c.config["env"].(map[string]interface{}); ok {
			for k, v := range envCfg {
				if vs, ok := v.(string); ok {
					env = append(env, k+"="+vs)
				}
			}
		}
		args := configStringSlice(c.config, "args")
		cl, err = client.NewStdioMCPClient(command, env, args...)
	} else {
		url, _ := c.config["url"].(string)
		if url == "" {
			return fmt.Errorf("MCP client %q has neither a URL nor a stdio command", c.name)
		}
		opts := []transport.ClientOption{}
		if headers, ok := c.config["headers"].(map[string]interface{}); ok {
			hs := map[string]string{}
			for k, v := range headers {
				if vs, ok := v.(string); ok {
					hs[k] = vs
				}
			}
			if len(hs) > 0 {
				opts = append(opts, transport.WithHeaders(hs))
			}
		}
		cl, err = client.NewSSEMCPClient(url, opts...)
	}
	if err != nil {
		return err
	}
	c.cl = cl

	if err := cl.Start(ctx); err != nil {
		return fmt.Errorf("MCP start failed: %w", err)
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "astrbot-go", Version: "1.0.0"}
	if _, err := cl.Initialize(ctx, initReq); err != nil {
		return fmt.Errorf("MCP initialize failed: %w", err)
	}

	return c.listTools(ctx)
}

// isStdio reports whether the configured transport is stdio.
func (c *MCPClient) isStdio() bool {
	t, _ := c.config["transport"].(string)
	if t == "" {
		t, _ = c.config["type"].(string)
	}
	return t == "stdio"
}

// listTools issues tools/list and caches the returned tool definitions.
func (c *MCPClient) listTools(ctx context.Context) error {
	resp, err := c.cl.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return fmt.Errorf("MCP list tools failed: %w", err)
	}
	c.tools = nil
	for _, t := range resp.Tools {
		c.tools = append(c.tools, MCPToolInfo{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: toolSchemaToMap(t),
		})
	}
	return nil
}

// toolSchemaToMap converts an mcp.ToolInputSchema (a JSON Schema struct) into a
// plain map[string]interface{} for the pipeline's tool schema injection.
func toolSchemaToMap(t mcp.Tool) map[string]interface{} {
	if len(t.RawInputSchema) > 0 {
		var m map[string]interface{}
		if err := json.Unmarshal(t.RawInputSchema, &m); err == nil {
			return m
		}
	}
	data, err := json.Marshal(t.InputSchema)
	if err != nil {
		return map[string]interface{}{}
	}
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]interface{}{}
	}
	return m
}

// CallTool invokes a tool on the MCP server.
func (c *MCPClient) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolCallResult, error) {
	c.mu.Lock()
	cl := c.cl
	c.mu.Unlock()
	if cl == nil {
		return nil, fmt.Errorf("MCP client not connected (call Connect first)")
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = toolName
	req.Params.Arguments = args
	resp, err := cl.CallTool(ctx, req)
	if err != nil {
		return nil, err
	}
	result := &MCPToolCallResult{IsError: resp.IsError}
	for _, content := range resp.Content {
		result.Content = append(result.Content, mcpContentToMap(content))
	}
	return result, nil
}

// mcpContentToMap converts an mcp.Content value into a plain map for the
// pipeline (type + text/image/etc. fields).
func mcpContentToMap(content mcp.Content) map[string]interface{} {
	switch v := content.(type) {
	case mcp.TextContent:
		return map[string]interface{}{"type": "text", "text": v.Text}
	case mcp.ImageContent:
		return map[string]interface{}{"type": "image", "data": v.Data, "mimeType": v.MIMEType}
	case mcp.AudioContent:
		return map[string]interface{}{"type": "audio", "data": v.Data, "mimeType": v.MIMEType}
	case mcp.EmbeddedResource:
		return map[string]interface{}{"type": "resource", "resource": v.Resource}
	default:
		return map[string]interface{}{"type": "text", "text": fmt.Sprintf("%v", content)}
	}
}

// Tools returns the available tools.
func (c *MCPClient) Tools() []MCPToolInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]MCPToolInfo, len(c.tools))
	copy(result, c.tools)
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// Name returns the server name.
func (c *MCPClient) Name() string { return c.name }

// IsActive returns whether the client is active.
func (c *MCPClient) IsActive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active && c.cl != nil
}

// SSEEndpoint is kept for API compatibility; the underlying library manages
// the SSE endpoint internally.
func (c *MCPClient) SSEEndpoint() string { return "" }

// SSEAlive reports whether the client connection is usable. The underlying
// library reconnects internally, so this returns the active flag.
func (c *MCPClient) SSEAlive() bool { return c.IsActive() }

// Cleanup disconnects from the server.
func (c *MCPClient) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	c.tools = nil
	if c.cl != nil {
		_ = c.cl.Close()
		c.cl = nil
	}
}

// Reconnect tears down the current connection and establishes a fresh one.
// Used by callers after a transport failure (e.g. SSE connection lost).
func (c *MCPClient) Reconnect(ctx context.Context) error {
	// Cleanup 会关闭并清空 cl，与并发的 Connect/CallTool 交错时可能重入破坏状态。
	// 用独立的 reconnectMu 串行化整段操作；内部 Connect 自行持 c.mu，二者不同
	// 锁序，不会死锁。
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	c.Cleanup()
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()
	return c.Connect(ctx)
}

// configStringSlice reads a []interface{} config value as []string.
func configStringSlice(cfg map[string]interface{}, key string) []string {
	var out []string
	if raw, ok := cfg[key].([]interface{}); ok {
		for _, a := range raw {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}
