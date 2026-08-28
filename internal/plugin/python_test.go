package plugin

import (	"context"
	"fmt"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"github.com/WaterGodFurina/Astrbot-golang/internal/pysdk"

	sdkv1 "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/gen/sdkv1"
	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

// sdkEvent builds a *pluginsdk.Event from a raw map (used to simulate host
// events crossing the RPC boundary).
// sdkEvent 从测试用的 map 直接构造 proto SDKEvent（P1 native，不经过
// SDK struct / JSON）。
func sdkEvent(t *testing.T, m map[string]any) *sdkv1.SDKEvent {
	t.Helper()
	se := &sdkv1.SDKEvent{}
	if v, ok := m["type"]; ok { se.Type = fmt.Sprint(v) }
	if v, ok := m["platform"]; ok { se.Platform = fmt.Sprint(v) }
	if v, ok := m["platform_id"]; ok { se.PlatformId = fmt.Sprint(v) }
	if v, ok := m["message_type"]; ok { se.MessageType = fmt.Sprint(v) }
	if v, ok := m["self_id"]; ok { se.SelfId = fmt.Sprint(v) }
	if v, ok := m["sender_id"]; ok { se.SenderId = fmt.Sprint(v) }
	if v, ok := m["sender_name"]; ok { se.SenderName = fmt.Sprint(v) }
	if v, ok := m["conv_id"]; ok { se.ConvId = fmt.Sprint(v) }
	if v, ok := m["group_name"]; ok { se.GroupName = fmt.Sprint(v) }
	if v, ok := m["message_str"]; ok { se.MessageStr = fmt.Sprint(v) }
	if v, ok := m["plain_text"]; ok { se.PlainText = fmt.Sprint(v) }
	if v, ok := m["raw_message"]; ok { se.RawMessage = fmt.Sprint(v) }
	if v, ok := m["message_id"]; ok { se.MessageId = fmt.Sprint(v) }
	if v, ok := m["is_group"]; ok { se.IsGroup = fmt.Sprint(v) == "true" }
	if v, ok := m["is_at_bot"]; ok { se.IsAtBot = fmt.Sprint(v) == "true" }
	if v, ok := m["is_admin"]; ok { se.IsAdmin = fmt.Sprint(v) == "true" }
	if v, ok := m["timestamp"]; ok {
		switch n := v.(type) {
		case float64: se.Timestamp = int64(n)
		case int: se.Timestamp = int64(n)
		case int64: se.Timestamp = n
		}
	}
	if md, ok := m["metadata"]; ok {
		if b, err := json.Marshal(md); err == nil {
			se.MetadataJson = b
		}
	}
	return se
}

// TestPythonPluginEndToEnd loads a real Python plugin through the subprocess
// runtime (go-plugin handshake + gRPC + the Python SDK bridge) and exercises
// Register/HandleCommand/HandleFilter/HandleLLMRequest/HandleTool.
//
// It requires a Python interpreter with (or able to install) grpcio/protobuf;
// the first run may create a venv and download packages.
func TestPythonPluginEndToEnd(t *testing.T) {
	// 跳过条件：未找到 python 解释器
	if pysdk.DiscoverPythonBin() == "" {
		t.Skip("python3 不可用")
	}
	// 独立 venv/缓存目录：不共享 ~/.cache/astrbot-go 下的 venv，避免测试
	// 环境污染与跨进程 pip 并发（t.Setenv 不可在并行测试中调用——本测试
	// 不 t.Parallel()）。
	t.Setenv("ASTRBOT_PYTHON_CACHE_DIR", t.TempDir())
	pluginDir := filepath.Join("testdata", "python_plugin")
	if _, err := os.Stat(pluginDir); err != nil {
		t.Skipf("缺少 testdata/python_plugin: %v", err)
	}

	dataDir := t.TempDir()
	ctx := context.Background()
	m := NewSubprocessManager(nil, dataDir)
	m.MaxRestarts = 2
	m.RestartBaseDelay = 100 * time.Millisecond
	// 独立端口区间：与真实宿主（10000-25000）隔离，避免握手/端口干扰。
	m.MinPort = 50100
	m.MaxPort = 50200
	t.Cleanup(m.Shutdown)

	// 预置插件配置 + 宿主 HostService（GetConfig/SetConfig 反向调用）
	cfgDir := filepath.Join(dataDir, "plugins_config", "test_pyplugin")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(`{"greeting": "你好", "enabled": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	SetHostService(nil, m, nil, nil, nil, nil, nil)
	// SetHostService 是包级全局（pluginsdk host hooks）：测试结束恢复为空，
	// 避免污染后续测试（如 runtime_test 的 TestHostServiceReverseCalls）。
	defer pluginsdk.SetHostHooks(pluginsdk.HostServiceHooks{})

	start := time.Now()
	inst, err := m.LoadLang(ctx, "test_pyplugin", pluginDir, "python")
	t.Logf("LoadLang 耗时 %v", time.Since(start))
	if err != nil {
		t.Fatalf("LoadLang: %v", err)
	}
	if inst == nil || inst.Meta == nil {
		t.Fatal("inst/Meta 为空")
	}
	if inst.Language != "python" {
		t.Fatalf("Language = %q, want python", inst.Language)
	}
	t.Logf("插件: name=%s version=%s", inst.Name, inst.Version)

	// _conf_schema.json → config schema 上报
	if len(inst.Meta.ConfigSchemaJson) == 0 {
		t.Fatal("ConfigSchemaJson 为空（未读取 _conf_schema.json）")
	}
	var schema map[string]any
	if err := json.Unmarshal(inst.Meta.ConfigSchemaJson, &schema); err != nil {
		t.Fatalf("ConfigSchemaJson 解析失败: %v", err)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) != 3 {
		t.Fatalf("schema properties 异常: %v", schema)
	}

	// Register 元数据
	found := map[string]bool{}
	for _, c := range inst.Meta.Commands {
		found["cmd:"+c.Name] = true
		t.Logf("command: %s perm=%s", c.Name, c.Permission)
	}
	for _, f := range inst.Meta.Filters {
		found["filter:"+f.Name] = true
	}
	for _, h := range inst.Meta.Hooks {
		found["hook:"+h.Event] = true
	}
	for _, tl := range inst.Meta.Tools {
		found["tool:"+tl.Name] = true
		t.Logf("tool: %s params=%s", tl.Name, string(tl.ParamsJson))
	}
	for _, k := range []string{"cmd:pyhello", "cmd:pyadd", "filter:python_plugin.main_echo", "hook:on_llm_request", "tool:py_add_tool"} {
		if !found[k] {
			t.Errorf("缺少 %s（已注册: %v）", k, found)
		}
	}

	ev := map[string]any{
		"type": "message", "platform": "qq_official", "sender_id": "123",
		"sender_name": "u", "conv_id": "c1", "is_group": false, "is_at_bot": true,
		"is_admin": false, "message_str": "pyhello", "plain_text": "pyhello",
		"timestamp": 0,
		"chain":     []map[string]any{{"type": "Plain", "text": "pyhello"}},
	}

	// HandleCommand
	cmdCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	text, chain, result, err := inst.Client.HandleCommand(cmdCtx, "pyhello", nil, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("HandleCommand: %v", err)
	}
	if len(chain) == 0 || chain[0].Text == "" || !strings.Contains(chain[0].Text, "Hello from Python") {
		t.Fatalf("pyhello 回复异常: text=%q chain=%v", text, chain)
	}
	if result.GetSent() {
		t.Fatal("pyhello 通过 Result 回复（非主动发送），sent 必须为 false")
	}
	t.Logf("pyhello -> %s", chain[0].Text)

	// 带参数命令
	_, chain2, _, err := inst.Client.HandleCommand(cmdCtx, "pyadd", []string{"3", "4"}, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("pyadd: %v", err)
	}
	if len(chain2) == 0 || !strings.Contains(chain2[0].Text, "7") {
		t.Fatalf("pyadd 回复异常: %v", chain2)
	}
	t.Logf("pyadd -> %s", chain2[0].Text)

	// HostService 反向调用（GetConfig：插件经 broker 读取宿主配置）
	_, chainCfg, _, err := inst.Client.HandleCommand(cmdCtx, "pycfg", nil, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("pycfg: %v", err)
	}
	if len(chainCfg) == 0 || !strings.Contains(chainCfg[0].Text, "你好") || !strings.Contains(chainCfg[0].Text, "enabled") {
		t.Fatalf("pycfg 未读到宿主配置: %v", chainCfg)
	}
	t.Logf("pycfg -> %s", chainCfg[0].Text)

	// HostService 反向调用（TextToImage：宿主 t2i 渲染返回 PNG）
	_, chainT2I, _, err := inst.Client.HandleCommand(cmdCtx, "pyt2i", nil, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("pyt2i: %v", err)
	}
	if len(chainT2I) == 0 || !strings.Contains(chainT2I[0].Text, "t2i_len=") {
		t.Fatalf("pyt2i 结果异常: %v", chainT2I)
	}
	t.Logf("pyt2i -> %s", chainT2I[0].Text)

	// HandleFilter（正则匹配）
	ev2 := map[string]any{
		"type": "message", "platform": "qq_official", "sender_id": "123",
		"conv_id": "c1", "is_group": false, "is_at_bot": true, "is_admin": false,
		"message_str": "pyecho hello", "plain_text": "pyecho hello", "timestamp": 0,
		"chain": []map[string]any{{"type": "Plain", "text": "pyecho hello"}},
	}
	allow, _, err := inst.Client.HandleFilter(cmdCtx, "python_plugin.main_echo", sdkEvent(t, ev2))
	if err != nil {
		t.Fatalf("HandleFilter: %v", err)
	}
	if !allow {
		t.Fatal("pyecho 应命中过滤器")
	}
	allow2, _, _ := inst.Client.HandleFilter(cmdCtx, "python_plugin.main_echo", sdkEvent(t, ev))
	if !allow2 {
		t.Fatal("pyhello 不应命中过滤器（应放行）")
	}

	// HandleLLMRequest（on_llm_request 注入 system prompt）
	sp, _, stop, _, err := inst.Client.HandleLLMRequest(cmdCtx, "python_plugin.main_llm_req", sdkEvent(t, ev), "SP", "hi")
	if err != nil {
		t.Fatalf("HandleLLMRequest: %v", err)
	}
	if stop || !strings.Contains(sp, "Python插件注入") {
		t.Fatalf("llm_req 注入失败: sp=%q stop=%v", sp, stop)
	}

	// HandleTool
	text2, isErr, _, err := inst.Client.HandleTool(cmdCtx, "py_add_tool", map[string]any{"a": 5, "b": 6}, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("HandleTool: %v", err)
	}
	if isErr || text2 != "11" {
		t.Fatalf("py_add_tool 结果异常: text=%q err=%v", text2, isErr)
	}

	// Cleanup 后进程退出
	_ = m.Unload("test_pyplugin")
	if m.Get("test_pyplugin") != nil {
		t.Fatal("卸载后实例仍在")
	}
}

// TestPythonRuntimeCacheSelfHeals: Python 运行时缓存（m.pythonEnv）持有的
// venv 被外部清理（如 ~/.cache 被系统回收、用户手动删除）后，再次解析必须
// 丢弃失效缓存并重建 venv——否则插件闲置休眠后工具调用唤醒（EnsureLoaded →
// startInstance → pythonRuntime）会拿一个不存在的解释器启动子进程
// （"python-venv-xxx/bin/python 路径不存在"），LLM 工具调用随之失败。
func TestPythonRuntimeCacheSelfHeals(t *testing.T) {
	if pysdk.DiscoverPythonBin() == "" {
		t.Skip("python3 不可用")
	}
	// 独立 venv/缓存目录：不共享 ~/.cache/astrbot-go 下的 venv，避免污染
	// 真实宿主缓存与跨进程 pip 并发；pip 缓存也隔离到临时目录（沙箱内
	// ~/.cache/pip 可能不可写导致 wheel 构建失败）。
	t.Setenv("ASTRBOT_PYTHON_CACHE_DIR", t.TempDir())
	t.Setenv("PIP_CACHE_DIR", t.TempDir())
	m := newTestManager(t)
	defer m.Shutdown()

	env1, err := m.pythonRuntimeWithStage(nil)
	if err != nil {
		t.Fatalf("首次准备 Python 运行时: %v", err)
	}
	if _, err := os.Stat(env1.PythonBin); err != nil {
		t.Fatalf("首次 PythonBin 不存在: %v", err)
	}
	// 缓存命中：再次解析返回同一指针（不重建）。
	envHit, err := m.pythonRuntimeWithStage(nil)
	if err != nil {
		t.Fatalf("缓存命中解析失败: %v", err)
	}
	if envHit != env1 {
		t.Fatal("缓存命中应返回同一 RuntimeEnv 指针（未走重建）")
	}

	// 模拟外部清理：删除 venv 根目录（.../python-venv-xxx）。系统 Python
	// 自带依赖（无 venv）时删除步骤跳过，该场景缓存校验自然通过。
	venvRoot := filepath.Dir(filepath.Dir(env1.PythonBin))
	if strings.HasPrefix(filepath.Base(venvRoot), "python-venv-") {
		if err := os.RemoveAll(venvRoot); err != nil {
			t.Fatalf("删除 venv 失败: %v", err)
		}
		if _, err := os.Stat(env1.PythonBin); err == nil {
			t.Fatal("venv 删除后解释器仍存在（前置条件失效）")
		}
	}

	// 关键断言：缓存失效后再次解析必须自愈重建（而非返回失效缓存）。
	env2, err := m.pythonRuntimeWithStage(nil)
	if err != nil {
		t.Fatalf("缓存失效后重建失败: %v", err)
	}
	if _, err := os.Stat(env2.PythonBin); err != nil {
		t.Fatalf("重建后 PythonBin 仍不存在: %v", err)
	}
	// 重建结果再次进入缓存：命中返回同一指针。
	envHit2, err := m.pythonRuntimeWithStage(nil)
	if err != nil {
		t.Fatalf("重建后缓存命中失败: %v", err)
	}
	if envHit2 != env2 {
		t.Fatal("重建后应缓存新的 RuntimeEnv 指针")
	}
}

// TestPythonPluginSendMarksSent: 插件 handler 里 event.send（主动发送）后，
// HandleCommand 必须返回 sent=true（对齐 Python _has_send_oper）——宿主管线
// 据此不再走 LLM。
func TestPythonPluginSendMarksSent(t *testing.T) {
	// 独立 venv/缓存目录（同 EndToEnd，避免与真实宿主/其他测试共享
	// ~/.cache/astrbot-go 下的 venv 与 pip 并发）。
	t.Setenv("ASTRBOT_PYTHON_CACHE_DIR", t.TempDir())
	m := newTestManager(t)
	// 独立端口区间：与真实宿主默认范围（10000-25000）及其他测试隔离。
	m.MinPort = 50900
	m.MaxPort = 51000
	ctx := context.Background()
	inst, err := m.LoadLang(ctx, "pysend", filepath.Join("testdata", "python_plugin"), "python")
	if err != nil {
		t.Fatalf("LoadLang: %v", err)
	}
	defer func() { _ = m.Unload("pysend") }()

	ev := map[string]any{
		"type": "message", "platform": "qq_official", "sender_id": "123",
		"conv_id": "c1", "is_group": false, "is_at_bot": true, "is_admin": true,
		"message_str": "pysend", "plain_text": "pysend", "timestamp": 0,
		"chain": []map[string]any{{"type": "Plain", "text": "pysend"}},
	}
	// pysend 不返回文本（Result 为空），只主动发送——sent 必须为 true。
	text, _, result, err := inst.Client.HandleCommand(ctx, "pysend", nil, sdkEvent(t, ev))
	if err != nil {
		t.Fatalf("HandleCommand(pysend): %v", err)
	}
	if text != "" {
		t.Fatalf("pysend 不应返回文本: %q", text)
	}
	if !result.GetSent() {
		t.Fatal("event.send 后 HandleCommand 必须返回 sent=true（否则宿主继续走 LLM）")
	}
}
