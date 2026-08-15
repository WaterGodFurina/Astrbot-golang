import enum

from astrbot.core.config import AstrBotConfig
from astrbot.core.platform.astr_message_event import AstrMessageEvent

from . import HandlerFilter


class PermissionType(enum.Flag):
    """权限类型。当选择 MEMBER，ADMIN 也可以通过。"""

    ADMIN = enum.auto()
    MEMBER = enum.auto()


class PermissionTypeFilter(HandlerFilter):
    def __init__(
        self, permission_type: PermissionType, raise_error: bool = True
    ) -> None:
        self.permission_type = permission_type
        self.raise_error = raise_error

    def filter(self, event: AstrMessageEvent, cfg: AstrBotConfig) -> bool:
        if self.permission_type == PermissionType.ADMIN:
            if not event.is_admin():
                return False
        return True


__all__ = ["PermissionType", "PermissionTypeFilter"]
