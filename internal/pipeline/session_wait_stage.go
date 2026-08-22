package pipeline

import (
	"context"
	"encoding/json"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
	"github.com/WaterGodFurina/Astrbot-golang/internal/star"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SessionWaitStage 是管线最前端的会话等待（SessionWaiter）消费阶段（对齐
// Python event_bus 在正式流水线之前先检查 session_waiter 的语义）：当事件
// 所属 umo 有插件注册了等待（HostService.RegisterSessionWait）时，把事件
// 经 PluginService.FeedSessionWait RPC 推送给对应插件；插件触发
// SessionWaiter.trigger 处理并消费（handled=true）后，事件不再走后续正常
// 管线（插件已在等待回调中自行处理回复），返回 Continue=false 拦截。
type SessionWaitStage struct {
	// subPlugins 是子进程插件运行时，经 PipelineContext.SubPlugins 注入。
	subPlugins *plugin.SubprocessManager
}

// NewSessionWaitStage creates the stage.
func NewSessionWaitStage() *SessionWaitStage {
	return &SessionWaitStage{}
}

func (s *SessionWaitStage) Name() string { return "session_wait" }

func (s *SessionWaitStage) Initialize(ctx *PipelineContext) error {
	s.subPlugins = ctx.SubPlugins
	return nil
}

func (s *SessionWaitStage) Process(ctx context.Context, event *core.Event) (*StageResult, error) {
	if s.subPlugins == nil {
		return &StageResult{Continue: true}, nil
	}
	// 会话等待只针对消息事件（Python SessionWaiter 消费 AstrMessageEvent）。
	if event.Type != core.EventMessage {
		return &StageResult{Continue: true}, nil
	}
	umo := event.UnifiedMsgOrigin()
	if umo == "" {
		return &StageResult{Continue: true}, nil
	}
	// 宿主与 Python SDK 的 unified_msg_origin 同为三段式
	// "platform_id:message_type:session_id"（MessageSession.__str__：
	// 好友=FriendMessage、群聊=GroupMessage、其他=OtherMessage），
	// 插件注册等待时用的就是这个格式，直接用 UnifiedMsgOrigin()
	// 查询注册表即可（PythonUMO 为其别名）。
	pythonUMO := event.PythonUMO()
	targets := s.subPlugins.SessionWaitForUmo(pythonUMO)
	if len(targets) == 0 {
		return &StageResult{Continue: true}, nil
	}
	// 事件序列化复用 HandleCommand/HandleFilter 的 SDK Event JSON 格式
	//（star.CoreEventToSDK），Python 侧 session_waiter.trigger 按该结构解析。
	eventJSON, err := json.Marshal(star.CoreEventToSDK(event))
	if err != nil {
		logger.I18nWarn("会话等待事件序列化失败: %v", err)
		return &StageResult{Continue: true}, nil
	}
	for _, t := range targets {
		inst := s.subPlugins.InstanceByName(t.PluginName)
		if inst == nil || inst.Client == nil {
			// 插件未加载（卸载/休眠窗口）：跳过，事件继续正常管线。
			continue
		}
		rpcCtx, rpcCancel := context.WithTimeout(ctx, pluginRPCTimeout)
		handled, err := inst.Client.FeedSessionWait(rpcCtx, eventJSON)
		rpcCancel()
		if err != nil {
			// UNIMPLEMENTED：旧版插件（编译时不带 FeedSessionWait RPC）
			// 视为无等待，静默跳过。
			if status.Code(err) == codes.Unimplemented {
				continue
			}
			logger.I18nWarn("插件 %s 会话等待 %s 推送失败: %v", t.PluginName, t.WaitID, err)
			continue
		}
		if handled {
			// 事件已被会话等待消费：停止管线（插件已在等待回调中处理）。
			return &StageResult{Continue: false}, nil
		}
	}
	// 所有等待均未消费（如插件钩子未匹配）：事件继续走正常管线。
	return &StageResult{Continue: true}, nil
}
