# AstrBot 4.29.0 → 4.29.4 变更总结（供 Go 移植参考）

> 对比对象：Python 版上游 `AstrBotDevs/AstrBot`。
> 对比仓库：`/tmp/opencode/AstrBot-git`（浅克隆，含 tag `v4.28.2`、`v4.29.0`~`v4.29.4`；历史连通、可离线 diff）。
> 基线：本仓 `main` 已对齐 **v4.28.2**，故 `4.28.2 → 4.29.x` 的差异即待移植内容。
>
> **行号约定**：文中 `path:行` 为该改动在**新版本**中的行号（取自 `git diff` hunk 头 `@@ -a,b +c,d @@` 的 `c`，即改动块起始行），仅作定位参考。
> 复现命令：
> ```
> cd /tmp/opencode/AstrBot-git
> git log --no-merges --pretty='%h %s' v4.28.2..v4.29.0
> git diff --unified=0 v4.28.2 v4.29.0 -- astrbot      # 仅后端，含行号
> git diff --stat v4.28.2 v4.29.0 -- dashboard/src     # 前端
> ```
> 原始分析产物：`/tmp/opencode/29x/{files,hunks}-<版本>.txt`。

## 总览

| 版本 | 日期 | commits | 文件 | 增/删 | 后端 `astrbot/**` 文件 | 前端 `dashboard/src` | 主题 |
|---|---|---|---|---|---|---|---|
| v4.29.0 | 2026-10-14 | 152 | 555 | +32,909 / −8,303 | 125 | 142 文件 +8,010/−2,919 | Computer Use 角色权限重写、图片链路重构、Provider 强化、WebUI 大改（日志/分片上传/配置校验）、plugin Pages→Views |
| v4.29.1 | 2026-10-17 | 11 | 81 | +4,619 / −615 | 23 | 8 文件 +279/−152 | SerpApi/DeepSeek 网页搜索、会话隔离内存泄漏修复、Computer Use Local/Windows 文件访问修复 |
| v4.29.2 | 2026-10-24 | 27 | 142 | +11,885 / −1,470 | 44 | 45 文件 +2,642/−925 | 消息追踪（有界 SQLite）、诊断包导出、备份补全、Gemini 生成参数、日志/DB 加固 |
| v4.29.3 | 2026-10-24 | 2 | 7 | +37 / −11 | 3 | — | 自更新绕过核心依赖锁 |
| v4.29.4 | 2026-10-31 | 30 | 261 | +17,126 / −1,861 | 95 | 69 文件 +3,672/−1,332 | 可移植消息按钮、Decision Provider + System One、SerpApi Google News、会话/发送者来源选择、删机器人选择性清理、cron 群共享任务、控制台背压 |

---

# v4.29.0（2026-10-14，152 commits，555 文件 +32,909/−8,303）

**主题**：本地电脑使用（Computer Use）从「布尔开关」升级为**基于角色的权限矩阵 + 进程沙箱**；消息图片/多媒体链路重写；Gemini/Anthropic/OpenAI Provider 强化；WebUI 大改（控制台日志搜索、分片上传、配置校验、plugin Pages→Views、侧栏重构、config profile 抽屉）。

## 后端改动

### Computer Use / 权限（本次最大）

| 改动 | 文件:行 | 说明 |
|---|---|---|
| Local 权限核心重写 | `astrbot/core/computer/booters/local.py`（+464/−204，导入自 :3 起） | 接入沙箱与权限矩阵，重构 shell/文件组件 |
| 新增：竞态安全文件打开 | `astrbot/core/computer/local_file_security.py`（新增 :1+187） | `open_file_in_allowed_roots(path, allowed_roots, access=read/write/edit)`，校验真实路径在允许根内 |
| 新增：进程沙箱 | `astrbot/core/computer/process_sandbox/base.py`（新增 :1+250）、`bubblewrap.py`(:1+178)、`seatbelt.py`(:1+214)、`unix.py`(:1+256)、`__init__.py`(:1+43) | `create_process_sandbox()` 按平台选 launcher；`SandboxLimits`（CPU/内存/文件/句柄上限） |
| 权限策略 | `astrbot/core/tools/computer_tools/util.py:88`（+70）、`:169`（+27）、`:263`（+32） | 新增 `LocalPermissionPolicy` / `get_local_permission_policy()` / `check_local_execution_permission()` |
| 文件工具适配 | `astrbot/core/tools/computer_tools/fs.py:87` 起（+217/−67）、`shell.py`（+83/−27）、`python.py`（+34/−5） | 走受限路径/沙箱 |
| Local 模式提示词 | `astrbot/core/astr_main_agent.py:140`（LLM error 扩展）、`:245`（+13） | `_build_local_mode_prompt()`、`_ensure_persona_and_skills()` 调整（PR #9472） |
| 配置默认值 | `astrbot/core/config/default.py:12`（+26）、`:93` | 新增角色化 local 权限默认 |

### 图片 / 多媒体链路

| 改动 | 文件:行 | 说明 |
|---|---|---|
| 媒体工具重写 | `astrbot/core/utils/media_utils.py:72`（+11/−9）、`:381`（+3） | 图片压缩阈值、`describe_media_ref()`、montage 逻辑（+274/−362） |
| 新增 图片输入准备 | `astrbot/core/utils/image_input.py:1+206` | `prepare_request_images()`：准备本地图片 + 生成原始路径提示 |
| 新增 图片子阶段 | `astrbot/core/pipeline/process_stage/method/agent_sub_stages/image_input.py:1+120` | pipeline 接入 |
| Agent 循环 | `astrbot/core/agent/runners/tool_loop_agent_runner.py:1039`（+43）、`:127`/`:290`/`:514`/`:814` | 异步生成器单任务、动画转 montage 提示（+100/−35） |
| 新增 异步生成器工具 | `astrbot/core/utils/async_generator.py:1+92` | `iterate_in_task()` |
| 工具执行 | `astrbot/core/astr_agent_tool_exec.py:150` 起（+118/−66） | 图片/工具链调整 |

### Provider

| 改动 | 文件:行 | 说明 |
|---|---|---|
| 统一 UA | `astrbot/core/provider/headers.py`（新增 +16） | 统一 User-Agent |
| Gemini | `astrbot/core/provider/sources/gemini_source.py:124`（+35）、`:674`（+10/−9） | +140/−67 |
| Anthropic | `astrbot/core/provider/sources/anthropic_source.py:528`（+19） | +64/−7 |
| OpenAI / Responses | `openai_source.py`(+28/−5)、`openai_responses_source.py`(+5) | |
| Embedding 默认 | `gemini_embedding_source.py` | 默认改 `gemini-embedding-001`（PR #10393） |
| Rerank / TTS | `xinference_rerank_source.py`、`azure_tts_source.py`、`fishaudio_tts_api_source.py` | |

### Pipeline / Agent / 工具

- Pipeline：`preprocess_stage/stage.py`(+51/−13)、`process_stage/.../internal.py`(+64/−123)、`respond/stage.py:17`(+21)、`scheduler.py:91`(+12/−11)、`waking_check/stage.py`、`whitelist_check/stage.py`、`rate_limit_check/stage.py`。
- Agent 上下文：`agent/context/manager.py`(+41/−6)、`compressor.py`、`token_counter.py`；`agent/tool.py`；`astr_main_agent_resources.py`；`astr_agent_run_util.py`。
- 新增 `astrbot/core/llm_error_messages.py`（+36，LLM 请求失败多语言文案）。
- 工具：`tools/cron_tools.py`(+66/−5)、`tools/message_tools.py`(+9/−7)。
- 核心：`star/base.py:66`(+32)、`star/context.py`(+43/−7)、`star/updater.py`(+32)、`star/filter/command_group.py`(+10，命令组名整词匹配 #10372)、`repo/db/sqlite.py`、`cron/manager.py:235`、`persona_mgr.py`(+81)、`file_token_service.py`、`event_bus.py`、`log.py`(+81)、`message/components.py`。

### 平台适配器
`lark/lark_adapter.py`(+68/−18)、`slack/{slack_adapter,slack_event}.py`(+54/−37, +147/−45)、`qqofficial/{message_event,platform_adapter}.py`(+54, +27/−9)、`satori/satori_adapter.py`、`telegram/tg_adapter.py`、`discord/*`、`weixin_oc_adapter.py`、`aiocqhttp_platform_adapter.py`。

### Dashboard 后端（`astrbot/dashboard/**`）
`server.py`（新增 +119，自建 hypercorn 服务）、`static_encoding.py`（新增 +76，gzip 静态资源）、`services/chunked_upload_service.py`（新增 +327，分片上传会话）、`services/config_service.py:210`(+21)、`:329`(+92)（校验重写）、`services/plugin_page_service.py`(+116/−329，Pages→Views）、`services/chat_service.py`(+199/−14)、`services/stat_service.py`(+68)、`services/backup_service.py`(+88/−154)、`api/plugins.py`(+83/−104)、`api/files.py`(+89/−5)、`api/backups.py`(+51/−18)、`api/static_files.py`、`plugin_page_auth.py`。

### 其他后端
`cli/utils/plugin.py`(+7/−23)、`builtin_stars/astrbot/main.py`、`builtin_stars/builtin_commands/commands/conversation.py`(+34/−6)。

## 前端 `dashboard/src`（142 文件 +8,010/−2,919）
侧栏重构 `layouts/full/vertical-sidebar/VerticalSidebar.vue`(+626/−151)、`NavItem.vue`(+308/−31)；`components/conversation/ConversationHistoryPreview.vue`(+588，会话分页)；`components/shared/ConsoleDisplayer.vue`(+560，日志搜索高亮)；`components/chat/ProjectDialog.vue`(+446)、`Chat.vue`(+443/−70)；`components/shared/LocalPermissionMatrix.vue`(+374)；`composables/useChunkedUpload.ts`(+264)；`components/extension/McpServersSection.vue`(+267)。

## 其他
docs 大量更新；tests 增加；`.github`、`requirements`、`pyproject`。commit 900a700b「match command group names as whole words」即 `star/filter/command_group.py`。

---

# v4.29.1（2026-10-17，11 commits，81 文件 +4,619/−615）

**主题**：新增 SerpApi / DeepSeek 网页搜索；修复开启「会话隔离」时的内存泄漏；修复 Computer Use Local 权限保存与 Windows 工作区文件访问；插件 list 配置项新增本地化 `subtitles`。

## 后端改动

| 改动 | 文件:行 | 说明 |
|---|---|---|
| 新增 网页搜索工具集 | `astrbot/core/tools/web_search_tools.py`（新增 +681） | `_brave_search`(:450+445)、`AnySearchWebSearchTool`(:1908+212)、`WEB_SEARCH_TOOL_NAMES`(:28+6)、`_ANYSEARCH_*`；新增 Brave/DeepSeek/SerpApi/AnySearch（PR #10599/#10601） |
| 接入搜索工具 | `astrbot/core/astr_main_agent.py:102`(+2)、`:108`(+4)、`:171`(+5，`WEB_SEARCH_CITATION_TOOL_NAMES`)、`:1315`(+8，`_apply_web_search_tools`) | |
| 会话隔离内存泄漏修复 | `astrbot/core/event_bus.py:71`(+22) | `EventBus` 记录会话并在 pipeline 注册前完成隔离（PR #10581） |
| 隔离相关瘦身 | `astrbot/core/pipeline/waking_check/stage.py`（−17/…，移除 `UmoAutoNameRecorder` 引用） | |
| Local 权限修复 | `astrbot/core/tools/computer_tools/util.py:141`(+59)、`:263`(+32) | `LocalPermissionPolicy`、`check_local_execution_permission` 语义调整（#10584） |
| Windows 文件访问 | `astrbot/core/computer/local_file_security_windows.py`（新增 +213） | 句柄相对访问、拒绝重解析点/硬链接别名 |
| 沙箱 | `astrbot/core/computer/process_sandbox/__init__.py:42`(+50) | |
| 文件探测 | `computer/file_read_utils.py:295`(+3)、`computer/local_file_security.py:61`(+17)、`tools/computer_tools/fs.py:893`(+5) | |
| Skills 健壮性 | `astrbot/core/skills/skill_manager.py:348`(+9)、`:577`(+20)、`:641`(+9) | 跳过不可读技能目录（#10591） |
| Edge TTS | `provider/sources/edge_tts_source.py`（−5） | 移除自定义 UA 设置，保留原生客户端 UA（#10588） |
| Telegram | `platform/sources/telegram/tg_adapter.py:470`(+11)、`:593`(+27)、`:653`(+5) | 保留隐藏链接（#10557） |
| Discord | `platform/sources/discord/discord_platform_event.py:308` | 全体提及修正（#10563） |
| Config | `config/default.py:3686`(+22)、`:3787`(+22)（list `subtitles`）；`config/i18n_utils.py:72`(+4) | |
| 迁移 | `utils/migra_helper.py:331`(+24) | |
| Dashboard | `services/config_service.py`（−57，移除部分校验）；`services/stat_service.py`（−46，移除 umo alias 冗余） | |
| 其他 | `core/core_lifecycle.py`、`dashboard/services/chat_service.py:279`(+5, `extract_web_search_refs`)、`live_chat_service.py:290`(+5) | |

## 前端（8 文件 +279/−152）
`components/shared/LocalPermissionMatrix.vue`(+91/−124)；`i18n/locales/{zh-CN,en-US,ja-JP,ru-RU}/features/config-metadata.json`（各 +39）；`components/shared/ConfigItemRenderer.vue`(+25/−23，渲染 list subtitles)。

---

# v4.29.2（2026-10-24，27 commits，142 文件 +11,885/−1,470）

**主题**：消息追踪（独立有界 SQLite 历史）；系统诊断包导出（脱敏）；备份补全定时任务/持久化文件；Gemini 生成参数；日志与 DB 加固；Shipyard Bay exec 解包、Windows shell 子进程清理。

## 后端改动

| 改动 | 文件:行 | 说明 |
|---|---|---|
| 新增 trace 存储 | `astrbot/core/utils/trace_store.py`（新增 +676） | 独立、有界 SQLite trace 历史（PR #10614） |
| Trace 改造 | `astrbot/core/utils/trace.py:155`（+300 区段） | `TraceSpan` 重构（+419/−42） |
| 新增 telemetry | `astrbot/core/utils/telemetry.py`（新增 +112） | span/log 的 bounded 属性（`bounded_text`，上限 16KB） |
| 新增 诊断服务 | `astrbot/dashboard/services/diagnostic_service.py`（新增 +418） | 采集+脱敏+序列化（#10418/#10659） |
| 日志服务 | `astrbot/dashboard/services/log_service.py:98`（+300）、`:405`(+39) | 日志导出/SSE |
| 日志 API | `astrbot/dashboard/api/logs.py:87`(+26)、`:121`(+45) | trace 设置等 |
| 备份模型补全 | `astrbot/core/backup/constants.py:50`/`:52`/`:64`(+7) | 新增 CronJob 等（#10644/#10661） |
| 备份导出/导入 | `backup/exporter.py:168`(+5)、`backup/importer.py:475`(+16)、`:552`(+4) | |
| 日志重构 | `astrbot/core/log.py:253`(+8)、`:270`(+35)、`:326`(+36) | `LogBroker`/`LogQueueHandler` 重构、丢弃统计 |
| Cron | `astrbot/core/cron/manager.py:466`(−65…)、`:544`(+39) | 动态 job context 移到对话历史后（#10549） |
| DB 可移植查询 | `astrbot/core/db/sqlite.py:386`(+2)、`:404`、`:418`、`:690` | 跨库兼容 JSON 查询（#10664） |
| 迁移 | `astrbot/core/utils/migra_helper.py:315`(+27) | |
| Gemini | `provider/sources/gemini_source.py:209`(+16)、`:326`(+40) | 生成参数保留零值、非字符串枚举（#10719/#10635） |
| NVIDIA rerank | `provider/sources/nvidia_rerank_source.py:46`、`:55`(+8) | 保留模型专属端点（#10732） |
| Shipyard Bay | `computer/booters/shipyard.py:27`(+36) | exec envelope 解包（#10671） |
| Windows 子进程清理 | `computer/booters/local.py:397` 起(+48)、新增 `computer/windows_job.py`(+157) | Job Object 管理进程树（#10729） |
| 事件钩子扩展 | `pipeline/context_utils.py:96`(+18)、`:122`(+25) | `call_event_hook` 扩展 |
| 请求子阶段 | `pipeline/process_stage/method/agent_request.py:52`(+29)、`star_request.py:50`(+23)、`:77`(+11)、`:95`(+12) | |
| Respond 阶段 | `pipeline/respond/stage.py:254`(+55) | |
| 调度/唤醒 | `pipeline/scheduler.py`(14/14)、`waking_check/stage.py:240`(+9) | |
| 事件基类 | `platform/astr_message_event.py:39`(+39)、`:133`(+8)、`:541`(+13) | 记录 sender/umo |
| Star Context | `core/star/context.py:685`(+18) | |
| ToolSet | `core/agent/tool.py:307`(+19) | 拷贝语义 |
| 存储清理 | `core/utils/storage_cleaner.py:112`(+21)、`:192`(+7)、`:202`(+37)、`:251`(+9) | 清理无效图片、目录（#10090） |
| 其他 | `builtin_stars/astrbot/main.py`(−15/…)、`core_lifecycle.py`、`backup/importer.py`、`schemas.py` | |

## 前端（45 文件 +2,642/−925）
`components/shared/TraceDisplayer.vue`(+1126/−570)；`components/shared/LogExportButton.vue`(+200)；`i18n/.../features/trace.json` ×4（各 +125~129）；`i18n/.../features/config-metadata.json` ×4。

---

# v4.29.3（2026-10-24，2 commits，7 文件 +37/−11）

**主题**：自更新绕过核心依赖锁。

| 改动 | 文件:行 | 说明 |
|---|---|---|
| pip 安装器 | `astrbot/core/utils/pip_installer.py:1027`(+16)、`:1070`(+5) | 自更新时绕过核心依赖锁（#10736） |
| 更新服务 | `astrbot/dashboard/services/update_service.py:220`(+4) | 配套 |
| 版本 | `astrbot/__init__.py:3` → 4.29.3 | |

---

# v4.29.4（2026-10-31，30 commits，261 文件 +17,126/−1,861）

**主题**：跨平台可移植**消息按钮**组件；新增**决策模型**（Decision Provider + System One 适配器）；SerpApi Google News；会话/发送者来源选择器；删机器人可选清理数据；cron 群共享任务 + 管理员删除；控制台背压修复；SILK `0x03`；Anthropic 兼容 API Key 头；DashScope 超时。

## 后端改动

### 消息按钮（大改，跨多平台）
| 改动 | 文件:行 | 说明 |
|---|---|---|
| 新增 按钮交互载荷 | `astrbot/core/platform/button_interaction.py`（新增 +203） | 回调载荷编解码、过期提示 `BUTTON_ACTION_EXPIRED_MESSAGE`、SQLite 持久化（PR #9809） |
| 组件定义 | `astrbot/core/message/components.py:71`(+3，`ComponentType`)、`:148`(+114) | 新增按钮组件（+140） |
| 事件基类 | `astrbot/core/platform/astr_message_event.py:236`(+18) | 按钮回调接入 |
| Star 过滤器 | `astrbot/core/star/filter/button_action.py`（新增 +26，`ButtonActionFilter`）；`star/register/star_handler.py:1+24`；`star/register/__init__.py` | 插件可注册按钮动作 |
| 各平台适配 | aiocqhttp `aiocqhttp_message_event.py`(+26)；discord `components.py`(+66)、`discord_platform_adapter.py`(+85/−8)、`discord_platform_event.py`；kook `kook_adapter.py`(+101)、`kook_event.py`(+69)；lark `lark_adapter.py`(+122)、`lark_event.py`(+140/−51)；line `line_adapter.py`(+43)、`line_event.py`(+59)；mattermost `client.py`(+95)、`mattermost_adapter.py`(+144)；qqofficial `qqofficial_message_event.py`(+189/−41)、`qqofficial_platform_adapter.py`(+144)；satori `satori_adapter.py`(+91)、`satori_event.py`(+98)；slack `slack_adapter.py`(+99)、`slack_event.py`(+54)；telegram `tg_adapter.py`(+94)、`tg_event.py`(+88)；webchat `message_parts_helper.py`(+86)；wecom `wecom_adapter.py`(+74)、`wecom_event.py`(+98)；wecom_ai_bot `wecomai_adapter.py`(+169)、`wecomai_event.py`(+127)、`wecomai_buttons.py`(+76)、`wecomai_api.py`(+34)、`wecomai_webhook.py`(+22)；weixin_oc `weixin_oc_adapter.py`(+35)；weixin_official `weixin_offacc_event.py`(+31) | |

### 决策模型（Decision Provider）
| 改动 | 文件:行 | 说明 |
|---|---|---|
| 新增 决策抽象 | `astrbot/core/provider/decision.py`（新增 +177） | `DecisionState` 等（PR #10775） |
| 新增 System One 适配 | `astrbot/core/provider/sources/system_one_decision_source.py`（新增 +349） | System One 适配器 |
| 类型定义 | `astrbot/core/provider/entities.py:40`(+98) | `DecisionAnswer`/`DecisionBoolean[Question|Answer]`/`DecisionChoice[Question|Answer]`/`DecisionError`/`DecisionQuestion` 等 |
| Provider 管理 | `astrbot/core/provider/manager.py:74`(+2)、`:227`(+12) | 选择/调用决策 provider（+127/−43） |
| Provider 基类 | `astrbot/core/provider/provider.py`(+47/−1) | |
| API 暴露 | `astrbot/api/provider/__init__.py`(+22)、`astrbot/api/all.py`(+1)、`astrbot/api/event/filter/__init__.py`(+6) | 插件可调用 |

### 网页搜索
- `astrbot/core/tools/web_search_tools.py:608`(+102)：`SerpApiWebSearchTool` 接入 Google News（#10773）。

### 数据库（按钮回调 + cron 群共享）
- `astrbot/core/db/sqlite.py:2335`(+107)；`astrbot/core/db/__init__.py:897`(+40，新抽象接口)；`astrbot/core/db/po.py:369`(+13，`UmoAlias` 等)。

### Cron 群共享任务
- `astrbot/core/cron/manager.py:232`(+27)、`:393`(+34)（PR #10785）；`astrbot/core/tools/cron_tools.py:302`(+9)、`:326`(+2) 等（+44/−26）。

### 会话 / 发送者来源
- `astrbot/core/pipeline/waking_check/umo_auto_name.py:52`(+30)、`:95`(+6)、`:102`(+8)（记录 sender 信息，PR #10747）；`pipeline/waking_check/stage.py:240` 附近 (+31/−4)；`utils/active_event_registry.py`(+13)。

### 日志背压 / 稳定性
- 新增 `astrbot/core/utils/console_sink.py`（+176，`BoundedConsoleSink`，守护线程有界写入）；`astrbot/core/log.py:411` 起(+21/−4)（PR #10902）。

### Provider 修复
- `astrbot/core/provider/sources/anthropic_source.py`(+7/−1)：保留 API Key 请求头（#10787）。
- `astrbot/core/agent/runners/dashscope/dashscope_agent_runner.py`(+3)：尊重 timeout（#10868）。
- `astrbot/core/provider/sources/request_retry.py`(+1/−1)。
- `astrbot/core/utils/tencent_record_helper.py`(+2/−2)：SILK `0x03` 前缀（#10744）。

### 其他核心
- `astrbot/core/skills/skill_manager.py`(+4/−3)：优先文件读取工具（#10789）。
- `astrbot/core/config/default.py`(+95/−11)：新配置项。
- `astrbot/core/utils/astrbot_path.py:92`(+16)：避免与 `.astrbot` 标记文件在系统临时目录冲突（#10784）。
- `astrbot/core/utils/pip_installer.py`(−11/+3)；`utils/trace_store.py`(+28/−13)。
- `astrbot/core/core_lifecycle.py`(+7)；`astrbot/core/backup/constants.py`(+2)。
- `astrbot/core/platform/sources/dingtalk/dingtalk_adapter.py`(+279)、`dingtalk_event.py`(+16)；新增 `misskey/misskey_utils.py`(+27)；`qqofficial_webhook/qo_webhook_adapter.py`(+13)。
- 新增 群信息：`lark/lark_group_info.py`(+99)、`qqofficial/qqofficial_group_info.py`(+91)。

### Dashboard 后端
| 改动 | 文件:行 | 说明 |
|---|---|---|
| 新增 机器人清理 | `astrbot/dashboard/services/bot_cleanup_service.py`（新增 +150） | 可选清理配置/定时/会话/消息/追踪/用量（#10761） |
| 统计服务 | `astrbot/dashboard/services/stat_service.py:573`(+239) | 大量扩展（+264/−5） |
| 会话管理 | `astrbot/dashboard/services/session_management_service.py`(+73/−9) |
| ChatUI 项目 | `astrbot/dashboard/services/chatui_project_service.py`(+42/−2) |
| API | `api/sessions.py`(+30)、`api/stats.py`(+32)、`api/bots.py`(+5/−3)、`api/chat_projects.py`(+8/−2) |
| 配置/其他 | `services/config_service.py`(+68/−10)、`services/chat_service.py`(+21/−3)、`services/live_chat_service.py`(+7/−4)、`schemas.py`(+16)、`diagnostic_service.py`(+2) |

## 前端（69 文件 +3,672/−1,332）
`components/shared/UmoSelector.vue`(+478)、`SenderSelector.vue`(+439)、`PypiMirrorSelector.vue`(+348)、`ProxySelector.vue`(+271/−75)、`components/platform/DeletePlatformDialog.vue`(+190)、`components/chat/ButtonActionRow.vue`(+176)、`TraceDisplayer.vue`(+158/−22)、`views/ProviderPage.vue`(+116/−15)、`UmoDisplay.vue`(+115/−57)、`styles/config-form.css`(+103)。

## 其他
docs 更新；`.github`；`Dockerfile`、安装脚本（多语言 + 大陆镜像，源自 #10629）。

---

## 移植优先级建议（初稿）

1. **接口/行为级（影响插件生态，建议优先）**：消息按钮（`button_interaction` + `message/components` + `star/filter/button_action`）、Decision Provider（`provider/decision` + `entities` + `provider.py`）、`astr_message_event` 的 sender/umo 记录、`db` 新增接口。
2. **平台适配**：按钮 / 群信息在各 platform/sources 的落地，需按 Go 端已支持平台裁剪。
3. **Core 修复**：会话隔离（event_bus）、控制台背压（console_sink）、日志 trace（trace/trace_store/telemetry）、DB 可移植 JSON 查询、cron 群共享、备份补全。
4. **Provider 修复**：Gemini 生成参数、Anthropic API Key 头、DashScope 超时、NVIDIA rerank 端点、SILK 0x03。
5. **前端**：`dashboard/src` 需按 commit 语义迁移（本仓前端已自定义，禁止整目录覆盖）。✅ **已完成**：以三方合并方式迁移，保留本仓认证/插件/市场自定义，详见文末「前端完整迁移」。

> 注：本文件为**上游变更梳理**。落地前需按 `memory.md` 约定先确认方案。

---

# 移植进度

## ✅ 消息按钮（4.29.4，优先级 1）——宿主核心 + aiocqhttp 已移植

| 上游文件 | 本仓对应改动 |
|---|---|
| `message/components.py`（ComponentType + Button/ActionRow/ButtonInteraction） | `pkg/message/components.go`：新增 `CompButton/CompActionRow/CompButtonInteraction`、`JSONValue`、`ButtonStyle`、`ButtonAction(Type)`、`Button`、`ActionRow`、`ButtonInteraction`（含 `ToDict()`、`ActionExpiresAt()`） |
| `platform/button_interaction.py` | `internal/platform/button_interaction.go`（新增）：`ButtonCallbackPrefix`、`ButtonActionExpiredError`、`EncodeButtonCallback`/`DecodeButtonCallback`、blake2s-128 令牌、内存缓存 + `ButtonCallbackStore` 持久化、过期语义 |
| `platform/astr_message_event.py:236` | `internal/core/event_bus.go`：`(*Event).IsButtonInteraction/GetButtonInteraction`；`internal/platform/platform.go`：`(*AstrMessageEvent)` 同名方法 |
| `star/filter/button_action.py` | `internal/star/filters.go`：`ButtonActionFilter`；`FilterContext.ButtonActionID`（`command.go`） |
| `star/register/star_handler.py`、`register/__init__.py` | `internal/star/registry.go`：`(*StarHandlerRegistry).AppendButtonAction`（对齐 `register_button_action`，EventType=Filter） |
| pipeline 分发 | `internal/pipeline/stages.go`：`findMatchingHandlers` 从 `event.GetButtonInteraction()` 注入 `FilterContext.ButtonActionID` |
| 持久化（upstream 自建表） | `internal/db/database.go`：`button_callbacks` 表 + `SaveButtonCallback`/`GetButtonCallback`；`internal/lifecycle/lifecycle.go`：DB 打开后 `platform.ConfigureButtonCallbackStore(database)` |
| aiocqhttp 适配 | `internal/platform/sources/aiocqhttp/adapter.go`：`convertToCQFormat` 新增 `ActionRow`/`Button` → 文本降级（URL 显示链接、回调标注不可用） |

**按钮端到端（4.29.4）——SDK 侧**
- 用现有 proto 通用字段承载（**不改 proto**）：`ButtonInteraction` → `Component{type=ButtonInteraction, id=action_id, text=interaction_id, data={data,source_message_id,callback_data}}`。
- 宿主：`internal/star/subprocess_bridge.go` `messageToSDKComponentsDepth` 新增 `ButtonInteraction`/`Button`/`ActionRow` 序列化。
- Go SDK（`astrbot-go-plugin-sdk`，commit `93c607f`）：新增 `CompButton/CompActionRow/CompButtonInteraction` 常量 + `Event.GetButtonInteraction/IsButtonInteraction`（转换本就通用，Data 自动透传）。
- Python SDK（`astrbot-golang-plugin-python-sdk`，commit `b6ade01`）：`components.py` 新增 `Button/ActionRow/ButtonInteraction` 与 `ComponentType`；`_bridge/serialize.py` `component_from_proto`/`component_to_proto` 映射；`astr_message_event.py` 新增 `is_button_interaction/get_button_interaction`。
- **已发布消费**：Python SDK 已发版 **v0.14.3**（`b6ade01` 功能 + `94e072f` bump）；Go SDK 已发版 **v2.0.5**（`93c607f`）。宿主 `go.mod` 升 `astrbot-golang-plugin-python-sdk v0.14.3` + `Astrbot-go-plugin-sdk/v2 v2.0.5`，`internal/pysdk/version.go` `SDKVersion="0.14.3"`、`embed.go` 登记 tarball 哈希，按钮可被插件实际消费。

**本次未做（按约定裁剪，待后续）**：其余平台适配器（discord/kook/lark/line/mattermost/qqofficial/satori/slack/telegram/webchat/wecom/wecom_ai_bot/weixin_oc/weixin_official）的按钮落地与入站 `ButtonInteraction` 解析；Go/Python 插件 SDK 的 `register_button_action` 与按钮组件序列化。

## ✅ Decision Provider（4.29.4，优先级 1）

| 上游文件 | 本仓对应改动 |
|---|---|
| `provider/entities.py`（ProviderType.DECISION + 决策问答类型 + DecisionError） | `internal/provider/entities.go`：`CapDecision`、`DecisionChoice/Boolean/ScoreQuestion`、`DecisionChoice/Boolean/ScoreAnswer`、`DecisionQuestion/Answer` 接口、`DecisionState`、`DecisionError` |
| `provider/provider.py`（DecisionProvider 抽象基类） | `internal/provider/provider.go`：`DecisionProvider` 接口 + `ProviderManager` 的 `decisionID`/`GetDecisionProvider`/`SetDefaultDecisionProvider` |
| `provider/decision.py`（DecisionService） | `internal/provider/decision_service.go`（新增）：`DecisionService.Ask/Choose/YesNo` |
| `provider/sources/system_one_decision_source.py` | `internal/provider/sources/system_one_decision_source.go`（新增）+ `register_decision.go`（注册 `system_one_decision`）；含并发闸门/总超时/重试/严格答案校验 |
| `provider/manager.py` 决策选择/默认持久化 | 已提供 manager 层选择接口；**运行时默认持久化键 `curr_provider_decision` 与 dashboard/插件 API 暴露待接线（SDK 范畴）** |

## ✅ 4.29.0/4.29.1/4.29.2/4.29.4 零散修复

- **4.29.0 命令组整词匹配**：`internal/star/command.go` 新增 `collapseSpaces`，`CommandFilter.Match`/`CommandGroupFilter.Match` 先归一空白再整词匹配。
- **4.29.1 Edge TTS UA**：`internal/provider/sources/edge_tts_source.go` 移除自定义 `User-Agent` 覆盖，保留 SDK 原生 UA。
- **4.29.2 NVIDIA Rerank 端点**：`internal/provider/sources/nvidia_rerank_source.go` 仅对三个旧模型做 `.`→`_` 替换。
- **4.29.4 SILK 0x03**：`internal/utils/silk.go` 同时剥离 `0x02`/`0x03` 前缀。
- **4.29.4 会话/发送者来源**：`internal/db/database.go` 新增 `umo_senders` 表 + `UpsertUmoSender`/`GetUmoSenders`；`internal/pipeline/umo_auto_name.go` 记录器同时维护发送者名称队列，flush 时调用 `UpsertUmoSender`。

## ✅ 4.29.1/4.29.2/4.29.4 第二批（本次新增）

- **4.29.4 控制台背压**：新增 `internal/log/console_sink.go`（`BoundedConsoleSink`，单 goroutine + 有界队列 + 丢弃计数 + Drain/Close/GetStatus）；`internal/lifecycle/lifecycle.go` 启动时 `SetOutput(sink)`、停止时 `Close()` flush。
- **4.29.1/4.29.4 网页搜索（SerpApi + DeepSeek）**：新增 `internal/pipeline/websearch_serpapi_deepseek.go`：
  - SerpApi `web_search_serpapi` / `news_search_serpapi` / `image_search_serpapi` / `finance_search_serpapi` / `scholar_search_serpapi`（GET `serpapi.com/search.json`，多 key 失败切换、`search_metadata.status` 判定、key 脱敏）。
  - DeepSeek `web_search_deepseek` / `deepseek_fetch_web_page`（POST `api.deepseek.com/v1/tools/{search,fetch}`）。
  - 接线：`internal/pipeline/stages.go`（collectTools `case "serpapi"/"deepseek"` + 分发 + 工具权限集合）、`internal/dashboard/handlers.go`（builtinToolNames）、`internal/config/default_config.go`（`websearch_serpapi_key`/`websearch_deepseek_key`）。
  - **前端 WebUI 的 provider 下拉/i18n 元数据**：已随前端 `dashboard/src` 迁移带入（见「前端完整迁移」）。
- **4.29.4 skills 提示词**：`internal/skills/manager.go` buildSkillsPrompt 改为“优先文件读取工具 / 兼容 shell”措辞（对齐 4.29.4）。
- **4.29.2 telemetry**：`internal/log/telemetry.go`（`BoundedText`/`BoundedAttributes`，UTF-8 字节预算 + 截断标记）；`TraceSpan.Record` 字段有界化。
- **4.29.2 trace 存储**：`internal/tracestore/trace_store.go`（独立有界 SQLite：`traces` 表、后台批量写、丢弃计数、`ClearHistory`/`ListTraces`/`GetTrace`）；`internal/log` 新增 `TraceSink` 接口 + `SetTraceSink`，`PublishTrace` 非阻塞落库；lifecycle 在 `trace_enable` 时打开、停机关闭；新增配置 `trace_db_path`。
- **4.29.2 存储清理**：`internal/storage/cleaner.go`（logs/cache/traces 三目标，保护活动日志与 SQLite 文件，traces 走 trace 存储 `ClearHistory`）；`internal/dashboard/storage.go` 暴露 `GET /api/v1/storage/status`、`POST /api/v1/storage/cleanup`；router 增加 `storage`（systemOnly）；lifecycle 构造并注入。

- **4.29.0 cron `edit` 细分错误文案**：`internal/pipeline/cron_tools.go` edit 非创建者时按“同会话无成员创建者（dashboard/legacy）/同群他人创建/非本人创建”给出区分文案（对齐 py 4.29.0）；测试断言同步。
- **4.29.4 cron 群共享任务**：`internal/pipeline/cron_tools.go`：群聊 `list` 列出本会话全部任务并附 `creator`（私有会话保持发送者隔离 + hidden note）；`delete` 允许任务创建者或 AstrBot 管理员（同会话群聊）删除；schema 描述同步；`stages.go` 传入 `isGroup/isAdmin`。
- **4.29.2 事件接收 trace**：`stages.go` 创建 trace span 时记录 `event_received`（kind/platform/message_type/message 概要）。
- **4.29.4 lark 群信息**：`internal/platform/sources/lark/adapter.go` 新增 `GetGroupInfo`（`Im.Chat.Get`：名称/头像/群主/人数）。
- **4.29.3 pip 自更新绕过依赖锁**：Go 宿主不通过 pip 自更新自身，**N/A**。

- **4.29.0 Computer Use 角色权限矩阵**：`internal/pipeline/config.go` 新增 `computer_use_local_permissions`；`computer_tools.go` 新增 `LocalPermissionPolicy`/`localPermissionPolicy`/`localToolDenied`；`stages.go` 在 Local 运行时按角色策略校验文件/执行工具。
- **4.29.0 OS 进程沙箱**：新增 `internal/procsandbox/`（`Limits`/`Spec`/`RunResult`/`TimeoutError`；Linux bubblewrap `BuildCommand`（workspace/host 作用域、网络开关、只读系统目录、可写工作区、`--unshare-all/--die-with-parent`）；macOS Seatbelt profile；资源限制经 POSIX `sh` ulimit 包装；`CreateProcessSandbox`/`GetProcessSandboxStatus` 探测）。接线：`localToolDenied` 在沙箱可用时放行；`astrbot_execute_shell`/`astrbot_execute_python` 在 policy 需要隔离时改走沙箱执行（`executeSandboxedShell/Python`）；`/logs/export` environment 附带沙箱状态。**平台差异**：`sh` ulimit 替代 py 的 `resource.setrlimit`；Seatbelt 未做 `otool` 依赖探测。
- **4.29.2 storage_cleaner**：`internal/storage/cleaner.go` + `dashboard/storage.go`（`/api/v1/storage/status|cleanup`）。见上。
- **4.29.4 cron 群共享 / lark 群信息 / event_received trace**：见上。
- **4.29.2 Gemini 枚举字符串化**：`internal/provider/sources/gemini_source.go` `geminiConvertSchema` 按 Gemini 约束处理 `enum`（STRING 保留并 JSON 化非字符串值；其它类型把允许集合写入 `description` 并移除 `enum`；含 null 时置 `nullable`）。
- **4.29.2 事件/钩子 trace**：`internal/pipeline/stages.go` 新增 `ensureEventTrace`（管线入口建 span + 记 `event_received`）与 `traceHook`（子进程钩子 `handler_start`/`handler_end`，含 plugin/handler/hook/status）；`callLLMAgent` 改为复用已有 span 并始终记录 `astr_agent_prepare`。
- **4.29.0 Dashboard 静态 gzip**：`internal/dashboard/server.go` 新增 `writeWebStatic`（Accept-Encoding 判定 + 可压缩扩展名 + BestSpeed gzip + 嵌入式产物压缩缓存 + `Vary: Accept-Encoding`），替换两处静态写入（外部目录不缓存、embed 缓存）。
- **4.29.2 trace 历史 API**：`internal/dashboard/handlers.go` `handleTrace` 新增 `GET /api/v1/trace/history`（分页列出持久化 trace，复用 `tracestore.ListTraces`）与 `GET /api/v1/trace/{span_id}`（`GetTrace` 明细）；`server.go` 新增 `traceStore` 字段 + `SetTraceStore`，lifecycle 注入。
- **4.29.2 诊断/日志导出**：`internal/dashboard/diagnostic.go` 新增 `GET /api/v1/logs/export?redact=`（application/zip：`config.json` 脱敏 + `environment.json` + `logs.txt` 脱敏）；`diagnosticRedactor`（按敏感键收集凭据 + 文本脱敏：ANSI/私钥/Bearer|Basic|ApiKey/Cookie/URL 内凭据/**token 模式**）。**未移植**：IP 字面量别名、自由赋值扫描（RE2 无 lookbehind）。
- **4.29.0 插件 Pages→Views（向后兼容）**：`internal/dashboard/plugin_pages.go` `pluginPageRoot` 优先 `views/` 目录、回退 `pages/`；`internal/plugin/metadata.go` `PluginMetadata` 新增 `Views` 字段，`validateMetadata`/YAML 解析对 `views`/`pages` 互相回填（views 优先）。前端迁移后入口走 `GET /api/v1/plugins/view`（复用 `handlePluginPage`），插件列表的 `pages` 元数据由 `runtime_admin.go` 输出。
- **4.29.0 通用分片上传**：`internal/dashboard/chunked_upload.go` 新增 `/api/v1/files/upload/{init,chunk,complete,abort,status}`（会话+分片落盘 `data/webui_files/.chunks/<id>`，合并写入 WebUI 文件存储并写元数据；含过期清理方法）；`handleFiles` 的 `upload` 分支接线；`Server` 新增 `chunkUploads`/`chunkUploadsMu`。
- **4.29.4 ghproxy 吞吐测速**：`internal/dashboard/stats_net.go` 新增 `POST /api/v1/stats/ghproxy/speed`（下载 shiki 运行时资产，4MiB/6s 采样，返回 speed/bytes/duration）；`server.go` ghproxy 分发支持 `test|speed`。
- **4.29.4 Dashboard API 增量**：`internal/dashboard/handlers.go` 新增 `GET /api/v1/sessions/senders`（`handleSessionSenders`，复用 `db.GetUmoSenders`：search/limit/page/sender_ids）；`/sessions/active-umos` 支持 `platform_id` 过滤（`server.go getActiveUMOs(platformID)`）。`internal/dashboard/stats_net.go` 新增 `POST /api/v1/stats/pypi-index/test`（连通性/延迟）与 `/stats/pypi-index/speed`（下载真实 wheel，8MiB 采样限速），复用 `validateOutboundURLStrict`/`newOutboundClientStrict` 防 SSRF。
- **4.29.2 `response_sent` trace**：`internal/pipeline/stages.go` RespondStage 在真正发送后记录 `response_sent`（流式 `streaming=true`，含消息概要），对齐 py respond/stage.py。scheduler 的 `event_context`/`trace_execution` 在 Go 侧等价为 `event_bus.go` 的 `ActiveRegistry` + `defer event.CleanupTemporaryFiles()`（N/A）。
- **4.29.4 删机器人选择性清理**：`internal/db/database.go` 新增 `CleanupBotData(botID, targets)`（cron_jobs/preferences/custom_rules/conversations/message_history/umo_aliases/umo_senders/stats，按实例前缀）；`internal/dashboard/handlers.go` `deleteBot` 支持重复 `cleanup` 查询参数，清理失败则保留配置。**traces/routing 目标未做**（Go trace 存储无 platform 维度）。
- **N/A（Go 无需或属插件 SDK）**：4.29.0 图片链路 `prepare_request_images`（Go `image_prepare.go` 已有）、`utils/async_generator`（py asyncio 专用）、4.29.2 `log.py` 重构（py 跨事件环订阅者绑定 + OTel trace_id；Go 为 channel+丢弃模型）、4.29.4 `astrbot_path` `.astrbot` 临时目录冲突（Go 无该路径体系）、`Context.llm_generate`/`html_renderer`/`star.text_to_image`（插件上下文 API，走 SDK）。

## ✅ 前端 `dashboard/src` 完整迁移（4.29.0–4.29.4，本轮）

| 项 | 结果 |
|---|---|
| 方式 | 以 `v4.28.2` 为基线做三方合并：base=upstream v4.28.2、ours=本仓前端、theirs=upstream v4.29.4 |
| 规模 | `git diff --name-status v4.28.2 v4.29.4 -- dashboard/src` → 183 文件；自动合并 118，手工解决 34 处冲突（含 13 个 i18n JSON）+ 1 处改名 |
| 新增/删除 | 新增 26 个上游文件（`useChunkedUpload.ts`、`SenderSelector/UmoSelector/PypiMirrorSelector/LogExportButton/LocalPermissionMatrix/ConfigProfileDrawer` 等、`stores/{headerContext,mobileDrawer}.ts`、`plugins/monaco.ts`、`styles/config-form.css`、i18n `index.ts` ×4）；删除 2 个（`SidebarCustomizer.vue`、`sidebarCustomization.js`）；改名 `views/PluginPagePage.vue`→`views/PluginViewPage.vue` |
| 保留本仓自定义 | 认证：`utils/token.ts`（sessionStorage）、`utils/wsTicket.ts`、`api/http.ts` 同源凭证裁剪、`ConsoleDisplayer` 日志流改用 `sessionStorage` token；插件页面 `pluginApi.page()`→`getPluginViewById`；市场 URL 指向 `WaterGodFurina/AstrBot-go-market`；`Settings.vue` 保留 `goproxy/goflags/pypi_index_url/plugin_idle_timeout_minutes` 分组；保留 `SleepPanel.vue`、`AddNewProvider.vue`、`utils/sse.ts` |
| OpenAPI 契约补齐 | `internal/dashboard/openspec/openapi-v1.yaml` 新增 19 个 operation（`files/upload/{init,chunk,complete,abort,status}`、`plugins/view`+`plugins/views` 及 `{plugin_id}/views...`、`trace/history`、`trace/{trace_id}`、`sessions/senders`、`conversations/filter-options`、`stats/ghproxy/speed`、`stats/pypi-index/{test,speed}`、`backups/upload/status`、`logs/export` 等）+ 6 个 schema（`ChatChunkUploadRequest`/`ChatUploadInitRequest`/`ChatUploadSessionRequest`/`PypiIndexTestRequest`/`BotCleanupTarget`/`RuntimeInfo`）+ `ProviderCapability` 增加 `decision`；`pnpm generate:api` 重新生成 `api/generated/openapi-v1` |
| 后端接线 | 新增 `GET /api/v1/plugins/view` 别名（`handlers.go` `case "page","view"` 复用 `handlePluginPage`）；其余新端点后端此前已实现（`files/upload/*`、`trace/history`、`sessions/senders`、`conversations/filter-options`、`stats/{ghproxy,pypi-index}`、`logs/export`） |
| 验证 | `pnpm typecheck` 0 错误；`pnpm build` 成功；产物同步至 `internal/dashboard/web/dist/`（`go:embed` 目标）；`go build ./...`、`go vet ./internal/dashboard/` 通过 |
| 未动 | upstream `plugins/{plugin_id}/views...` 的其余只读端点（前端未调用；spec 已声明、后端未接线）；全部改动**未提交 git** |

## ⏸ 待移植 / 未接线（其余已在 ✅ 中落地或标 N/A）

- **按钮（4.29.4）**：除宿主核心 + aiocqhttp 外的其余平台适配器（discord/kook/lark/line/mattermost/qqofficial/satori/slack/telegram/webchat/wecom/wecom_ai_bot/weixin_oc/weixin_official）按钮落地与入站 `ButtonInteraction` 解析；Go/Python 插件 SDK 的 `register_button_action`（组件与事件 API 已发版，注册 API 未加）。
- **Decision Provider（4.29.4）**：运行时默认持久化键 `curr_provider_decision` 与 dashboard/插件 API 暴露。
- **平台**：`qqofficial` 的 `group_info`（lark 已做）、dingtalk/misskey 扩展。
- **诊断导出（4.29.2）**：IP 字面量别名、自由赋值扫描（RE2 无 lookbehind）。
- **删机器人清理（4.29.4）**：`traces`/`routing` 目标（Go trace 存储无 platform 维度）。
- **前端插件视图（4.29.0）**：upstream `plugins/{plugin_id}/views...` 其余只读端点后端未接线（前端未调用，spec 已声明）。
- **N/A / 已等价**：`event_context` ContextVar 执行所有权（Go `event_bus.go` ActiveRegistry + `CleanupTemporaryFiles`）、图片链路 `image_input`/`async_generator` 重构（Go `image_prepare.go` 已覆盖）、4.29.2 `log.py` 跨事件环重构（Go channel+丢弃模型）、`astrbot_path`（Go 无该体系）、`Context.llm_generate`/`html_renderer`/`star.text_to_image`（SDK）、backup 模型补全（Go 整库备份）、pip 自更新绕过锁（Go 宿主不经 pip 自更新）、Gemini 生成参数（已落地）、Anthropic API Key 头（Go 手动 `x-api-key`，行为等价）、config list `subtitles`（前端元数据已随前端迁移带入）。
