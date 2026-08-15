"""sdk.Event / 消息组件 JSON 与 Python 对象互转（对齐 Go SDK Component 字段）。"""
from __future__ import annotations

from astrbot.core.message.components import (
    At,
    AtAll,
    BaseMessageComponent,
    Face,
    File,
    Forward,
    Image,
    Json,
    Node,
    Nodes,
    Plain,
    Record,
    Reply,
    Unknown,
    Video,
)
from astrbot.core.message.message_event_result import MessageEventResult
from astrbot.core.platform.astr_message_event import AstrMessageEvent


def component_from_json(data: dict) -> BaseMessageComponent:
    """把宿主 sdk.Component JSON 还原为 Python 组件对象。"""
    if not data:
        return Unknown(text="")
    ctype = str(data.get("type", "Unknown"))
    text = data.get("text", "")
    if ctype == "Plain":
        return Plain(text=text)
    if ctype == "At":
        target = data.get("target_id", "")
        if target in ("all", "0"):
            return AtAll()
        return At(qq=target or "0", name=data.get("name", ""))
    if ctype == "AtAll":
        return AtAll()
    if ctype == "Image":
        return Image(
            file=data.get("file") or None,
            url=data.get("url") or None,
            path=data.get("path") or None,
        )
    if ctype == "Record":
        return Record(
            file=data.get("file") or None,
            url=data.get("url") or None,
            path=data.get("path") or None,
        )
    if ctype == "File":
        return File(
            name=data.get("name") or "",
            file=data.get("file") or data.get("path") or "",
            url=data.get("url") or "",
        )
    if ctype == "Video":
        return Video(
            file=data.get("file") or data.get("url") or "",
            url=data.get("url") or None,
            path=data.get("path") or None,
        )
    if ctype == "Face":
        return Face(id=int(data.get("id", 0) or 0))
    if ctype == "Emoji":
        comp = Unknown(text="")
        comp.type = "Emoji"
        comp.id = data.get("id", "")
        comp.url = data.get("url", "")
        return comp
    if ctype == "Json":
        return Json(data=data.get("data") or {})
    if ctype == "Reply":
        return Reply(id=data.get("id", ""), message_str=data.get("text", ""))
    if ctype == "Node":
        return Node(content=[], uin=data.get("uin", "0"), name=data.get("name", ""))
    comp = Unknown(text=text)
    comp.raw = data
    return comp


def component_to_json(comp: BaseMessageComponent) -> dict:
    """把 Python 组件对象转为宿主 sdk.Component JSON。"""
    ctype = str(getattr(comp, "type", "Unknown"))
    if isinstance(comp, Plain):
        return {"type": "Plain", "text": comp.text}
    if isinstance(comp, AtAll):
        return {"type": "AtAll"}
    if isinstance(comp, At):
        return {"type": "At", "target_id": str(comp.qq), "name": comp.name or ""}
    if isinstance(comp, Image):
        out = {"type": "Image"}
        file_ = getattr(comp, "file", None)
        if file_ and (file_.startswith("http://") or file_.startswith("https://")):
            out["url"] = file_
            return out
        if getattr(comp, "url", None):
            out["url"] = comp.url
        if getattr(comp, "path", None):
            out["path"] = comp.path
        if file_:
            if file_.startswith("base64://"):
                out["base64"] = file_[len("base64://"):]
            else:
                out["file"] = file_
        return out
    if isinstance(comp, Record):
        out = {"type": "Record"}
        file_ = getattr(comp, "file", None)
        if file_ and (file_.startswith("http://") or file_.startswith("https://")):
            out["url"] = file_
            return out
        if getattr(comp, "url", None):
            out["url"] = comp.url
        if getattr(comp, "path", None):
            out["path"] = comp.path
        if file_:
            if file_.startswith("base64://"):
                out["base64"] = file_[len("base64://"):]
            else:
                out["file"] = file_
        return out
    if isinstance(comp, File):
        out = {"type": "File", "name": comp.name or "file"}
        if getattr(comp, "url", None):
            out["url"] = comp.url
        file_ = getattr(comp, "file_", None)
        if file_:
            out["file"] = file_
        if getattr(comp, "file", None) and not file_:
            out["path"] = comp.file
        return out
    if isinstance(comp, Video):
        out = {"type": "Video"}
        if getattr(comp, "url", None):
            out["url"] = comp.url
        if getattr(comp, "path", None):
            out["path"] = comp.path
        if getattr(comp, "file", None) and not getattr(comp, "url", None):
            if comp.file.startswith("base64://"):
                out["base64"] = comp.file[len("base64://"):]
            else:
                out["path"] = comp.file
        return out
    if isinstance(comp, Face):
        return {"type": "Face", "id": str(getattr(comp, "id", 0))}
    if getattr(comp, "type", None) == "Emoji":
        return {"type": "Emoji", "id": str(getattr(comp, "id", "")), "url": getattr(comp, "url", "")}
    if isinstance(comp, Json):
        return {"type": "Json", "data": comp.data or {}}
    if isinstance(comp, Reply):
        return {"type": "Reply", "id": str(getattr(comp, "id", "")), "text": getattr(comp, "message_str", "") or ""}
    if isinstance(comp, Forward):
        return {"type": "Forward", "id": str(getattr(comp, "id", ""))}
    if isinstance(comp, Node):
        return {
            "type": "Node",
            "uin": str(getattr(comp, "uin", "0")),
            "name": getattr(comp, "name", "") or "",
            "data": {"content": [component_to_json(c) for c in (comp.content or [])]},
        }
    if isinstance(comp, Nodes):
        return {
            "type": "Nodes",
            "data": {"nodes": [component_to_json(n) for n in (comp.nodes or [])]},
        }
    if isinstance(comp, Unknown):
        return {"type": "Unknown", "text": getattr(comp, "text", "")}
    return {"type": "Unknown", "text": getattr(comp, "text", "") or str(comp)}


def result_to_json(result: MessageEventResult | str | None) -> list[dict]:
    """把插件 handler 返回的结果转为 Component JSON 列表。

    返回 (chain_json, stop)。str → Plain；None → 空；MessageEventResult → chain。
    """
    if result is None:
        return [], False
    if isinstance(result, str):
        return [{"type": "Plain", "text": result}], False
    if isinstance(result, MessageEventResult):
        stop = bool(result.result_type and str(getattr(result.result_type, "value", "")) == "stop")
        return [component_to_json(c) for c in (result.chain or [])], stop
    if isinstance(result, dict):
        return [result], False
    return [], False
