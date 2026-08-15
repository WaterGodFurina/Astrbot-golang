from __future__ import annotations

import enum


class MessageType(enum.Enum):
    GROUP_MESSAGE = "GROUP"
    FRIEND_MESSAGE = "FRIEND"
    OTHER_MESSAGE = "OTHER"

    @property
    def is_group(self) -> bool:
        return self == MessageType.GROUP_MESSAGE

    @property
    def is_friend(self) -> bool:
        return self == MessageType.FRIEND_MESSAGE
