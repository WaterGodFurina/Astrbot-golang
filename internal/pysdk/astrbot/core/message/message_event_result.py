"""消息事件结果（MessageChain / MessageEventResult / CommandResult）。"""
from __future__ import annotations

import enum
from typing import TYPE_CHECKING

from astrbot.core.message.components import (
    At,
    BaseMessageComponent,
    Image,
    Plain,
)

if TYPE_CHECKING:
    from collections.abc import AsyncGenerator


class MessageChain:
    """一个消息链。"""

    def __init__(self, chain: list[BaseMessageComponent] | None = None) -> None:
        self.chain = chain if chain is not None else []

    def derive(self, chain: list[BaseMessageComponent] | None = None) -> "MessageChain":
        if chain is None:
            chain = list(self.chain)
        return MessageChain(chain)

    def message(self, message: str):
        self.chain.append(Plain(message))
        return self

    def at(self, name: str, qq: str | int):
        self.chain.append(At(qq=qq, name=name))
        return self

    def at_all(self):
        from astrbot.core.message.components import AtAll

        self.chain.append(AtAll())
        return self

    def error(self, message: str):
        self.chain.append(Plain(message))
        return self

    def url_image(self, url: str):
        self.chain.append(Image.fromURL(url))
        return self

    def file_image(self, path: str):
        self.chain.append(Image.fromFileSystem(path))
        return self

    def base64_image(self, base64_str: str):
        self.chain.append(Image.fromBase64(base64_str))
        return self

    def use_t2i(self, use_t2i: bool):
        # 兼容标记：Go 宿主管线自行决定 t2i，插件无需显式开启
        return self

    def use_markdown(self, use: bool | None = True):
        return self

    def get_plain_text(self, with_other_comps_mark: bool = False) -> str:
        ret = ""
        for comp in self.chain:
            if isinstance(comp, Plain):
                ret += comp.text
            elif with_other_comps_mark and not isinstance(comp, Image):
                ret += "[其他组件]"
        return ret

    def squash_plain(self):
        # 相邻 Plain 合并（与本体语义一致）
        squashed: list[BaseMessageComponent] = []
        for comp in self.chain:
            if isinstance(comp, Plain) and squashed and isinstance(squashed[-1], Plain):
                squashed[-1].text += comp.text
            else:
                squashed.append(comp)
        self.chain = squashed
        return self


class EventResultType(enum.Enum):
    """事件结果类型"""

    CONTINUE = "continue"
    """事件继续传播"""
    STOP = "stop"
    """事件停止传播"""
    ASYNC_STREAM = "async_stream"
    """异步流式传输"""
    ASYNC = "async"
    """异步传输"""


class ResultContentType(enum.Enum):
    """结果的内容类型"""

    TEXT = "text"
    IMAGE = "image"
    OTHER = "other"
    UNKNOWN = "unknown"


class MessageEventResult(MessageChain):
    def __init__(self, chain: list[BaseMessageComponent] | None = None):
        super().__init__(chain)
        self.result_type = EventResultType.CONTINUE
        self.result_content_type = ResultContentType.UNKNOWN
        self._async_stream: AsyncGenerator | None = None
        self._is_llm_result = False
        self._is_model_result = False
        self._console_log = ""

    def set_result_type(self, result_type: EventResultType) -> "MessageEventResult":
        self.result_type = result_type
        return self

    def set_console_log(self, log: str) -> "MessageEventResult":
        self._console_log = log
        return self

    def stop_event(self) -> "MessageEventResult":
        self.result_type = EventResultType.STOP
        return self

    def continue_event(self) -> "MessageEventResult":
        self.result_type = EventResultType.CONTINUE
        return self

    def is_stopped(self) -> bool:
        return self.result_type == EventResultType.STOP

    def set_async_stream(self, stream: AsyncGenerator) -> "MessageEventResult":
        self._async_stream = stream
        self.result_type = EventResultType.ASYNC_STREAM
        return self

    def set_result_content_type(self, typ: ResultContentType) -> "MessageEventResult":
        self.result_content_type = typ
        return self

    def is_llm_result(self) -> bool:
        return self._is_llm_result

    def is_model_result(self) -> bool:
        return self._is_model_result

    def mark_llm_result(self) -> "MessageEventResult":
        self._is_llm_result = True
        return self

    def mark_model_result(self) -> "MessageEventResult":
        self._is_model_result = True
        return self


class CommandResult(MessageEventResult):
    def __init__(self, command: str, chain: list[BaseMessageComponent] | None = None):
        super().__init__(chain)
        self.command = command
        self.result_type = EventResultType.STOP
