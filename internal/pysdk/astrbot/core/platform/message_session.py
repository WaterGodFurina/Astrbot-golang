from __future__ import annotations

from dataclasses import dataclass, field

from astrbot.core.platform.message_type import MessageType


@dataclass
class MessageSession:
    """描述一条消息在 AstrBot 中对应的会话的唯一标识。"""

    platform_name: str
    message_type: MessageType
    session_id: str
    platform_id: str = field(init=False)

    def __str__(self) -> str:
        return f"{self.platform_id}:{self.message_type.value}:{self.session_id}"

    def __post_init__(self):
        self.platform_id = self.platform_name

    @staticmethod
    def from_str(session_str: str):
        platform_id, message_type, session_id = session_str.split(":", 2)
        return MessageSession(platform_id, MessageType(message_type), session_id)


MessageSesion = MessageSession  # back compatibility
