"""Star Handler 注册装饰器（Go 宿主兼容运行时）。

与 Python 本体 `astrbot.core.star.register.star_handler` API 对齐。
"""
from __future__ import annotations

import logging
import re
import typing
from collections.abc import Awaitable, Callable
from typing import Any

from astrbot.core.provider.func_tool_manager import (
    PY_TO_JSON_TYPE,
    SUPPORTED_TYPES,
    llm_tools,
    parse_docstring,
)
from astrbot.core.star.filter.command import CommandFilter
from astrbot.core.star.filter.command_group import CommandGroupFilter
from astrbot.core.star.filter.custom_filter import CustomFilter
from astrbot.core.star.filter.event_message_type import EventMessageType, EventMessageTypeFilter
from astrbot.core.star.filter.permission import PermissionType, PermissionTypeFilter
from astrbot.core.star.filter.platform_adapter_type import (
    PlatformAdapterType,
    PlatformAdapterTypeFilter,
)
from astrbot.core.star.filter.regex import RegexFilter
from astrbot.core.star.star_handler import EventType, StarHandlerMetadata, star_handlers_registry

logger = logging.getLogger("astrbot")


def get_handler_full_name(awaitable: Callable) -> str:
    """获取 Handler 的全名"""
    return f"{awaitable.__module__}_{awaitable.__name__}"


def get_handler_or_create(
    handler: Callable,
    event_type: EventType,
    dont_add: bool = False,
    **kwargs,
) -> StarHandlerMetadata:
    handler_full_name = get_handler_full_name(handler)
    md = star_handlers_registry.get_handler_by_full_name(handler_full_name)
    if md:
        return md
    md = StarHandlerMetadata(
        event_type=event_type,
        handler_full_name=handler_full_name,
        handler_name=handler.__name__,
        handler_module_path=handler.__module__,
        handler=handler,
        event_filters=[],
    )
    if handler.__doc__:
        md.desc = handler.__doc__.strip()
    if "desc" in kwargs:
        md.desc = kwargs["desc"]
        del kwargs["desc"]
    md.extras_configs = kwargs

    if not dont_add:
        star_handlers_registry.append(md)
    return md


def register_command(
    command_name: str | None = None,
    sub_command: str | None = None,
    alias: set | None = None,
    **kwargs,
):
    """注册一个 Command。"""
    new_command = None
    add_to_event_filters = False
    if isinstance(command_name, RegisteringCommandable):
        if sub_command is not None:
            parent_command_names = command_name.parent_group.get_complete_command_names()
            new_command = CommandFilter(
                sub_command,
                alias,
                None,
                parent_command_names=parent_command_names,
            )
            command_name.parent_group.add_sub_command_filter(new_command)
        else:
            logger.warning(
                f"No sub_command argument was provided while registering a "
                f"subcommand for {command_name}.",
            )
    elif command_name is None:
        logger.warning(
            "No command_name argument was provided while registering a bare command."
        )
    else:
        new_command = CommandFilter(command_name, alias, None)
        add_to_event_filters = True

    def decorator(awaitable):
        if not add_to_event_filters:
            kwargs["sub_command"] = True
        handler_md = get_handler_or_create(
            awaitable,
            EventType.AdapterMessageEvent,
            **kwargs,
        )
        if new_command:
            new_command.init_handler_md(handler_md)
            handler_md.event_filters.append(new_command)
        return awaitable

    return decorator


def register_custom_filter(custom_type_filter, *args, **kwargs):
    """注册一个自定义的 CustomFilter。"""
    add_to_event_filters = False
    raise_error = True
    if isinstance(custom_type_filter, RegisteringCommandable):
        parent_register_commandable = custom_type_filter
        custom_filter = args[0]
        if len(args) > 1:
            raise_error = args[1]
    else:
        add_to_event_filters = True
        custom_filter = custom_type_filter
        if len(args) > 0:
            raise_error = args[0]

    def decorator(awaitable):
        if isinstance(custom_filter, type) and issubclass(custom_filter, CustomFilter):
            f = custom_filter(raise_error=raise_error)
        elif isinstance(custom_filter, CustomFilter):
            f = custom_filter
        else:
            raise ValueError(
                "custom_filter must be a CustomFilter class or instance."
            )

        if add_to_event_filters:
            handler_md = get_handler_or_create(
                awaitable,
                EventType.AdapterMessageEvent,
                **kwargs,
            )
            handler_md.event_filters.append(f)
        else:
            parent_register_commandable.parent_group.add_custom_filter(f)
        return awaitable

    return decorator


class RegisteringCommandable:
    """用于指令组级联注册"""

    def __init__(self, parent_group: CommandGroupFilter) -> None:
        self.parent_group = parent_group

    @property
    def group(self):
        return globals()["register_command_group"]

    @property
    def command(self):
        return globals()["register_command"]

    @property
    def custom_filter(self):
        return globals()["register_custom_filter"]


def register_command_group(
    command_group_name: str | None = None,
    sub_command: str | None = None,
    alias: set | None = None,
    **kwargs,
):
    """注册一个 CommandGroup。"""
    new_group = None
    if isinstance(command_group_name, RegisteringCommandable):
        if sub_command is None:
            logger.warning(
                f"No sub_command was specified for a subgroup of command group "
                f"{command_group_name}."
            )
        else:
            new_group = CommandGroupFilter(
                sub_command,
                alias,
                parent_group=command_group_name.parent_group,
            )
            command_group_name.parent_group.add_sub_command_filter(new_group)
    elif command_group_name is None:
        logger.warning("No name was specified for the root command group.")
    else:
        new_group = CommandGroupFilter(command_group_name, alias)

    def decorator(obj):
        if new_group:
            handler_md = get_handler_or_create(
                obj,
                EventType.AdapterMessageEvent,
                **kwargs,
            )
            handler_md.event_filters.append(new_group)
            return RegisteringCommandable(new_group)
        raise ValueError("注册指令组失败。")

    return decorator


def register_event_message_type(event_message_type: EventMessageType, **kwargs):
    """注册一个 EventMessageType"""

    def decorator(awaitable):
        handler_md = get_handler_or_create(
            awaitable,
            EventType.AdapterMessageEvent,
            **kwargs,
        )
        handler_md.event_filters.append(EventMessageTypeFilter(event_message_type))
        return awaitable

    return decorator


def register_platform_adapter_type(
    platform_adapter_type: PlatformAdapterType,
    **kwargs,
):
    """注册一个 PlatformAdapterType"""

    def decorator(awaitable):
        handler_md = get_handler_or_create(
            awaitable,
            EventType.AdapterMessageEvent,
            **kwargs,
        )
        handler_md.event_filters.append(PlatformAdapterTypeFilter(platform_adapter_type))
        return awaitable

    return decorator


def register_regex(regex: str | re.Pattern, **kwargs):
    """注册一个 Regex"""

    def decorator(awaitable):
        handler_md = get_handler_or_create(
            awaitable,
            EventType.AdapterMessageEvent,
            **kwargs,
        )
        handler_md.event_filters.append(RegexFilter(regex))
        return awaitable

    return decorator


def register_permission_type(
    permission_type: PermissionType, raise_error: bool = True, **kwargs
):
    """注册一个 PermissionType"""

    def decorator(awaitable):
        handler_md = get_handler_or_create(
            awaitable,
            EventType.AdapterMessageEvent,
            **kwargs,
        )
        handler_md.event_filters.append(
            PermissionTypeFilter(permission_type, raise_error),
        )
        return awaitable

    return decorator


def register_on_astrbot_loaded(**kwargs):
    """当 AstrBot 加载完成时"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnAstrBotLoadedEvent, **kwargs)
        return awaitable

    return decorator


def register_on_platform_loaded(**kwargs):
    """当平台加载完成时"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnPlatformLoadedEvent, **kwargs)
        return awaitable

    return decorator


def register_on_plugin_error(**kwargs):
    """当插件处理消息异常时触发。"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnPluginErrorEvent, **kwargs)
        return awaitable

    return decorator


def register_on_plugin_loaded(**kwargs):
    """当有插件加载完成时"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnPluginLoadedEvent, **kwargs)
        return awaitable

    return decorator


def register_on_plugin_unloaded(**kwargs):
    """当有插件卸载完成时"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnPluginUnloadedEvent, **kwargs)
        return awaitable

    return decorator


def register_on_waiting_llm_request(**kwargs):
    """当等待调用 LLM 时的通知事件（在获取锁之前）"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnWaitingLLMRequestEvent, **kwargs)
        return awaitable

    return decorator


def register_on_llm_request(**kwargs):
    """当有 LLM 请求时的事件"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnLLMRequestEvent, **kwargs)
        return awaitable

    return decorator


def register_on_llm_response(**kwargs):
    """当有 LLM 请求后的事件"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnLLMResponseEvent, **kwargs)
        return awaitable

    return decorator


def register_on_agent_begin(**kwargs):
    """当 Agent 开始运行时的事件"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnAgentBeginEvent, **kwargs)
        return awaitable

    return decorator


def register_on_agent_done(**kwargs):
    """当 Agent 运行完成后的事件"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnAgentDoneEvent, **kwargs)
        return awaitable

    return decorator


def register_on_using_llm_tool(**kwargs):
    """当调用函数工具前的事件。会传入 tool 和 tool_args 参数。"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnUsingLLMToolEvent, **kwargs)
        return awaitable

    return decorator


def register_on_llm_tool_respond(**kwargs):
    """当调用函数工具后的事件。会传入 tool、tool_args 和 tool_result 参数。"""

    def decorator(awaitable):
        _ = get_handler_or_create(awaitable, EventType.OnLLMToolRespondEvent, **kwargs)
        return awaitable

    return decorator


def register_llm_tool(name: str | None = None, **kwargs):
    """为函数调用（function-calling / tools-use）添加工具。"""
    name_ = name

    def decorator(awaitable):
        llm_tool_name = name_ if name_ else awaitable.__name__
        func_doc = awaitable.__doc__ or ""
        docstring = parse_docstring(func_doc)
        args = []
        for arg in docstring.params:
            sub_type_name = None
            type_name = arg.type_name
            if not type_name:
                raise ValueError(
                    f"LLM 函数工具 {awaitable.__module__}_{llm_tool_name} 的参数 {arg.arg_name} 缺少类型注释。",
                )
            match = re.match(r"(\w+)\[(\w+)\]", type_name)
            if match:
                type_name = match.group(1)
                sub_type_name = match.group(2)
            type_name = PY_TO_JSON_TYPE.get(type_name, type_name)
            if sub_type_name:
                sub_type_name = PY_TO_JSON_TYPE.get(sub_type_name, sub_type_name)
            if type_name not in SUPPORTED_TYPES or (
                sub_type_name and sub_type_name not in SUPPORTED_TYPES
            ):
                raise ValueError(
                    f"LLM 函数工具 {awaitable.__module__}_{llm_tool_name} 不支持的参数类型：{arg.type_name}",
                )
            arg_json_schema = {
                "type": type_name,
                "name": arg.arg_name,
                "description": arg.description,
            }
            if sub_type_name:
                if type_name == "array":
                    arg_json_schema["items"] = {"type": sub_type_name}
            args.append(arg_json_schema)

        doc_desc = docstring.description.strip() if docstring.description else ""
        md = get_handler_or_create(awaitable, EventType.OnCallingFuncToolEvent)
        llm_tools.add_func(llm_tool_name, args, doc_desc, md.handler)
        return awaitable

    return decorator


def register_on_decorating_result(**kwargs):
    """在发送消息前的事件"""

    def decorator(awaitable):
        _ = get_handler_or_create(
            awaitable,
            EventType.OnDecoratingResultEvent,
            **kwargs,
        )
        return awaitable

    return decorator


def register_after_message_sent(**kwargs):
    """在消息发送后的事件"""

    def decorator(awaitable):
        _ = get_handler_or_create(
            awaitable,
            EventType.OnAfterMessageSentEvent,
            **kwargs,
        )
        return awaitable

    return decorator


class RegisteringAgent:
    """用于 Agent 注册（简化：工具照常注册进 llm_tools）。"""

    def llm_tool(self, *args, **kwargs):
        kwargs["registering_agent"] = self
        return register_llm_tool(*args, **kwargs)

    def __init__(self, agent: Any) -> None:
        self._agent = agent


def register_agent(
    name: str,
    instruction: str,
    tools: list | None = None,
    run_hooks: Any | None = None,
):
    """注册一个 Agent（Go 宿主兼容运行时：注册为 handoff 工具）。"""

    def decorator(awaitable):
        tools_ = tools or []
        # 简化：把 agent 注册为函数工具 transfer_to_<name>
        tool = llm_tools.spec_to_func(
            name=f"transfer_to_{name}",
            func_args=[{"type": "string", "name": "input", "description": "给该子代理的任务说明"}],
            desc=instruction or f"将任务移交给子代理 {name}",
            handler=awaitable,
        )
        llm_tools.func_list.append(tool)
        return RegisteringAgent(tool)

    return decorator
