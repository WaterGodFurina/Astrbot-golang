from __future__ import annotations

from astrbot.core.config import AstrBotConfig
from astrbot.core.platform.astr_message_event import AstrMessageEvent

from . import HandlerFilter
from .command import CommandFilter
from .custom_filter import CustomFilter


class CommandGroupFilter(HandlerFilter):
    def __init__(
        self,
        group_name: str,
        alias: set | None = None,
        parent_group: CommandGroupFilter | None = None,
    ) -> None:
        self.group_name = group_name
        self.alias = alias if alias else set()
        self._original_group_name = group_name
        self.sub_command_filters: list[CommandFilter | CommandGroupFilter] = []
        self.custom_filter_list: list[CustomFilter] = []
        self.parent_group = parent_group
        self._cmpl_cmd_names: list | None = None

    def add_sub_command_filter(self, sub_command_filter: CommandFilter | CommandGroupFilter) -> None:
        self.sub_command_filters.append(sub_command_filter)

    def add_custom_filter(self, custom_filter: CustomFilter) -> None:
        self.custom_filter_list.append(custom_filter)

    def get_complete_command_names(self) -> list[str]:
        if self._cmpl_cmd_names is not None:
            return self._cmpl_cmd_names
        parent_cmd_names = (
            self.parent_group.get_complete_command_names() if self.parent_group else []
        )
        if not parent_cmd_names:
            return [self.group_name] + list(self.alias)
        result = []
        candidates = [self.group_name] + list(self.alias)
        for parent_cmd_name in parent_cmd_names:
            for candidate in candidates:
                result.append(parent_cmd_name + " " + candidate)
        self._cmpl_cmd_names = result
        return result

    def custom_filter_ok(self, event: AstrMessageEvent, cfg: AstrBotConfig) -> bool:
        for custom_filter in self.custom_filter_list:
            if not custom_filter.filter(event, cfg):
                return False
        return True

    def find_sub_command(
        self,
        command_str: str,
        event: AstrMessageEvent,
        cfg: AstrBotConfig,
    ) -> CommandFilter | None:
        """在子指令中匹配 command_str（跳过自定义过滤器不通过的）。"""
        cmpl_cmd_names = self.get_complete_command_names()
        command_str = command_str.strip()
        for cmd_filter in self.sub_command_filters:
            if isinstance(cmd_filter, CommandGroupFilter):
                if not cmd_filter.custom_filter_ok(event, cfg):
                    continue
                found = cmd_filter.find_sub_command(command_str, event, cfg)
                if found:
                    return found
            elif isinstance(cmd_filter, CommandFilter):
                full_names = cmd_filter.get_complete_command_names()
                for full_name in full_names:
                    if command_str.startswith(full_name + " ") or command_str == full_name:
                        if cmd_filter.custom_filter_ok(event, cfg):
                            return cmd_filter
        return None

    def has_sub_command_filters(self) -> bool:
        return len(self.sub_command_filters) > 0

    def equals(self, message_str: str) -> bool:
        return message_str in self.get_complete_command_names()

    def startswith(self, message_str: str) -> bool:
        for full_cmd in self.get_complete_command_names():
            if message_str.startswith(full_cmd + " ") or message_str == full_cmd:
                return True
        return False

    def filter(self, event: AstrMessageEvent, cfg: AstrBotConfig) -> bool:
        if not event.is_at_or_wake_command:
            return False
        if not self.custom_filter_ok(event, cfg):
            return False
        return self.startswith(event.get_message_str().strip())


__all__ = ["CommandGroupFilter"]
