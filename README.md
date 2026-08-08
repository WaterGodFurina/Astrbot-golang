# 注意:

本项目还在移植（使用DeepSeek作为辅助），默认日志为debug（目前无法更改）。本项目不负责在使用本项目时所造成的任何后果

# 原仓库

[https://github.com/AstrBotDevs/AstrBot](https://github.com/AstrBotDevs/AstrBot)

# AstrBot-Go

AstrBot 的 Go 语言移植版本，从 AstrBot 的 v4.27.2 移植而来。

## 架构

```
astrbot-go/
├── cmd/astrbot/               # 主入口
├── internal/
│   ├── agent/                 # LLM Agent 系统 (tool, message, response, MCP client)
│   ├── backup/                # 备份导入导出
│   ├── config/                # 配置管理
│   ├── contentsafety/        # 内容安全检查 (Keyword/LLM 策略)
│   ├── conversation/          # 会话管理 (Conversation, SessionServiceManager)
│   ├── core/                  # 事件总线 + Pipeline 调度器 + Event 模型
│   ├── cron/                  # 定时任务管理
│   ├── dashboard/             # WebUI API 服务器 + 路由 + 认证
│   ├── db/                    # SQLite 数据库
│   ├── i18n/                  # 国际化翻译系统
│   ├── knowledgebase/         # 知识库
│   ├── lifecycle/             # 生命周期管理，组装所有模块
│   ├── log/                   # 日志系统
│   ├── persona/               # 人格/提示词管理
│   ├── pipeline/              # 消息处理管线 (9 阶段 + Computer Use 工具执行)
│   ├── platform/              # 平台适配器接口 + 注册系统
│   │   └── sources/
│   │       ├── aiocqhttp/     # OneBot v11 适配器
│   │       ├── qqofficial/    # QQ 官方机器人适配器 (含 C2C 流式下发)
│   │       ├── telegram/      # Telegram Bot 适配器
│   │       └── webchat/       # 内置 WebChat 适配器
│   ├── plugin/                # .so 插件加载系统
│   ├── provider/              # LLM Provider 管理 (含 SSE 流式解析)
│   │   └── sources/
│   │       ├── openai_source.go      # OpenAI 兼容
│   │       ├── anthropic_source.go   # Anthropic Claude
│   │       ├── gemini_source.go      # Google Gemini
│   │       ├── ollama_source.go      # Ollama 本地模型
│   │       ├── dashscope_source.go   # 阿里通义千问
│   │       └── sse.go                # 共享 SSE 解析器
│   ├── ratelimit/             # 限流器 (Fixed Window, Stall/Discard)
│   ├── sandbox/               # 沙箱管理 (Docker 容器执行)
│   ├── session/               # 会话等待器
│   ├── skills/                # 技能管理 (SKILL.md 发现 + LLM 注入)
│   ├── star/                  # 指令系统 (含 .so 插件指令桥接)
│   ├── t2i/                   # 文本转图片渲染
│   └── utils/                 # IO 工具 + 媒体工具
├── pkg/
│   ├── message/               # 消息组件 + MessageEventResult + MessageChain
│   ├── sdk/                   # 插件开发 SDK
│   └── types/                 # 公共类型
├── examples/
│   └── echo_plugin/           # 示例 .so 插件
└── go.mod
```

## Pipeline 消息处理管线 (完整 9 阶段)

对齐 Python 版 `astrbot/core/pipeline/` 的 9 个有序阶段：

1. **WakingCheckStage** — 检查唤醒条件（前缀、@提及、私聊自动唤醒）
2. **WhitelistCheckStage** — 白名单/黑名单过滤
3. **SessionStatusCheckStage** — 会话启用状态检查
4. **RateLimitStage** — 频率限制（Fixed Window 算法，Stall/Discard 策略）
5. **ContentSafetyCheckStage** — 内容安全检查（Keyword/LLM 策略）
6. **PreProcessStage** — 预处理（媒体路径映射、STT 语音转文字）
7. **ProcessStage** — 插件 Handler 执行 + LLM Agent 调用（含技能注入、Computer Use 工具执行、流式输出）
8. **ResultDecorateStage** — 结果装饰（回复前缀、@提及、引用回复、T2I）
9. **RespondStage** — 发送消息链到平台

## Provider 系统

本项目仅支持6种 LLM Provider:

| Provider | 说明 |
|----------|------|
| OpenAI | OpenAI 兼容 API（含 tool calls 解析、SSE 流式响应） |
| Anthropic | Claude 系列模型 |
| Gemini | Google Gemini |
| Ollama | 本地大模型 |
| DashScope | 阿里通义千问 |
| OpenRouter | OpenAI 兼容格式 |

## 插件系统

AstrBot-Go 使用 Go 原生 `plugin` 包加载 `.so` 共享库，替代 Python 的 importlib。

### 插件接口

每个 `.so` 插件需导出以下函数：

```go
func PluginName() string
func PluginVersion() string
func PluginDescription() string
func Init(ctx *plugin.Context) error
func RegisterHandlers(reg *plugin.HandlerRegistry)
func Cleanup() error
```

插件指令/过滤器/钩子会在生命周期启动时桥接到 star 指令系统，WebUI 中的启停/重载会自动重新桥接。

### 编译插件

```bash
go build -buildmode=plugin -o data/plugins/myplugin.so myplugin.go
```

### 示例插件

见 `examples/echo_plugin/echo.go`

## 构建

```bash
# 主程序
go build -o bin/astrbot ./cmd/astrbot

# 插件
go build -buildmode=plugin -o examples/echo_plugin/echo.so examples/echo_plugin/echo.go
```

## 测试

```bash
go test ./... -v
```

## 运行

```bash
./bin/astrbot
```

默认 API 端口 6185。

## 代码规模

- 79 个 Go 文件
- ~23,000 行 Go 代码
- 对齐 Python AstrBot v4.27.2 的核心架构
