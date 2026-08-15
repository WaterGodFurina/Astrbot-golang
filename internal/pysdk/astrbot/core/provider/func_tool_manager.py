"""LLM 工具注册表（Go 宿主兼容运行时，docstring 解析用内置实现替代 docstring_parser）。"""
from __future__ import annotations

import copy
import re
from typing import Any, Callable

SUPPORTED_TYPES = ["string", "number", "object", "array", "boolean"]

PY_TO_JSON_TYPE = {
    "int": "number",
    "float": "number",
    "bool": "boolean",
    "str": "string",
    "dict": "object",
    "list": "array",
    "tuple": "array",
    "set": "array",
}


class DocParam:
    def __init__(self, arg_name: str, type_name: str | None, description: str):
        self.arg_name = arg_name
        self.type_name = type_name
        self.description = description


class DocString:
    def __init__(self):
        self.description: str | None = None
        self.params: list[DocParam] = []


def parse_docstring(doc: str) -> DocString:
    """简化版 docstring 解析：支持 Google 风格 Args: 与 :param 风格。"""
    result = DocString()
    if not doc:
        return result
    lines = doc.splitlines()
    param_pattern = re.compile(r"^\s*(?:Args|参数)[:\s]*(.*)$", re.I)
    param_line = re.compile(r"^\s*(\w+)\s*(?:\(([^)]*)\))?\s*:\s*(.*)$")
    colon_param = re.compile(r"^\s*:param\s+(\w+)(?:\s*:\s*(\w+))?\s*:\s*(.*)$")

    description_lines: list[str] = []
    in_args = False
    current_type_ctx = ""
    for raw in lines:
        line = raw.strip()
        if not line:
            if not in_args and description_lines and description_lines[-1]:
                description_lines.append("")
            continue
        m = colon_param.match(raw)
        if m:
            in_args = True
            result.params.append(DocParam(m.group(1), m.group(2), m.group(3).strip()))
            continue
        m = param_pattern.match(raw)
        if m:
            in_args = True
            if m.group(1).strip().lower().startswith("type"):
                continue
            continue
        if in_args:
            m = param_line.match(raw)
            if m and (m.group(2) or ":" in raw):
                result.params.append(DocParam(m.group(1), m.group(2), m.group(3).strip()))
                current_type_ctx = m.group(2) or ""
                continue
            if current_type_ctx and result.params:
                result.params[-1].description += " " + line
            continue
        description_lines.append(line)

    result.description = "\n".join(description_lines).strip()
    return result


class FuncTool:
    def __init__(self, name: str, parameters: dict, description: str, handler: Callable):
        self.name = name
        self.parameters = parameters
        self.description = description
        self.handler = handler
        self.active = True  # activate_llm_tool / deactivate_llm_tool 控制

    def to_schema(self) -> dict:
        schema = {
            "type": "function",
            "function": {
                "name": self.name,
                "description": self.description or "",
                "parameters": self.parameters or {"type": "object", "properties": {}},
            },
        }
        return schema


class ToolSet:
    def __init__(self, tools: list | None = None):
        self.tools = tools if tools is not None else []

    def add_tool(self, tool) -> None:
        self.tools.append(tool)


class FunctionToolManager:
    def __init__(self) -> None:
        self.func_list: list[FuncTool] = []

    def spec_to_func(self, name: str, func_args: list[dict], desc: str, handler: Callable) -> FuncTool:
        params = {"type": "object", "properties": {}}
        for param in func_args:
            p = copy.deepcopy(param)
            p.pop("name", None)
            params["properties"][param["name"]] = p
        return FuncTool(name=name, parameters=params, description=desc, handler=handler)

    def add_func(self, name: str, func_args: list, desc: str, handler: Callable) -> None:
        self.remove_func(name)
        self.func_list.append(self.spec_to_func(name=name, func_args=func_args, desc=desc, handler=handler))

    def remove_func(self, name: str) -> None:
        self.func_list = [f for f in self.func_list if f.name != name]

    def get_func_by_name(self, name: str) -> FuncTool | None:
        for f in self.func_list:
            if f.name == name:
                return f
        return None

    def activate(self, name: str) -> bool:
        for f in self.func_list:
            if f.name == name:
                f.active = True
                return True
        return False

    def deactivate(self, name: str) -> bool:
        for f in self.func_list:
            if f.name == name:
                f.active = False
                return True
        return False

    def list_funcs(self, only_active: bool = False) -> list[FuncTool]:
        if not only_active:
            return list(self.func_list)
        return [f for f in self.func_list if f.active]


llm_tools = FunctionToolManager()
"""全局 LLM 函数工具注册表"""
