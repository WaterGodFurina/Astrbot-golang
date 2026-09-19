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
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var mcpLogger = log.GetDefault().WithComponent("MCP")

// MCPClient represents a connection to an MCP server.
type MCPClient struct {
	mu          sync.Mutex
	reconnectMu sync.Mutex // 串行化 Reconnect 的 Cleanup+Connect 整段，防并发重入
	name        string
	active      bool
	config      map[string]interface{}
	cl          *client.Client
	tools       []MCPToolInfo
	// connectCancel 取消连接生命周期 ctx。mcp-go 会把传入 Start 的 ctx 绑定
	// 为连接生命周期（SSE 读循环、stdio 子进程均随其取消而终止），因此该 ctx
	// 由客户端自管（Connect 创建、Cleanup 释放），调用方不得提前 cancel。
	connectCancel context.CancelFunc
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
//
// 连接生命周期完全由客户端自管：mcp-go 的传输层会把 Start 收到的 ctx 绑定
// 为连接生命周期（SSE 读循环、stdio 子进程随其取消而终止），因此绝不能在
// Connect 返回后 cancel 该 ctx，也不能用 WithTimeout 派生（30s 到期同样会
// 杀死连接，且调用方传入的 deadline 会沿父链传播）。这里改从 Background
// 派生无 deadline 的 ctx，握手 30s 限时用 watchdog 定时器实现：握手完成后
// 停表，ctx 存活至 Cleanup 由 connectCancel 释放。参数 ctx 仅为兼容签名保留，
// 不参与连接生命周期。
func (c *MCPClient) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	handshakeCtx, cancel := context.WithCancel(context.Background())
	// 30s 握手看门狗：握手未在限时内完成则中止连接；成功后停表，连接不再
	// 受超时影响（cancel 交由 Cleanup 在断开时调用，终止读循环/子进程）。
	watchdog := time.AfterFunc(30*time.Second, cancel)
	defer watchdog.Stop()
	c.connectCancel = cancel

	var cl *client.Client
	var err error
	if c.isStdio() {
		command, _ := c.config["command"].(string)
		if command == "" {
			cancel()
			return fmt.Errorf("MCP stdio server %q is missing the command", c.name)
		}
		if err := validateMCPStdioConfig(c.name, command, configStringSlice(c.config, "args")); err != nil {
			cancel()
			return err
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
	} else if c.isStreamableHTTP() {
		// Streamable HTTP transport（对齐原版 Python MCP 的
		// transport: "streamable_http" 语义）。
		url, _ := c.config["url"].(string)
		if url == "" {
			cancel()
			return fmt.Errorf("MCP streamable HTTP client %q is missing the url", c.name)
		}
		var opts []transport.StreamableHTTPCOption
		if hs := configHeaders(c.config); len(hs) > 0 {
			opts = append(opts, transport.WithHTTPHeaders(hs))
		}
		cl, err = client.NewStreamableHttpClient(url, opts...)
	} else {
		url, _ := c.config["url"].(string)
		if url == "" {
			cancel()
			return fmt.Errorf("MCP client %q has neither a URL nor a stdio command", c.name)
		}
		var opts []transport.ClientOption
		if hs := configHeaders(c.config); len(hs) > 0 {
			opts = append(opts, transport.WithHeaders(hs))
		}
		cl, err = client.NewSSEMCPClient(url, opts...)
	}
	if err != nil {
		cancel()
		return err
	}
	c.cl = cl
	// Any failure after this point must tear down the client (for stdio this
	// kills/wait the subprocess) and reset state so the client is not reported
	// as active while holding a half-open connection.
	fail := func(err error) error {
		_ = cl.Close()
		c.cl = nil
		c.active = false
		c.tools = nil
		cancel()
		return err
	}

	if err := cl.Start(handshakeCtx); err != nil {
		mcpLogger.Error("MCP server %s start failed: %v", c.name, err)
		return fail(fmt.Errorf("MCP start failed: %w", err))
	}
	initReq := mcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = mcp.Implementation{Name: "astrbot-go", Version: "1.0.0"}
	if _, err := cl.Initialize(handshakeCtx, initReq); err != nil {
		mcpLogger.Error("MCP server %s initialize failed: %v", c.name, err)
		return fail(fmt.Errorf("MCP initialize failed: %w", err))
	}

	if err := c.listTools(handshakeCtx); err != nil {
		mcpLogger.Error("MCP server %s list tools failed: %v", c.name, err)
		return fail(err)
	}
	mcpLogger.Debug("MCP server %s connected: %d tools", c.name, len(c.tools))
	return nil
}

// mcpStdioCommandAllowlist 是 stdio MCP 允许启动的命令白名单（对齐 Python 版
// _DEFAULT_STDIO_COMMAND_ALLOWLIST）。可用环境变量
// ASTRBOT_MCP_STDIO_ALLOWED_COMMANDS（逗号分隔）整体覆盖。
var mcpStdioCommandAllowlist = map[string]struct{}{
	"python": {}, "python3": {}, "py": {},
	"node": {}, "npx": {}, "npm": {}, "pnpm": {}, "yarn": {},
	"bun": {}, "bunx": {}, "deno": {},
	"uv": {}, "uvx": {},
}

// mcpStdioDeniedCommands 是一律拒绝的危险命令黑名单（对齐 Python 版
// _DENIED_STDIO_COMMANDS：shell、远程下载、文件破坏、提权/关机类）。
var mcpStdioDeniedCommands = map[string]struct{}{
	"bash": {}, "sh": {}, "zsh": {}, "fish": {},
	"cmd": {}, "cmd.exe": {}, "powershell": {}, "powershell.exe": {},
	"pwsh": {}, "pwsh.exe": {},
	"osascript": {}, "open": {},
	"curl": {}, "wget": {}, "nc": {}, "netcat": {}, "telnet": {},
	"ssh": {}, "scp": {},
	"rm": {}, "mv": {}, "cp": {}, "dd": {}, "mkfs": {},
	"sudo": {}, "su": {}, "chmod": {}, "chown": {},
	"kill": {}, "killall": {},
	"shutdown": {}, "reboot": {}, "poweroff": {}, "halt": {},
}

// jsInlineCodeFlags 是 node/deno/bun 的内联求值参数（对齐 _JS_INLINE_CODE_FLAGS）。
var jsInlineCodeFlags = map[string]struct{}{
	"-e": {}, "--eval": {}, "-p": {}, "--print": {},
}

// deniedDockerArgs 是 docker 启动时拒绝的危险参数（对齐 _DENIED_DOCKER_ARGS）。
var deniedDockerArgs = map[string]struct{}{
	"--privileged": {}, "--pid=host": {}, "--network=host": {},
	"--net=host": {}, "--ipc=host": {},
}

// validateMCPStdioConfig 在启动任何子进程前校验 stdio MCP 配置（移植自
// Python 版 astrbot/core/agent/mcp_client.py 的 validate_mcp_stdio_config）：
// 命令去空白/去扩展名规范化后，先拒绝 shell 元字符与黑名单命令，再强制白
// 名单（可用 ASTRBOT_MCP_STDIO_ALLOWED_COMMANDS 覆盖），最后按命令类型
// 校验 args（禁止 python/node 系内联求值、禁止 docker 危险参数）。
// serverName 仅用于错误信息定位，不参与校验逻辑。
func validateMCPStdioConfig(serverName, command string, args []string) error {
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("MCP stdio server %q requires a non-empty command", serverName)
	}
	if strings.ContainsAny(command, "\r\n\x00;&|<>`$") {
		return fmt.Errorf("MCP stdio server %q command contains unsafe shell metacharacters", serverName)
	}

	commandName := normalizeStdioCommandName(command)
	if _, denied := mcpStdioDeniedCommands[commandName]; denied {
		return fmt.Errorf("MCP stdio server %q command `%s` is not allowed", serverName, commandName)
	}

	allowlist := mcpStdioCommandAllowlist
	if configured := strings.TrimSpace(os.Getenv("ASTRBOT_MCP_STDIO_ALLOWED_COMMANDS")); configured != "" {
		allowlist = map[string]struct{}{}
		for _, item := range strings.Split(configured, ",") {
			if item = strings.TrimSpace(item); item != "" {
				allowlist[normalizeStdioCommandName(item)] = struct{}{}
			}
		}
	}
	if _, ok := allowlist[commandName]; !ok {
		names := make([]string, 0, len(allowlist))
		for n := range allowlist {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf(
			"MCP stdio server %q command `%s` is not allowed. Allowed commands: %s. Set ASTRBOT_MCP_STDIO_ALLOWED_COMMANDS to override this list if you trust another launcher",
			serverName, commandName, strings.Join(names, ", "),
		)
	}

	if err := validateStdioArgs(commandName, args); err != nil {
		return fmt.Errorf("MCP stdio server %q: %w", serverName, err)
	}
	return nil
}

// normalizeStdioCommandName 对齐 Python 版 _normalize_stdio_command_name：
// 取路径最后一段（兼容 Windows 反斜杠路径），转小写并剥离 .exe/.cmd/.bat。
func normalizeStdioCommandName(command string) string {
	command = strings.TrimSpace(command)
	commandName := command
	if i := strings.LastIndexAny(command, `/\`); i >= 0 {
		commandName = command[i+1:]
	}
	commandName = strings.ToLower(commandName)
	for _, suffix := range []string{".exe", ".cmd", ".bat"} {
		if strings.HasSuffix(commandName, suffix) {
			return strings.TrimSuffix(commandName, suffix)
		}
	}
	return commandName
}

// validateStdioArgs 对齐 Python 版 _validate_stdio_args：拒绝控制字符，并
// 按命令类型封堵内联代码执行面（python -c、node -e/eval、docker 危险参数）。
func validateStdioArgs(commandName string, args []string) error {
	for _, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("MCP stdio args cannot contain control characters")
		}
	}

	isPython := strings.HasPrefix(commandName, "python") || commandName == "py"
	isJS := commandName == "node" || commandName == "deno" || commandName == "bun" || strings.HasPrefix(commandName, "node")

	switch {
	case isPython:
		for _, arg := range args {
			if arg == "-c" || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.Contains(arg, "c")) {
				return errors.New("MCP stdio Python servers must be launched from a module or file; inline code flags such as -c are not allowed")
			}
		}
	case isJS:
		for _, arg := range args {
			if _, bad := jsInlineCodeFlags[arg]; bad || arg == "eval" {
				return errors.New("MCP stdio JavaScript servers must be launched from a package or file; inline eval flags are not allowed")
			}
			if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsAny(arg, "ep") {
				return errors.New("MCP stdio JavaScript servers must be launched from a package or file; inline eval flags are not allowed")
			}
		}
	case commandName == "docker":
		for i, arg := range args {
			if _, bad := deniedDockerArgs[arg]; bad {
				return fmt.Errorf("MCP stdio Docker args are unsafe and not allowed: %s", arg)
			}
			if (arg == "--network" || arg == "--net" || arg == "--pid" || arg == "--ipc") && i+1 < len(args) && args[i+1] == "host" {
				return fmt.Errorf("MCP stdio Docker args are unsafe and not allowed: %s host", arg)
			}
		}
	}
	return nil
}

// isStdio reports whether the configured transport is stdio.
func (c *MCPClient) isStdio() bool {
	t, _ := c.config["transport"].(string)
	if t == "" {
		t, _ = c.config["type"].(string)
	}
	return t == "stdio"
}

// isStreamableHTTP reports whether the configured transport is streamable
// HTTP（对齐原版 Python MCP 的 transport 命名）。
func (c *MCPClient) isStreamableHTTP() bool {
	t, _ := c.config["transport"].(string)
	if t == "" {
		t, _ = c.config["type"].(string)
	}
	switch t {
	case "streamable_http", "streamable", "http":
		return true
	}
	return false
}

// configHeaders extracts the configured HTTP headers into a map[string]string.
func configHeaders(config map[string]interface{}) map[string]string {
	hs := map[string]string{}
	if headers, ok := config["headers"].(map[string]interface{}); ok {
		for k, v := range headers {
			if vs, ok := v.(string); ok {
				hs[k] = vs
			}
		}
	}
	return hs
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
	mcpLogger.Debug("MCP tool call: server=%s tool=%s", c.name, toolName)
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
	case mcp.ResourceLink:
		text := v.URI
		if v.Name != "" {
			text = v.Name + ": " + v.URI
		}
		return map[string]interface{}{"type": "text", "text": text}
	default:
		if b, err := json.Marshal(content); err == nil {
			return map[string]interface{}{"type": "text", "text": string(b)}
		}
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
	// 释放握手 ctx 的 cancel（SSE 读循环 / stdio 子进程均随其终止）。
	if c.connectCancel != nil {
		c.connectCancel()
		c.connectCancel = nil
	}
	if c.cl != nil {
		_ = c.cl.Close()
		c.cl = nil
	}
}

// Reconnect tears down the current connection and establishes a fresh one.
// Used by callers after a transport failure (e.g. SSE connection lost).
//
// expected is the underlying *client.Client the caller's failed call used.
// Under reconnectMu we double-check the live connection: if it has already been
// replaced by a concurrent Reconnect, this is a no-op so a burst of concurrent
// failures cannot tear down a freshly rebuilt connection (reconnect storm).
func (c *MCPClient) Reconnect(ctx context.Context, expected *client.Client) error {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()
	c.mu.Lock()
	cur := c.cl
	c.mu.Unlock()
	if cur != nil && cur != expected {
		return nil
	}
	// Cleanup 会关闭并清空 cl，与并发的 Connect/CallTool 交错时可能重入破坏状态。
	// 用独立的 reconnectMu 串行化整段操作；内部 Connect 自行持 c.mu，二者不同
	// 锁序，不会死锁。
	c.Cleanup()
	c.mu.Lock()
	c.active = true
	c.mu.Unlock()
	return c.Connect(ctx)
}

// Conn returns the underlying mcp-go client handle currently in use, or nil
// when not connected. Callers pass the handle their failed call observed into
// Reconnect so a concurrent reconnect is not torn down twice.
func (c *MCPClient) Conn() *client.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cl
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
