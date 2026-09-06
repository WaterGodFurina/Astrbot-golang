# AstrBot-golang

[![Go Version](https://img.shields.io/badge/Go-1.26+-blue.svg)](https://golang.org/)
[![License](https://img.shields.io/badge/License-AstrBot-orange.svg)](https://github.com/WaterGodFurina/Astrbot-golang/blob/main/LICENSE)

> 由 [AstrBot](https://github.com/AstrBotDevs/AstrBot) 迁移而来的 Go 语言实现，**核心功能已验证**，当前移植进度 **97%**。

---

## 项目简介

AstrBot-golang 是一个**高性能、可扩展**的聊天机器人框架，原生支持 **18 个消息平台**、**14 类 LLM Provider**、**子进程插件系统**（兼容 Python/Go 插件）以及 **WebUI 管理面板**。项目采用纯 Go 编写，核心二进制不依赖 CGO，可轻松跨平台部署（Windows / macOS / Linux / Termux）。

---

## 架构概览

```text
astrbot-go/
├── cmd/astrbot/               # 主程序入口（--webui-dir / --reset-password）
├── internal/                  # 核心（27 个包）
│   ├── lifecycle/             # 生命周期管理（模块组装与启动顺序）
│   ├── core/                  # 事件总线 + Pipeline 调度器
│   ├── pipeline/              # 消息处理管线（9 阶段）
│   ├── star/                  # 指令系统（命令/filter/hook 桥接）
│   ├── plugin/                # 子进程插件运行时（go-plugin + gRPC）
│   ├── pysdk/                 # Python 插件 SDK 运行管理（venv/运行时）
│   ├── toolchain/             # 自带 Go / Clang / Python 工具链（自动下载与管理）
│   ├── platform/              # 18 个平台适配器
│   ├── provider/              # LLM Provider 管理（Chat/TTS/STT/Embedding/Rerank）
│   ├── agent/                 # Agent 系统（FunctionTool / MCP / 子代理 / 上下文压缩）
│   ├── conversation/          # 会话管理
│   ├── session/               # 会话自定义规则
│   ├── persona/               # 人格管理
│   ├── knowledgebase/         # 知识库（nanovec 向量检索 + SQLite 双写）
│   ├── skills/                # 技能管理（SKILL.md + LLM 注入）
│   ├── sandbox/               # 沙箱（Docker/本地/Shipyard）
│   ├── t2i/                   # 文本转图片（远程 t2i + 本地 gg 回退）
│   ├── contentsafety/         # 内容安全（关键词 + 百度 AIP）
│   ├── ratelimit/             # 消息速率限制
│   ├── cron/                  # 定时任务
│   ├── backup/                # 备份（导出/导入/校验）
│   ├── dashboard/             # WebUI API 服务器 + 认证（前端 embed）
│   ├── config/                # 配置管理
│   ├── db/                    # SQLite 数据库（纯 Go 驱动，WAL）
│   ├── i18n/                  # 国际化（520+ 内置 key）
│   ├── log/                   # 日志（loguru ANSI 配色）
│   ├── utils/                 # 通用工具
│   └── version/               # 版本信息
├── pkg/
│   ├── message/               # 消息组件
│   └── sdk/                   # 插件 SDK 文档入口
├── dashboard/                 # 前端源码（Vue）
└── go.mod
```

---

## Agent 改进

本项目在 AstrBot 的基础上引入了部分 opencode 的 Agent 特性，确保模型在执行 Agent 操作时不会重复执行，提升多轮交互的效率与稳定性。

---

## 插件系统

插件系统采用 子进程方案（go-plugin + gRPC），保证隔离性和稳定性

为防止因为宿主强退而带来的插件孤儿进程，本项目在启动时会检查孤儿进程是否存在，如果有，则会执行清理

### 核心特性

- 跨语言兼容：支持 Go 与 Python 插件，通过 main.go / main.py 自动识别类型。
- 进程隔离：每个插件运行在独立子进程中，崩溃自动重启（带退避与次数上限）。
- 热重载：零停机更新（先起新进程，再杀旧进程）。
- 闲置休眠：可配置 plugin_idle_unload_minutes，超时无 RPC 活动的插件进程被终止（内存归还），但命令/过滤器/LLM 工具仍保留在注册表中；下次触发时自动懒加载唤醒，用户无感。
- 兼容 AstrBot 生态：Python 插件通过独立 gRPC 桥接，支持直接从市场安装（zip/Git），requirements.txt 自动安装依赖，_conf_schema.json 渲染 WebUI 配置面板。

> 注意：Python 插件每个进程约占用 60-80MB 内存（因 gRPC 隔离）。

---

### Go 插件 SDK

golang 插件通过[独立 module](github.com/WaterGodFurina/astrbot-go-plugin-sdk)桥接进宿主。

#### 自带 Go 工具链

首次编译插件时，程序自动下载官方 Go 免安装包到用户目录，无需用户预先安装 Go。

- 环境变量控制（见下方表格）
- 版本默认：Go 1.26

#### 静态扫描 + 风险提示

安装插件前，自动扫描源码中的危险包（os/exec、syscall、unsafe 等），WebUI 弹出风险对话框并显示具体代码行，用户可选择“继续安装”或“取消”。

局限：仅能检测插件自身源码的直接导入，间接依赖（如第三方包）无法可靠拦截，该提示仅供人工判断。

#### cgo 支持

插件需在 metadata.json 中显式声明 "cgo": true 才启用 cgo（默认 false）。宿主自动检测并选择 C 编译器：

- 系统已有 GCC（按 ASTRBOT_CC > CC 环境变量 > PATH 中的 gcc）→ WebUI 询问使用系统 GCC 还是 Clang
- 系统已有 Clang → 直接使用
- 都没有 → WebUI 询问是否自动下载 zig（zig cc 内置 clang，约 50MB），下载后解压即可用
- 支持断点续传，安装中断后自动清理并重试

---

### Python 插件 SDK

为兼容 AstrBot 生态，Python 插件通过[独立 module](github.com/WaterGodFurina/astrbot-golang-plugin-python-sdk) 桥接进宿主。

- API 对齐：astrbot.api.* 与 astrbot.core.* 对齐 Python AstrBot v4.27.4
- 能力桥接：Context.get_all_stars/get_all_providers 等经宿主 RPC 反向调用；session_waiter 跨进程喂入

#### 自带 Python 解释器

若系统无 python3/python，程序自动下载 python-build-standalone（Astral 维护的 CPython 独立发行版）到用户目录，跨重启复用。

- 环境变量控制（见下方表格）
- 版本默认：20260814（内置 CPython 3.12）
- pip 默认走阿里云镜像（可配置 pypi_index_url 或环境变量 ASTRBOT_PYPI_INDEX/PIP_INDEX_URL 覆盖）

---

## 平台适配器（18 个）

| 适配器 | 说明 | 关键特性 |
|--------|------|----------|
| aiocqhttp | OneBot v11 | HTTP 接收 + 反向 WebSocket，CQ 码转换，群转发/撤回/引用，去重 |
| qq_official | QQ 开放平台 | botpy WS 网关，原生 C2C 流式（StreamFragmenter） |
| qq_official_webhook | QQ 开放平台 Webhook | Ed25519 签名校验，webhook 回调，REST 发送 |
| telegram | Telegram | 长轮询，完整多媒体（photo/voice/document/video），reaction，30s 超时 |
| webchat | 内置 Web 聊天 | HTTP `/chat` `/poll`，SSE/WebSocket 双 transport，JWT 鉴权 |
| lark | 飞书 | SDK，socket/Webhook 双模式，post 富文本，reaction，AES-256-CBC 解密 |
| discord | Discord | Gateway + Application Command（斜杠指令），content+files，reaction |
| weixin_oc | 个人微信（iLink） | QR 登录 + 长轮询，一键注册，token/context 持久化 |
| weixin_official_account | 微信公众号 | MpAccount SDK，消息校验+解密，客服消息 |
| wecom | 企业微信 | WXBizMsgCrypt（AES-256-CBC + SHA1），应用+客服双模式，素材上传 |
| wecom_ai_bot | 企业微信 AI 机器人 | WXBizJsonMsgCrypt，webhook/长连接双模式，队列管理，流聚合 |
| line | LINE | Webhook X-Line-Signature 校验，GetMessageContent，事件去重 |
| slack | Slack | Socket Mode + Webhook 双模式，blocks 解析，react，附件上传 |
| mattermost | Mattermost | WS + REST（posts/files/users），multipart 上传，@提及解析 |
| misskey | Misskey | REST + WS streaming，note/renote/引用，文件上传，react，重连 |
| kook | KOOK | Gateway WS（心跳/信令）+ REST，kmarkdown/卡片解析，(met)/(rol) 选择器 |
| dingtalk | 钉钉 | Stream 协议（connections/open WS + 心跳），REST 机器人消息，AES，重连退避 |
| satori | Satori 通用协议 | WS 信令（IDENTIFY/心跳/READY/EVENT），元素解析/发送，react |

所有适配器统一支持 PlatformWebhook 接口，dashboard 统一注册/分发回调。

---

## Provider 支持

| 能力类别 | 支持的 Provider |
|----------|-----------------|
| Chat / LLM | OpenAI、OpenRouter、Anthropic、Gemini、Ollama、DashScope、Groq、xAI、智谱、LongCat、AIHubMix、小米（OpenAI 兼容）、Kimi-Code（Anthropic 协议）、OpenAI Responses API |
| TTS (9) | OpenAI、Azure、ElevenLabs、FishAudio、Edge-TTS、MiniMax、火山引擎、Gemini、MiMo |
| STT (2) | OpenAI Whisper、MiMo |
| Embedding (5) | OpenAI、DashScope、Gemini、NVIDIA、Ollama |
| Rerank (5) | TEI、百炼、NVIDIA、vLLM、Xinference |

## 技术选型说明

- **向量检索（知识库）**：使用纯 Go 的 `nanovec` 作为向量检索后端，替代原版 AstrBot 依赖的 FAISS（FAISS 依赖 C++ 编译链，与项目"纯 Go、无 CGO"的目标冲突）。知识库采用"nanovec 向量检索 + SQLite chunk 列表"双写：入库先写 SQLite、删除先删向量、启动自愈，保证两边一致。
- **文本转图片（t2i）**：渲染策略**优先远程 t2i 服务**（HTML 模板 → 浏览器截图，效果最好），**本地回退用内置 gg 引擎**（`fogleman/gg` + 中英双字体回退 + Twemoji），无需 CGO。策略由 `t2i_strategy`（remote/local）控制：remote 优先用户配置的 `t2i_endpoint`，不可用或失败时自动回退本地渲染。

---

## 环境变量

以下环境变量用于控制工具链下载、编译器选择等行为，配置于 WebUI 或启动脚本中。

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| `ASTRBOT_GO_BIN` | 指定已有的 `go` 可执行文件路径（最高优先） | （自动检测） |
| `ASTRBOT_GO_VERSION` | 要下载的 Go 版本 | `1.26` |
| `ASTRBOT_GO_MIRROR` | Go 下载镜像前缀（如 `https://goproxy.cn/`） | （官方源） |
| `ASTRBOT_CLANG_BIN` | 指定已有的 `clang` 可执行文件路径 | （自动检测） |
| `ASTRBOT_CLANG_VERSION` | 要下载的 zig 版本（含 clang） | `0.16.0` |
| `ASTRBOT_CLANG_MIRROR` | zig 下载镜像前缀 | （官方源） |
| `ASTRBOT_CC` | 覆盖系统 C 编译器（优先级高于 `CC`） | （自动检测） |
| `ASTRBOT_PYTHON_BIN` | 指定已有的 `python3`/`python` 可执行文件路径 | （自动检测） |
| `ASTRBOT_PYTHON_VERSION` | python-build-standalone 版本 tag（内置 CPython 版本） | `20260814` |
| `ASTRBOT_PYTHON_MIRROR` | Python 发行版下载镜像前缀 | （官方源） |
| `ASTRBOT_PYTHON_SKIP_VERIFY` | 跳过下载后的完整性检查（不推荐） | `false` |
| `ASTRBOT_PYTHON_CACHE_DIR` | 覆盖 Python 缓存目录（venv 与 bundled-Python 共用） | `~/.local/share/astrbot-go` |
| `ASTRBOT_PYPI_INDEX` / `PIP_INDEX_URL` | pip 源索引 URL（阿里云镜像为 `https://mirrors.aliyun.com/pypi/simple/`） | 阿里云镜像 |
| `ASTRBOT_SKIP_PYTHON_DOWNLOAD` | 禁止自动下载 Python（若系统无 Python 则安装失败） | `false` |

---

## Release发布的安卓二进制文件局限
经检查，release的安卓二进制文件运行时无法使用插件功能（会提示无权限）
目前没有尚可的懒人方案
如果你想要在安卓上运行本项目，请把本项目的代码拉下来，进行go源码编译：

```bash
pkg update && pkg install golang git
git clone https://github.com/WaterGodFurina/Astrbot-golang.git && cd Astrbot-golang
go build -o astrbot ./cmd/astrbot/main.go
```

---

## 下载
前往[Release](https://github.com/WaterGodFurina/Astrbot-golang/releases)获取对应平台的二进制文件

## 构建

```bash
# 主程序（纯 Go，无需 CGO，CGO_ENABLED=0 亦可）
go build -o bin/astrbot ./cmd/astrbot
```

CGO 说明：主程序使用纯 Go SQLite 驱动（modernc.org/sqlite），不依赖 CGO，可交叉编译出静态二进制，适合 Windows / macOS / Linux（含 Termux）及无 GCC 的 Docker 环境。
仅当插件在 metadata.json 中声明 "cgo": true 时才需要 C 编译器（宿主自动选择 zig/clang/GCC）。

- 模块路径：github.com/WaterGodFurina/Astrbot-golang
- 插件 SDK：作为普通依赖从 GitHub 拉取（github.com/WaterGodFurina/Astrbot-go-plugin-sdk），无需本地 replace，直接 go build 即可（需联网拉取依赖）。

## 测试

```bash
go test ./... -v
```

## 运行

```bash
./bin/astrbot
```

默认 WebUI API 端口：6185
访问 http://IP地址:6185 进入管理面板。

---

## 代码规模

- Go 文件：354 个（非测试 226，测试 128，58 个包）
- 代码行数：约 118,715 行（核心 ~90,746 行 + 测试 ~27,969 行）
- 平台适配器：18 个
- Provider 能力：Chat 14 / TTS 9 / STT 2 / Embedding 5 / Rerank 5
- 对齐版本：Python AstrBot v4.27.4

---

## 许可证

本项目采用与 AstrBot 相同的许可证 [AGPL-3.0](https://github.com/WaterGodFurina/Astrbot-golang/blob/main/LICENSE)

---

提示：本 README 持续更新，更多细节请查阅 [原项目文档](https://docs.astrbot.app/what-is-astrbot.html)。

```