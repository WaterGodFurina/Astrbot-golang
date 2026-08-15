import time
from dataclasses import dataclass

from astrbot.core.message.components import BaseMessageComponent

from .message_type import MessageType


@dataclass
class MessageMember:
    user_id: str  # 发送者id
    nickname: str | None = None

    def __str__(self) -> str:
        return (
            f"User ID: {self.user_id},"
            f"Nickname: {self.nickname if self.nickname else 'N/A'}"
        )


@dataclass
class Group:
    group_id: str
    group_name: str | None = None
    group_avatar: str | None = None
    group_owner: str | None = None
    group_admins: list[str] | None = None
    members: list["MessageMember"] | None = None

    def __str__(self) -> str:
        return (
            f"Group ID: {self.group_id}\n"
            f"Name: {self.group_name if self.group_name else 'N/A'}\n"
            f"Avatar: {self.group_avatar if self.group_avatar else 'N/A'}\n"
            f"Owner ID: {self.group_owner if self.group_owner else 'N/A'}\n"
            f"Admin IDs: {self.group_admins if self.group_admins else 'N/A'}\n"
            f"Members Len: {len(self.members) if self.members else 0}\n"
            f"First Member: {self.members[0] if self.members else 'N/A'}\n"
        )


class AstrBotMessage:
    """AstrBot 的消息对象"""

    type: MessageType
    self_id: str
    session_id: str
    message_id: str
    group: Group | None
    sender: MessageMember
    message: list[BaseMessageComponent]
    message_str: str
    raw_message: object
    timestamp: int

    def __init__(self) -> None:
        self.timestamp = int(time.time())
        self.group = None

    def __str__(self) -> str:
        return str(self.__dict__)

    @property
    def group_id(self) -> str:
        if self.group:
            return self.group.group_id
        return ""

    @group_id.setter
    def group_id(self, value: str | None) -> None:
        if value:
            if self.group:
                self.group.group_id = value
            else:
                self.group = Group(group_id=value)
        else:
            self.group = None
