"""Star 元数据（Go 宿主兼容运行时）。"""
from __future__ import annotations

from dataclasses import dataclass, field
from types import ModuleType
from typing import TYPE_CHECKING

if TYPE_CHECKING:
    from .base import Star


star_registry: list["StarMetadata"] = []
star_map: dict[str, "StarMetadata"] = {}
"""key 是模块路径，__module__"""


@dataclass
class StarMetadata:
    """插件的元数据。"""

    name: str | None = None
    author: str | None = None
    desc: str | None = None
    short_desc: str | None = None
    version: str | None = None
    repo: str | None = None

    star_cls_type: type["Star"] | None = None
    module_path: str | None = None
    star_cls: "Star" | None = None
    module: ModuleType | None = None
    root_dir_name: str | None = None
    reserved: bool = False
    activated: bool = True

    config: dict | None = None

    star_handler_full_names: list[str] = field(default_factory=list)
    display_name: str | None = None
    logo_path: str | None = None
    support_platforms: list[str] = field(default_factory=list)
    astrbot_version: str | None = None
    i18n: dict[str, dict] = field(default_factory=dict)
    pages: list[dict] = field(default_factory=list)

    @property
    def plugin_id(self) -> str:
        p_name = (self.name or "unknown").lower().replace("/", "_")
        p_author = (self.author or "unknown").lower().replace("/", "_")
        return f"{p_author}/{p_name}"

    def __str__(self) -> str:
        return f"Plugin {self.name} ({self.version}) by {self.author}: {self.desc}"
