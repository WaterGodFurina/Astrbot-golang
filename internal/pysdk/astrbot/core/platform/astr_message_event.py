"""AstrMessageEvent：Go 宿主兼容运行时的事件对象。

API 与 Python 本体 `astrbot.core.platform.AstrMessageEvent` 对齐。
"""
from __future__ import annotations

import abc
import logging
from time import time
from typing import Any

from astrbot.core.message.components import BaseMessageComponent
from astrbot.core.message.message_event_result import (
    MessageChain,
    MessageEventResult,
)
from astrbot.core.platform.message_type import MessageType
from astrbot.core.platform.message_session import MessageSession

from .astrbot_message import AstrBotMessage, Group, MessageMember
from .platform_metadata import PlatformMetadata

logger = logging.getLogger("astrbot")


class AstrMessageEvent(abc.ABC):
    def __init__(
        self,
        message_str: str,
        message_obj: AstrBotMessage,
        platform_meta: PlatformMetadata,
        session_id: str,
    ) -> None:
        self.message_str = message_str
        self.message_obj = message_obj
        self.platform_meta = platform_meta
        self.role = "member"
        self.is_wake = False
        self.is_at_or_wake_command = False
        self._extras: dict[str, Any] = {}
        self._force_stopped: bool = False

        message_type = getattr(message_obj, "type", None)
        if not isinstance(message_type, MessageType):
            try:
                message_type = MessageType(str(message_type))
            except (ValueError, TypeError, AttributeError):
                logger.warning(
                    f"Failed to convert message type {message_obj.type!r} to MessageType. "
                    f"Falling back to FRIEND_MESSAGE."
                )
                message_type = MessageType.FRIEND_MESSAGE
        self.session = MessageSession(
            platform_name=platform_meta.id,
            message_type=message_type,
            session_id=session_id,
        )
        self._result: MessageEventResult | None = None
        self.created_at = time()
        self._has_send_oper = False
        self.call_llm = False
        self.plugins_name: list[str] | None = None
        self.platform = platform_meta

    @property
    def unified_msg_origin(self) -> str:
        return str(self.session)

    @unified_msg_origin.setter
    def unified_msg_origin(self, value: str) -> None:
        if isinstance(value, str):
            try:
                self.session = MessageSession.from_str(value)
            except BaseException:
                pass

    @property
    def session_id(self) -> str:
        return self.session.session_id

    @session_id.setter
    def session_id(self, value: str) -> None:
        self.session.session_id = value

    def get_platform_name(self):
        return self.platform_meta.name

    def get_platform_id(self):
        return self.platform_meta.id

    def get_message_str(self) -> str:
        return self.message_str

    def _outline_chain(self, chain: list[BaseMessageComponent] | None) -> str:
        if not chain:
            return ""
        from astrbot.core.message.components import Image, Plain

        parts = []
        for i in chain:
            if isinstance(i, Plain):
                parts.append(i.text)
            elif isinstance(i, Image):
                parts.append("[图片]")
        return " ".join(parts)

    def get_message_outline(self) -> str:
        return self._outline_chain(self.get_messages())

    def get_messages(self) -> list[BaseMessageComponent]:
        if self.message_obj:
            return self.message_obj.message
        return []

    def get_message_type(self) -> MessageType:
        return self.session.message_type

    def get_session_id(self) -> str:
        return self.session.session_id

    def get_group_id(self) -> str:
        if self.message_obj and self.message_obj.group:
            return self.message_obj.group.group_id
        return ""

    def get_self_id(self) -> str:
        return self.message_obj.self_id if self.message_obj else ""

    def get_sender_id(self) -> str:
        if self.message_obj and self.message_obj.sender:
            return self.message_obj.sender.user_id
        return ""

    def get_sender_name(self) -> str:
        if self.message_obj and self.message_obj.sender:
            return self.message_obj.sender.nickname or ""
        return ""

    def set_extra(self, key, value) -> None:
        self._extras[key] = value

    def get_extra(self, key: str | None = None, default=None) -> Any:
        if key is None:
            return self._extras
        return self._extras.get(key, default)

    def clear_extra(self) -> None:
        self._extras.clear()

    def is_private_chat(self) -> bool:
        return self.session.message_type == MessageType.FRIEND_MESSAGE

    def is_wake_up(self) -> bool:
        return self.is_wake

    def is_admin(self) -> bool:
        return self.role == "admin"

    def set_result(self, result: MessageEventResult | str) -> None:
        if isinstance(result, str):
            result = MessageEventResult().message(result)
        self._result = result

    def stop_event(self) -> None:
        if self._result:
            self._result.set_result_type(MessageEventResult.STOP)
        else:
            self._force_stopped = True

    def continue_event(self) -> None:
        self._force_stopped = False
        if self._result:
            self._result.continue_event()

    def is_stopped(self) -> bool:
        if self._force_stopped:
            return True
        if self._result:
            return self._result.is_stopped()
        return False

    def should_call_llm(self, call_llm: bool) -> None:
        self.call_llm = call_llm

    def get_result(self) -> MessageEventResult | None:
        return self._result

    def clear_result(self) -> None:
        self._result = None

    def make_result(self) -> MessageEventResult:
        self._result = MessageEventResult()
        return self._result

    def plain_result(self, text: str) -> MessageEventResult:
        return MessageEventResult().message(text)

    def image_result(self, url_or_path: str) -> MessageEventResult:
        from astrbot.core.message.components import Image

        if url_or_path.startswith("http"):
            return MessageEventResult().url_image(url_or_path)
        return MessageEventResult().file_image(url_or_path)

    def chain_result(self, chain: list[BaseMessageComponent]) -> MessageEventResult:
        return MessageEventResult(chain)

    def request_llm(
        self,
        prompt: str,
        func_tool_manager=None,
        tool_set=None,
        session_id: str = "",
        image_urls: list[str] | None = None,
        audio_urls: list[str] | None = None,
        contexts: list | None = None,
        system_prompt: str = "",
        conversation=None,
    ):
        """创建 LLM 请求。Go 宿主桥：返回的 ProviderRequest 会以
        llm_continue 方式反馈宿主继续走默认 LLM（提示词为本消息原文）。"""
        from astrbot.core.provider.entities import ProviderRequest

        if image_urls is None:
            image_urls = []
        if audio_urls is None:
            audio_urls = []
        if contexts is None:
            contexts = []
        return ProviderRequest(
            prompt=prompt,
            session_id=session_id,
            image_urls=image_urls,
            audio_urls=audio_urls,
            func_tool=tool_set,
            contexts=contexts,
            system_prompt=system_prompt,
            conversation=conversation,
        )

    async def send(self, message: MessageChain) -> None:
        """发送消息到消息平台（经宿主 HostService.SendMessage）。"""
        from astrbot.core.star.context import get_host_bridge

        bridge = get_host_bridge()
        if bridge is None:
            logger.warning("send(): 宿主桥未就绪，消息未发送")
            return
        await bridge.send_message(self.session, message)

    async def react(self, emoji: str) -> None:
        """对当前消息添加表情回应（宿主 PlatformManager.React，平台不支持时
        返回失败并告警）。"""
        from astrbot.core.star.context import get_host_bridge

        bridge = get_host_bridge()
        if bridge is None:
            logger.warning("react(): 宿主桥未就绪")
            return
        try:
            ok = await bridge.react_message(self, emoji)
            if not ok:
                logger.warning(f"react(): 平台不支持表情回应或消息不存在（emoji={emoji}）")
        except Exception as e:
            logger.warning(f"react() 失败: {e}")

    async def get_group(self, group_id: str | None = None, **kwargs) -> Group | None:
        """获取群信息（经宿主 CallAction；平台不支持时返回 None）。"""
        from astrbot.core.star.context import get_host_bridge

        bridge = get_host_bridge()
        if bridge is None:
            return None
        gid = group_id or self.get_group_id()
        if not gid:
            return None
        try:
            info = await bridge.call_action_async(
                self.session.platform_id, "get_group_info", {"group_id": int(gid) if str(gid).isdigit() else gid}
            )
            if not info:
                return None
            return Group(
                group_id=str(info.get("group_id", gid)),
                group_name=info.get("group_name") or info.get("name") or "",
            )
        except Exception as e:
            logger.warning(f"get_group({gid}) 失败: {e}")
            return None

    @classmethod
    def from_event_json(cls, data: dict) -> "AstrMessageEvent":
        """从宿主 sdk.Event JSON 重建事件对象。"""
        from astrbot.core.message.components import ComponentType
        from astrbot._bridge.serialize import component_from_json

        platform = data.get("platform", "")
        self_id = data.get("self_id", "")
        sender_id = data.get("sender_id", "")
        sender_name = data.get("sender_name", "")
        conv_id = data.get("conv_id", "")
        is_group = data.get("is_group", False)
        is_at_bot = data.get("is_at_bot", False)
        is_admin = data.get("is_admin", False)
        message_str = data.get("message_str", "")
        plain_text = data.get("plain_text", "")
        raw_message = data.get("raw_message")
        message_id = data.get("message_id", "")
        timestamp = data.get("timestamp", 0)
        metadata = data.get("metadata") or {}

        msg_type = MessageType.GROUP_MESSAGE if is_group else MessageType.FRIEND_MESSAGE
        chain = [component_from_json(c) for c in (data.get("chain") or [])]

        obj = AstrBotMessage()
        obj.type = msg_type
        obj.self_id = self_id
        obj.session_id = conv_id
        obj.message_id = message_id
        obj.sender = MessageMember(user_id=sender_id, nickname=sender_name)
        obj.message = chain
        obj.message_str = message_str
        obj.raw_message = raw_message
        obj.timestamp = timestamp or int(time())
        if is_group:
            obj.group = Group(group_id=conv_id)

        meta = PlatformMetadata(
            name=platform,
            description="",
            id=platform,
        )
        event = cls(message_str or plain_text, obj, meta, conv_id)
        event.is_at_or_wake_command = metadata.get("is_at_or_wake_command", False)
        event.is_wake = metadata.get("is_wake", False)
        event.call_llm = bool(metadata.get("call_llm", False))
        event.role = "admin" if is_admin else "member"
        event.is_at_bot = is_at_bot
        return event
