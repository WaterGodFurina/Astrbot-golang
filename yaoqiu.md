请对这个项目进行深度 Code Review。

项目背景：
这是一个使用 Go 重写 AstrBot 的机器人框架项目，核心目标包括：
- Go 实现的 Bot Runtime
- 插件系统
- 独立插件进程
- gRPC / Hashicorp go-plugin 通信
- LLM Agent
- MCP 工具调用
- OneBot 等消息平台适配
- 数据持久化

请不要只检查语法错误，而是重点寻找真实运行环境中可能出现的问题。

请重点关注以下方向：

========================
一、插件系统与进程隔离
========================

该项目使用 github.com/hashicorp/go-plugin 和 gRPC 实现插件机制。

请重点检查：

1. 插件启动、连接、退出流程是否完整：
- 插件 panic 后主程序是否能感知？
- 插件进程被 SIGKILL 后是否正确清理？
- 是否存在僵尸插件状态？
- 是否存在资源泄漏？

2. 检查：
- plugin.Client 生命周期
- RPC connection 生命周期
- grpc.ClientConn 是否正确 Close
- plugin.Kill() 是否正确调用
- plugin.Exited() 是否处理

3. 检查是否存在：
- goroutine 泄漏
- channel 未关闭
- context 未取消
- 阻塞等待无法恢复

请给出：
- 文件位置
- 函数名称
- 问题原因
- 复现条件
- 修复方案


========================
二、并发安全
========================

这是一个长期运行的机器人服务，请重点检查：

1. 是否存在：

map[string]xxx

这种共享 map 没有：

sync.Mutex
sync.RWMutex

保护的问题。

2. 检查：

- session 管理
- 用户上下文
- 群聊状态
- 插件注册表
- 配置缓存
- LLM memory

是否存在：

fatal error:
concurrent map read and map write


3. 检查 goroutine：

- 是否可能无限创建？
- 是否有退出机制？
- 是否绑定 context？
- 服务关闭时是否正确停止？


========================
三、Context 使用
========================

重点检查 Go context 使用是否正确。

寻找：

context.Background()

大量滥用的问题。

检查：

- HTTP 请求
- gRPC 请求
- LLM 请求
- MCP Tool 调用

是否应该使用：

request context

以及：

context.WithTimeout()

context.WithCancel()


重点寻找：

- 用户取消请求后后台任务仍运行
- LLM 请求无法终止
- Tool 调用永久等待


========================
四、gRPC / protobuf 设计
========================

检查：

1. protobuf 是否考虑版本兼容：

- 字段删除
- 字段修改
- reserved
- version negotiation

2. 插件 SDK 与主程序版本不一致时：

- 是否会崩溃？
- 是否有兼容机制？

3. 检查：

- timeout
- retry
- error handling
- streaming


========================
五、MCP 和 Agent 系统
========================

这是 AI Agent 框架，请重点检查：

1. Tool 调用：

- 超时处理
- 错误恢复
- 重试机制

2. Agent loop：

检查是否可能：

用户消息
↓
LLM
↓
Tool
↓
LLM
↓
Tool

无限循环。


3. 检查：

- memory
- conversation history
- context window 管理

是否可能：

- 无限增长
- 内存泄漏
- 超出模型限制


========================
六、数据库与存储
========================

检查：

sqlite 使用：

github.com/mattn/go-sqlite3

关注：

- 并发访问
- connection 生命周期
- migration
- 数据损坏风险

检查：

- 是否需要事务
- 是否存在 SQL 注入
- 是否存在锁竞争


========================
七、部署问题
========================

检查：

go.mod：

go 版本要求：

是否合理。

检查：

CGO 依赖：

github.com/mattn/go-sqlite3

是否导致：

- Docker 构建失败
- 交叉编译失败
- 单文件部署失败


检查：

replace:

github.com/WaterGodFurina/Astrbot-go-plugin-sdk => ../astrbot-go-plugin-sdk

是否导致：

其他用户无法构建。


========================
八、安全问题
========================

检查：

- 插件认证
- RPC 鉴权
- WebSocket 安全
- Token 管理
- API Key 泄露
- 文件路径穿越
- 插件权限隔离


========================
九、代码质量
========================

请重点寻找：

- AI 生成代码常见问题
- 错误处理不足
- panic 滥用
- 空 error 被忽略
- 不合理默认值
- 隐藏 race condition


========================
输出格式
========================

请按照严重程度分类：

## Critical（可能导致崩溃、安全漏洞、数据损坏）

- 文件：
- 函数：
- 问题：
- 触发条件：
- 修复：

## High（严重稳定性问题）

同上。

## Medium（设计缺陷）

同上。

## Low（代码质量问题）

同上。


如果没有发现问题，也不要简单说“没问题”，请说明检查过哪些模块以及为什么没有发现问题。