"""AstrBot 消息组件（Go 宿主兼容运行时）。

与 Python 本体 `astrbot.core.message.components` API 对齐（普通类实现，
不依赖 pydantic）。插件代码在 Python 本体与 Go 宿主子进程中均可运行。
"""
from __future__ import annotations

import base64
import enum
import json
import os
import tempfile
import urllib.request
from pathlib import Path, PurePosixPath


class ComponentType(str, enum.Enum):
    # Basic Segment Types
    Plain = "Plain"  # plain text message
    Image = "Image"  # image
    Record = "Record"  # audio
    Video = "Video"  # video
    File = "File"  # file attachment

    # IM-specific Segment Types
    Face = "Face"  # Emoji segment for Tencent QQ platform
    At = "At"  # mention a user in IM apps
    Node = "Node"  # a node in a forwarded message
    Nodes = "Nodes"  # a forwarded message consisting of multiple nodes
    Poke = "Poke"  # a poke message for Tencent QQ platform
    Reply = "Reply"  # a reply message segment
    Forward = "Forward"  # a forwarded message segment
    RPS = "RPS"  # TODO
    Dice = "Dice"  # TODO
    Shake = "Shake"  # TODO
    Share = "Share"
    Contact = "Contact"  # TODO
    Location = "Location"  # TODO
    Music = "Music"
    Json = "Json"
    Unknown = "Unknown"


def is_file_uri(s: str) -> bool:
    return isinstance(s, str) and s.startswith("file://")


def file_uri_to_path(uri: str) -> str:
    from urllib.parse import unquote, urlparse

    parsed = urlparse(uri)
    path = unquote(parsed.path)
    if parsed.netloc and parsed.netloc not in ("", "localhost"):
        path = f"//{parsed.netloc}{path}"
    return path


def _download_to_temp(url: str, suffix: str = "") -> str:
    """把 http(s) 媒体下载到临时文件，返回本地路径。"""
    os.makedirs(tempfile.gettempdir(), exist_ok=True)
    tmp = tempfile.NamedTemporaryFile(delete=False, suffix=suffix)
    tmp.close()
    try:
        with urllib.request.urlopen(url, timeout=30) as resp, open(tmp.name, "wb") as f:
            f.write(resp.read())
        return tmp.name
    except Exception:
        try:
            os.remove(tmp.name)
        except OSError:
            pass
        raise


class MediaResolver:
    """简化版媒体解析：http(s) 下载到临时目录，本地路径原样返回。"""

    def __init__(self, source: str, media_type: str = "", default_suffix: str = ""):
        self.source = source
        self.media_type = media_type
        self.default_suffix = default_suffix

    async def to_path(self, target_format: str | None = None) -> str:
        source = self.source
        if source.startswith("base64://"):
            suffix = f".{target_format}" if target_format else self.default_suffix or ".bin"
            return self._write_base64(source[len("base64://"):], suffix)
        if source.startswith("data:"):
            import re

            m = re.match(r"data:[^;]+;base64,(.*)", source, re.S)
            if m:
                suffix = f".{target_format}" if target_format else ".bin"
                return self._write_base64(m.group(1), suffix)
        if source.startswith("http://") or source.startswith("https://"):
            suffix = self.default_suffix or ".bin"
            return _download_to_temp(source, suffix)
        if is_file_uri(source):
            return file_uri_to_path(source)
        return source

    async def to_base64(self, target_format: str | None = None) -> str:
        path = await self.to_path(target_format)
        with open(path, "rb") as f:
            return base64.b64encode(f.read()).decode()

    @staticmethod
    def _write_base64(b64: str, suffix: str) -> str:
        import binascii

        try:
            data = base64.b64decode(b64)
        except binascii.Error:
            data = b64.encode()
        tmp = tempfile.NamedTemporaryFile(delete=False, suffix=suffix)
        tmp.write(data)
        tmp.close()
        return tmp.name


class BaseMessageComponent:
    type: ComponentType = ComponentType.Unknown

    def __init__(self, **kwargs):
        for k, v in kwargs.items():
            setattr(self, k, v)

    def __repr__(self):
        parts = []
        for k, v in self.__dict__.items():
            if v is None:
                continue
            text = str(v)
            if len(text) > 64:
                text = f"{text[:64]}...<{len(text)} chars>"
            parts.append(f"{k}={text!r}")
        return f"{self.__class__.__name__}({', '.join(parts)})"

    def toDict(self):
        data = {}
        for k, v in self.__dict__.items():
            if k == "type" or v is None:
                continue
            if k == "_type":
                k = "type"
            if isinstance(v, BaseMessageComponent):
                v = v.toDict()
            elif isinstance(v, list):
                v = [i.toDict() if isinstance(i, BaseMessageComponent) else i for i in v]
            data[k] = v
        return {"type": self.type.lower(), "data": data}

    async def to_dict(self) -> dict:
        return self.toDict()


class Plain(BaseMessageComponent):
    type: ComponentType = ComponentType.Plain
    text: str

    def __init__(self, text: str, convert: bool = True, **_) -> None:
        super().__init__(text=text)

    def toDict(self) -> dict:
        return {"type": "text", "data": {"text": self.text}}

    async def to_dict(self) -> dict:
        return {"type": "text", "data": {"text": self.text}}


class Face(BaseMessageComponent):
    type: ComponentType = ComponentType.Face
    id: int

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Record(BaseMessageComponent):
    type: ComponentType = ComponentType.Record
    file: str | None = ""
    url: str | None = ""
    text: str | None = None
    path: str | None = None

    def __init__(self, file: str | None, **_) -> None:
        super().__init__(file=file, **_)

    @staticmethod
    def fromFileSystem(path, **_):
        file_path = Path(path).resolve(strict=False)
        return Record(file=file_path.as_uri(), path=str(file_path), **_)

    @staticmethod
    def fromURL(url: str, **_):
        if url.startswith("http://") or url.startswith("https://"):
            return Record(file=url, **_)
        raise Exception("not a valid url")

    @staticmethod
    def fromBase64(bs64_data: str, **_):
        return Record(file=f"base64://{bs64_data}", **_)

    async def convert_to_file_path(self) -> str:
        return await MediaResolver(self.file or self.url or "", media_type="audio", default_suffix=".wav").to_path()

    async def convert_to_base64(self) -> str:
        return await MediaResolver(self.file or self.url or "", media_type="audio", default_suffix=".wav").to_base64()


class Video(BaseMessageComponent):
    type: ComponentType = ComponentType.Video
    file: str
    url: str | None = ""
    cover: str | None = ""
    path: str | None = ""

    def __init__(self, file: str, **_) -> None:
        super().__init__(file=file, **_)

    @staticmethod
    def fromFileSystem(path, **_):
        file_path = Path(path).resolve(strict=False)
        return Video(file=file_path.as_uri(), path=str(file_path), **_)

    @staticmethod
    def fromURL(url: str, **_):
        if url.startswith("http://") or url.startswith("https://"):
            return Video(file=url, **_)
        raise Exception("not a valid url")

    @staticmethod
    def fromBase64(base64_data: str, **_):
        return Video(file=f"base64://{base64_data}", **_)

    async def convert_to_file_path(self) -> str:
        return await MediaResolver(self.file or self.url or "", media_type="video", default_suffix=".mp4").to_path()

    async def convert_to_base64(self) -> str:
        return await MediaResolver(self.file or self.url or "", media_type="video", default_suffix=".mp4").to_base64()


class At(BaseMessageComponent):
    type: ComponentType = ComponentType.At
    qq: int | str
    name: str | None = ""

    def __init__(self, **_) -> None:
        super().__init__(**_)

    def toDict(self):
        return {"type": "at", "data": {"qq": str(self.qq)}}


class AtAll(At):
    qq: str = "all"

    def __init__(self, **_) -> None:
        super().__init__(**_)


class RPS(BaseMessageComponent):
    type: ComponentType = ComponentType.RPS

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Dice(BaseMessageComponent):
    type: ComponentType = ComponentType.Dice

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Shake(BaseMessageComponent):
    type: ComponentType = ComponentType.Shake

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Share(BaseMessageComponent):
    type: ComponentType = ComponentType.Share
    url: str
    title: str
    content: str | None = ""
    image: str | None = ""

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Contact(BaseMessageComponent):
    type: ComponentType = ComponentType.Contact
    _type: str
    id: int | None = 0

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Location(BaseMessageComponent):
    type: ComponentType = ComponentType.Location
    lat: float
    lon: float
    title: str | None = ""
    content: str | None = ""

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Music(BaseMessageComponent):
    type: ComponentType = ComponentType.Music
    _type: str
    id: int | None = 0
    url: str | None = ""
    audio: str | None = ""
    title: str | None = ""
    content: str | None = ""
    image: str | None = ""

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Image(BaseMessageComponent):
    type: ComponentType = ComponentType.Image
    file: str | None = ""
    _type: str | None = ""
    url: str | None = ""
    path: str | None = ""

    def __init__(self, file: str | None, **_) -> None:
        super().__init__(file=file, **_)

    @staticmethod
    def fromURL(url: str, **_):
        if url.startswith("http://") or url.startswith("https://"):
            return Image(file=url, **_)
        raise Exception("not a valid url")

    @staticmethod
    def fromFileSystem(path, **_):
        file_path = Path(path).resolve(strict=False)
        return Image(file=file_path.as_uri(), path=str(file_path), **_)

    @staticmethod
    def fromBase64(base64: str, **_):
        return Image(f"base64://{base64}", **_)

    @staticmethod
    def fromBytes(byte: bytes):
        return Image.fromBase64(base64.b64encode(byte).decode())

    @staticmethod
    def fromIO(IO):
        return Image.fromBytes(IO.read())

    async def convert_to_file_path(self) -> str:
        return await MediaResolver(self.url or self.file or "", media_type="image").to_path()

    async def convert_to_base64(self) -> str:
        return await MediaResolver(self.url or self.file or "", media_type="image").to_base64()


class Reply(BaseMessageComponent):
    type: ComponentType = ComponentType.Reply
    id: str | int
    chain: list["BaseMessageComponent"] | None = []
    sender_id: int | None | str = 0
    sender_nickname: str | None = ""
    time: int | None = 0
    message_str: str | None = ""
    text: str | None = ""
    qq: int | None = 0
    seq: int | None = 0

    def __init__(self, **_) -> None:
        super().__init__(**_)

    def toDict(self):
        return {"type": "reply", "data": {"id": str(self.id)}}


class Poke(BaseMessageComponent):
    type: ComponentType = ComponentType.Poke
    _type: str | int = "126"
    id: int | str | None = 0
    qq: int | str | None = 0

    def __init__(self, poke_type: str | int | None = None, **_) -> None:
        legacy_type = _.pop("type", None)
        if poke_type is None:
            poke_type = legacy_type
        if poke_type in (None, "", "poke", "Poke"):
            poke_type = "126"
        super().__init__(_type=str(poke_type), **_)

    def target_id(self) -> str | None:
        for value in (self.id, self.qq):
            if value is None:
                continue
            text = str(value).strip()
            if text and text != "0":
                return text
        return None

    def toDict(self):
        target_id = self.target_id()
        data = {"type": str(self._type or "126")}
        if target_id:
            data["id"] = target_id
        return {"type": "poke", "data": data}


class Forward(BaseMessageComponent):
    type: ComponentType = ComponentType.Forward
    id: str

    def __init__(self, **_) -> None:
        super().__init__(**_)


class Node(BaseMessageComponent):
    type: ComponentType = ComponentType.Node
    id: int | None = 0
    name: str | None = ""
    uin: str | None = "0"
    content: list["BaseMessageComponent"] = []
    seq: str | list | None = ""
    time: int | None = 0

    def __init__(self, content: list["BaseMessageComponent"], **_) -> None:
        if isinstance(content, Node):
            content = [content]
        super().__init__(content=content, **_)

    async def to_dict(self):
        data_content = []
        for comp in self.content:
            if isinstance(comp, (Image, Record)):
                try:
                    bs64 = await comp.convert_to_base64()
                    data_content.append({"type": comp.type.lower(), "data": {"file": f"base64://{bs64}"}})
                except Exception:
                    data_content.append(comp.toDict())
            elif isinstance(comp, (Plain, File, Node, Nodes)):
                data_content.append(await comp.to_dict())
            else:
                data_content.append(comp.toDict())
        return {"type": "node", "data": {"user_id": str(self.uin), "nickname": self.name, "content": data_content}}


class Nodes(BaseMessageComponent):
    type: ComponentType = ComponentType.Nodes
    nodes: list[Node]

    def __init__(self, nodes: list[Node], **_) -> None:
        super().__init__(nodes=nodes, **_)

    def toDict(self):
        return {"messages": [{"type": "node", "data": {"user_id": str(n.uin), "nickname": n.name, "content": [c.toDict() for c in n.content]}} for n in self.nodes]}

    async def to_dict(self) -> dict:
        return {"messages": [await n.to_dict() for n in self.nodes]}


class Json(BaseMessageComponent):
    type: ComponentType = ComponentType.Json
    data: dict

    def __init__(self, data: str | dict, **_) -> None:
        if isinstance(data, str):
            data = json.loads(data)
        super().__init__(data=data, **_)


class Unknown(BaseMessageComponent):
    type: ComponentType = ComponentType.Unknown
    text: str


def _sanitize_file_component_name(name: str | None) -> str:
    if not name:
        return "file"
    normalized = str(name).replace("\\", "/")
    basename = PurePosixPath(normalized).name.replace("\x00", "").strip()
    for char in ':*?"<>|':
        basename = basename.replace(char, "_")
    if basename in {"", ".", ".."}:
        return "file"
    return basename


class File(BaseMessageComponent):
    type: ComponentType = ComponentType.File
    name: str | None = ""
    file_: str | None = ""
    url: str | None = ""

    def __init__(self, name: str, file: str = "", url: str = "") -> None:
        super().__init__(name=name, file_=file, url=url)

    @property
    def file(self) -> str:
        if self.file_:
            path = file_uri_to_path(self.file_) if is_file_uri(self.file_) else self.file_
            if os.path.exists(path):
                return os.path.abspath(path)
            if self.file_.startswith("http"):
                return _download_to_temp(self.file_)
            return os.path.abspath(path)
        if self.url and (self.url.startswith("http://") or self.url.startswith("https://")):
            return _download_to_temp(self.url)
        return self.file_ or self.url or ""

    def toDict(self):
        data = {"name": self.name or "file"}
        if self.file_:
            data["file"] = self.file_
        if self.url:
            data["url"] = self.url
        return {"type": "file", "data": data}

    async def to_dict(self) -> dict:
        return self.toDict()
