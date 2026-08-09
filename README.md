# 注意:

本项目还在移植（使用DeepSeek作为辅助）。本项目不负责在使用本项目时所造成的任何后果。日志级别可通过配置文件 `data/cmd_config.json` 中的 `log_level`（DEBUG/INFO/WARN/ERROR/CRITICAL）或环境变量 `ASTRBOT_LOG_LEVEL` 设置，环境变量优先级更高，默认 INFO。

# 原仓库

[https://github.com/AstrBotDevs/AstrBot](https://github.com/AstrBotDevs/AstrBot)

# AstrBot-Go

AstrBot 的 Go 语言移植版本，从 AstrBot 的 v4.27.2 移植而来。

## 架构

```
astrbot-go/
├── cmd/astrbot/               # 主入口
├── internal/
│   ├── plugin/                # 子进程插件运行时 (go-plugin + gRPC：加载/编译/静态扫描/崩溃重启)
│   ├── toolchain/             # 自带 Go 工具链 (自动下载、解压、路径管理)
│   ├── star/                  # 指令系统 (子进程插件命令/filter/hook 桥接进管线)
│   ├── lifecycle/             # 生命周期管理，组装所有模块
│   ├── pipeline/              # 消息处理管线 (9 阶段)
│   ├── platform/              # 平台适配器 (qqofficial/telegram/aiocqhttp/webchat)
│   ├── provider/              # LLM Provider 管理
│   ├── dashboard/             # WebUI API 服务器 + 路由 + 认证
│   ├── core/                  # 事件总线 + Pipeline 调度器
│   ├── sandbox/               # 沙箱 (Docker/本地/Shipyard)
│   ├── skills/                # 技能管理
│   ├── conversation/          # 会话管理
│   ├── config/                # 配置管理
│   ├── db/                    # SQLite 数据库
│   └── ...                    # 其余支持包
├── pkg/
│   ├── message/               # 消息组件
│   └── sdk/                   # 插件 SDK 文档入口 (SDK 本体在独立 module)
├── examples/
│   └── echo_plugin/           # 子进程插件示例 (sdk.Serve)
└── go.mod
```

# 插件系统重构

插件系统已从 Linux 专用的 Go 原生 `.so` 方案重构为**子进程方案**（go-plugin + gRPC），解决旧方案的四大痛点：

| 痛点 | 旧 `.so` 方案 | 新子进程方案 |
|------|--------------|--------------|
| 平台 | 仅 Linux | Windows / macOS / Linux（含 Termux） |
| 热卸载 | `.so` 无法卸载，内存不释放 | 杀掉子进程即被 OS 完全回收 |
| 分发 | 每个平台单独编译二进制 | 只发 Go 源码，用户侧自动编译 |
| 隔离 | 与主进程同地址空间，崩溃拖垮主进程 | 独立子进程，崩溃不影响主进程 |

## 插件 SDK

SDK 是独立 module `github.com/WaterGodFurina/Astrbot-go-plugin-sdk`（本地 `~/astrbot-go-plugin-sdk`）。插件作者只需写一个 `main`，实现命令/过滤器/钩子：

```go
package main

import (
    sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

func main() {
    sdk.Serve(&sdk.Plugin{
        Name:        "echo",
        Version:     "1.0.0",
        Description: "Echoes your message back",
        OnLoad:      setup, // 启动钩子，可在里面动态注册
    })
}
```

命令处理函数可以拆到独立文件，通过 `setup()`（OnLoad 钩子）或 `init()` 注册：

```go
func setup() error {
    sdk.RegisterCommand(sdk.Command{
        Name:    "echo",
        Aliases: []string{"repeat"},
        Handler: func(e *sdk.Event, args []string) (string, error) {
            return strings.Join(args, " "), nil
        },
    })
    return nil
}
```

SDK 也支持声明式写法（直接在 `sdk.Plugin{Commands: []sdk.Command{...}}` 里声明），两者等价。

## 自带 Go 工具链

首次编译插件时，程序自动下载官方 Go 免安装包到用户私有目录（`~/.local/share/astrbot-go/` 等），无需用户装 Go：

- `ASTRBOT_GO_BIN`：指定已有的 `go` 二进制（最高优先）
- `ASTRBOT_GO_VERSION`：指定要下载的 Go 版本（默认 1.24.3）
- `ASTRBOT_GO_MIRROR`：下载镜像

## 静态扫描 + 风险提示

安装插件前会静态扫描源码，发现 `os/exec`、`syscall`、`unsafe` 等危险包时，**WebUI 弹出风险对话框并列出具体代码行**，由用户选择"无视风险，继续安装"或"取消安装"。

## 已知局限与注意事项

1. **cgo 支持（自适应）**：用户机器若装有 C 编译器（`cc`/`gcc`/`clang`），插件编译自动启用 `CGO_ENABLED=1`，因此依赖 cgo 的库（如 `github.com/mattn/go-sqlite3`）在**有 C 工具链的机器上可编译**；无 C 编译器时回退纯 Go（`CGO_ENABLED=0`）。可用 `ASTRBOT_PLUGIN_CGO=0/1` 强制。注意：自带的 Go 工具链不含 C 编译器，cgo 构建依赖系统已有的 C 环境；纯 Go 替代库（如 `modernc.org/sqlite`）则任何机器都能编。
2. **首次安装需下载 Go 工具链（约 150–200MB）**：WebUI 安装对话框会显示实时下载进度，日志每 10% 一报。可设置 `ASTRBOT_GO_BIN` 指向本机已有的 Go 以跳过下载。
3. **静态扫描的检测范围与盲区**：
   - 已检测：插件自身源码直接 `import` 的 `os/exec`、`syscall`、`unsafe`、`reflect`，以及 `//go:linkname`、`//go:generate` 指令（均有文件/行号/代码行，可人工判断）
   - **固有盲区（无法可靠拦截）**：
     - **间接导入**：插件 A 导入的外部包 B 内部使用 `os/exec`——由于 SDK 本身经 go-plugin 就依赖 `os/exec`，对依赖树做此扫描会把所有插件都标红，故不设阻断
     - **误报**：`os/exec` 执行 `ls` 等无害命令也会被标记
   - 结论：风险提示仅供人工判断，不构成安全保证。
4. **编译需要网络**：`go mod download` 会拉取依赖，离线环境安装插件会失败。
5. **Termux（Android）**：官方没有 Android 的 Go 包，需先 `pkg install golang` 再设 `ASTRBOT_GO_BIN` 指向它，或手动安装 Go。

## 安装与热重载

- WebUI「安装插件」支持上传 `.zip` 归档或填 Git/归档 URL
- 安装 = 下载源码 → 静态扫描 → 编译 → 启动子进程 → 桥接进管线
- 崩溃自动重启（带退避与次数上限）；重载零停机（先起新进程再杀旧进程）

## 插件方案

插件系统已全面采用子进程方案（go-plugin + gRPC），旧 `.so` 插件方案及其 `legacy_plugin_mode` 配置项已彻底移除，不再提供 `.so` 加载路径。

## Provider 支持情况

| 能力 | 支持 |
|------|------|
| Chat / LLM (14) | OpenAI、OpenRouter、Anthropic、Gemini、Ollama、DashScope、Groq、xAI、智谱、LongCat、AIHubMix、小米（OpenAI 兼容薄封装）、Kimi-Code（Anthropic 协议）、OpenAI Responses API |
| TTS (9) | OpenAI、Azure、ElevenLabs、FishAudio、Edge-TTS、MiniMax、火山引擎、Gemini、MiMo |
| STT (2) | OpenAI Whisper、MiMo |
| Embedding (5) | OpenAI、DashScope、Gemini、NVIDIA、Ollama |
| Rerank (5) | TEI、百炼、NVIDIA、vLLM、Xinference |

## 构建

```bash
# 主程序
go build -o bin/astrbot ./cmd/astrbot
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

- 124 个 Go 文件（含测试 150 个）
- 约 34,000 行 Go 代码
- 对齐 Python AstrBot v4.27.2 的核心架构
