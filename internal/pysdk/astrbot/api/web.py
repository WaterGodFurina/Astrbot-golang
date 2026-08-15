"""插件 Web API 请求对象（Go 宿主兼容运行时，无 fastapi/starlette 依赖）。

对齐 Python 本体 `astrbot.api.web`：PluginMultiDict / PluginUploadFile /
PluginRequest / PluginRequestProxy / request / bind_request_context。
"""
from __future__ import annotations

import contextvars
from collections.abc import Callable, KeysView
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Generic, TypeVar, overload

ValueT = TypeVar("ValueT")
DefaultT = TypeVar("DefaultT")


class PluginMultiDict(Generic[ValueT]):
    """Dictionary-like request values that preserves duplicate keys."""

    def __init__(self, pairs: list[tuple[str, ValueT]]) -> None:
        self._pairs = pairs

    def get(self, key: str, default: Any = None, type: Callable | None = None):
        for item_key, item_value in reversed(self._pairs):
            if item_key != key:
                continue
            if type is None:
                return item_value
            try:
                return type(item_value)
            except (TypeError, ValueError):
                return default
        return default

    def getlist(self, key: str) -> list[ValueT]:
        return [item_value for item_key, item_value in self._pairs if item_key == key]

    def keys(self) -> KeysView[str]:
        return dict.fromkeys(item_key for item_key, _ in self._pairs).keys()

    def values(self) -> list[ValueT]:
        return [self[key] for key in self.keys()]

    def items(self) -> list[tuple[str, ValueT]]:
        return [(key, self[key]) for key in self.keys()]

    def multi_items(self) -> list[tuple[str, ValueT]]:
        return list(self._pairs)

    def __contains__(self, key: str) -> bool:
        return any(item_key == key for item_key, _ in self._pairs)

    def __getitem__(self, key: str) -> ValueT:
        value = self.get(key)
        if value is None and key not in self:
            raise KeyError(key)
        return value

    def __bool__(self) -> bool:
        return bool(self._pairs)


class PluginUploadFile:
    """Uploaded file wrapper exposed to plugin Web API handlers."""

    def __init__(self, filename: str, content_type: str = "", content: bytes = b"") -> None:
        self.filename: str | None = filename
        self.content_type: str | None = content_type
        self.content_length: int | None = len(content)
        self._content = content
        self._pos = 0

    async def save(self, destination: str | Path) -> None:
        path = Path(destination)
        with path.open("wb") as output:
            output.write(self._content)

    async def read(self, size: int = -1) -> bytes:
        if size is None or size < 0:
            data = self._content[self._pos:]
        else:
            data = self._content[self._pos:self._pos + size]
        self._pos += len(data)
        return data

    async def write(self, data: bytes) -> None:
        self._content += data
        self.content_length = len(self._content)

    async def seek(self, offset: int) -> None:
        self._pos = offset

    async def close(self) -> None:
        pass


class PluginRequest:
    """Request object exposed to plugin Web API handlers."""

    def __init__(
        self,
        *,
        method: str,
        path: str,
        query: list[tuple[str, str]] | None = None,
        headers: dict[str, str] | None = None,
        body: bytes = b"",
        files: list[tuple[str, PluginUploadFile]] | None = None,
        path_params: dict[str, Any] | None = None,
        plugin_name: str | None = None,
        username: str | None = None,
        client_host: str | None = None,
    ) -> None:
        self.method = method.upper()
        self.path = path
        self.headers = {k.lower(): v for k, v in (headers or {}).items()}
        self.cookies: dict[str, str] = _parse_cookies(self.headers.get("cookie", ""))
        self.content_type: str | None = self.headers.get("content-type")
        self.client_host = client_host
        self.path_params = path_params or {}
        self.plugin_name = plugin_name
        self.username = username
        self.query = PluginMultiDict[str](query or [])
        self._raw_body = body
        self._form_cache: PluginMultiDict[str] | None = None
        self._files_cache: PluginMultiDict[PluginUploadFile] | None = None
        if files:
            self._files_cache = PluginMultiDict(files)

    async def body(self) -> bytes:
        return self._raw_body

    async def json(self, default: DefaultT | None = None) -> Any | DefaultT | None:
        import json as _json

        try:
            return _json.loads(self._raw_body.decode("utf-8"))
        except Exception:
            return default

    async def _load_form_parts(self) -> None:
        if self._form_cache is not None:
            return
        import json as _json

        try:
            data = _json.loads(self._raw_body.decode("utf-8"))
            if isinstance(data, dict):
                self._form_cache = PluginMultiDict([(str(k), str(v)) for k, v in data.items()])
                return
        except Exception:
            pass
        self._form_cache = PluginMultiDict([])

    async def form(self) -> PluginMultiDict[str]:
        await self._load_form_parts()
        assert self._form_cache is not None
        return self._form_cache

    async def files(self) -> PluginMultiDict[PluginUploadFile]:
        if self._files_cache is None:
            self._files_cache = PluginMultiDict([])
        return self._files_cache


def _parse_cookies(header: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for part in header.split(";"):
        part = part.strip()
        if not part or "=" not in part:
            continue
        k, _, v = part.partition("=")
        out[k.strip()] = v.strip()
    return out


_request_var: contextvars.ContextVar[PluginRequest] = contextvars.ContextVar(
    "astrbot_plugin_web_request"
)


class PluginRequestProxy:
    """Typed proxy for the request bound to the current plugin Web handler."""

    def _get_current(self) -> PluginRequest:
        try:
            return _request_var.get()
        except LookupError as exc:
            raise RuntimeError(
                "astrbot.api.web.request is only available inside a plugin Web API "
                "handler."
            ) from exc

    @property
    def method(self) -> str:
        return self._get_current().method

    @property
    def path(self) -> str:
        return self._get_current().path

    @property
    def headers(self) -> dict[str, str]:
        return self._get_current().headers

    @property
    def cookies(self) -> dict[str, str]:
        return self._get_current().cookies

    @property
    def content_type(self) -> str | None:
        return self._get_current().content_type

    @property
    def client_host(self) -> str | None:
        return self._get_current().client_host

    @property
    def path_params(self) -> dict[str, Any]:
        return self._get_current().path_params

    @property
    def plugin_name(self) -> str | None:
        return self._get_current().plugin_name

    @property
    def username(self) -> str | None:
        return self._get_current().username

    @property
    def query(self) -> PluginMultiDict[str]:
        return self._get_current().query

    async def body(self) -> bytes:
        return await self._get_current().body()

    async def json(self, default: DefaultT | None = None) -> Any | DefaultT | None:
        return await self._get_current().json(default=default)

    async def form(self) -> PluginMultiDict[str]:
        return await self._get_current().form()

    async def files(self) -> PluginMultiDict[PluginUploadFile]:
        return await self._get_current().files()

    def __getattr__(self, key: str) -> Any:
        return getattr(self._get_current(), key)


request: PluginRequestProxy = PluginRequestProxy()

WebApiHandler = Callable[..., Any]


@contextmanager
def bind_request_context(request_: PluginRequest):
    token = _request_var.set(request_)
    try:
        yield request_
    finally:
        _request_var.reset(token)
