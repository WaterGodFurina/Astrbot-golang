// Package agent - MCP (Model Context Protocol) client.
// Ported from astrbot/core/agent/mcp_client.py
//
// This is a Go implementation of the MCP client that connects to MCP servers
// via SSE or stdio transport, lists tools, and calls them.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// MCPClient represents a connection to an MCP server.
type MCPClient struct {
	mu        sync.Mutex
	name      string
	active    bool
	config    map[string]interface{}
	tools     []MCPToolInfo
	server    *http.Client
	baseURL   string
	headers   map[string]string
	transport string // "sse" or "streamable_http" or "stdio"
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
		server: &http.Client{
			Timeout: 60 * time.Second,
		},
		headers: make(map[string]string),
	}

	// Extract URL and transport
	if url, ok := config["url"].(string); ok {
		c.baseURL = url
	}
	if t, ok := config["transport"].(string); ok {
		c.transport = t
	} else if t, ok := config["type"].(string); ok {
		c.transport = t
	} else {
		c.transport = "sse" // default
	}

	// Extract headers
	if h, ok := config["headers"].(map[string]interface{}); ok {
		for k, v := range h {
			if vs, ok := v.(string); ok {
				c.headers[k] = vs
			}
		}
	}

	return c
}

// Connect establishes connection and lists tools.
func (c *MCPClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// For SSE/HTTP transport, test connectivity
	if c.baseURL != "" {
		// Send initialize request
		payload := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "initialize",
			"id":      0,
			"params": map[string]interface{}{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]interface{}{},
				"clientInfo": map[string]interface{}{
					"name":    "astrbot-go",
					"version": "1.0.0",
				},
			},
		}

		_, err := c.doJSONRPC(ctx, payload)
		if err != nil {
			return fmt.Errorf("MCP initialize failed: %w", err)
		}

		// List tools
		toolsResp, err := c.doJSONRPC(ctx, map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "tools/list",
			"id":      1,
		})
		if err != nil {
			return fmt.Errorf("MCP list tools failed: %w", err)
		}

		if result, ok := toolsResp["result"].(map[string]interface{}); ok {
			if tools, ok := result["tools"].([]interface{}); ok {
				for _, t := range tools {
					if tool, ok := t.(map[string]interface{}); ok {
						info := MCPToolInfo{}
						if name, ok := tool["name"].(string); ok {
							info.Name = name
						}
						if desc, ok := tool["description"].(string); ok {
							info.Description = desc
						}
						if schema, ok := tool["inputSchema"].(map[string]interface{}); ok {
							info.InputSchema = schema
						}
						c.tools = append(c.tools, info)
					}
				}
			}
		}
	}

	return nil
}

// CallTool invokes a tool on the MCP server.
func (c *MCPClient) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolCallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.baseURL == "" {
		return nil, fmt.Errorf("MCP client not connected (no URL)")
	}

	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      time.Now().UnixNano(),
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": args,
		},
	}

	resp, err := c.doJSONRPC(ctx, payload)
	if err != nil {
		return nil, err
	}

	result := &MCPToolCallResult{}
	if r, ok := resp["result"].(map[string]interface{}); ok {
		if content, ok := r["content"].([]interface{}); ok {
			for _, c := range content {
				if cm, ok := c.(map[string]interface{}); ok {
					result.Content = append(result.Content, cm)
				}
			}
		}
		if isError, ok := r["isError"].(bool); ok {
			result.IsError = isError
		}
	}

	return result, nil
}

// Tools returns the available tools.
func (c *MCPClient) Tools() []MCPToolInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]MCPToolInfo, len(c.tools))
	copy(result, c.tools)
	return result
}

// Name returns the server name.
func (c *MCPClient) Name() string { return c.name }

// IsActive returns whether the client is active.
func (c *MCPClient) IsActive() bool { return c.active }

// Cleanup disconnects from the server.
func (c *MCPClient) Cleanup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	c.tools = nil
}

// doJSONRPC sends a JSON-RPC request.
func (c *MCPClient) doJSONRPC(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}

	resp, err := c.server.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	if errStr, ok := result["error"]; ok {
		return nil, fmt.Errorf("JSON-RPC error: %v", errStr)
	}

	return result, nil
}
