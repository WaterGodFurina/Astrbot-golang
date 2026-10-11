# AstrBot-golang 最近 3 个 Commit 代码审查报告

- 审查日期：2026-10-11
- 仓库：/home/xiaowei/Astrbot-golang
- 审查范围：`1447dc5`、`c09e74f`、`f18967e` 三个 commit 的 Go 后端代码（排除 dashboard/dist 构建产物）
- 修复要求：对照 astrbot-py（Python 原版）源码行为修复

---

## Commit `1447dc5`（相比1.3.3，修复了多个问题）

### 高

1. **群聊 + 流式 + 显示推理时，最终回复会整体丢失** — `internal/pipeline/stages.go:2554-2568, 2366-2367`
   `sendReasoningSegment` 不检查 `segMode/frag/群聊`，无条件 `SendByEvent` 并置 `ss.sent = true`。群聊设计上不发增量正文（frag=nil 且 segMode=false，`flush()` 不发 pending），但推理片段照样直发 → `finalizeAgentReply` 标记 `streamed=true` → RespondStage (stages.go:5261) 跳过最终全文发送。结果：群聊开启 `display_reasoning_text` 后用户只收到推理片段，收不到答案。
   修复方向：`sendReasoningSegment` 增加 `ss.segMode || ss.frag != nil` 门控，或推理不参与 `sent` 标记。对照 py：推理链的发送策略。

2. **第三方 runner（dify/coze/dashscope/deerflow）强制依赖本地 chat provider** — `internal/pipeline/stages.go`（prepareAgentRequest → resolveProvider 先于 dispatch）
   `runner_type=dify` 等纯第三方配置（无任何 chat provider）在进入 runner 分发前就因 `errNoAvailableProvider` 静默失败。且 `resolveProvider` 的"第一个 enabled provider"回退不排除 `agent_runner` 类型条目，可能选中后 `CreateProvider("dify")` 报 unknown type。
   修复方向：第三方 runner 跳过本地 provider 解析；resolveProvider 排除 agent_runner provider。对照 py：third_party runner 不需要 chat provider。

### 中

3. **dashscope runner goroutine 泄漏** — `internal/agent/runners/dashscope/runner.go`：`for line := range stream` 内部 early return 后，SDK 生产 goroutine 在缓冲 500 行后永久阻塞在 channel 发送上。
4. **dashscope 非流式模式可能拿不到输出**：恒设 `X-DashScope-SSE: enable`，且仅解析 `data:` 行前缀。
5. **deerflow 超时部分结果路径不可达** — `runner.go:140-150`：`ctx.Err()` 检查的是父 ctx，应改为 `errors.Is(streamErr, context.DeadlineExceeded)`。
6. **migrate.go 非 local runner 找不到 provider 时静默回退默认配置** → API key 静默丢失。
7. **`insertedBefore` 守卫永不触发** — migrate.go：`mapsEqual` 用 `reflect.DeepEqual`，JSON 的 float64(5) vs 默认值 int 5 永不相等（Python `==` 语义相等）。该状态配置会被 cleanup 分支不迁移直接删除 legacy 键。
8. **chat_stream.go `absorbThink` 合并分支未重置 `lastPlainIndex`** → 交错流时持久化 parts 顺序错乱。
9. **coze/dify `doJSON` 不检查 HTTP 状态码** — 非 2xx 错误响应被当正常 JSON 解析。
10. **telegram drafts map 泄漏**：流中途异常终止时 `takeDraft` 永不执行。
11. **discord/satori/misskey 入站语音转 wav 临时文件不回收**：discord 用 `os.CreateTemp("")` 系统临时目录。

### 低

- dify `parseResult` `[]interface{}` 分支：混列表时 dump 整个 Go 切片；workflow 无 `workflow_finished` 时回复字面 `"<nil>"`。
- deerflow `proxy` 配置参数被忽略。
- telegram `snake_case_word` 被误判为斜体（有纯文本兜底）。
- telegram 私聊流式每次白打 2 次 `sendMessageDraft` 才回退。
- lifecycle groupResolver 按 type 匹配第一个适配器，多实例时可能取错。
- dashscope appID 未 URL 转义。
- 无问题方面：HTTP body 关闭、SSE 缓冲上限、event_bus 并发、SSRF 防护（SafeDownloadBytes）。

---

## Commit `c09e74f`（重新加回 so/dll 原生插件加载）

### 高

1. **崩溃隔离彻底丢失** — `internal/plugin/runtime.go:1983`：`go entry()` 无 recover；OnLoad panic、cgo 段错误击落宿主进程。启动失败路径泄漏僵尸插件 goroutine + gRPC server + 临时目录。
2. **多 Native 插件 SDK 全局状态串扰/越权** — `runtime.go:1980-1983`：`os.Setenv` 与异步 `go entry()` 读取竞态；`nativeHostTarget` 进程级单值，插件 A 反向调用可能以 B 的身份通过校验。
3. **官方 release（CGO_ENABLED=0）下 Native 模式 Linux/macOS 开箱即坏** — `native_loader_unix.go:18`：`plugin.Open` 依赖 cgo；`-buildmode=plugin` 构建指纹要求未校验。

### 中

4. **Native 插件"禁用→启用"二次 `plugin.Open`**（Go plugin 禁止）；`NativeTarget` reconnect 字段死代码。
5. **`entry()` 返回值被丢弃**，启动失败只能靠 15s 超时。
6. **Windows：`FindProc` 失败不 `dll.Release()`**；卸载删除被映射 dll 失败被静默忽略。
7. **Windows 回环 gRPC 无鉴权**；已卸载插件代码仍驻留。

### 低

- 宿主侧 `MkdirTemp` 目录泄漏；`native_entry.go` 注入覆盖同名源文件；rendezvous 文件在共享 /tmp；`startInstanceMu` 全程持有串化所有插件启动；`ASTRBOT_NATIVE_*` 环境变量不清理。
- 无问题方面：cgo 指针规则、加载路径校验、stderr 读取、命令注入防护。

---

## Commit `f18967e`（支持 Python 插件共享进程模式）

### 高

1. **共享 Runtime 崩溃后无自动恢复** — `shared_runtime.go` `handleSharedExit`：只写 manifest RECOVERY_PENDING，不重启进程不重载插件；实例 Client 指向死连接，消息全部失败直到手动干预。
2. **"迁回共享"是死代码** — `runtime.go:377`：`inst.Runtime == "python-grpc"` 永不成立（`dispensePlugin` 不设置 Runtime 字段，python-grpc 实例 Runtime==""）。
3. **`PythonRuntimeManager.Shutdown()` 从未被调用**：优雅关闭共享 Python 进程不被 kill、端口段（rt.minp）永不释放；Windows/macOS 泄漏孤儿进程；崩溃重建路径泄漏 16 端口段。

### 中

4. **状态镜像串扰** — `RefreshStatus` 匹配 `ps.PluginID == inst.ID || ps.PluginID == ""`：共享模式下空 ID 条目被所有实例命中。
5. **单次 UNHEALTHY 立即持久隔离**：无失败次数阈值；迁移窗口重复触发。
6. **共享模式租户边界弱化**：`resolveSharedPluginID` 重名时 map 迭代随机取第一个（非 fail-closed）。
7. **reload 顺序竞态**：旧 teardown 的 `UnloadPlugin` 会把新加载的同 id 插件从共享 Runtime 卸载。
8. **`handlePluginWebProxy` 大文件静默截断**：旧 413 行为被移除。
9. **install 时无条件 pip install 进共享 venv**：冲突防护不完整。
10. **`EnsurePlugin` Register 失败路径**：共享进程内累积僵尸插件模块。
11. **`UnloadPlugin` 先删 `rt.plugins[id]` 再发 RPC**：失败后状态永久不一致。

### 低

- `singleLineTruncate` 按字节截断碎 UTF-8；死代码（marshalSharedPlugins/rt.gen）；Go 插件传 "shared" 静默按 grpc；heartbeat 重复告警；refreshAllStatus 串行 N×15s。
- 无问题方面：proto wire 兼容、pip 无 shell 注入、zig 镜像 sha256 pin。

---

## 修复优先级

| 优先级 | 问题 | Commit |
|---|---|---|
| P0 | 群聊开推理显示丢最终回复（stages.go） | 1447dc5 |
| P0 | 共享 Runtime 崩溃无恢复 + Shutdown 未接线 + 迁回共享死代码 | f18967e |
| P0 | Native 模式崩溃隔离丢失 + 多插件身份串扰 | c09e74f |
| P1 | 第三方 runner 依赖本地 provider / dashscope 泄漏 / 状态镜像串扰 / webproxy 截断 | 各 |
| P2 | 其余中低项 | 各 |

## 修复约束

- 所有修复需对照 astrbot-py 源码确认行为语义后再改。
- SDK（go-plugins-sdk / python pysdk）行为也要核对。

---

## 修复记录（2026-10-11 本轮全部完成）

> 状态：`go build ./...` ✅、`go vet ./internal/...` ✅、`go test ./internal/...` 除 `TestPythonPluginRelativeCachePath`（系统 Python PEP 668 环境问题，memory.md 已记录，CI 正常）外全部通过 ✅。修复经 12 个并行子任务完成，对照 py v4.28.2（/tmp/opencode/AstrBot-git）。

### P0 修复

| # | 问题 | 修复 | 文件 |
|---|---|---|---|
| 1 | 群聊+推理显示丢最终回复 | `sendReasoningSegment` 增加 `frag==nil && !segMode` 门控：群聊/不支持流式平台推理不直发、不置 `sent`，最终由 RespondStage 连同 `🤔 思考:` 前缀全文发送（对齐 py `stream_to_general` 语义 + ResultDecorate 注入）。新增 `stream_reasoning_test.go` 2 用例 | `internal/pipeline/stages.go` |
| 2 | 第三方 runner 强制依赖 chat provider | `callLLMAgent` 对 `errNoAvailableProvider` + 第三方 runner（dify/coze/dashscope/deerflow）放行继续；`resolveProvider` 回退排除 `agent_runner` 类型。新增 `TestIsThirdPartyAgentRunner` 等 | `internal/pipeline/stages.go`、`agent_runner_thirdparty_test.go` |
| 3 | 共享 Runtime 崩溃无恢复 | 实现完整闭环（方案§六）：`handleSharedExit` → markCrashRecovery（保留隔离语义）→ 异步 `recoverSharedRuntime`：disposeRuntime（Kill/Close/释放端口段）→ 重置 rt/pluginReqs/gen++ → 逐插件串行 Reload（按持久化状态分流 shared/grpc），单插件失败仅告警；`recoveryMu` 保证多轮恢复串行可重入 | `internal/plugin/shared_runtime.go` |
| 4 | Shutdown 未接线 + 端口泄漏 | `SubprocessManager.Shutdown()` 调用 `sharedRuntimeManagerIfAny().Shutdown()`（sync.Once 幂等）；`shutdown()` 补 `releasePluginPort(minp)`；`ensureRuntime` 重建路径复用 `disposeRuntime` 归还端口 | `shared_runtime.go`、`runtime.go` |
| 5 | 迁回共享死代码 | `startInstance` python-grpc 分支补 `inst.Runtime = "python-grpc"`（原零值 ""），runtime.go:377 迁回判定复活；全字段消费点核对无回归（Go 插件路径不变） | `internal/plugin/runtime.go` |
| 6 | Native 崩溃隔离丢失 | `go entry()` 具名 goroutine + recover（cgo 段错误诚实注释不可恢复）；entry 退出码与 rendezvous 等待 select 竞争，失败即返不再傻等 15s；宿主 listener MkdirTemp 目录清理 | `internal/plugin/runtime.go`、`native_loader_unix.go` |
| 7 | Native CGO=0 开箱即坏 | 双管齐下：(a) `loadNativeInstance` 前置 CGO 检测，不可用时返回 i18n 明确错误（zh/en 各 3 键）；(b) release.yml CGO 矩阵——linux amd64（gnu/musl）、linux arm64-gnu（装 gcc-aarch64-linux-gnu）、macOS（Xcode clang）改 CGO_ENABLED=1，其余目标（arm64-musl/loongarch/windows-msvc）保持 0 由运行时检测兜底 | `runtime.go`、`native_loader_unix.go`、`.github/workflows/release.yml`、`i18n/locales/*.json` |

### P1 修复

| # | 问题 | 修复 | 文件 |
|---|---|---|---|
| 8 | 状态镜像串扰 | `RefreshStatus` 的 `ps.PluginID == ""` 回退仅在 `!inst.shared` 生效 | `internal/plugin/runtime.go` |
| 9 | UnloadPlugin 先删后 RPC | `delete(rt.plugins, id)` 移到 ManagePlugin(unload) 成功后；失败保留条目供重试（含 pluginReqs） | `shared_runtime.go` |
| 10 | EnsurePlugin Register 失败残留僵尸 | Register 失败尽力 ManagePlugin(unload) 回滚，失败仅告警；不进 rt.plugins/pluginReqs | `shared_runtime.go` |
| 11 | reload 顺序竞态 | （随 #3/#9 的跟踪语义收口；teardown 与恢复共用条目状态，避免新实例被旧 teardown 卸载） | `shared_runtime.go` |
| 12 | webproxy 大文件静默截断 | 恢复 64MB+1 读取 + 413（文件与 Value 字段都检查），抽 `collectPluginWebProxyMultipart` | `internal/dashboard/handlers.go` |
| 13 | dashscope goroutine 泄漏 | WithCancel ctx + abort()（cancel + drain）；顺带 session_id 落库去重 | `internal/agent/runners/dashscope/runner.go` |
| 14 | dashscope 非流式拿不到输出 | `!streaming` 走普通 POST+JSON（不带 SSE 头），状态码手动检查 | 同上 |
| 15 | deerflow 超时路径不可达 | `isStreamTimeout(streamErr) && ctx.Err()==nil`（errors.Is DeadlineExceeded/Canceled + Client.Timeout 兜底），超时返回部分结果 role=err（对齐 py） | `deerflow/runner.go` |
| 16 | coze/dify doJSON 不查状态码 | `checkHTTPStatus`（非 2xx 读 4KB body 报错）；dify 状态码放宽非 2xx；错误 body 限 4KB | `coze/api_client.go`、`dify/api_client.go` |
| 17 | dify "<nil>" 回复 | 空结果仅 warning 返回空 chain；`parseResult(nil)` 防护；`appended` 死变量清理（py 对照：纯文件列表同样不 append 文本） | `dify/runner.go` |
| 18 | absorbThink 顺序错乱 | 合并 think 分支补 `lastPlainIndex = -1` | `internal/dashboard/chat_stream.go` |
| 19 | migrate insertedBefore 永不触发 | `mapsEqualNormalized`（数值统一 float64 比较，对齐 py `==` 语义）；`configVersionOf` 兼容 int/json.Number；provider 缺失回退补警告日志（py 无日志，Go 增强）；normalizeValue 丢未知键经核 py 相同不改 | `internal/config/migrate.go` |

### P2 修复

| # | 问题 | 修复 | 文件 |
|---|---|---|---|
| 20 | telegram drafts 泄漏 | `tgDraft.createdAt` + `sweepOrphanDrafts`（30min TTL，startDraft 惰性清扫） | `telegram/adapter.go` |
| 21 | discord/satori/misskey wav 临时文件泄漏 | 新增 `utils.ScheduleTempCleanup`（30min 延迟删除，集中 mattermost 同款模式），三处注册；sessionGroup/groupSourceType 加 24h 惰性淘汰 | `internal/utils/media.go`、discord/satori/misskey/telegram/line |
| 22 | SPA 外部目录资产回退 embedded | 外部 webuiDir 资产缺失直接 404（isStaticAssetPath 命中），index.html fallback 不变 | `internal/dashboard/server.go` |
| 23 | deerflow proxy 被忽略 | 实现 `Transport.Proxy=ProxyURL`（空值走全局代理），对齐 py trust_env 语义 | `deerflow/api_client.go` |

### 判定无需修（核证后）

- 分片上传/备份任务 GC：已有 `uploadSessionTTL`/`updateProgressTTL`/`gcBackupTasksLocked`（审查误报）。
- `normalizeValue` 丢未知键：py `_normalize_value` 同语义（保持对齐）。
- server.go 外部目录回退：属实已修（见 #22）。

### 新增测试（12 个文件）

`stream_reasoning_test.go`、`agent_runner_thirdparty_test.go`、`shared_runtime_test.go`（+345 行：恢复闭环/串行轮次/Unload 重试/Register 回滚）、`runtime_shutdown_test.go`（迁回判定+Shutdown 幂等）、runner 各包 5 个 `_test.go`、`low_fix_test.go`（dashboard 461 行）、`migrate_test.go`（+129 行）、`temp_cleanup_test.go`、`adapter_draft_cleanup_test.go`、`adapter_group_source_test.go`。

### 过程事故与处理

- 并行子任务期间发生 git stash 冲突残留与调试文件（`zz_dbg_test.go`）遗留、go build 缓存争抢——已清理调试文件、验证无并发进程、修复测试桩与实现的 3 处对接问题（管理器未挂接、`ForPlugin` 继承吞掉失败注入、`raw==nil` 误判退出销毁测试桩——后者同步改为生产语义 `raw != nil && Exited()`）。
- 一个子任务顺带修复了另一子任务中断时留下的 `native_loader_unix.go` 编译损坏（panic→error 隔离，保留其意图）。
