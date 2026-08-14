# Astrbot-golang 全量代码审查报告（bug.md）

> 审查范围：196 个非测试 Go 文件（约 6.9 万行，不含 `tmp/`、`examples/`、`testdata/`）。
> 审查方式：11 个并行子任务逐文件审查，可疑点均追入调用链 / 第三方 SDK 源码确认，部分经可执行复现验证。
> 统计：高危 30 项、中危 44 项、低危 46 项。
> 未发现问题的领域：SQL 注入（database.go 全参数化）、命令注入（git/build 均参数数组传参）、zip-slip（safeJoin 双重校验）、Ed25519 验签实现本身、wecom AES-CBC 加解密的签名顺序与 receiveid 校验、webchat 常量时间比较。

---

# 一、高危（30 项）

## H-01 负数取模导致 panic：收到一条图片消息即可崩溃整个进程

- 位置：`internal/platform/sources/wecom_ai_bot/api.go:166`（触发点 `adapter.go:567-574`）
- 问题：移植 Python 的 `"=" * (-len(key) % 4)` 补位习惯，但 Go 的 `%` 对负数结果取被除数符号：

  ```go
  aesKey, err := base64.StdEncoding.DecodeString(aesKeyBase64 + strings.Repeat("=", (-len(aesKeyBase64))%4))
  ```

  实测 `(-43)%4 == -3`，`strings.Repeat("=", -3)` 直接 panic（已用最小程序验证）。密钥长度非 4 的倍数（典型的 43 位不带 padding 的 EncodingAESKey）必触发。
- 触发链：`adapter.go:569-571` 在消息 `aeskey` 缺失时回退使用 43 位的 `a.encodingAESKey`；该调用发生在 `convertMessage` 里新起的 goroutine 中（adapter.go:567），`handleQueuedMessage` 的 recover 无法捕获子 goroutine panic。
- 影响：**收到一条图片消息即可导致整个进程崩溃**。`api_test.go:144` 只传了 44 位带 padding 的 key，测试未覆盖。
- 修复建议：

  ```go
  pad := (4 - len(aesKeyBase64)%4) % 4
  aesKey, err := base64.StdEncoding.DecodeString(aesKeyBase64 + strings.Repeat("=", pad))
  ```

  并在该并行 goroutine 内加 `defer recover()`。

## H-02 Discord：DM 中使用斜杠命令 `i.Member` 为 nil → panic 击穿进程

- 位置：`internal/platform/sources/discord/adapter.go:479-482`
- 问题：discordgo 的 `Interaction.Member` 仅在服务器（guild）内填写，DM（私信）中为 nil（discordgo 注释明确要求判空）。代码未判空直接解引用：

  ```go
  abm.Sender = platform.MessageMember{
      UserID:   i.Member.User.ID,
  ```

  discordgo 的 handler 在独立 goroutine 中运行且无 recover（已核对 discordgo@v0.25.1 `event.go:166-173`），该 panic 不经过 pipeline 的 recover。
- 影响：用户在私信里使用任意已注册斜杠命令 → 空指针 panic → 整个机器人进程退出。
- 修复建议：

  ```go
  if i.Member != nil && i.Member.User != nil {
      // 用 i.Member.User
  } else if i.User != nil {
      // 用 i.User
  }
  ```

  两处取 ID/用户名均需兼容（同文件其他使用 `i.Member` 的位置一并排查）。

## H-03 Misskey：userCache 并发读写无锁 → 不可恢复的 fatal error

- 位置：写 `internal/platform/sources/misskey/utils.go:492`；读 `adapter.go:390、548、550`；声明 `adapter.go:60`
- 问题：WebSocket 监听 goroutine 的 `convertMessage/convertChatMessage/convertRoomMessage` 调 `CacheUserInfo` 写 `userCache[...] = ...`；同时回复管道 goroutine 的 `Send`/`resolveMessageVisibility` 直接读同一 map。adapter.go:60 声明的 `mu sync.Mutex` 从未用于保护它。
- 影响：Go 运行时对并发 map 读写直接 `throw("concurrent map read and map write")`，进程崩溃且无法 recover。只要 bot 在回复某用户时又收到新消息即可命中。
- 修复建议：用 `a.mu`（或专用 `sync.RWMutex`）保护 userCache 全部读写；顺带做容量上限/过期淘汰（当前无界增长）。

## H-04 qqofficial：C2C 消息缺 author 字段时链式类型断言 panic，无人 recover

- 位置：`internal/platform/sources/qqofficial/adapter.go:487-495`
- 问题：不带 ok 的链式断言：

  ```go
  if senderOpenID == "" {
      senderOpenID, _ = d["author"].(map[string]interface{})["member_openid"].(string)
  }
  ```

  `d["author"]` 缺失时为 nil 接口，`.(map[string]interface{})` 直接 panic。调用链 `runLoop → connectOnce → handleDispatch → handleMessage` 全程无 recover，panic 杀死整个进程。另：487-489 与 493-495 是完全相同的重复代码块。
- 影响：一条格式异常的 C2C_MESSAGE_CREATE（或协议字段变更）导致整个 bot 崩溃退出。
- 修复建议：改带 ok 的两段式断言；删除重复块；`runLoop` 外层加 `defer recover()` 兜底。

## H-05 star/builtin：`/name`、`/provider` 无锁读全局 map → fatal error

- 位置：读 `internal/star/builtin/commands.go:156、314`；写 `commands.go:179-181、335-337`（持 `state.mu`）
- 问题：

  ```go
  cur := state.umoAliases[umo]        // /name 查询路径，未持 state.mu
  if state.selectedLLM[umo] == p.ID { // /provider 列表路径，未持 state.mu
  ```

- 影响：多会话并发执行 `/name`、`/provider` 构成 concurrent map read and map write，Go 运行时抛不可恢复的 fatal error，整个进程崩溃（pipeline 的 recover 无法捕获）。
- 修复建议：两处读取纳入 `state.mu`（或改 `sync.Map`/局部快照）。

## H-06 OpenAI 流式 tool_calls 负数 index → 越界 panic

- 位置：`internal/provider/sources/openai_source.go:252-263`
- 问题：

  ```go
  for len(toolCalls) <= tc.Index { append... }
  toolCalls[tc.Index] = ...
  ```

  服务器/代理返回负数 index 直接越界 panic（流式 goroutine 内 → 整个进程崩溃）；超大 index 分配巨大切片。
- 影响：仅畸形服务端数据触发，但崩溃的是整个 bot 进程。
- 修复建议：`if tc.Index < 0 { continue }`，并给 index 设上限（如 64）。

## H-07 QQ 官方 Webhook：op=13 验证端点构成签名预言机，可伪造任意事件

- 位置：`internal/platform/sources/qqofficial_webhook/adapter.go:250-255、329-342`；`signature.go:39-47、51-71`
- 问题：`handleCallback` 对 `op==13`（URL 验证）请求**在签名校验之前**直接用 secret 派生的 Ed25519 私钥对 `event_ts + plain_token` 签名并返回：

  ```go
  msg := strOf(validationPayload["event_ts"]) + strOf(validationPayload["plain_token"])
  signature := hex.EncodeToString(ed25519.Sign(privateKey, []byte(msg)))
  ```

  而正常回调校验的是 `sign(timestamp + body)`（signature.go:71），两者拼接结构完全相同。攻击者无需认证，向回调地址 POST `{"op":13,"d":{"event_ts":"<T>","plain_token":"<B>"}}` 即可取得对**任意** `T+B` 的有效签名，再用它作为 `X-Signature-Ed25519`/`X-Signature-Timestamp` 头发送伪造的 `op=0` 事件，`verifyQQWebhookSignature` 会通过（Go 的 ed25519 签名恒为规范形，`signatureBuffer[63]&224==0` 检查不构成阻碍）。
- 影响：独立服务器模式默认监听 `0.0.0.0:6196`，公网暴露即可被任意人伪造 C2C/群消息事件（伪造 admin 的 openid 可触发管理员命令）、刷 LLM 调用。此问题同样存在于 Python 原版（移植保真），但必须修复。
- 修复建议：对 op=13 请求本身也要求/校验签名头；或仅在平台"待验证"窗口期内允许 op=13；至少限制 `event_ts` 必须为近期数字时间戳并对验证端点限速。同时给正常回调补时间戳新鲜度校验（见 L-40）。

## H-08 Whisper STT 下载音频时把 API key 发给任意第三方 URL

- 位置：`internal/provider/sources/stt_source.go:130-137`
- 问题：`fetchAudio` 用 `DoWithRetry` 下载用户提供的音频 URL 时无条件附上 provider 的 API key：

  ```go
  req, _ := http.NewRequestWithContext(ctx, http.MethodGet, audioURL, nil)
  req.Header.Set("Authorization", "Bearer "+s.apiKey)
  ```

  `audioURL` 来自聊天平台消息（语音消息 URL），可指向任意主机。对照组 `mimo_common.go:mimoFetchAudio` 就正确地没有带 Authorization。
- 影响：任何能控制语音消息 URL 的人（或该 URL 指向的服务器）都能直接收到 provider 的 API key —— 密钥泄露。
- 修复建议：下载音频时不发送任何凭证头；Authorization 只在请求 `s.apiBase` 时设置。

## H-09 Gemini 系列 provider 把 API key 拼进 URL query，泄漏到日志与错误信息

- 位置：`internal/provider/sources/gemini_source.go:66,78,131,143`；`gemini_embedding_source.go:54,103`；`gemini_tts_source.go:85`
- 问题：

  ```go
  url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", s.apiBase, model, s.apiKey)
  logger.Debug("LLM request: url=%s model=%s messages=%d", url, ...)
  ```

  key 在 URL 中：(1) DEBUG 日志直接打印完整 URL；(2) 实证确认 Go 的 `*url.Error` 会把完整 URL（含 `?key=...`）写进错误串，`client.Do` 的任何网络错误经 `DoWithRetry` `%w` 包装后原样返回，pipeline 的 `logger.Error("LLM 调用失败: %v", err)` 会把它打进日志；(3) key 暴露给 HTTP 代理/中间件。
- 影响：Gemini API key 泄漏到日志文件与代理，可被盗刷。
- 修复建议：改用 `x-goog-api-key` 请求头传 key；日志只打 path 不打 query。

## H-10 pipeline web_fetch 工具存在 SSRF：无内网/元数据地址限制

- 位置：`internal/pipeline/stages.go:2651-2693`（`executeWebFetch`）、`stages.go:2602-2622`（`builtinTools` 无条件注册）
- 问题：仅校验 `http://`/`https://` 前缀后直接：

  ```go
  client := &http.Client{Timeout: 30 * time.Second}
  resp, err := client.Get(rawURL)
  ```

  无私网 IP（127.0.0.1、10.x、172.16/12、192.168.x、169.254.169.254 等）过滤，默认跟随重定向，结果全文回给模型。该工具不受 `web_search` 开关控制，始终注入。
- 影响：prompt 注入（网页内容/用户消息）可诱导模型抓取云元数据端点（`http://169.254.169.254/...`）、本机管理端口、内网服务，并把响应内容泄露到聊天/上下文。
- 修复建议：解析 URL 主机并解析 IP，拒绝环回/私网/链路本地地址；自定义 `CheckRedirect` 对每一跳重校验；限制响应 Content-Type 与大小。

## H-11 astrbot_grep_tool 可读取宿主机任意文件（路径不受工作区限制）

- 位置：`internal/pipeline/computer_tools.go:885-893`（`executeGrep`），对照 `computer_tools.go:233-261`（`resolveLocalPath`）
- 问题：`astrbot_file_read_tool`/`write`/`edit` 都经 `resolveLocalPath` 强制限定在 workspace/skills/plugins/temp 内，但 grep 直接拼接：

  ```go
  resolved := searchPath
  if !filepath.IsAbs(resolved) {
      resolved = filepath.Join(workspaceRoot(umo), searchPath)
  }
  resolved = filepath.Clean(resolved)   // 绝对路径原样通过，无 roots 校验
  ```

- 影响：模型（或注入指令）传 `path="/etc"`、`/root/.ssh` 即可逐行读出宿主机敏感文件（如 `pattern="."`），完全绕过文件工具的越权防护。
- 修复建议：`executeGrep` 复用 `resolveLocalPath(path, umo, false)`；同时注意 `resolveLocalPath` 为纯词法检查，workspace 内符号链接可逃逸（shell 工具可先建链接），建议用 `filepath.EvalSymlinks` 后再判定。

## H-12 微信公众号：SDK 不校验 msg_signature（报文体不在签名内）+ 畸形密文 panic

- 位置：`internal/platform/sources/weixin_official_account/adapter.go:218-224`；SDK `wx@v1.3.3` 的 `mp.go:38-59`、`mp_api/message.go:28-37、133-170`（已读 SDK 源码确认）
- 问题：
  1. `MessageQuery.Validate` 仅计算 `sha1(sort(token,timestamp,nonce))`，解析出的 `MsgSignature` 字段从未使用 —— POST 报文体（明文或密文）不在签名覆盖范围内，且适配器未做任何时间戳新鲜度检查。捕获一次合法 query 三元组即可永久重放并替换任意报文（明文模式下可直接伪造用户消息）。wechatpy（Python 原版所用）在 safe mode 下会校验 `msg_signature`。
  2. `ShouldDecode` 中 `int(raw[len(raw)-1])` 与 `binary.BigEndian.Uint32(raw[16:20])` 均未做边界/取值校验，`raw[20:_length+20]`、`raw[:len(raw)-_pad]` 对畸形密文会 slice 越界 panic（net/http 会 recover，仅断连，但可反复触发）。
  3. SDK 解出 `msg.AppId` 后未与 `account.AppId` 比对（wechatpy 会校验）。
- 影响：报文伪造/重放可注入假消息（公众号场景可冒充用户触发 bot 回复与管理指令）；畸形密文造成连接级 DoS。
- 修复建议：在 `callbackCommand` 中自行校验 `msg_signature = sha1(sort(token, timestamp, nonce, Encrypt))` 与时间戳窗口（如 ±5 分钟）；对 SDK 解密做输入校验或替换为自行实现的 WXBizMsgCrypt；校验解密后的 appId。

## H-13 SkillManager.DeleteSkill 路径穿越 → 任意目录删除

- 位置：`internal/skills/manager.go:381-398`
- 问题：`name` 直接拼接并删除，无任何校验：

  ```go
  skillDir := filepath.Join(sm.skillsRoot, name)
  if _, err := os.Stat(skillDir); err == nil {
      if err := os.RemoveAll(skillDir); err != nil {
  ```

  dashboard 的 `skill_name` 来自 query/body 原样传入（`internal/dashboard/handlers.go:6026-6046` → `server.go:1753`），全程无 `skillNameRe` 校验。同文件其他入口（`skillFilePath`，handlers.go:6135-6139）明确做了 `..` 防护，唯独删除路径漏掉。
- 影响：认证后的 dashboard 用户传 `../../someDir` 可删除 data/skills 之外的任意目录（受进程权限限制）。
- 修复建议：DeleteSkill/SetSkillActive 入口先用 `skillNameRe` + safeJoin 语义校验 name，拒绝含 `/`、`\`、`..` 的值。

## H-14 插件卸载旧版 manifest 条目会删除整个 dataDir（数据毁灭性 bug）

- 位置：`internal/plugin/runtime_admin.go:343-351、354-366、375-385`
- 问题：`Uninstall` 用 `safeDataDirPath(entry.ConfigDir)` 覆盖按 name 推导的默认路径，而 `safeDataDirPath` 对空字符串返回**整个 dataDir**：

  ```go
  func (m *SubprocessManager) safeDataDirPath(sub string) (string, error) {
      if sub == "" { return m.dataDir, nil }
  ```

  `ManifestEntry.ConfigDir/DataDir/DocsDir` 均为 `omitempty`（manifest.go:39-41），旧版本安装的条目这三个字段为空 → `cfgDir/dataRoot/docsDir` 全部变成 `m.dataDir`，随后 `deleteData=true` 时 `os.RemoveAll(dataRoot)` / `deleteConfig=true` 时 `os.RemoveAll(cfgDir)`。
- 影响：对旧条目插件执行"卸载并删除数据/配置"会把整个数据目录（所有插件、全部配置、manifest 本身）一并删除。dashboard `POST /plugins/by-id` 甚至默认传 `(true, true)`（handlers.go:1877）。
- 修复建议：`safeDataDirPath("")` 应返回错误（或保持调用方已推导的默认路径）；空字段时回退到 339-341 行的 name 推导值，绝不返回 `m.dataDir`。

## H-15 统一 Webhook 回调被全局鉴权拦截，webhook 模式平台全部 401

- 位置：`internal/dashboard/router.go:183-219`（`apiAuthAllowed` 白名单无 `webhooks`）；`internal/dashboard/server.go:125-142`（`handleWebhooks`）；注册入口 `internal/lifecycle/lifecycle.go:838-840`
- 问题：所有 `/api/` 请求经过 `apiAuthAllowed` 全局认证门，白名单只有 `auth`、`unified-chat`、`live-chat`：

  ```go
  case "unified-chat", "live-chat":
      return true
  }
  return s.auth.IsAuthenticated(extractToken(r))
  ```

  而 `RegisterWebhook` 注册的平台回调入口是 `/api/v1/webhooks/platforms/{uuid}`（LINE/Slack/Lark/QQ 官方 webhook/企业微信/微信公众号等统一入口），外部平台服务器回调不携带 dashboard token → 一律 401。`webhook_test.go` 直接调用 `s.handleWebhooks(...)` 绕过了鉴权层，因此测试未暴露。
- 影响：所有依赖统一 webhook 入口的平台适配器收不到任何消息，功能整体不可用（3 个子任务独立确认）。
- 修复建议：在 `apiAuthAllowed` 中为 `parts[0] == "webhooks"` 放行（uuid 本身不可猜测 + 各平台回调内部已有签名校验）；注意放行后 `GET /api/v1/webhooks`（server.go:127-130）会向未认证方枚举 uuid 列表，建议一并收紧（只放行带 uuid 后缀的具体路径）；补经过 `apiHandler` 的集成测试。

## H-16 钉钉 access_token 解析只查嵌套 data 字段，REST 全链路失效

- 位置：`internal/platform/sources/dingtalk/adapter.go:456-457`
- 问题：`POST /v1.0/oauth2/accessToken` 的响应是**扁平**结构 `{"accessToken":"...","expireIn":7200}`（已对照钉钉官方 dingtalk-stream-sdk-python 的 `result['accessToken']` 确认）。此处只从嵌套结构取值：

  ```go
  inner, _ := data["data"].(map[string]interface{})
  token := getString(inner, "accessToken")
  ```

  （同文件 `downloadDingFile` 对 downloadUrl 却做了扁平+嵌套双兼容，佐证 v1.0 响应应为扁平。）
- 影响：`getAccessToken` 恒返回空 → 图片/语音/文件下载、媒体上传、群聊/私聊消息发送全部报 "access_token 为空"，适配器收得到消息但永远回不了。
- 修复建议：优先解析顶层 `data["accessToken"]`/`data["expireIn"]`，嵌套结构作兜底；补充基于 httptest 的单测。

## H-17 Lark：群聊消息 GroupID 写死空串，群聊会话完全不可用

- 位置：`internal/platform/sources/lark/adapter.go:245、335-339`
- 问题：`convertMsg` 中 `abm.Group = &platform.Group{GroupID: ""}` 写死为空，且整个 lark 包从未读取 `msg.ChatId`（grep 确认）。群消息时：

  ```go
  sessionID := senderOpenID
  if abm.Type == platform.GroupMessage {
      sessionID = abm.GroupID()   // 永远返回 ""
  }
  ```

- 影响：群聊消息 `SessionID`/`ConvID` 为空串，回复经 `Send(event.Source.ConvID, ...)` 传入空会话 → 发送必失败。Lark 群聊功能整体瘫痪（socket 与 webhook 模式均受影响）。
- 修复建议：解析 `msg.ChatId`（非 p2p 时）填入 `abm.Group.GroupID` 与 `SessionID`。

## H-18 Slack 统一 webhook 模式下 Start 永久阻塞，整个进程启动挂死

- 位置：`internal/platform/sources/slack/adapter.go:273-277`
- 问题：统一 webhook 模式下 `startWebhookMode` 直接阻塞等待 ctx 结束：

  ```go
  if a.unifiedWebhookMode && a.webhookUUID != "" {
      logger.I18nInfo(...)
      <-ctx.Done()
      return nil
  }
  ```

  而 `lifecycle.go:841` 在 `loadPlatforms` 中**同步串行**调用 `adapter.Start(ctx)`，且 `loadPlatforms`（lifecycle.go:284）在 dashboard、event bus 启动之前执行。
- 影响：一旦配置了 Slack webhook+统一模式，`loadPlatforms` 永远不返回：后续所有平台无法加载、dashboard 无法启动、事件总线无法启动，整个进程假死。`ReloadPlatforms`（WebUI 触发）同样挂死。
- 修复建议：统一模式下直接 `return nil`（与 `qqofficial_webhook/adapter.go:168-175` 一致），不要阻塞。

## H-19 LINE：本地媒体文件服务绑定 127.0.0.1 且用 http，媒体发送必然失败

- 位置：`internal/platform/sources/line/message.go:337-362、389-403`
- 问题：发送图片/音频/视频/文件时把本地文件注册到内置服务，返回 `http://127.0.0.1:<port>/api/file/{token}` 作为 `originalContentUrl`。LINE Messaging API 要求该 URL 为公网可访问的 HTTPS 地址。
- 影响：LINE 服务器无法访问 localhost，任何含媒体的消息发送失败；文本消息不受影响。Python 原版用 `callback_api_base` 公网地址，移植时丢失了该语义。
- 修复建议：使用配置的 `callback_api_base`（dashboard 已有该配置）拼接公网 HTTPS URL；至少提供可配置的对外 base URL。

## H-20 aiocqhttp：反向 WS 读循环内同步 CallAction 等待自身读取的 echo → 自死锁

- 位置：`internal/platform/sources/aiocqhttp/adapter.go:333-335`（同步调用）；`adapter.go:416-421`（CallAction 等 echo）；`quoted_parser.go:212-231、237-256`（读循环内触发 get_forward_msg/get_msg）
- 问题：反向 WS 模式下，事件与 API 响应走同一条连接。读循环收到事件后同步进入 `handleEvent → enrichForwardAndQuoted → CallAction`，而 CallAction 等待的 echo 帧只能由这同一个被阻塞的读循环读取：

  ```go
  if _, hasPost := msg["post_type"]; hasPost {
      a.handleEvent(msg)   // 阻塞在读循环里等 echo
      continue
  }
  ```

- 影响：任何含远端转发 id 的消息必然令 `get_forward_msg` 超时（10s/个 id，BFS 最多 32 个 → 读循环最长阻塞 320s），合并转发内容永远取不到，期间心跳/事件全部停摆，可能被对端断连。
- 修复建议：`handleEvent` 投递到独立 goroutine（HTTP 路径已是 `go a.handleEvent`），保证读循环只负责收帧分发。

## H-21 aiocqhttp：缺少 `"reply"` 段分支，QQ 引用消息被静默丢弃

- 位置：`internal/platform/sources/aiocqhttp/quoted_parser.go:75-146`（switch 无 `case "reply"`）
- 问题：OneBot v11 引用消息以 `{"type":"reply","data":{"id":...}}` 段下发，解析 switch 中没有该分支。Python 原版会调用 `get_msg` 构造完整 `Reply` 组件；Go 侧 `enrichForwardAndQuoted`（adapter.go:688-712）依赖链中存在 `*message.Reply` 才触发 `fetchQuotedContent`，因此整个 quoted-parser 回复路径是死代码。
- 影响：引用消息上下文完全丢失；LLM 无法看到被引内容。
- 修复建议：增加 `case "reply"`：先落一个 `&message.Reply{MessageID: id}` 组件，再由 `enrichForwardAndQuoted` 补全（同时按 Python 语义支持一层嵌套 get_msg）。

## H-22 aiocqhttp：多 goroutine 并发 WriteMessage 同一 WS 连接，无串行化

- 位置：`internal/platform/sources/aiocqhttp/adapter.go:236-242`（sendAction）、`409-415`（CallAction）
- 问题：gorilla/websocket 明确只允许一个并发写者。写入方可来自：pipeline 应答（EventBus 调度 goroutine）、WS 读循环的 `CallAction`、`go a.handleEvent`（HTTP POST）、插件 host_service 的 gRPC handler goroutine。任意两路同时 `c.WriteMessage(...)` 即为数据竞争。
- 影响：WS 帧交错损坏 → 消息丢失/协议错误/对端断连，难以复现。
- 修复建议：为每条连接加写锁（或每连接一个发送队列 goroutine）；`SetWriteDeadline` 同样需在锁内。

## H-23 cron：不可解析的表达式导致任务每 10 秒永久重复触发

- 位置：`internal/cron/manager.go:318-334`（`computeNextRunLocked`）、`manager.go:39-41`（`IsDue`）；触发入口 `internal/pipeline/cron_tools.go:84-111`（无表达式校验）
- 问题：解析失败时静默保持 `NextRun` 不变，新建任务的 NextRun 为零值（公元 1 年）：

  ```go
  if t, err := m.nextRunFn(job); err == nil { job.NextRun = t }   // err != nil 时零值被保留
  ```

  而 `IsDue` = `!j.NextRun.After(now)` 对零值恒为 true → 每 10s tick 都判定到期、触发、再次计算失败、再次到期……死循环。`ParseCron` 只接受 5 字段，LLM 很容易给出 Python croniter 兼容的 6 字段（带秒）表达式直接中招。
- 影响：任务每 ~10s 触发一次完整 LLM 管线并向真实聊天会话发消息，token 消耗和消息轰炸持续到用户手动删除任务。
- 修复建议：`computeNextRunLocked` 解析失败时置 `NextRun` 为远期时间并禁用任务（记错误日志）；`AddActiveJob`/edit 路径先 `ParseCron` 校验；考虑兼容 6 字段表达式。

## H-24 dashboard：任意一次配置保存会静默清除 TOTP 双因素

- 位置：`internal/dashboard/auth.go:841-867`（`saveTOTPToConfig`）；`internal/dashboard/server.go:1449-1465`（`injectAuthFields`）
- 问题：`EnableTOTP`/`DisableTOTP` 通过 `saveTOTPToConfig` 直接读写 `cmd_config.json`，而 `ConfigManager` 只在启动时 `Load()`（lifecycle.go:110-113），内存快照永远看不到 `dashboard.totp` 段。之后任何一次 `cfg.Set/Update + Save()` 都会用内存数据覆盖整个文件；保存 dashboard 键时 `injectAuthFields` 只回填 `username/pbkdf2_password/password/jwt_secret`，**没有保留 `totp` 字段**，且 `cfg.Set("dashboard", value)` 是整体替换而非合并。
- 影响：用户启用 TOTP 后，下一次保存任何配置就会丢掉 `totp` 段；重启后 `NewPasswordManager` 读不到该段，2FA 静默失效 —— 用户以为开着 2FA，实际没有，属于安全回归。
- 修复建议：`injectAuthFields` 中从 `PasswordManager` 一并回填 totp 段（或提供 `TOTPConfig()`）；更彻底的做法是让 auth 的持久化走 ConfigManager（或保存后调用其 reload），消除双写（见 M-08）。

## H-25 所有流式 chat：SSE 错误被吞 + Client.Timeout=120s 拦腰截断长流且当作正常完成

- 位置：`openai_source.go:37-39,267`；`anthropic_source.go:31-33,187`；`gemini_source.go:31-33,196`；`openai_responses_source.go:80,284`；`kimi_code_source.go:40,311`；`ollama_source.go:132-136`；`sse.go:27-49`
- 问题：
  1. 每个流式 goroutine 都 `_ = reader.scan()`，SSE 读取错误（连接中断、超时）全部丢弃，之后照常发送 final chunk。
  2. `http.Client{Timeout: 120 * time.Second}` 的超时**覆盖读响应体**。流式回答超过 120s（推理/思考模型很常见）时 body 读取报 `Client.Timeout exceeded`，被 (1) 吞掉，消费方拿到的是被截断的"完整"回答。
  3. Anthropic 流完全没处理 SSE `error` 事件；`ollama_source.go:136` 在 decode 出错时直接 `return`，连 final chunk 都不发（usage 丢失）。
- 影响：长回答静默截断、错误不可见，pipeline 把半截回答当正常回复写进会话历史，用户与统计都感知不到失败。
- 修复建议：保留并上抛 `scan()` 的错误（在 final chunk 上带 err 标记或发 err chunk）；流式请求改用无整体 Timeout 的 client + 基于 ctx 的空闲超时（如每 chunk 间隔超时）；Anthropic 补 error 事件处理。

## H-26 MCP：Connect 失败后 stdio 子进程泄漏，客户端进入"假活"状态

- 位置：`internal/agent/mcp_client.go:105-124`；调用方 `internal/pipeline/stages.go:2464-2470`
- 问题：`Connect` 中 `c.cl = cl` 赋值后，若 `Start` 成功但 `Initialize`/`listTools` 失败，直接返回错误**而不调用 `cl.Close()`**。stdio transport 的子进程只有 `Close()` 才会 kill/wait（mcp-go v0.57.0 `transport/stdio.go`）。调用方对失败的服务器直接丢弃 client 对象。失败后 `c.cl != nil && c.active == true`，`IsActive()`/`SSEAlive()`（mcp_client.go:227-239）返回 true；`Reconnect`（255-266）失败时同样残留半连接状态。
- 影响：每次 `loadMCPTools`（配置重载/重试）对初始化失败的 stdio 服务器（npx/node 进程）泄漏一个孤儿进程，stdin 管道未关可能永久挂起；假活状态误导后续判断。
- 修复建议：`Connect` 在任一步失败后、返回前执行 `cl.Close()`（并置 `c.cl = nil`、`active = false`）；或调用方失败时调用 `client.Cleanup()`。

## H-27 t2i：全局缓存的 font.Face 并发 data race

- 位置：`internal/t2i/image.go:144-157`（`cachedFontFace`）
- 问题：`fontFaceCache sync.Map` 按 `"path|size"` 缓存 `font.Face` 跨 goroutine 共享，但底层是 freetype 的 `*truetype.face`，其 `Glyph()`/`GlyphAdvance()` 会写实例字段 `glyphBuf`（freetype/truetype/face.go:230），非并发安全。并发路径已确认可达：`dashboard/chat_stream.go:310-316` 在 bus 不可用时于独立 goroutine 中运行 `scheduler.Process`，与 event bus 的串行 pipeline 并发执行 `applyT2I` → `RenderTextToPNG`（stages.go:3483）→ 共享同一 face。
- 影响：多条消息同时触发文转图时产生 data race（`-race` 可检出），可导致字形数据损坏、渲染错乱甚至崩溃。
- 修复建议：缓存 `*truetype.Font`（本身并发安全），每次渲染用 `truetype.NewFace` 创建局部 face；或对整个渲染路径加全局互斥。

## H-28 sandbox：DockerBooter 复用已停止的容器导致沙箱永久失效

- 位置：`internal/sandbox/manager.go:292-305`
- 问题：复用逻辑只检查 `docker inspect` 命令是否出错，完全忽略其输出。对已停止的容器 `inspect -f {{.State.Running}}` 退出码为 0、输出 "false"，停止的容器照样被复用：

  ```go
  if _, err := dockerOutput(ctx, "inspect", "-f", "{{.State.Running}}", line); err == nil {
      b.containerID = line
      b.running = true   // 容器实际已停止
  ```

  容器启动命令无 `--restart` 策略（manager.go:307-309），宿主机或 docker daemon 重启后容器处于 Exited 状态，恰好落入此分支。
- 影响：重启后沙箱"启动成功"但所有 `docker exec` 失败，computer-use 功能永久失效，需手动 `docker rm`。
- 修复建议：检查 inspect 输出是否为 `"true"`；或复用前先 `docker start`，失败再重建。

## H-29 wecom_ai_bot：长连接读循环内同步 SendCommand 自阻塞 + 重复发送

- 位置：`internal/platform/sources/wecom_ai_bot/long_connection.go:235-237、254-283、288-312`；`adapter.go:434-443、459-466`
- 问题：读循环同步调用 `c.messageHandler(payload)`，而 handler（`processLongConnectionPayload`）在配置了 `initialRespondText` 或 enter_chat 欢迎语时会调用 `SendCommand → sendAndWaitResponse`，等待以 req_id 为键的 waiter —— 该 waiter 只能由当前被阻塞的读循环喂入。`SendCommand` 持有 `commandLock` 重试 4 次（每次重新 `sendJSON`），读循环被卡约 40s+，期间心跳的 `SendCommand("ping")` 也阻塞在同一把锁上。
- 影响：开启上述任一配置后，每条消息回调读循环停摆 40s+（消息积压、心跳饿死、可能被服务端断连）；超时重试会把 `aibot_respond_msg` 重复发送最多 4 次。Python asyncio 中 handler await 时事件循环仍可读帧，属移植引入的缺陷。
- 修复建议：读循环内将 `messageHandler(payload)` 投递到独立 goroutine 执行；或为回调类消息改用"只发不等 ack"的路径。

## H-30 事件总线单 goroutine 串行分发，慢事件造成全局队头阻塞

- 位置：`internal/core/event_bus.go:250-271、356-373`
- 问题：`Start` 的调度循环对每个事件同步调用 `bus.dispatch(ctx, event)` → `scheduler.Process(ctx, event)`，全程在一个 goroutine 里执行 9 个 stage。LLM 调用（默认 120s）、百度审核同步 HTTP、插件 RPC 全部串在这一次调用里（代码注释自己承认 "a slow pipeline (up to minutes)"）。
- 影响：任何一个会话的一次慢 LLM 请求会阻塞所有平台、所有会话的消息处理（含 `PublishDelayed` 的限流重入事件），延迟无上限累积。Python 原版为 asyncio 并发处理，此处为移植引入的吞吐退化。
- 修复建议：每事件一个 goroutine（`go dispatch`，可按 UMO 哈希到固定 worker 保持同会话有序、跨会话并发）；或至少给 LLM 调用加独立超时并放行后续事件。

---

# 二、中危（44 项）

## M-01 dashboard：多个 handler 对 nil map 写入导致 panic（空/非法 JSON body 即触发）

- 位置：
  - `internal/dashboard/handlers.go:1219-1223`（`handleProviders` list POST）
  - `handlers.go:3985-3987、4011-4016`（`handlePersonas` POST / by-id PUT）
  - `handlers.go:4100-4102、4126-4128`（`handlePersonaFolders` PUT/POST）
  - 实际 panic 点：`personas_store.go:116、193`
- 问题：典型模式：

  ```go
  var body map[string]interface{}
  _ = json.NewDecoder(r.Body).Decode(&body)
  if body["config"] == nil {
      body["config"] = map[string]interface{}{}   // body 为 nil 时：assignment to entry in nil map → panic
  }
  ```

  JSON 解码失败、空 body、或 body 为 `null` 时 `body` 保持 nil，后续 `body["k"] = v` 或 store 内部 `p["persona_id"] = id` 直接 panic。其余 handler 都做了 nil 检查，这几处漏了。
- 影响：认证用户发送空/畸形 body 即可触发 panic（net/http recover 为 500/连接重置），低成本 DoS 面并污染日志。
- 修复建议：decode 后统一 `if body == nil { body = map[string]interface{}{} }`，或在 store 层入口判 nil。

## M-02 dashboard：PUT /conversations/{id}/messages 静默失效但返回成功

- 位置：`internal/dashboard/handlers.go:6535-6540` → `internal/dashboard/server.go:1567-1579`
- 问题：handler 用 `[]map[string]interface{}` 接收 history 后包进 map 传入：

  ```go
  if s.conversationUpdateByCID(convID, map[string]interface{}{"history": body.History}) {
  ```

  而 `conversationUpdateByCID` 内断言 `patch["history"].([]interface{})` —— `body.History` 的动态类型是 `[]map[string]interface{}`，断言必然失败，`ReplaceHistoryByCID` 永远不会被调用，函数尾部返回 true。
- 影响：替换会话历史的 API 完全无效，却回报 "conversation ... messages updated"。
- 修复建议：handler 侧先把 `[]map[string]interface{}` 转成 `[]interface{}`，或 `conversationUpdateByCID` 增加对 `[]map[string]interface{}` 的分支。

## M-03 dashboard：知识库 URL 导入 / MCP 连通测试存在 SSRF（未复用已有出站校验）

- 位置：`internal/dashboard/handlers.go:2762-2781`（import-url）、`handlers.go:4589-4596`（testMCPServer）
- 问题：import-url 对用户提供的 URL 直接发起服务端请求（`client.Do(req)`）。项目已有 `validateOutboundURL`（market.go:26，拒绝私网/链路本地/云元数据地址）且被市场/模型列表拉取使用，但此处未调用。下载内容存入 KB 文档目录，可通过 `GET /knowledge-bases/{kb}/documents/{doc}` 的 `content` 字段读回。
- 影响：认证后的攻击者（或被劫持的 WebUI 会话）可探测/读取内网服务与云元数据端点（如 `http://169.254.169.254/...`）并回传内容，完整 SSRF 数据外泄链。
- 修复建议：import-url 与 testMCPServer 出站前统一调用 `validateOutboundURL`，并对重定向后的目标复核。同类遗漏：`lark_registration.go:34-56`、`qqofficial_registration.go:45-57`、`weixin_oc_registration.go:70-90` 的自定义域名/主机也无出站校验（已认证用户可借服务器探测内网）。

## M-04 dashboard：配置读-改-写非原子，并发保存丢失更新

- 位置：`handlers.go:1458-1478`（upsertProvider）、`1496-1511`（setProviderEnabled）、`1768-1788`（upsertBot）、`5438-5491`（updateCommand）、`4311-4317`（updateTool permission）、`1515-1581`（deleteProviderByID + cleanProviderSettingsRefs）、`provider_sources.go:64-107`（deleteProviderSource 级联）
- 问题：大量写路径遵循 `cfg := s.getConfigSnapshot()`（深拷贝）→ 修改 → `setConfigData(key, …)` 的模式，`setConfigData`（server.go:1383-1414）只对单次 `cfg.Set+Save` 加锁，不覆盖"快照→写回"整个区间。`deleteProviderByID` 先保存 `provider` 再另取快照保存 `provider_settings`；`deleteProviderSource` 级联删除用的还是第一次快照里的 `cfg["provider"]`。
- 影响：两个并发管理请求交错时后写者用过期快照覆盖前者，静默丢失配置修改；级联删除可能"复活"刚被并发请求删除的 provider。
- 修复建议：Server 层引入针对配置写路径的全局互斥（或提供 `mutateConfig(func(cfg))` 原子入口），把"取快照-修改-写回"整体纳入临界区。

## M-05 dashboard：multipart 上传无总量限制、zip 解压无解压后大小限制（zip 炸弹）

- 位置：`handlers.go:2878、2918-2929`（KB 文档上传 io.Copy 无上限）、`6289-6315 + 6372`（skills batch：`ParseMultipartForm(64<<20)` 后 `io.Copy(out, rc)` 无上限）、`server.go:231-240`（未配合 `http.MaxBytesReader`）
- 问题：`ParseMultipartForm(64<<20)` 只是内存阈值，超过部分落盘临时文件，请求总大小无上限；KB 文档与技能包解压的 `io.Copy` 均无字节数上限。对比：import-url 有 64MB LimitReader、market 有 4MB 上限，说明项目有此意识但上传路径漏了。
- 影响：认证用户可通过超大上传或高压缩比 zip（64MB 压缩可膨胀数百 GB）耗尽磁盘/inode。
- 修复建议：请求统一套 `http.MaxBytesReader`（如 256MB）；解压循环用 `io.LimitReader`/计数器限制单文件与总解压字节数。

## M-06 dashboard：mcpStore/personaStore 返回内部 map 引用，并发读写 fatal 风险

- 位置：`mcp_store.go:119-127`（get 直接 `return cfg`）、`107-116`（setEnabled 锁内 `cfg["active"] = enabled`）；`personas_store.go:151-174`（listFolders 直接 append）、`176-185`（getFolder 返回内部引用）、`248-270`（reorder 锁内写 `f["sort_order"]`）
- 问题：`get/listFolders/getFolder` 把存储中的 map 引用直接交给 HTTP handler，序列化发生在锁外（如 handlers.go:4490 `writeJSON(..., s.mcp.get(serverName))`），而 `setEnabled`、`reorder` 会在锁内修改同一个 map。`personas_store.go` 对 personas 特意做了 `copyPersonaMap`（注释写明原因），folders/MCP 却漏了。
- 影响：并发管理时 map 并发读写可触发 runtime fatal（concurrent map read and map write），或返回撕裂数据。
- 修复建议：`mcpStore.get/list`、`personaStore.listFolders/getFolder` 返回深拷贝（对齐 `copyPersonaMap` 的做法）。

## M-07 dashboard：TOTP "重新生成恢复码"会重置密钥，用户验证器立即失效

- 位置：`internal/dashboard/auth.go:743-764`（`GenerateTOTP`）；`server.go:530-545`（recovery 分支）
- 问题：

  ```go
  _, _, codes, err := s.auth.GenerateTOTP()   // 重新生成 totpSecret + enabled=false
  s.auth.EnableTOTPNoop()                      // 只恢复 enabled，secret 已被换掉
  ```

  recovery 端点为拿新恢复码复用了 `GenerateTOTP`，把 `totpSecret` 换成全新值；`EnableTOTPNoop` 只把 enabled 置回 true。用户手机验证器里存的还是旧 secret，下次登录 TOTP 必失败。setup 第一步（server.go:510-522）每次调用也会覆盖现有 secret、把已启用的 TOTP 降级为未启用、旧恢复码全部作废。
- 影响：已启用 TOTP 的用户调用"重新生成恢复码"或误触 setup 后，验证器与服务器失配，实际后果是把 2FA 打掉。
- 修复建议：恢复码重生成只重算 `totpRecoveryCodes`（保留原 secret）；setup 第一步在 `TOTPEnabled()` 时拒绝或要求显式确认重置。

## M-08 dashboard：auth 直写配置文件与 ConfigManager 保存之间无互斥，丢失更新

- 位置：`auth.go:153-181`（saveToConfig）、`184-211`（ensureJWTSecret）、`841-867`（saveTOTPToConfig） vs `internal/config/config.go:292`（save）
- 问题：三处 auth 持久化都是"读整个文件 → 改 dashboard 段 → 原子写回"，使用 `PasswordManager.mu`（或无锁），而 `ConfigManager.Save` 用自己的 `c.mu`，互不感知。并发时后完成者以自己读到的旧文件为基线覆盖对方刚写入的内容。
- 影响：密码哈希/用户名/TOTP 段可能被回滚到旧值（H-24 的必然性正是这个双写架构的产物）；`jwt_secret` 若被回滚，所有已发 token 失效。
- 修复建议：统一配置写入通道（auth 落盘也走 ConfigManager，或共享一把文件写锁）。

## M-09 dashboard：market 拉取失败且无缓存时返回 (nil, nil)，前端误报成功空市场

- 位置：`internal/dashboard/market.go:94-116`；调用方 `handlers.go:1922-1931`
- 问题：

  ```go
  if entry, ok := s.marketCache[url]; ok { return entry.data, nil }
  return nil, err   // Get 成功但 status != 200 / decode 失败时 err == nil
  ```

  调用方判 `err != nil` 才报错，于是走 `apiOK(nil)` → `data: {}`。另外全函数（含 10s 网络请求）持有 `marketMu`，并发市场请求被串行阻塞。
- 影响：市场源挂掉时 WebUI 静默显示空插件市场，无任何错误提示。
- 修复建议：各失败分支显式构造错误返回；缓存查询移到锁内、网络 IO 移到锁外。

## M-10 pipeline：doom_loop 拒绝路径从不清理 pausedTool，发起者消息被永久拦截

- 位置：`internal/pipeline/doom_loop.go:74-77`（declined 分支）、`96-99`；`stages.go:1013-1016`
- 问题：`maybeHandleDoomConfirm` 只有 confirm 分支清空 `pausedTool`；decline 分支回复后直接返回，暂停状态永久残留。此后该会话中该发送者的**每一条**消息都会先进入此拦截逻辑，非确认消息被 `event.Stop()` 吞掉。且确认关键词包含单字 `"是"` 和 `"yes"`（doom_loop.go:58-60），任何恰好含"是"的消息（如"是的"、"是不是"）都会意外触发旧请求重放。
- 影响：触发过一次死循环暂停后，发起者在群里发的所有正常消息被静默丢弃。
- 修复建议：decline 分支清空 `pausedTool`/`askSender` 并放行消息走正常流程；确认词改为整句精确匹配（如 `== "继续"`）。

## M-11 pipeline：doom 恢复流程在群聊不可达

- 位置：`internal/pipeline/doom_loop.go:61-73`（resume 只改写文本）；`stages.go:1113-1123`
- 问题：resume 分支仅设置 `event.PlainText/MessageStr`，不设置 `IsAtOrWakeCommand`/`llm_wake`。群聊里裸文本"继续"不满足唤醒条件，随后 `event.Stop()` —— 恢复的请求从未到达 LLM，而机器人已回复"已解除…正在继续执行"。
- 影响：群聊中死循环恢复功能静默失效，且有误导性输出。
- 修复建议：resume 时同步 `event.IsAtOrWakeCommand = true; event.SetExtra("llm_wake", true)`（或 `event.CallLLM = true`）。

## M-12 pipeline：tool_call_timeout 配置完全无效（executeToolWithTimeout 是死代码）

- 位置：`internal/pipeline/provider_options.go:343-357`；`stages.go:1610`
- 问题：全仓库无任何调用点。工具循环直接 `result := s.executeTool(event, ...)`，且 `executeToolWithTimeout` 本身也未把 ctx 传给 executeTool（超时后 goroutine 继续跑）。
- 影响：`provider_settings.tool_call_timeout` 静默失效；一个挂死的工具（见 M-13）无超时兜底。配合总线串行（H-30），一次挂起即冻结全部消息处理。
- 修复建议：工具循环改用带超时包装，并把 context 传入 `executeTool` 签名以真正取消底层命令/请求。

## M-13 pipeline：executeSandboxTool 默认无超时，sandbox 命令可无限挂起

- 位置：`internal/pipeline/stages.go:2825-2831`
- 问题：

  ```go
  ctx := context.Background()
  timeout := argInt(args, "timeout", 0)
  if timeout > 0 { ... context.WithTimeout ... }
  ```

  模型不传 timeout 时对 `mgr.Exec`（docker exec 等待）无任何期限。对比本地路径 `executeLocalShell` 默认 300s、python 默认 30s。
- 影响：`sleep infinity` 之类命令会让该事件处理永久阻塞；因总线串行（H-30），整个 bot 停止响应（等同 DoS）。
- 修复建议：设置默认超时（如 300s），与本地 runtime 对齐。

## M-14 pipeline：非流式路径不解析 XML 工具调用，仅静默剥离

- 位置：`stages.go:1703-1711`（非流式仅 `stripToolCallXML`）对照 `stages.go:1765-1775`（流式才有 `parseXMLToolCalls`）
- 问题：同一模型的 `<function_calls>` 输出，`streaming_response=true` 时会转成真实工具调用执行，`false` 时被直接删除。
- 影响：关闭流式后 Anthropic 风格 XML 工具调用功能整体失效且无提示，行为随流式开关不一致。
- 修复建议：非流式分支同样调用 `parseXMLToolCalls` 并把结果合入 `resp.ToolsCall*`。

## M-15 pipeline：skills_like 模式原地改写 MCP schema 缓存，永久破坏参数定义

- 位置：`stages.go:2321-2333`（`collectLightTools` 原地 `fn["parameters"] = 空`）；`2416-2424`（`mcpSchemasSnapshot` 浅拷贝，内层 map 共享）；`2338-2357`（`collectParamToolsFor` 再次读取同一缓存）
- 问题：snapshot 返回的是 `s.mcpSchemas` 缓存中的同一 inner map；`collectLightTools` 将其 `parameters` 替换为空对象后，缓存被永久污染。后续 `requeryToolArgs → collectParamToolsFor` 拿到的也是空参数 schema。
- 影响：skills_like + MCP 组合下，参数补全重查询永远拿不到参数定义，工具参数生成必然劣化/失败。
- 修复建议：`collectLightTools` 对 schema 做深拷贝后再改写，或缓存只读、按模式派生副本。

## M-16 pipeline：Brave 搜索 GET 请求把查询参数放进 JSON body，从未真正发送

- 位置：`internal/pipeline/websearch.go:411-421`（params 作为 payload）；`websearch.go:17-46`（`httpJSON` 仅序列化为 body，从不构造 query string）
- 问题：`httpJSON(http.MethodGet, "https://api.search.brave.com/...", headers, params)` 将 `q/count/country` 等放进 GET body。Brave Web Search API 只接受 query 参数。
- 影响：`web_search_brave` 永远返回 422/400（缺 `q`），功能完全不可用。
- 修复建议：为 GET 构建 `url.Values` 拼接到 URL，payload 仅用于 POST。

## M-17 pipeline：future_task edit 在无锁状态下修改正在调度的 Job（fatal 风险）

- 位置：`internal/pipeline/cron_tools.go:147-173`；对照 `internal/cron/manager.go:192-196`（Get 返回活跃指针）、`manager.go:268-313`（tick 在锁内读写同字段）、`manager.go:442-455`（SerializeJob/List 读 Payload）
- 问题：`job := mgr.Get(jobID)` 拿到的是调度器正使用的 `*Job`，随后在管理器锁外执行 `job.Payload["note"] = n`（map 并发写）、`job.CronExpression = e; job.RunOnce = false`，与 cron tick goroutine（每 10s）和其他请求的 `List()` 并发访问。
- 影响：Go map 并发读写可直接触发 fatal 使整个进程崩溃；也可能导致 `computeNextRunLocked` 读到半更新状态。
- 修复建议：提供并使用 `UpdateJob(id, mutations)`（管理器内加锁拷贝/替换）；至少在锁内完成字段修改。

## M-18 pipeline：shell session write/write_line 恒失败 + 操作不校验会话归属

- 位置：`internal/pipeline/computer_tools.go:620-638`（StdinPipe 立即关闭且 `Stdin: nil`）、`727-741`（`shellSessionWrite` 判 `s.Stdin == nil`）、`695-763`（仅 `list` 按 Owner 过滤，poll/write/interrupt/terminate 只查 ID）
- 问题：后台会话注册时 `Stdin: nil` 且 `_ = stdin.Close()`，因此 write/write_line 永远返回 "does not accept input" —— 而系统提示词（computer_tools.go:302-314）明确宣传此功能。另外除 list 外的操作不检查 `s.Owner == umo`。
- 影响：模型按提示使用 write_line 必然失败，浪费循环步骤；任一会话得知（或猜中）16 位 hex session id 后可跨会话注入输入/终止他人进程。
- 修复建议：保留 stdin 管道并挂到 `shellSession.Stdin`；所有操作统一校验 Owner。

## M-19 pipeline：computer_use_runtime=none 时本地宿主机工具仍可被执行

- 位置：`stages.go:2757-2774`（最终 switch 无 runtime 门控），对照 `stages.go:2243-2247`（仅 collectTools 注入门控）
- 问题：`collectTools` 仅在 `runtime=="local"` 时注入 shell/python/file 工具，但 `executeTool` 的兜底 switch 对 `astrbot_execute_shell` 等名字不看 runtime 直接执行在宿主机。sandbox 分支有门控（stages.go:2737），本地分支没有。
- 影响：管理员显式关闭 Computer Use（runtime=none）后，模型仍可通过直接输出未注册的工具名（OpenAI 兼容 API 不校验名称）调用宿主机 shell/python，配置防线被绕过；prompt 注入即可利用。
- 修复建议：该 switch 仅在 `runtime == "local"` 时执行；none 时返回"工具未启用"。

## M-20 pipeline：send_message_to_user 允许向任意平台/会话发消息，无授权约束

- 位置：`internal/pipeline/message_tools.go:114-124、138`
- 问题：`session` 参数形如 `platform:message_type:session_id`，直接拆分后用于 `platformMgr.Send(platform, sessionID, chain)`，无白名单/会话成员校验。
- 影响：prompt 注入（如网页内容指示）可让 bot 向任意已配置会话/平台发送任意内容（含文件/媒体），造成跨会话骚扰或钓鱼消息外发。
- 修复建议：限制目标为当前会话，或仅允许管理员/白名单配置的目标会话。

## M-21 pipeline：共享 provider_settings map 并发写 + conv.History 无锁读

- 位置：`stages.go:1349、1356、1367`（`providerSettings["persona"] = ...`）；`stages.go:3111`（conv.History 无锁读）；`internal/dashboard/chat_stream.go:308-317`（总线不可用时独立 goroutine 直接跑 scheduler）
- 问题：`resolveProvider` 返回 `s.config["provider_settings"]` 的共享嵌套 map，每次请求写入 `persona`；`conversationHistory` 在 conversation.Manager 锁外读 `conv.History`（与 `AppendHistory` 的锁内 append 竞争）。正常平台事件在总线单 goroutine 串行，但 dashboard 兜底路径可与总线并发运行同一 `ProcessStage`。
- 影响：并发 map 写可导致 fatal 崩溃；History 竞争属未定义行为（race detector 必报）。
- 修复建议：`providerSettings` 每次请求深拷贝或只读传递 persona；`conversationHistory` 在管理器内加读锁导出快照（见 M-32）。

## M-22 pipeline：snapshotFileMutation 逻辑恒为 no-op（快照在变更之后才拍）

- 位置：`internal/pipeline/git_snapshot.go:71-91`；调用点 `stages.go:2768-2772`
- 问题：注释称"captures a git snapshot before a file-mutating tool"，但调用发生在 `executeFileWrite/executeFileEdit` 完成之后。函数内 `before := gitTreeHash(ws)`（含 `git add -A` + `write-tree`）已把变更纳入树，随后的 `gitDiffTree(ws, before)` 恒为空。
- 影响："工具修改文件的工作区快照审计"功能从未生效，变更检测/日志/补丁附加全部无效。
- 修复建议：在工具执行前拍 `gitTreeHash`，执行后 diff；将两个时机作为参数传入（snapshotBefore → 执行 → snapshotFileMutation(before, ...)）。

## M-23 aiocqhttp：escapeCQText 对 array 格式文本段做 CQ 转义（显示乱码且转义不完整）

- 位置：`internal/platform/sources/aiocqhttp/adapter.go:737-743、806-812`
- 问题：发送走的是 array 段格式（`{"type":"text","data":{"text":...}}`），NapCat/Lagrange/go-cqhttp 对 array 格式的 text 段不做 CQ 反转义，含 `[`、`]`、`,` 的文本（LLM 输出的 Markdown 列表、代码极其常见）会以 `&#91;`/`&#44;` 原样显示。同时 OneBot 规范要求先转义 `&`→`&amp;`，此处未做，防护本身不完整。Python 原版不做此转义。
- 影响：普通文本显示乱码；CQ 注入防护形同虚设。
- 修复建议：array 格式下移除转义；若确需兼容 CQ 字符串模式，按规范补齐 `&` 转义并仅在该模式启用。

## M-24 discord：媒体发送三连缺陷（base64 未解码 / URL 拉取无超时 / typed-nil panic）

- 位置：`internal/platform/sources/discord/adapter.go:527-541`（mediaFile）、`534-536`、`553-573`（mustFetchURL）
- 问题：
  1. `strings.NewReader(b64)` 把 base64 字符串原文当作图片二进制上传（Python 原版是解码后上传）；
  2. `mustFetchURL` 用无超时的 `http.Get`、不校验状态码、无大小上限全量读入内存；
  3. 失败时返回 `*strings.Reader(nil)` 装入非 nil 的 `io.Reader` 接口，discordgo 的 `io.Copy` 调用 `(*strings.Reader)(nil).WriteTo` → nil 解引用 panic（已核对实现）。
- 影响：base64 图片发出的是损坏文件；坏 URL/失败打开文件 → 发送路径 panic；文件名无扩展名。
- 修复建议：解码 base64；mustFetchURL 加 context 超时、状态码检查、大小上限；失败时返回 nil `*discordgo.File`；文件名补扩展名。

## M-25 discord：followups 残留过期 interaction，普通回复被错误路由到失效 webhook

- 位置：`internal/platform/sources/discord/adapter.go:399-402`（Send 消费）、`518-520`（注册）
- 问题：每次斜杠命令都向 `followups[channelID]` 写入 interaction；仅当该频道发生一次 Send 才被消费删除。命令无回复时条目永久残留；之后该频道任何普通消息的回复都会被改道 `FollowupMessageCreate`（interaction token 15 分钟失效）→ 发送失败，且残留条目无限累积。
- 影响：用过一次不回显的斜杠命令后，该频道后续正常回复可能全部失败；map 缓慢泄漏。
- 修复建议：记录注册时间戳，Send 时校验时效（如 <3min）否则走普通通道；定期清理；interaction id 关联到具体事件而非仅频道。

## M-26 kook：KookClient 多字段跨 goroutine 无锁读写（数据竞争）

- 位置：`internal/platform/sources/kook/client.go:51-54`（字段）、`302-304`（listen goroutine 写 `lastSN`） vs `423-426`（心跳 goroutine 读）、`354-357`（`lastHeartbeatTime` 写） vs `401`（读）、`341-343/363-364/369-371`（`sessionID`）；另 `kook/adapter.go:95/164` 与 `dingtalk/adapter.go:57` 的 `running` 均为 Stop 写/主循环读无同步
- 影响：竞态必报；极端情况下心跳 sn 错乱、误判心跳超时触发无谓重连。
- 修复建议：用一把 mutex 或 atomic 保护这些字段；`running` 改 atomic.Bool。

## M-27 lark：Webhook 签名校验可通过"省略请求头"绕过

- 位置：`internal/platform/sources/lark/webhook.go:106-116`
- 问题：

  ```go
  if timestamp != "" && nonce != "" && signature != "" {
      if !s.verifySignature(...) { ... 401 ... }
  }
  ```

  配置了 `encrypt_key` 但三个头任一缺失时直接跳过签名验证。若用户未配置 `verification_token`（可选项），知道 webhook uuid 的攻击者可提交未加密伪造事件。token 校验在 `encrypt_key` 未配置时也完全缺失。
- 影响：伪造消息注入事件管道（触发 bot 回复/命令执行）。
- 修复建议：配置了 `encrypt_key` 时强制要求签名头存在并校验；token 缺失时告警。

## M-28 lark：解密路径长度校验不足，恶意密文可致 panic

- 位置：`internal/platform/sources/lark/webhook.go:62-74`
- 问题：`len(enc) < aes.BlockSize` 只拦截 <16 字节。`len(enc)==16` 时 `ct` 为空，`pt[len(pt)-1]` 即越界 panic；`len(enc)%16 != 0` 时 `CryptBlocks` panic。
- 影响：知道 webhook URL 的攻击者可反复触发（连接级骚扰）。
- 修复建议：校验 `len(enc) > aes.BlockSize && (len(enc)-aes.BlockSize)%aes.BlockSize == 0`。

## M-29 lark：Webhook 事件去重读取错误字段位置，去重失效

- 位置：`internal/platform/sources/lark/adapter.go:119-125`
- 问题：`eventData["event_id"]` 读顶层；而处理的是 schema v2 事件（`event_type` 从 `header.event_type` 取），v2 的 `event_id` 只存在于 `header` 中，顶层取不到 → `isDuplicateEvent` 从不执行。
- 影响：飞书 webhook 失败重推（官方会重试）时消息被重复处理、bot 重复回复。
- 修复建议：与 eventType 一致地从 `header.event_id` 读取。

## M-30 misskey：数值配置全部用 `.(int)` 断言，但 JSON 数值均为 float64，配置静默失效

- 位置：`internal/platform/sources/misskey/adapter.go:77、99、103、107、426`
- 问题：配置经 `json.Unmarshal` 解析，数字一律是 float64。`config["misskey_max_download_bytes"].(int)` 等断言恒失败，全部回落默认值。
- 影响：`max_message_length`、`download_timeout`、`chunk_size`、`upload_concurrency`、尤其 `misskey_max_download_bytes`（下载大小上限）设置后不生效 —— 上限为 0 时 `downloadWithSSL` 变为无限制下载，DoS 风险。
- 修复建议：仿照 mattermost 的 `.(float64)` 或用通用数字转换（lifecycle 已有 `floatValue`）。

## M-31 mattermost/misskey：WebSocket 无有效保活，空闲连接每 90 秒断连重连

- 位置：`mattermost/adapter.go:218-231`；`misskey/streaming.go:179-204`
- 问题：两处都在循环里 `SetReadDeadline(now+90s)` 后 `ReadMessage`。Mattermost 侧没有 ping/challenge 处理；Misskey 侧虽每 30s 发 ping，但未安装 pong handler 刷新 deadline —— gorilla 的读 deadline 是绝对时间，收到 pong 控制帧不会重置，只有数据帧进入下一轮循环才重置。Mattermost 官方协议还要求客户端响应服务端 challenge。
- 影响：空闲时段连接每 ~90s 被误判失联并重连，日志刷屏、重连窗口内丢消息。
- 修复建议：Misskey 加 `conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(...) })`；Mattermost 实现 challenge_response/定期 ping。

## M-32 conversation：会话对象在管理器锁外共享读写，数据竞争

- 位置：`internal/conversation/manager.go:520-532`（persist 在解锁后 Marshal conv.History）、`218-231`（GetConversationByCID 解锁后序列化）、`internal/pipeline/stages.go:3111`（无锁读）
- 问题：`Manager.mu` 只保护 map，返回的 `*Conversation` 是共享可变指针。管线 goroutine（AppendHistory 持锁 append）与 dashboard HTTP goroutine（ReplaceHistoryByCID、锁外序列化）并发访问同一 History 切片/Title 字段。
- 影响：race 可报；序列化历史撕裂、偶发 panic。
- 修复建议：管理器内提供"锁内快照/锁内序列化"接口；persist/序列化先在持锁期间深拷贝 History。

## M-33 conversation：会话历史永不截断，内存与 DB 行无限增长

- 位置：`internal/conversation/manager.go:439-457`；`internal/pipeline/config.go:36`（`dequeue_context_length` 全项目无使用点）；`default_config.go` 默认 `max_context_length: -1`
- 问题：截断只发生在发给 LLM 的副本上（stages.go:3119-3131），`conv.History` 本体和 `content` 列随消息数单调增长。Python 原版有 dequeue 机制，Go 版从未实现。
- 影响：长期活跃会话内存与 SQLite 行体积无限膨胀，拖慢每次 UpsertConversation。
- 修复建议：`AppendHistory` 中实现按 `dequeue_context_length` 出队；`llm_compress` 成功后把压缩结果写回存储。

## M-34 cron：run_once 任务的 run_at 不持久化，重启后立即错时触发

- 位置：`internal/cron/manager.go:338-371`（Load）；`internal/db/database.go:206-224`（cron_jobs 表无 run_at 列）
- 问题：`Load` 重建 Job 时从不恢复 `RunAt`，`RunOnce=true, RunAt=零值`。下次 tick 时 `cronNextRun` 报 "run_once job has no run_at"，NextRun 保持零值 → 立即"到期"→ 触发一次后按 RunOnce 分支删除。
- 影响：用户预约"明天 8 点执行一次"的任务，只要 bot 在此之前重启，就会在启动后立刻执行并被删除。
- 修复建议：迁移表结构加 `run_at` 列（或从 payload 恢复），Load 时回填；对已过期且未执行的 run_once 任务决定明确策略（补跑或丢弃并告警）。

## M-35 lifecycle：优雅停机无全局兜底，第二次 Ctrl+C 被吞掉

- 位置：`cmd/astrbot/main.go:67-71`；`internal/lifecycle/lifecycle.go:544-573`
- 问题：main 只从 `sigCh` 读一次信号；`signal.Notify` 生效后 SIGINT 不再有默认终止行为，后续 Ctrl+C 只进缓冲为 1 的 channel 无人消费。`Stop()` 中除 EventBus（30s）、cron（10s）、插件 Cleanup 外，`platformMgr.StopAll()`、`dashboard.Stop()`、`database.Close()` 均无超时；`Stop()` 全程持 `l.mu`。
- 影响：任一组件停机卡死时，进程只能 SIGKILL（又会留下孤儿插件进程）。
- 修复建议：循环读信号，第二次信号直接 `os.Exit(130)`；或给整个 `Stop()` 包一层超时看门狗。

## M-36 log：日志历史字节账目对 trace 条目单向漂移，最终每次打日志清空全部历史

- 位置：`internal/log/log.go:259-267`（驱逐只减 Message 长度）vs `log.go:340-341`（PublishTrace 计入序列化 payload 大小）
- 问题：trace 条目 Message 为空串，入库时按 `len(json.Marshal(payload))` 计入 `historyBytes`；驱逐循环对 trace 只减 0。漂移累计超过 1 MiB 后 `historyBytes > maxHistoryBytes` 恒真，每次 `log()` 都把 history 逐条驱逐到空。
- 影响：开启 trace 一段时间后，WebUI 控制台日志缓冲近乎恒空。
- 修复建议：抽出 `entrySize(e)`（trace 用序列化长度、普通用 Message 长度）供添加和驱逐两侧统一使用。

## M-37 provider：Anthropic 工具调用与多模态静默失效，历史格式不转换

- 位置：`internal/provider/sources/anthropic_source.go:212-233`（buildRequestBody）、`87-116`（TextChat 解析）、`230`
- 问题：
  1. `buildRequestBody` 从不发送 `req.Tools`；响应解析只取 `type=="text"`，`tool_use` 块被丢弃 → 工具能力静默失效（对照 `kimi_code_source.go` 已实现转换，说明移植目标包含此能力）。
  2. pipeline 工具循环第二轮会把 OpenAI 格式 `{"role":"assistant","tool_calls":[...]}` 和 `{"role":"tool","tool_call_id":...}` 塞进 contexts，buildRequestBody 只过滤 system 后原样转发 → Anthropic API 400 或忽略。
  3. `ToUserMessage()` 生成 OpenAI 格式 `image_url` 内容块直接发给 Anthropic（要求 `source/base64` 格式）→ 带图消息 400。
  4. `max_tokens` 硬编码 4096，`stop_reason` 从不检查。
- 影响：Anthropic 用户配置工具/图片即静默失败或报 400。
- 修复建议：参考 kimi 实现补 tools 转换、tool_use 解析、历史格式转换；max_tokens 走配置。

## M-38 provider：Kimi 工具循环第二轮起历史格式不兼容

- 位置：`internal/provider/sources/kimi_code_source.go:368-377`
- 问题：`buildRequestBody` 正确转换了 `req.Tools` 并解析 `tool_use`，但 contexts 逐条原样透传（仅跳过 system）。第一轮模型返回 tool_use 后，pipeline 追加 OpenAI 格式 tool_calls/tool 消息，下一轮请求对 Anthropic 协议端点无效（需要 `tool_use` content block + `tool_result` block）。
- 影响：Kimi coding 场景（工具密集）第二轮 follow-up 必然失败。
- 修复建议：buildRequestBody 里把 OpenAI 风格 tool_calls/tool 消息转换为 Anthropic 的 `tool_use`/`tool_result` 内容块。

## M-39 provider：Gemini chat 完全丢弃多模态输入

- 位置：`internal/provider/sources/gemini_source.go:229-251`
- 问题：

  ```go
  content, _ := msg["content"].(string)   // 多模态数组 content → ""
  "parts": []map[string]interface{}{{"text": req.Prompt}},  // ImageURLs/AudioURLs 全部忽略
  ```

  `AssembleContext()` 构造的多模态内容对 Gemini 不生效；历史里任何数组 content 消息被静默清空。
- 影响：Gemini 用户发图片/音频时模型完全看不到；多模态会话历史丢失。
- 修复建议：把 image/audio URL 转为 Gemini `inline_data`/`file_data` parts，content 数组逐块转换。

## M-40 provider：Anthropic 流式 usage 输入 token 恒为 0；Azure TTS 竞态；Edge TTS 阻塞

- 位置：`anthropic_source.go:168-175`（usage）；`azure_tts_source.go:56-63,159-160,199-200,206`（竞态）；`edge_tts_source.go:87-95,115-129`（阻塞）
- 问题：
  1. Anthropic 只在 `message_delta` 上取 usage，而它只含 `output_tokens`；`message_start.message.usage`（含 input_tokens）未解析（kimi 版正确处理了）→ 输入侧统计恒 0。
  2. Azure TTS 的 `token/tokenExpire`（native）与 `timeOffset/lastSync`（OTTS）共享字段无锁，`GetAudio` 会被并发调用 → 数据竞争、交错刷新 401。
  3. Edge TTS 读循环 `conn.ReadMessage()` 无 `SetReadDeadline` 也不监听 ctx → 服务端挂起时 goroutine 永久阻塞。
- 修复建议：增加 message_start 处理；Azure 用一把 mutex 保护 token 刷新与时间同步；Edge 加 ReadDeadline 并续期或 ctx 监听后 Close 解除阻塞。

## M-41 provider：Gemini 重试用 req.Clone 共享请求体，重试语义靠标准库内部行为"碰巧"成立

- 位置：`internal/provider/sources/gemini_source.go:51-58`
- 问题：

  ```go
  return DoWithRetry(ctx, s.client, func() (*http.Request, error) {
      clone := req.Clone(ctx)   // Body 浅拷贝、共享同一个 reader
      return clone, nil
  }, ...)
  ```

  实证（go1.26）：第一次尝试耗尽 `bytes.Reader`，第二次 Clone 共享已耗尽的 reader。当前能发出完整 body 是因为 `GetBody` + header 未 flush 时 transport 以 `nothingWrittenError` 内部重发（每次外层重试多烧一次内部往返）；一旦 body 被包装、走 HTTP/2 或代理，直接得到 `ContentLength=30 with Body length 0` 且不重试（已实证复现）。其他所有 source 都在 factory 里 `bytes.NewReader(jsonBody)` 重建，唯独 Gemini 不是。
- 修复建议：factory 内每次 `http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonBody))`。

## M-42 plugin：插件身份绑定竞态 + 键不匹配，HostService 配置隔离可被破坏

- 位置：`internal/plugin/runtime.go:605-618` vs `673-675`；SDK host.go
- 问题：
  1. SDK 侧 `hostPluginID` 是进程级全局变量，宿主 `Load` 不持有 per-id `lockOp`（runtime.go:392-428），不同插件的 `startInstance` 可并发执行 → A 设置 id 后、握手 accept 前，B 覆盖全局值 → A 的 HostService 连接被绑定为 B 的身份，`GetConfig/SetConfig` 的身份隔离校验形同虚设。
  2. `acceptHostService` 以 manifest Name 为 key 记录 `hostServers[pluginName]`，而 `BindHostServiceName(id, meta.Name)` 以 manifest id 查找；Name≠id 时查找 miss，绑定不生效 → 插件无法读写自己的配置。
- 影响：并发装载时插件 A 可读写插件 B 的 config；或合法插件配置读写被永久拒绝。
- 修复建议：宿主在 `startInstance` 全程持有一把全局（或 per-load）互斥序列化 Set/Dispense 窗口；`BindHostServiceName` 与 accept 使用同一 key（统一用 manifest id 并支持 Name 别名）。

## M-43 plugin：生命周期钩子 RPC 无超时且 Unload 持锁广播，卡死插件冻结全部 manifest 操作

- 位置：`internal/plugin/runtime.go:488`（context.Background()）、`871-885`（逐插件串行 RPC）；`runtime_admin.go:182-212`（SetEnabled 持 manifestMu 调 Load/Unload）
- 问题：`Unload` 先从 instances 移除，再用无 deadline 的 Background ctx 同步调用所有其他插件的 `on_plugin_unloaded` 钩子（gRPC 默认无超时）。一个挂死的插件（死循环/死锁）会让 Unload 永久阻塞；若经由 `SetEnabled(false)` 进入，还持有 `manifestMu` 与 per-id `lockOp`。
- 影响：安装/卸载/启用/禁用等所有 manifest 操作被级联阻塞（WebUI 假死）；`LoadInstalled` 启动路径同理。
- 修复建议：`TriggerHookPayload` 内部为每次 RPC 包 `context.WithTimeout`（如 10-30s）；避免在持 `manifestMu` 时执行长 RPC。

## M-44 plugin：从 URL 下载 .tar.gz 插件包必定失败（扩展名被截断为 .gz）

- 位置：`internal/plugin/source.go:215、426-445`
- 问题：

  ```go
  archive := filepath.Join(tmp, "src-archive"+filepath.Ext(source)) // "x.tar.gz" → Ext()==".gz"
  ...
  case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
  ```

  `isArchiveURL` 放行 `.tar.gz`，但缓存文件名只剩 `.gz` 后缀，extractArchive 的 switch 皆不匹配 → "unsupported archive"。
- 影响：所有 `.tar.gz` 归档 URL 安装 100% 失败（.zip/.tgz 与本地 .tar.gz 不受影响）。
- 修复建议：按后缀映射固定扩展名（如 `.tar.gz` → `"src-archive.tar.gz"`），或 switch 中增加 `.gz` 按 gzip+tar 处理。

### 其余中危（平台侧补充）

## M-45（补充）wecom：File/Video 组件把 URL 传给了 file 形参，URL-only 组件永远发送失败

- 位置：`internal/platform/sources/wecom/send.go:91、110`
- 问题：`resolveComponentFile(path, file, url, b64, suffix)` 中 `file` 走 `os.Stat` 本地路径逻辑，而 File/Video 把 `c.URL` 放在第二参：

  ```go
  path, err := resolveComponentFile(c.Path, c.URL, "", "", ".bin")   // File
  path, err := resolveComponentFile(c.Path, c.URL, "", "", ".mp4")  // Video
  ```

  URL 会被 os.Stat 失败后丢弃，url 形参为空 → "媒体组件没有可用的 path/url/base64"。同目录 `wecom_ai_bot/webhook.go:290` 的 `componentFilePath(cc.Path, cc.URL)` 参数顺序是对的。
- 修复建议：改为 `resolveComponentFile(c.Path, "", c.URL, "", ".bin")`（Video 同理）。

## M-46（补充）wecom：KF 消息同步游标不推进，has_more 时死循环且只处理最后一条

- 位置：`internal/platform/sources/wecom/adapter.go:280-297`（client.go:288-299 的 cursor 参数从未被喂入）
- 问题：

  ```go
  for hasMore {
      ret, err := a.client.KFSyncMsg(context.Background(), msg.Token, msg.OpenKfId, "", 1000)
  ```

  `next_cursor` 被忽略，每轮用空 cursor 重新拉取同一批；积压 > 1000 条（has_more=true）时无限循环。循环体内 `latest = msgList[len-1]` 反复覆盖，同批其余消息被静默丢弃。
- 影响：积压场景下回调 goroutine 死循环狂刷 API、永不回包；平时也只取最新一条。
- 修复建议：读取 `next_cursor` 传入下一轮；逐条转换而非只留最后一条。

## M-47（补充）wecom_ai_bot：流式轮询在非 finish 阶段取出的图片增量被永久丢弃

- 位置：`internal/platform/sources/wecom_ai_bot/adapter.go:345-356、372-388`
- 问题：drain 循环把 `image` 项从 back queue 取出放入 `imageBase64`，但只有 `finish` 为真才组装进 `msgItems`；`!finish` 的轮询中图片已出队却不上行（msgItems 为 nil，序列化为 `"msg_item":null`）。
- 影响：轮询时机插在图片入队与 end 入队之间时（先图后文的常见时序），图片内容丢失。
- 修复建议：非 finish 轮询也把 imageBase64 放入 msgItems，或未 finish 时不消费 image 项。

## M-48（补充）misskey：重连后频道被重复订阅，每条事件处理两次（重复回复）

- 位置：`internal/platform/sources/misskey/streaming.go:73-79`；`adapter.go:214-233`
- 问题：`GetStreamingClient()` 跨重连复用同一实例。第二次 `Connect()` 会按 `desiredChannels` 重订阅（生成新 channel id），随后 adapter 又显式 `SubscribeChannel("main"/"messaging"/...)` 再订一次。服务端对同一频道持有 2 个订阅，每条事件下发 2 帧，`handleMessage` 分发两次；且旧 channel id 永不清理。
- 影响：第一次断线重连后，mention/私聊/群聊事件全部翻倍 → bot 对每条消息回复两次。
- 修复建议：重连后只走一条订阅路径；重连时清空 `s.channels`。

## M-49（补充）misskey：URL 下载无内网校验（SSRF）+ allow_insecure 静默降级 TLS

- 位置：`internal/platform/sources/misskey/api.go:121-131、155-191`
- 问题：`uploadAndFindFile` 对组件携带的任意 `http(s)://` URL 发起服务端下载，无 scheme/host 白名单；SSL 校验失败且 `misskey_allow_insecure_downloads=true` 时自动用 `InsecureSkipVerify` 重试。
- 修复建议：限制下载目标为 bot 所在实例域名或公网地址；不安全降级至少显式告警并保持默认关闭。

## M-50（补充）slack：事件处理与文件下载使用 context.Background()（无超时），goroutine 可被无限挂起

- 位置：`internal/platform/sources/slack/adapter.go:334、340、385、411`
- 问题：`convertMessage` 对每条消息同步调用 2+ 次 Slack API、图片消息再调 `GetFileContext(context.Background(), url, &buf)`（下载量无上限）。slack-go client 默认无超时。
- 影响：Slack API/CDN 挂起时，事件处理 goroutine 与连接长期占用堆积（fd/goroutine 泄漏）。
- 修复建议：统一使用带超时的 ctx（如 30s），下载加 `io.LimitReader`。

## M-51（补充）satori：发送时图片/语音用 http.Get（DefaultClient 无超时）且不限制大小

- 位置：`internal/platform/sources/satori/message.go:799-810、845-855`
- 问题：`imageToDataURL`/`recordToBase64` 中 `http.Get(img.URL)` 后 `io.ReadAll(resp.Body)`，无超时、无大小上限。
- 影响：URL 指向慢速服务器时 `Send` 永久阻塞（pipeline 输出阶段卡死该会话）；超大文件内存耗尽。
- 修复建议：带超时的 http.Client + `io.LimitReader`（如 20MB）。

## M-52（补充）qqofficial：心跳 goroutine 与读循环并发写同一 WebSocket 连接

- 位置：`internal/platform/sources/qqofficial/adapter.go:264-285`（心跳）、`314-324`（identify/resume 写）、`349-371`（心跳写）
- 问题：gorilla/websocket 要求同一时刻仅一个 goroutine 调用写方法。读循环在收到 HELLO 时调用 `sendFrame` 写 identify/resume，而 `heartbeatLoop` goroutine 同时可能写心跳帧。另外 `heartbeatCancel` 由心跳 goroutine 写、由 `connectOnce` 的 defer 读，无同步。
- 影响：并发写导致帧损坏、连接异常断开、甚至 gorilla 内部 panic。
- 修复建议：所有写操作收敛到单一 goroutine（channel 交给一个写 goroutine）或对写加互斥锁；heartbeatCancel 用锁或仅由持有者取消。

## M-53（补充）qqofficial / qqofficial_webhook：语音（Record）上传被当作视频类型（file_type=2）

- 位置：`qqofficial/adapter.go:876-887、905-916`；`qqofficial_webhook/adapter.go:697-708、726-737`
- 问题：`extractSendParts` 对 Record/Video 只填 `fileRef`、不填 `fileName`，发送时 `fileName == ""` → `fileType = fileTypeVideo`。QQ 的语音 file_type 应为 3（常量 `fileTypeVoice` 定义了但从未使用）。
- 影响：TTS 语音回复在 QQ 官方平台以视频类型上传，发送失败或无法播放。
- 修复建议：`extractSendParts` 保留组件类型，Record 用 fileTypeVoice。

## M-54（补充）qqofficial_webhook：频道/频道私聊消息链丢失 At/Plain 文本组件

- 位置：`internal/platform/sources/qqofficial_webhook/parse.go:153-157、171-175`
- 问题：`abm.Message = msg` 赋值发生在追加 At/Plain 之前：

  ```go
  msg = append(msg, appendAttachments(...)...)
  abm.Message = msg          // len 就此固定
  msg = append(msg, &message.At{...}); msg = append(msg, &message.Plain{...})
  ```

  slice 头已拷贝，`abm.Message` 的 len 不会随后续 append 增长。对比 kindGroup/kindC2C 分支（119-120、134-135）在末尾赋值，行为正确。
- 影响：webhook 模式下频道/私聊消息的文本组件从消息链中消失，依赖 chain 的下游逻辑拿不到正文。
- 修复建议：将 `abm.Message = msg` 移到两个分支的追加语句之后。

## M-55（补充）satori：重连计数器跨连接累计，10 次断连后适配器永久停机

- 位置：`internal/platform/sources/satori/adapter.go:183-222`
- 问题：`connectWebsocket` 仅在优雅退出路径返回 nil；正常连接被服务器断开一律返回 error → `retryCount++` 且跨连接生命周期累计，`retryCount >= maxRetries` 后 break。
- 影响：进程存活期间总共断连/失败 10 次（哪怕每次成功运行数天），适配器即永久放弃重连，satori 平台静默失效。
- 修复建议：连接成功（收到 READY 或维持超过某时长）后重置 `retryCount`，语义改为"连续失败次数"。

## M-56（补充）webchat：默认端口 6185 与 dashboard 冲突

- 位置：`internal/platform/sources/webchat/adapter.go:49-54`；dashboard 端 `lifecycle.go:302` 硬编码 6185
- 问题：webchat 未配置端口时默认 6185，与 dashboard 完全相同；平台先于 dashboard 启动。
- 影响：用户启用 webchat 且不指定端口时，dashboard 绑定 6185 失败，WebUI 整体不可用（错误只在 goroutine 里打日志）。
- 修复建议：webchat 默认端口改为独立值（如 6195），或启动时探测冲突并报错。

## M-57（补充）knowledgebase：ChunkText 在换行处截断时静默丢失文本

- 位置：`internal/knowledgebase/vecdb.go:164-180`（关键 170-172）
- 问题：chunk 内换行位置 `nl > chunkSize/2` 时截断 `content[:nl]`，但下一个 chunk 起点是 `start+step`（默认 chunkSize=512、overlap=50 时 step=462）。当 `nl < step`（nl∈(256,462) 均满足）时，区间 `[nl, start+step)` 的内容不属于任何 chunk。已实测复现：默认参数下丢失 23 个连续标记（约 160 字符）。
- 影响：知识库文档内容静默缺失，检索结果不完整且难以察觉。
- 修复建议：截断时将下一 chunk 起点同步前移到截断点，保证相邻 chunk 无缝隙。

## M-58（补充）sandbox：LocalBooter 继承宿主全部环境变量；Docker 容器无资源/网络隔离

- 位置：`internal/sandbox/manager.go:166-171`（Env 未设置）；`manager.go:307-309`（run 参数）
- 问题：
  1. 注释声称 "restricted env" 但 `exec.CommandContext` 未设置 `Env`，默认继承父进程全部环境变量。该 booter 在 Docker 不可用时被启用，执行的是 LLM 生成的命令。
  2. 容器创建命令只有 `run -d --name ... --workdir /workspace <image> tail -f /dev/null`，缺少 `--memory`/`--cpus`/`--pids-limit`、`--network none`、`--cap-drop=ALL`。
- 影响：沙箱内代码执行 `env` 即可读取宿主进程全部 API key、token；Docker 沙箱可耗尽宿主资源、以完整网络权限访问内网。
- 修复建议：显式设置最小化 `c.Env`；容器补齐资源限额与网络/权限收紧（至少 `--network none --memory ... --cpus ... --cap-drop=ALL`）。

## M-59（补充）toolchain：校验和校验失败后仍然继续安装（供应链攻击面）

- 位置：`internal/toolchain/toolchain.go:193-195`
- 问题：

  ```go
  if err := tc.verifyChecksum(dest, archive); err != nil {
      logger.Warn("Checksum verification failed for %s: %v (proceeding anyway)", archive, err)
  }
  ```

  sha256 不匹配时仅警告后照常解压安装；且 checksum 与 archive 默认可同源于用户配置的 `ASTRBOT_GO_MIRROR`，镜像被劫持时校验形同虚设。
- 影响：被篡改的 Go 工具链随后用于编译并运行插件代码。
- 修复建议：mismatch 时删除已下载文件并返回错误，仅允许显式跳过（独立的环境变量）；校验值考虑内置白名单。

## M-60（补充）sandbox：ShipyardNeo HTTP 客户端 60s 总超时与 300s 命令超时矛盾

- 位置：`internal/sandbox/shipyard_neo.go:70-72` 与 `245、260`
- 问题：`client: &http.Client{Timeout: 60 * time.Second}`，而 Exec 请求体携带 `"timeout": 300`。Client.Timeout 覆盖整个请求周期，超过 60s 的命令必被本地掐断。
- 影响：运行超过 1 分钟的沙箱命令报超时且结果丢失（远端仍在跑）。
- 修复建议：exec 端点使用不设总超时（依赖 ctx）或 ≥ 命令 timeout 的独立 client。

---

# 三、低危（46 项）

## L-01 dashboard：/stat/start-time 返回当前时间而非进程启动时间

- 位置：`internal/dashboard/server.go:667-670`。`"start_time": time.Now().Unix()`，而 `getBaseStats`（server.go:738）正确使用 `s.startTime`。前端运行时长恒为 0。修复：改 `s.startTime.Unix()`。

## L-02 dashboard：auth == nil 防御分支不一致（handleCheck panic / handleLogin 发废票）

- 位置：`server.go:467-476`（handleCheck 中 `if s.auth != nil` 判定后同 map 无条件调 `s.auth.Username()` → nil panic）、`413-422`（handleLogin nil 分支返回的 guest token 从未 RegisterToken）。当前均为死分支（NewServer 总会创建 auth），属潜伏雷。修复：补 nil 判断；guest 分支要么注册 token 要么删除。

## L-03 dashboard：敏感数据落盘（明文密码兼容字段 + 聊天记录 0644）

- 位置：`auth.go:153-171`（`dash["password"] = plain`，为兼容 Python 版持久化）；`chat_store.go:59`（`writeFileAtomic(cs.path, data, 0644)`）。同机其他用户可读完整对话。修复：chat_sessions.json 改 0600；长期去掉明文密码回写。

## L-04 dashboard：trust_proxy 开启时登录限速可被伪造 X-Forwarded-For 绕过

- 位置：`auth.go:653-667`（clientIP 取 XFF 第一段作为限速 key）。攻击者为每请求伪造不同 XFF，每个"IP"各得一桶，暴力破解限速失效（默认 trust_proxy=false，需显式配置）。修复：文档强调仅在确有反代时开启；或取 XFF 最后一个可信跳。

## L-05 dashboard：weixin_oc 的 base64URLSafe 实际返回 hex

- 位置：`internal/dashboard/weixin_oc_registration.go:224-227`。`func base64URLSafe(n int) string { ...; return hex.EncodeToString(b) }`。Python 原版发送 urlsafe-base64。修复：改 `base64.URLEncoding.EncodeToString(b)` 或重命名。

## L-06 dashboard：personaStore 持久化非原子写

- 位置：`personas_store.go:49-58、296-303`（os.WriteFile 直写）。同目录 chat_store/mcp_store 均已用 writeFileAtomic。崩溃可留截断 JSON，重启后 persona 全丢。修复：复用 writeFileAtomic。

## L-07 dashboard：kbDeleteDoc 向量库打开失败时仅清 SQLite，残留孤儿向量

- 位置：`internal/dashboard/kb_vec.go:269-289`。`OpenVecDB` 失败时静默跳过向量删除，nanovec 中该文档向量永久残留。修复：删除文档时同步删除 .store/.idx 中对应 chunk 或记录待清理标记。

## L-08 dashboard：system-config 保存接口吞掉解码错误仍报"保存成功"

- 位置：`handlers.go:44-69`（同模式 5840-5856）。空 body/畸形 JSON/`null` 时 `body.Config == nil` 跳过保存并返回成功。修复：解码失败返回 400；Config == nil 返回明确错误。

## L-09 dashboard：legacy /api/config set|update 为假实现

- 位置：`handlers.go:1150-1156`。读入 body 后不落地任何修改，固定返回 `{"message": "config updated"}`（reload 同理）。旧脚本静默丢失全部修改。修复：返回 410/501 或接入真实保存。

## L-10 dashboard：多处写操作吞错误后仍返回成功

- 位置：`handlers.go:1827-1839`（deleteBotByID）、`2859-2864`（KB 文档删除）、`4040-4041、4049-4050`（personas move/reorder）、`5800-5803`（config-profiles POST）、`4317、5491、1971`（tool permission / command_configs / log_level）均为 `_ = s.setConfigData` 后报成功。修复：记录 warn 日志；关键路径回传错误。

## L-11 dashboard：WebSocket 聊天连接无服务端 Ping，空闲 10 分钟必断

- 位置：`internal/dashboard/chat_stream.go:454-458`。设置了 ReadDeadline(10min) 与 PongHandler，但没有任何发送 Ping 的 ticker；浏览器 JS 只有收到 ping 才会回 pong。修复：增加每 1-5 分钟一次的 PingMessage ticker（在 writeMu 内发送）。

## L-12 dashboard：同一聊天 session 的并发订阅会互相串台

- 位置：`chat_stream.go:53-71`（Send 按 sessionID fan-out）、`174-175、595-596`（SSE 与 WS 均按 sessionID subscribe）。同一 session 两条并发消息时，每个订阅者都会把对方请求的回复累积进自己的 full 并持久化。修复：订阅键改为 sessionID+runID，或按 message_id 过滤。

## L-13 dashboard：kbTasks / installProgress 状态 map 只增不删

- 位置：`server.go:63、228、2727-2738、1788-1805`。getKBTask/recordKBTask 从不 delete；deleteKB 也不清理该 KB 的遗留任务。修复：完成/出错后 TTL 清理（time.AfterFunc），deleteKB 时同步清理。

## L-14 pipeline：executeSandboxTool 在 sandboxMgr 为 nil 时吞掉所有工具名

- 位置：`stages.go:2821-2824`。`if s.sandboxMgr == nil { return "Sandbox manager not configured.", true }` 位于 switch 之前，对非 sandbox 工具（web_search、get_current_time 等）也返回 handled=true。修复：nil 时按名单判断，非本名单工具返回 `("", false)`。

## L-15 pipeline：materializeToolResult 将 provider 返回的 toolCallID 未净化拼入文件名（路径穿越写）

- 位置：`internal/pipeline/tool_result.go:31-37`。`safeID` 来自 `resp.ToolsCallIDs[i]`（OpenAI 兼容端点可控），含 `../` 时 Join 后逃出 data/temp/tool_results。修复：对 safeID 做 `[^A-Za-z0-9_-]` 替换。

## L-16 pipeline：compressImageForProvider 临时文件永不清理

- 位置：`internal/pipeline/provider_options.go:254-267`。`os.CreateTemp("", "astrbot-compress-*.jpg")` 无删除逻辑，磁盘缓慢填充。修复：请求完成后 os.Remove，或复用 data/temp 统一清理。

## L-17 pipeline：shellSession.status() 读取 ProcessState 与 cmd.Wait() 数据竞争；会话表永不清理

- 位置：`internal/pipeline/computer_tools.go:570-589`（读 s.Cmd.ProcessState）、`639-642`（Wait goroutine 不持锁写）。另外已完成会话无任何回收，无限增长。修复：用 channel/atomic 包装退出状态；进程结束后延迟删除会话条目。

## L-18 pipeline：doom 计数器跨请求不复位，可能跨请求误判死循环

- 位置：`internal/pipeline/doom_loop.go:100-105`。lastTool/count 只在工具名变化或人工 resume 时复位，不区分请求边界。上一请求调工具 A 3 次、新请求再调 2 次即触发暂停。修复：每次 callLLMAgent 入口重置该会话计数（保留 pausedTool 状态）。

## L-19 pipeline：subagent 工具名未做合法性处理

- 位置：`internal/pipeline/subagent.go:78`（`"transfer_to_" + a.Name`）。含中文/空格的名字违反 OpenAI `^[a-zA-Z0-9_-]+$`，provider 会拒绝整份请求。修复：复用 `pluginToolSafeName` 并维护映射回查。

## L-20 pipeline：GroupChatContext.RemoveSession 持锁删除锁映射（并发窗口产生双锁）

- 位置：`internal/pipeline/group_context.go:165-177`。持有 per-umo 锁的同时 `delete(g.locks, umo)`；并发 getLock 可在新旧两个锁实例上同时进入临界区。当前生产无调用点（仅测试），属潜在 API 缺陷。修复：删除锁表条目改由全局锁保护，或不在 RemoveSession 里删除锁。

## L-21 pipeline：流式控制标记检测无法识别跨 chunk 拆分的标签

- 位置：`stages.go:1736`（`containsControlText(chunk.CompletionText)`）。`<function` 与 `_calls>` 拆在两个 chunk 时逐 chunk 检测漏判，控制标记片段会推送给用户。修复：维护跨 chunk 的滑动缓冲，确认非控制文本后释放前缀。

## L-22 pipeline：CommandContext 超时仅杀直接子进程，后台孙进程成孤儿

- 位置：`internal/pipeline/computer_tools.go:648-655`（`sh -c` + CombinedOutput）。超时后只 kill sh，`sh -c "a; b &"` 派生的孙进程继续运行。修复：`SysProcAttr{Setpgid: true}` + 按进程组 kill。

## L-23 pipeline：dashboard 的 selected_provider/selected_model 元数据从未被管线消费

- 位置：写入侧 `internal/dashboard/chat_stream.go:290-295`；pipeline 全目录无读取（grep 确认）。WebUI 选择的前缀/模型对实际 LLM 请求无效。修复：callLLMAgent 解析该 metadata 并覆盖 providerCfg。

## L-24 aiocqhttp：杂项缺陷

- 位置与问题：
  1. `adapter.go:115-123` —— Stop 仅 `server.Shutdown`，不会关闭被劫持的 WS 连接，conns 中的连接与读循环 goroutine 停机后仍存活；
  2. `adapter.go:665-676` —— `raw["message"]` 为 CQ 字符串格式时静默返回空链（Python 会打 critical 日志）；`quoted_parser.go:97-103` —— file 段未调 get_group_file_url，NapCat 文件消息得到空 URL 的 File 组件；
  3. `quoted_parser.go:299-316` —— collectNodeForwardIDs 只遍历 Nodes.Nodes，漏收集嵌套 Nodes.ForwardIDs；且 `adapter.go:714-726` 多个转发占位符只替换第一个（break）；
  4. `adapter.go:446-460` —— parseActionResult 在 status!="ok" 但无 msg 时吞掉错误当成功。
- 修复：逐项修补，至少为 2/3 补日志与分支。

## L-25 aiocqhttp / qqofficial：SelfID 无锁读写

- 位置：`aiocqhttp/adapter.go:489-495`（写） vs `573、586、639、647`（读）；`qqofficial/adapter.go:384-393、553-555`（写持锁） vs `703`（读无锁）。技术性数据竞争，实际影响小。修复：用已有 mutex 或 atomic 保护/持锁快照。

## L-26 dingtalk/kook/retry：重连/退避指数移位溢出，长时间连续失败后延迟归零变热循环

- 位置：`dingtalk/stream.go:25-35`（`1 << (safeRetryCount-1)` 无上限）；`kook/adapter.go:190-193`；`internal/provider/sources/retry.go:113-119`。移位数 ≥64 后溢出为 0/负，退避失效。修复：饱和运算（`if next > max/10 { delay = max }`）或移位前钳制指数。

## L-27 dingtalk：会话映射均为内存态，重启后私聊/群聊发送失效

- 位置：`dingtalk/adapter.go:61-67、335-359、794-810`。Python 原版将 senderId→staffId 持久化；Go 版 staffIDMap/knownGroups 仅内存。重启后回退用 unionId 充当 staffId → API 报错（且错误只写日志，Send 恒返回 nil）。修复：映射落库；发送失败返回 error。

## L-28 dingtalk：Callback 帧同步处理（含文件下载/转码）后才发送 ack

- 位置：`dingtalk/stream.go:239-265`；下载链路 `adapter.go:362-411`（httpClient 超时 60s）。单帧可阻塞数十秒，超过钉钉 stream 的 ack 时限会触发消息重投/断连。修复：先 ack 再异步处理（或 handler 池化）。

## L-29 discord：referencePrefix 生成无效前缀 `<@> `，messageID 未使用

- 位置：`discord/adapter.go:404-407、425-427`。`fmt.Sprintf("<@%s> ", "")` 固定输出 `<@> `，引用消息 id 被丢弃，slash 回复出现垃圾字符。修复：用消息链接或删掉该前缀。

## L-30 discord：Stop 在未 Ready/启动失败后调用会 nil panic；2000 截断按字节切

- 位置：`discord/adapter.go:185-195`（`a.session.State.User.ID` 未判空）；`388-392`（`content[:2000]` 可切断 UTF-8，Discord API 拒绝非法 UTF-8）。修复：Stop 判空；按 rune 截断。

## L-31 kook：pendingFetch.roles 在 close(done) 之后才赋值

- 位置：`internal/platform/sources/kook/roles_record.go:176-180`（先 close 后赋值） vs `160-168`（等待方无锁读）。无 happens-before 边，并发角色查询偶发把成功结果误读为 nil。修复：先赋值再 close（均持锁）。

## L-32 lark/line/mattermost/misskey：临时媒体文件从不清理，部分路径可预测

- 位置：`lark/message.go:188-209、524-540`；`line/adapter.go:514-535`、`message.go:430-445`；`mattermost/client.go:404-409`（`astrbot_mattermost_<fileID><ext>` 固定名 + 0644 写入共享 /tmp，存在符号链接覆盖面）。修复：`os.CreateTemp` 随机名；消费完后统一清理（misskey 已有该逻辑可参考，adapter.go:494-498）。

## L-33 line：未检查的类型断言可致 panic；`Sprintf("%v")` 产生 `"<nil>"` 字面量

- 位置：`line/adapter.go:231-232`（`event["webhookEventId"].(string)` 无 ok）；`217-219`（缺失字段时 Sprintf 得 `"<nil>"`，SessionID/Sender 被污染——LINE 隐私场景 group 内 userId 会省略）。修复：带 ok 断言或统一 stringVal 辅助函数。

## L-34 line：destination 与 mediaBaseURL 数据竞争

- 位置：`line/adapter.go:167`（webhook 并发请求 goroutine 写 destination）；`message.go:389-397`（并发 Send 写 mediaBaseURL）。修复：加锁或 sync.OnceValue/atomic.Value。

## L-35 line：readBody 忽略读取错误；replyTokens/mediaServer tokens 无过期清理；webhook 读体无大小限制

- 位置：`line/line_api.go:182-196`（非 EOF 错误也当成功，返回不完整字节）；`adapter.go:298-302、671-684`（replyToken 仅在被取用时删除）；`message.go:379-385、314-363`（tokens 只增不减，server 永不 Shutdown）；`adapter.go:133`（io.ReadAll 无 LimitReader，对比 lark 限制了 1MB）。修复：区分 io.EOF；TTL 清理；LimitReader。

## L-36 misskey：杂项

- 位置与问题：
  1. `adapter.go:153/172/199` —— `running` 裸字段跨 goroutine；`lark/adapter.go:171`、`line/adapter.go:118`、`mattermost/adapter.go:140`、`misskey/adapter.go:173` —— `close(a.stopCh)` 无 once 保护，二次 Stop（重启/重载）触发 "close of closed channel" panic（telegram/adapter.go:82-85 同）；
  2. `adapter.go:148-161` —— Start 在配置错误/认证失败时返回 nil，平台状态显示 running 实际无连接；
  3. `adapter.go:408-410、280-282、309-321` —— 按字节截断消息文本/日志，UTF-8 中间截断产生乱码（line/message.go:80-95 的 5000 字节 vs LINE 的 5000 字符同类）；
  4. `adapter.go:413、506-507、521-522、554-556` —— `fallbackURLs` 死代码，上传失败的文件组件直接消失；
  5. `api.go:296-321` —— apiRequest 对非幂等 POST（发消息）做最多 3 次重试，可能重复发送。
- 修复：running 用 atomic.Bool；stopCh 用 sync.Once；Start 返回 error；按 []rune 截断；上传失败回退或告警；create 类端点不重试。

## L-37 mattermost：strings.ToLower 可能改变字节长度导致提及切分下标错位

- 位置：`mattermost/adapter.go:394-424`（findMentionSpans 在 lowerText 上取下标，却用其切片原文）。个别 Unicode 字符（如 `İ`）小写后字节长度变化，匹配位置偏移切出错乱文本。修复：ASCII 等长小写映射或等长性断言。

## L-38 mattermost/misskey：发布事件时间戳用 time.Now() 而非已解析的消息时间

- 位置：`mattermost/adapter.go:336 vs 536、546`；`misskey/adapter.go:729、739`。依赖时间戳的逻辑（限速、防 doom loop 判定）对延迟到达的消息产生偏差。修复：透传 abm.Timestamp。

## L-39 qqofficial / qqofficial_webhook：`"data:"` 前缀但无逗号的输入导致 index 越界 panic

- 位置：`qqofficial/adapter.go:843-845`；`qqofficial_webhook/adapter.go:665-667`。`strings.SplitN(fileData, ",", 2)[1]` —— `fileData == "data:"` 时长度为 1，`[1]` panic。pipeline 的 Process 有 recover，但 StreamStart 等其他调用路径无保护。修复：检查 `len(parts) == 2`。

## L-40 QQ / Slack webhook 签名均无时间戳新鲜度校验（重放窗口无限）

- 位置：`qqofficial_webhook/signature.go:51-71`；`slack/adapter.go:719-727`。时间戳参与签名但从不校验新旧。QQ 侧仅有 60 秒事件 id 去重缓解，60 秒后同一捕获请求可重放成功；Slack 侧完全无去重。修复：拒绝 |now−ts| > 5 分钟的请求。

## L-41 slack：readRequestBody 读取请求体无大小限制

- 位置：`slack/client.go:162-176`。手写循环无上限（对比 qqofficial_webhook 用了 4MB LimitReader）；签名校验在读体之后。独立 webhook 服务器（默认 0.0.0.0:3000）可被大报文打爆内存。修复：`io.LimitReader(r.Body, 1<<20)`。

## L-42 webchat：pollClients 通道永不清理

- 位置：`webchat/adapter.go:261-266`。`/poll` 为每个新 session_id 注册一个 cap=10 的 channel，无任何 delete。修复：引用计数清理或 TTL 清扫。

## L-43 weixin_official_account：TokenGuard 并发 refreshAccessToken 无锁

- 位置：`weixin_official_account/adapter.go:102-136`。SDK 每次请求前调用 TokenGuard，并发回复多个用户时 AccessToken/ExpireAt 无锁读写；并发刷新重复请求 gettoken（微信有频率限制且互相作废 token）。修复：sync.Mutex/singleflight 包裹刷新。

## L-44 weixin_oc：Send 静默丢弃 Record（语音）与仅 URL 的图片

- 位置：`weixin_oc/adapter.go:287-339`。switch 无 `*message.Record` 分支（TTS 语音回复被吞）；sendImage/sendFile/sendVideo 在 path=="" 时直接 return nil，URL-only 组件无提示丢失。修复：增加 Record 分支（Upload+SendVoice）；URL 情形下载后再上传或返回明确错误。

## L-45 wecom / wecom_ai_bot 杂项

- 位置与问题：
  1. `wecom/client.go:63-87、133-136` —— access_token 失效（40014/42001）无强制刷新，继续返回坏 token 直至自然到期（最长近 2 小时）；
  2. `wecom_ai_bot/queue_mgr.go:196-220`、`message.go:17-52`、`adapter.go:717-732` —— 无 pendingResponse 的 back queue 永不清理，满 512 条后 Send 永久阻塞；streamPlainCache 同源泄漏；
  3. `wecom/wxcrypt.go:123-137`、`wecom_ai_bot/crypt.go:134-152` —— PKCS7 仅校验末字节；wxcrypt.go:132 长度字段在 32 位平台可为负绕过检查后 panic（入口有验签保护，实际可利用性极低，属加固项）；
  4. `wecom/adapter.go:558-563`、`wecom_ai_bot/server.go:134-139` —— ListenAndServe 在 goroutine 中运行，端口冲突错误仅记日志，Start 总返回 nil；
  5. `wecom/adapter.go:239-274、328-338` —— 应用消息回调内同步下载媒体（语音 30s 超时）易超微信 5s 窗口被重推；无 msgid 去重；
  6. `wecom_ai_bot/api.go:62-71` —— NewWecomAIBotAPIClient 吞掉 EncodingAESKey 校验错误（wxcpt 留空，运行期才以 -40001 暴露）。
- 修复：40014/42001 时清空缓存重试一次；清理孤立 backQueue/streamPlainCache + channel 写入加 select+timeout；校验全部 padding 字节；net.Listen 同步绑定；先回 success 再异步转换 + msgid 去重；构造期校验 AESKey。

## L-46 provider / plugin / 基础设施杂项

- 位置与问题：
  1. `provider/entities.go:89-104` —— ToOpenAIToolCalls 三切片长度不一致时越界（当前无调用方，导出 API 潜雷）；`provider/provider.go:84-88 vs 121` —— Meta() 无锁读 capability；
  2. `provider/sources/retry.go:198-199、223-226` —— 重试耗尽返回"Body 已关闭的 resp + 非 nil error"；`openai_responses_source.go:297-305` —— fallback 用 map 迭代组装 tool calls 顺序随机；`groq_source.go:30-35` —— delete(msg, "reasoning_content") 改写共享会话历史；`xiaomi/longcat` 构造函数改写传入的共享 config map；`sse.go:19、27-49` —— 不做 SSE 规范多行 data 拼接、ctx 参数未用；`stt_source.go:158`、`mimo_common.go:128` —— 音频下载无大小上限；
  3. `plugin/ccompiler.go:299-302、388-393` —— 解压失败（含取消）时误删安装锁，半成品 zig 下次被信任；损坏归档不被清除导致 cgo 安装持续失败；`plugin/compiler.go:294-307` —— findSDKDir 回退依赖系统 go 且用进程 CWD；`plugin/runtime.go:397-421` —— Load/InstallFromSource 不参与 lockOp，与 Uninstall 竞态可"复活"已卸载插件；`plugin/runtime_admin.go:794-799` —— rawRepoDocURLs 的 git@ 解析在特定输入下越界 panic；`plugin/source.go:257-311` —— 归档下载 SSRF 校验不覆盖重定向；`plugin/testbuild.go:19、80-81` —— 全局缓存无锁且产物不清理；
  4. `star/registry.go:227-251` —— GetHandlersByEventType 按 module path 匹配（子进程插件全为常量 "data.plugins"），仅测试引用；`star/filters.go:48-52` —— EventMessageTypeFilter.Match 恒 true；`star/command_descriptors.go:83-94` —— pluginNameFor 归属错误（应优先 h.PluginName）；
  5. `skills/manager.go:473-476` —— skills.json 非原子写；`353-363` —— SetSkillActive 不持锁，与 ListSkills 的自动补全保存互相覆盖；
  6. `lifecycle/lifecycle.go:881-933` —— 孤儿插件清理按 cmdline 子串 "plugins-bin" 匹配，可能误杀无关进程；`lifecycle.go:302` —— dashboard 端口硬编码 6185，配置项 dashboard.port 无效；`lifecycle.go:109-112` —— 配置文件损坏时静默降级为空配置运行；`lifecycle.go:321-328` —— ctx 变量被后台 goroutine 闭包引用后又重赋值（数据竞争）；
  7. `config/config.go:179-188、567-572` —— New/Load 完整性回写非原子、Load 无锁替换 c.data；`log/log.go:246` —— level 无锁读；`ratelimit/ratelimit.go:87-100` —— 空切片不 delete，map 槽位泄漏；`session/waiter.go:212-228` —— TriggerByFilter 递归 RLock 自死锁窗口；`waiter.go:60-73` —— Keep 忽略 resetTimeout 参数；`backup/backup.go:40-74` —— 直接复制运行中的 SQLite（含 WAL）可能撕裂、Walk 中 defer Close 累积 FD、Exporter 全仓库无调用方；
  8. `pkg/message/components.go:240-248` —— Nodes.Clone 丢 ForwardIDs；`components.go:292` —— Json.Clone 浅拷贝共享 Data map；
  9. `agent/mcp_client.go:198-211` —— mcpContentToMap 未覆盖 ResourceLink/ToolUseContent 等类型（输出 Go 结构体 dump）；`pipeline/stages.go:2507-2523` —— 并发 MCP 工具失败触发 Reconnect 风暴；
  10. `t2i/t2i.go:57-65` —— RUnlock 后裸读 templates map（LoadTemplate 当前无生产调用，潜伏）；`t2i/image.go:716-745` —— emojiImage 并发下载非原子写缓存文件，损坏后负缓存永久空白；
  11. `utils/media.go:42、68` —— DownloadToBase64/DownloadFile 用 http.DefaultClient 无超时、无大小上限；`sandbox/manager.go:413-419` —— ReadFile 以哨兵字符串 "__NO_SUCH_FILE__" 判断不存在，可被文件内容误触发；`sandbox/shipyard_neo.go:157-162` —— 失败路径用已取消的 ctx 删除远端沙箱（DELETE 必失败，实例泄漏靠 TTL 回收）。
- 修复方向均已内联注明，按需逐项处理。

---

# 四、修复优先级建议

## P0（几行代码修复 / 防崩溃 / 防数据毁灭）
1. **H-15** webhook 鉴权白名单加 `webhooks`（救活全部 webhook 平台）
2. **H-01** wecom_ai_bot 负数取模（一行，防"一条图片消息崩进程"）
3. **H-02** discord DM Member 判空（防进程崩溃）
4. **H-16** 钉钉 token 扁平解析（救活钉钉回复）
5. **H-18** Slack webhook Start 不阻塞（防启动挂死）
6. **H-14** 插件卸载 dataDir 毁灭（防删库）
7. **H-13** Skill 删除路径校验（防任意目录删除）
8. **M-01** dashboard nil map panic（防 500/DoS 面）

## P1（安全）
9. **H-07** QQ webhook 签名预言机 + **H-12** 微信 msg_signature（防伪造事件）
10. **H-08** STT key 外泄 + **H-09** Gemini key 入 URL（防密钥泄漏）
11. **H-10** web_fetch SSRF + **H-11** grep 越权读 + **M-19** runtime=none 绕过 + **M-20** 任意会话发送（prompt 注入防线）
12. **H-24/M-07/M-08** TOTP 丢失/重置/双写

## P2（平台功能修复）
13. **H-17** Lark 群聊 GroupID、**H-19** LINE 媒体公网 URL、**H-20/H-21/H-22** aiocqhttp 自死锁/reply 缺分支/写串行化、**M-54** webhook 消息链、**M-53** 语音 file_type、**M-45/M-46** wecom 发送与游标

## P3（架构与稳定性）
14. **H-30** 事件总线并发化（全局吞吐）
15. **H-23/M-34/M-17** cron 死循环/run_at 持久化/Job 并发
16. **H-25** 流式 SSE 错误上抛与超时策略
17. **M-37/M-38/M-39** Anthropic/Kimi 工具与多模态
18. 其余中低危按模块批量处理（数据竞争统一加锁、资源泄漏统一清理、吞错误统一上抛）
