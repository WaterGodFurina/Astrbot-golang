# ruff: noqa: F401, F403
"""旧风格兼容导出：from astrbot import *（对齐 Python astrbot.api.all.py）。"""
from astrbot import logger
from astrbot.core.config.astrbot_config import AstrBotConfig
from astrbot.core.message.message_event_result import (
    CommandResult,
    EventResultType,
    MessageChain,
    MessageEventResult,
    ResultContentType,
)
from astrbot.core.platform import AstrMessageEvent
from astrbot.core.star import Context, Star
from astrbot.core.star.register import register_llm_tool as llm_tool
from astrbot.core.star.register import (
    register_command as command,
    register_command_group as command_group,
    register_event_message_type as event_message_type,
    register_platform_adapter_type as platform_adapter_type,
    register_regex as regex,
    register_star as register,
)
from astrbot.core.star.filter.event_message_type import (
    EventMessageType,
    EventMessageTypeFilter,
)
from astrbot.core.star.filter.platform_adapter_type import (
    PlatformAdapterType,
    PlatformAdapterTypeFilter,
)
from astrbot.core.star.config import *  # noqa
from astrbot.core.provider.entities import LLMResponse, ProviderRequest
from astrbot.core.platform import (
    AstrBotMessage,
    Platform,
    MessageMember,
    MessageType,
    PlatformMetadata,
)
from astrbot.core.message.message_event_result import MessageChain
from astrbot.api.message_components import *  # noqa
