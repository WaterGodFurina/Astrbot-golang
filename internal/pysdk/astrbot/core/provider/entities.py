"""Provider 实体（Go 宿主兼容运行时）。"""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


@dataclass
class ProviderRequest:
    prompt: str | None = None
    session_id: str | None = ""
    image_urls: list[str] = field(default_factory=list)
    audio_urls: list[str] = field(default_factory=list)
    extra_user_content_parts: list = field(default_factory=list)
    func_tool: Any | None = None
    contexts: list[dict] = field(default_factory=list)
    system_prompt: str = ""
    conversation: Any | None = None
    tool_calls_result: Any | None = None
    model: str | None = None


@dataclass
class LLMResponse:
    role: str = "assistant"
    result_chain: Any | None = None
    tools_call_args: list[dict[str, Any]] = field(default_factory=list)
    tools_call_name: list[str] = field(default_factory=list)
    tools_call_ids: list[str] = field(default_factory=list)
    tools_call_extra_content: dict = field(default_factory=dict)
    reasoning_content: str | None = None
    reasoning_signature: str | None = None
    raw_completion: Any | None = None
    _completion_text: str = ""

    @property
    def completion_text(self) -> str:
        if self.result_chain is None:
            return self._completion_text
        return self.result_chain.get_plain_text()

    def __str__(self) -> str:
        return self.completion_text


@dataclass
class ToolCall:
    tool_name: str = ""
    tool_args: dict[str, Any] = field(default_factory=dict)
    tool_id: str = ""


@dataclass
class PluginError:
    handler_name: str = ""
    error: str = ""
