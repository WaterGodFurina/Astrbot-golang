package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/agent"
)

// MCPBridge 抽象宿主 MCP 客户端集合对插件桥的最小能力（只读列出 + 调用），
// 由 lifecycle 注入实现（MCPClientPool），避免 plugin 包依赖 pipeline 的
// ProcessStage 内部状态（插件自管 MCP 不经此通道）。
type MCPBridge interface {
	// ListTools 汇总全部已连接 MCP server 的活跃工具，每项 map 含
	// server/name/description/schema_json（schema_json 为入参 JSON Schema
	// 字符串）。
	ListTools() []map[string]any
	// CallTool 调用指定 server 的工具：result 为完整结果对象
	//（{"content": [...], "isError": bool}），text 为纯文本摘要，isError
	// 标记宿主侧调用是否出错。
	CallTool(ctx context.Context, server, toolName string, args map[string]any) (result map[string]any, text string, isError bool, err error)
}

// MCPClientPool 是 MCPBridge 的宿主实现：持有自己的 agent.MCPClient 集合，
// 惰性连接 data/mcp_server.json 中 active 的 server，并按配置文件 mtime
// 增量重建（对齐 pipeline ProcessStage.loadMCPTools 的数据源与 active 语义）。
//
// 注：ProcessStage 的客户端集合服务于 LLM 工具循环，按管线生命周期重建；
// 本池独立按需连接（仅插件实际调用 McpListTools/McpCallTool 时才建连），
// 不与管线共享连接——对 SSE/HTTP server 是轻量冗余，对 stdio server 意味着
// 一次额外的子进程连接（见 ⚠️ 存疑项）。
type MCPClientPool struct {
	mu      sync.Mutex
	cfgPath string
	clients map[string]*agent.MCPClient // 原始 server 名 → client
	modTime time.Time
	loaded  bool
}

// NewMCPClientPool 创建池。dataDir 为宿主数据目录（配置位于
// <dataDir>/mcp_server.json）。
func NewMCPClientPool(dataDir string) *MCPClientPool {
	return &MCPClientPool{
		cfgPath: filepath.Join(dataDir, "mcp_server.json"),
		clients: map[string]*agent.MCPClient{},
	}
}

// ensureLoadedLocked 按配置文件 mtime 判断是否需要（重）建客户端集合。
// 缺失/损坏的配置视为空集合（与 ProcessStage 行为一致），不报错。
// 调用方须持有 p.mu。
func (p *MCPClientPool) ensureLoadedLocked() {
	info, err := os.Stat(p.cfgPath)
	if err != nil {
		p.resetLocked(nil, time.Time{})
		return
	}
	if p.loaded && info.ModTime().Equal(p.modTime) {
		return
	}
	data, err := os.ReadFile(p.cfgPath)
	if err != nil {
		p.resetLocked(nil, info.ModTime())
		return
	}
	var cfg struct {
		McpServers map[string]map[string]interface{} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		p.resetLocked(nil, info.ModTime())
		return
	}
	clients := map[string]*agent.MCPClient{}
	for name, srvCfg := range cfg.McpServers {
		if active, _ := srvCfg["active"].(bool); !active {
			continue
		}
		client := agent.NewMCPClient(name, srvCfg)
		// 连接用独立 context（SSE 传输可能共享它做读循环），超时对齐
		// ProcessStage.loadMCPTools 的 30s。
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cerr := client.Connect(ctx)
		cancel()
		if cerr != nil {
			continue
		}
		clients[name] = client
		// 兼容按净化名寻路（对齐 ProcessStage 的 sanitize 工具名路由）。
		if safe := sanitizeMCPName(name); safe != name {
			clients[safe] = client
		}
	}
	p.resetLocked(clients, info.ModTime())
}

// resetLocked 一次性替换池状态（旧连接全部清理）。调用方须持有 p.mu。
func (p *MCPClientPool) resetLocked(clients map[string]*agent.MCPClient, modTime time.Time) {
	for _, c := range p.clients {
		c.Cleanup()
	}
	p.clients = clients
	p.modTime = modTime
	p.loaded = true
}

// ListTools implements MCPBridge.
func (p *MCPClientPool) ListTools() []map[string]any {
	p.mu.Lock()
	p.ensureLoadedLocked()
	// 净化名条目是同一 client 的别名，按 client.Name() 去重后按名排序。
	byName := map[string]*agent.MCPClient{}
	for _, c := range p.clients {
		byName[c.Name()] = c
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	type serverTools struct {
		name  string
		tools []agent.MCPToolInfo
	}
	all := make([]serverTools, 0, len(names))
	for _, name := range names {
		c := byName[name]
		if c == nil || !c.IsActive() {
			continue
		}
		all = append(all, serverTools{name: name, tools: c.Tools()})
	}
	p.mu.Unlock()

	out := make([]map[string]any, 0)
	for _, st := range all {
		for _, t := range st.tools {
			schemaJSON, err := json.Marshal(t.InputSchema)
			if err != nil {
				schemaJSON = []byte("{}")
			}
			out = append(out, map[string]any{
				"server":      st.name,
				"name":        t.Name,
				"description": t.Description,
				"schema_json": string(schemaJSON),
			})
		}
	}
	return out
}

// CallTool implements MCPBridge. 按 server 名（原始名或净化名）寻路；调用
// 短超时失败后重连一次再试（对齐 ProcessStage.executeMCPTool 的快速失败 +
// 重连语义）。
func (p *MCPClientPool) CallTool(ctx context.Context, server, toolName string, args map[string]any) (map[string]any, string, bool, error) {
	if server == "" {
		return nil, "", false, fmt.Errorf("MCP server name is empty")
	}
	p.mu.Lock()
	p.ensureLoadedLocked()
	c := p.clients[server]
	p.mu.Unlock()
	if c == nil {
		return nil, "", false, fmt.Errorf("MCP server %q not connected", server)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn := c.Conn()
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	result, err := c.CallTool(callCtx, toolName, args)
	cancel()
	if err != nil {
		reconnCtx, reconnCancel := context.WithTimeout(ctx, 30*time.Second)
		rc := c.Reconnect(reconnCtx, conn)
		reconnCancel()
		if rc != nil {
			return nil, "", false, fmt.Errorf("MCP server %q 工具调用失败: %w（重连亦失败: %v）", server, err, rc)
		}
		retryCtx, retryCancel := context.WithTimeout(ctx, 60*time.Second)
		result, err = c.CallTool(retryCtx, toolName, args)
		retryCancel()
		if err != nil {
			return nil, "", false, fmt.Errorf("MCP server %q 工具调用失败: %w", server, err)
		}
	}
	content := []map[string]any{}
	isError := false
	if result != nil {
		if result.Content != nil {
			content = result.Content
		}
		isError = result.IsError
	}
	out := map[string]any{"content": content, "isError": isError}
	return out, mcpContentTextFromMaps(content), isError, nil
}

// sanitizeMCPName 与 pipeline.ProcessStage 的 sanitizeToolName 语义一致：
// 非字母数字/_/- 字符替换为 "_"，作为 MCP 工具名的 server 前缀段。
func sanitizeMCPName(name string) string {
	var sb strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			sb.WriteRune(r)
		default:
			sb.WriteRune('_')
		}
	}
	return sb.String()
}

// mcpContentTextFromMaps 提取结果内容块的纯文本摘要（对齐 pipeline
// mcpContentText：text 块取 text 字段，其余块有 text 字段也采纳）。
func mcpContentTextFromMaps(content []map[string]any) string {
	var parts []string
	for _, block := range content {
		if text, ok := block["text"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}
