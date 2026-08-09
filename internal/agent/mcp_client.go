// Package agent - MCP (Model Context Protocol) client.
// Ported from astrbot/core/agent/mcp_client.py
//
// This is a Go implementation of the MCP client that connects to MCP servers
// via SSE, streamable HTTP, or stdio transport, lists tools, and calls them.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
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
	transport string // "sse" | "streamable_http" | "stdio"

	// stdio transport state
	proc    *exec.Cmd
	procIn  io.WriteCloser
	procOut *bufio.Reader
	procErr lockedBuffer
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

// lockedBuffer is a mutex-guarded writer used to capture a subprocess's
// stderr without racing against our own reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// isStdio reports whether the configured transport uses the stdio protocol.
func (c *MCPClient) isStdio() bool {
	return c.transport == "stdio"
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

	// stdio transport: spawn the configured subprocess and speak newline-
	// delimited JSON-RPC over its stdin/stdout.
	if c.isStdio() {
		if err := c.startStdio(ctx); err != nil {
			return err
		}
		if _, err := c.doRequest(ctx, map[string]interface{}{
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
		}); err != nil {
			return fmt.Errorf("MCP initialize failed: %w", err)
		}
		return c.listTools(ctx)
	}

	if c.baseURL == "" {
		return fmt.Errorf("MCP client %q has neither a URL nor a stdio command", c.name)
	}

	// For SSE/HTTP transport, test connectivity
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

	if _, err := c.doJSONRPC(ctx, payload); err != nil {
		return fmt.Errorf("MCP initialize failed: %w", err)
	}

	return c.listTools(ctx)
}

// listTools issues tools/list and caches the returned tool definitions.
func (c *MCPClient) listTools(ctx context.Context) error {
	toolsResp, err := c.doRequest(ctx, map[string]interface{}{
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
	return nil
}

// doRequest dispatches a JSON-RPC request over the configured transport.
func (c *MCPClient) doRequest(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, error) {
	if c.isStdio() {
		return c.doStdioRequest(ctx, payload)
	}
	return c.doJSONRPC(ctx, payload)
}

// startStdio spawns the stdio-transport MCP server process.
func (c *MCPClient) startStdio(ctx context.Context) error {
	command, _ := c.config["command"].(string)
	if command == "" {
		return fmt.Errorf("MCP stdio server %q is missing the command", c.name)
	}
	var args []string
	if rawArgs, ok := c.config["args"].([]interface{}); ok {
		for _, a := range rawArgs {
			if s, ok := a.(string); ok {
				args = append(args, s)
			}
		}
	}

	cmd := exec.CommandContext(ctx, command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("MCP stdio stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("MCP stdio stdout pipe: %w", err)
	}
	cmd.Stderr = &c.procErr
	if env, ok := c.config["env"].(map[string]interface{}); ok {
		cmd.Env = os.Environ()
		for k, v := range env {
			if vs, ok := v.(string); ok {
				cmd.Env = append(cmd.Env, k+"="+vs)
			}
		}
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("MCP stdio start %q: %w", command, err)
	}

	c.proc = cmd
	c.procIn = stdin
	c.procOut = bufio.NewReader(stdout)
	return nil
}

// doStdioRequest sends one newline-delimited JSON-RPC request on the child's
// stdin and reads the matching response line from its stdout.
func (c *MCPClient) doStdioRequest(ctx context.Context, payload map[string]interface{}) (map[string]interface{}, error) {
	if c.proc == nil {
		return nil, fmt.Errorf("MCP stdio server not started")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	body = append(body, '\n')
	if _, err := c.procIn.Write(body); err != nil {
		return nil, fmt.Errorf("MCP stdio write: %w", err)
	}

	reqID := payload["id"]
	type result struct {
		msg map[string]interface{}
		err error
	}
	ch := make(chan result, 1)
	go func() {
		for {
			line, err := c.procOut.ReadString('\n')
			if err != nil {
				ch <- result{nil, fmt.Errorf("MCP stdio read: %w", err)}
				return
			}
			line = string(bytes.TrimSpace([]byte(line)))
			if len(line) == 0 {
				continue
			}
			var msg map[string]interface{}
			// UseNumber keeps the JSON id as an exact string, avoiding float64
			// precision loss for large request ids (e.g. unix-nano ids).
			dec := json.NewDecoder(strings.NewReader(line))
			dec.UseNumber()
			if err := dec.Decode(&msg); err != nil {
				continue
			}
			// Match by request id; skip notifications and stale responses.
			if id, ok := msg["id"]; ok && id != nil {
				if reqID != nil && fmt.Sprintf("%v", id) != fmt.Sprintf("%v", reqID) {
					continue
				}
				ch <- result{msg, nil}
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(120 * time.Second):
		return nil, fmt.Errorf("MCP stdio request timed out")
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		if errStr, ok := r.msg["error"]; ok {
			return nil, fmt.Errorf("JSON-RPC error: %v", errStr)
		}
		return r.msg, nil
	}
}

// CallTool invokes a tool on the MCP server.
func (c *MCPClient) CallTool(ctx context.Context, toolName string, args map[string]interface{}) (*MCPToolCallResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.isStdio() {
		if c.proc == nil {
			return nil, fmt.Errorf("MCP stdio server not started")
		}
	} else if c.baseURL == "" {
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

	resp, err := c.doRequest(ctx, payload)
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
	if c.proc != nil {
		if c.procIn != nil {
			c.procIn.Close()
		}
		done := make(chan struct{})
		go func() {
			c.proc.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			c.proc.Process.Kill()
			<-done
		}
		c.proc = nil
	}
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
