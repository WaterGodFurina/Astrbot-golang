from .message_type import MessageType
from .platform_metadata import PlatformMetadata


class Platform:
    """平台适配器占位（Go 宿主兼容运行时）。"""

    def __init__(self, platform_config: dict, platform_settings: dict | None = None):
        self.platform_config = platform_config
        self.platform_settings = platform_settings

    def meta(self) -> PlatformMetadata:
        return PlatformMetadata(
            name="unknown",
            description="",
            id=str(self.platform_config.get("id", "")),
        )


__all__ = [
    "AstrBotMessage",
    "AstrMessageEvent",
    "Group",
    "MessageMember",
    "MessageSesion",
    "MessageSession",
    "MessageType",
    "Platform",
    "PlatformMetadata",
]

from .astrbot_message import AstrBotMessage, Group, MessageMember  # noqa: E402
from .astr_message_event import AstrMessageEvent  # noqa: E402
from .message_session import MessageSesion, MessageSession  # noqa: E402
