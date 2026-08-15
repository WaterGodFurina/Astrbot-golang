# AstrBot-golang

该项目由[AstrBot](https://github.com/AstrBotDevs/AstrBot)迁移而来，目前移植进度94%左右，已验证60%的功能(核心功能已得到全部验证)

## 原仓库

[https://github.com/AstrBotDevs/AstrBot](https://github.com/AstrBotDevs/AstrBot)

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
│   ├── platform/              # 平台适配器 (18 个：QQ官方/Telegram/WebChat/Discord/Lark/微信系列/OneBot/Satori/Line/Slack/Mattermost/Misskey/Kook/DingTalk/WeCom 等)
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

## Agent改进

本项目在基于AstrBot上，加入了一些opencode的Agent特性，使模型在执行Agent操作时不会重复执行

## 插件 SDK

SDK 是独立 module `github.com/WaterGodFurina/Astrbot-go-plugin-sdk`（作为依赖从 GitHub 拉取；开发时本地 clone 到 `~/astrbot-go-plugin-sdk`）。插件作者只需写一个 `main`，实现命令/过滤器/钩子。插件身份信息（名称/版本/描述/作者/仓库/是否 cgo）统一放在包根目录的 `metadata.json`，`main.go` 只保留代码逻辑：

```go
package main

import (
    sdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk"
)

func main() {
    sdk.Serve(&sdk.Plugin{
        OnLoad: setup, // 启动钩子，可在里面动态注册
    })
}
```

```json
{
  "name": "echo",
  "desc": "Echoes your message back",
  "author": "AstrBot Devs",
  "version": "1.0.0",
  "repo": "https://github.com/AstrBotDevs/AstrBot",
  "cgo": false
}
```

插件包（zip/Git 仓库）根目录**必须**包含 `metadata.json` 与 `main.go`，缺任一即安装失败。`cgo` 字段声明该插件是否需要 C 编译器：为空/缺省视为 `false`（纯 Go，`CGO_ENABLED=0`）。

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

### 自带 Go 工具链

首次编译插件时，程序自动下载官方 Go 免安装包到用户私有目录（`~/.local/share/astrbot-go/` 等），无需用户装 Go：

- `ASTRBOT_GO_BIN`：指定已有的 `go` 二进制（最高优先）
- `ASTRBOT_GO_VERSION`：指定要下载的 Go 版本（默认 1.24.3）
- `ASTRBOT_GO_MIRROR`：下载镜像

cgo 插件的 C 编译器（zig/clang/GCC）也存放在同一私有目录下（`clang-download/` 缓存归档、`clang/` 解压产物）：

- `ASTRBOT_CLANG_BIN`：直接指向已装好的 clang 可执行文件
- `ASTRBOT_CLANG_VERSION`：指定要下载的 zig 版本（默认 0.16.0）
- `ASTRBOT_CLANG_MIRROR`：C 编译器下载镜像（如 gh-proxy 加速地址）
- `ASTRBOT_CC`：系统 GCC 覆盖（检测优先级 `ASTRBOT_CC` > `CC` > PATH `gcc`）

### 静态扫描 + 风险提示

安装插件前会静态扫描源码，发现 `os/exec`、`syscall`、`unsafe` 等危险包时，**WebUI 弹出风险对话框并列出具体代码行**，由用户选择"无视风险，继续安装"或"取消安装"。

### 已知局限与注意事项

1. **cgo 支持（声明式 + 自动选择 C 编译器）**：插件在 `metadata.json` 里显式声明 `"cgo": true` 才启用 cgo（缺省为 false，纯 Go 编译）。声明 cgo 后，宿主自动检测/选择 C 编译器：
   - 系统已有 GCC（按 `ASTRBOT_CC` > `CC` 环境变量 > PATH 中的 `gcc` 检测）→ WebUI 询问"使用系统 GCC 还是 Clang"
   - 系统已有 Clang → 直接使用
   - 都没有 → WebUI 询问是否自动下载 Clang；确认后下载 **zig**（`zig cc` 内置 clang，约 50MB，解压数秒，远快于 ~1GB 的 LLVM 完整包），或以 `zig cc`/`zig c++` 作为 CC/CXX 编译
   - 可手动取消，或设 `ASTRBOT_CC`/`ASTRBOT_CLANG_BIN` 指定编译器
   - 依赖 cgo 的库（如 `github.com/mattn/go-sqlite3`）因此在 Windows / macOS / Linux（含 Termux）上都能编译
2. **首次安装需下载 Go 工具链（约 150–200MB）**：WebUI 安装对话框会显示实时下载进度，日志每 10% 一报。可设置 `ASTRBOT_GO_BIN` 指向本机已有的 Go 以跳过下载。
3. **静态扫描的检测范围与盲区**：
   - 已检测：插件自身源码直接 `import` 的 `os/exec`、`syscall`、`unsafe`、`reflect`，以及 `//go:linkname`、`//go:generate` 指令（均有文件/行号/代码行，可人工判断）
   - **固有盲区（无法可靠拦截）**：
     - **间接导入**：插件 A 导入的外部包 B 内部使用 `os/exec`——由于 SDK 本身经 go-plugin 就依赖 `os/exec`，对依赖树做此扫描会把所有插件都标红，故不设阻断
     - **误报**：`os/exec` 执行 `ls` 等无害命令也会被标记
   - 结论：风险提示仅供人工判断，不构成安全保证。
4. **编译需要网络**：`go mod download` 会拉取依赖，离线环境安装插件会失败。
5. **Termux（Android）**：官方没有 Android 的 Go 包，需先 `pkg install golang` 再设 `ASTRBOT_GO_BIN` 指向它，或手动安装 Go；cgo 插件需 `pkg install clang`（Termux 无官方 zig 包）。
6. **cgo 编译器下载中断安全**：下载缓存于 `~/.local/share/astrbot-go/clang-download/`，支持断点续传（HTTP Range）；解压前写 `.install-lock`，若上次安装被中断，下次会自动丢弃半成品并重新下载。

### 安装与热重载

- WebUI「安装插件」支持上传 `.zip` 归档或填 Git/归档 URL
- 插件包根目录须含 `metadata.json`（身份/cgo 声明）与 `main.go`（入口），缺任一安装失败
- 安装 = 下载源码 → 校验 metadata.json + main.go → 静态扫描 → （如需 cgo 则选择/下载 C 编译器）→ 编译 → 启动子进程 → 桥接进管线
- 安装后把 metadata.json 内容写入 `data/plugins_config/<name>/config.json` 开头，供 WebUI 展示插件信息
- 崩溃自动重启（带退避与次数上限）；重载零停机（先起新进程再杀旧进程）

## 插件方案

插件系统已全面采用子进程方案（go-plugin + gRPC），旧 `.so` 插件方案及其 `legacy_plugin_mode` 配置项已彻底移除，不再提供 `.so` 加载路径。

## 平台适配器

共 18 个平台适配器，对齐 Python AstrBot v4.27.3 全平台覆盖：

| 适配器 | 说明 | 关键实现 |
|--------|------|----------|
| aiocqhttp | OneBot v11 | HTTP 接收 + 反向 WebSocket 发送，CQ 码转换，群转发/撤回/引用解析，同 msgID 去重 |
| qq_official | QQ 开放平台 | botpy WS 网关，原生 C2C 流式（StreamFragmenter） |
| qq_official_webhook | QQ 开放平台 Webhook | Ed25519 签名校验，webhook 回调，REST 发送 |
| telegram | Telegram | 长轮询，完整多媒体（photo/voice/document/video），setMessageReaction，30s 超时 |
| webchat | 内置 Web 聊天 | HTTP `/chat` `/poll`，SSE/WebSocket 双 transport，JWT 鉴权 |
| lark | 飞书 | 官方 SDK，socket 长连接 / webhook 双模式，post 富文本，im/v1/message_reaction，AES-256-CBC webhook 解密 |
| discord | Discord | gateway + Application Command（斜杠指令 1:1 star 桥接），content+files 发送，MessageReactionAdd |
| weixin_oc | 个人微信 | iLink 协议（QR 登录 + 长轮询），一键注册，token/context 持久化 |
| weixin_official_account | 微信公众号 | MpAccount SDK，ReadMessage 校验+解密，客服消息 custom/send |
| wecom | 企业微信 | WXBizMsgCrypt（AES-256-CBC + SHA1），应用+客服双模式，素材上传 |
| wecom_ai_bot | 企业微信 AI 机器人 | WXBizJsonMsgCrypt，webhook/长连接双模式，队列管理，流聚合 |
| line | LINE | 官方 SDK，webhook X-Line-Signature 校验，GetMessageContent，事件去重 |
| slack | Slack | Socket Mode + Webhook 双模式，blocks 解析，react，附件上传 |
| mattermost | Mattermost | WS 长连接 + REST（posts/files/users），multipart 上传，@提及解析 |
| misskey | Misskey | REST + WS streaming，note/renote/引用，文件上传，react，重连 |
| kook | KOOK | gateway WS（心跳/信令）+ REST，kmarkdown/卡片解析，(met)/(rol) 选择器 |
| dingtalk | 钉钉 | Stream 协议（connections/open WS + 心跳），REST 机器人消息，AES，重连退避 |
| satori | Satori 通用协议 | WS 信令（IDENTIFY/心跳/READY/EVENT），元素解析/发送，react |

统一 Webhook 入口：所有适配器均支持 `PlatformWebhook` 接口，dashboard 统一注册/分发 webhook 回调。

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
# 主程序（纯 Go，无需 C 编译器，CGO_ENABLED=0 亦可构建）
go build -o bin/astrbot ./cmd/astrbot
```

> **CGO 说明**：主程序数据库使用纯 Go 驱动 `modernc.org/sqlite`，**不依赖 CGO**，
> 可用 `CGO_ENABLED=0 go build` 产出静态二进制，交叉编译 / 无 GCC 的 Docker
> 镜像 / Windows / Termux 均无需 C 工具链。仅**插件**在 `metadata.json` 声明
> `"cgo": true` 时才需要 C 编译器（宿主自动选择 zig/clang/GCC，见上文"cgo 支持"）。
>
> **模块路径**：`module github.com/WaterGodFurina/Astrbot-golang`，与仓库 URL 一致；
> 插件 SDK 作为普通依赖从 GitHub 拉取（`github.com/WaterGodFurina/Astrbot-go-plugin-sdk`），
> 无本地 `replace`，clone 后直接 `go build` 即可（需联网拉依赖）。

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

- 317 个 Go 文件（非测试 203，测试 114，56 个包）
- 约 97,900 行（核心代码 ~73,900 行 + 测试 ~24,000 行）
- 18 个平台适配器 + 14 类 LLM Provider 能力
- 对齐 Python AstrBot v4.27.3 全平台全核心架构
