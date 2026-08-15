"""Star Handler 元数据与注册表（Go 宿主兼容运行时）。

与 Python 本体 `astrbot.core.star.star_handler` API 对齐。
"""
from __future__ import annotations

import enum
from dataclasses import dataclass, field
from typing import Any, Callable


class EventType(enum.Enum):
    """表示一个 AstrBot 内部事件的类型。"""

    OnAstrBotLoadedEvent = enum.auto()
    OnPlatformLoadedEvent = enum.auto()

    AdapterMessageEvent = enum.auto()
    OnWaitingLLMRequestEvent = enum.auto()
    OnLLMRequestEvent = enum.auto()
    OnLLMResponseEvent = enum.auto()
    OnAgentBeginEvent = enum.auto()
    OnAgentDoneEvent = enum.auto()
    OnDecoratingResultEvent = enum.auto()
    OnCallingFuncToolEvent = enum.auto()
    OnUsingLLMToolEvent = enum.auto()
    OnLLMToolRespondEvent = enum.auto()
    OnAfterMessageSentEvent = enum.auto()
    OnPluginErrorEvent = enum.auto()
    OnPluginLoadedEvent = enum.auto()
    OnPluginUnloadedEvent = enum.auto()


# 事件类型 → 宿主 hook event 名（与 Go SDK EventOn* 常量一致）
EVENT_TYPE_TO_HOOK_NAME: dict[EventType, str] = {
    EventType.AdapterMessageEvent: "on_message",
    EventType.OnWaitingLLMRequestEvent: "on_waiting_llm_request",
    EventType.OnLLMRequestEvent: "on_llm_request",
    EventType.OnLLMResponseEvent: "on_llm_response",
    EventType.OnAgentBeginEvent: "on_agent_begin",
    EventType.OnAgentDoneEvent: "on_agent_done",
    EventType.OnDecoratingResultEvent: "on_decorating_result",
    EventType.OnCallingFuncToolEvent: "on_tool_call",
    EventType.OnUsingLLMToolEvent: "on_using_llm_tool",
    EventType.OnLLMToolRespondEvent: "on_llm_tool_respond",
    EventType.OnAfterMessageSentEvent: "on_after_message_sent",
    EventType.OnPluginErrorEvent: "on_plugin_error",
    EventType.OnPluginLoadedEvent: "on_plugin_loaded",
    EventType.OnPluginUnloadedEvent: "on_plugin_unloaded",
    EventType.OnAstrBotLoadedEvent: "on_astrbot_loaded",
    EventType.OnPlatformLoadedEvent: "on_platform_loaded",
}


@dataclass
class StarHandlerMetadata:
    """描述一个 Star 所注册的某一个 Handler。"""

    event_type: EventType
    handler_full_name: str
    handler_name: str
    handler_module_path: str
    handler: Callable
    event_filters: list = field(default_factory=list)
    desc: str = ""
    extras_configs: dict = field(default_factory=dict)
    enabled: bool = True

    def __lt__(self, other: "StarHandlerMetadata"):
        return self.extras_configs.get("priority", 0) < other.extras_configs.get(
            "priority", 0
        )


class StarHandlerRegistry:
    def __init__(self) -> None:
        self.star_handlers_map: dict[str, StarHandlerMetadata] = {}
        self._handlers: list[StarHandlerMetadata] = []

    def append(self, handler: StarHandlerMetadata) -> None:
        if "priority" not in handler.extras_configs:
            handler.extras_configs["priority"] = 0
        self.star_handlers_map[handler.handler_full_name] = handler
        self._handlers.append(handler)
        self._handlers.sort(key=lambda h: -h.extras_configs["priority"])

    def get_handler_by_full_name(self, name: str) -> StarHandlerMetadata | None:
        return self.star_handlers_map.get(name)

    def get_handlers_by_event_type(self, event_type: EventType):
        return [
            h
            for h in self._handlers
            if h.event_type == event_type
            and h.enabled
            and (h.extras_configs.get("activated", True) is not False)
        ]

    def all(self) -> list[StarHandlerMetadata]:
        return list(self._handlers)

    def __iter__(self):
        return iter(self._handlers)

    def __len__(self) -> int:
        return len(self._handlers)

    def clear(self) -> None:
        self._handlers.clear()
        self.star_handlers_map.clear()


star_handlers_registry = StarHandlerRegistry()
"""全局 Star Handler 注册表"""
