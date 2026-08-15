"""常驻 asyncio event loop：插件 initialize() 与所有 async handler 在此执行。

插件可能创建长期任务（asyncio.create_task），因此进程内必须有一个常驻
event loop（grpc-python 的 RPC 线程池无法承载）。
"""
from __future__ import annotations

import asyncio
import logging
import threading

logger = logging.getLogger("astrbot")

_loop: asyncio.AbstractEventLoop | None = None
_thread: threading.Thread | None = None


def start() -> asyncio.AbstractEventLoop:
    global _loop, _thread
    if _loop is not None:
        return _loop
    _loop = asyncio.new_event_loop()

    def run():
        asyncio.set_event_loop(_loop)
        _loop.run_forever()

    _thread = threading.Thread(target=run, name="astrbot-plugin-loop", daemon=True)
    _thread.start()
    return _loop


def get_loop() -> asyncio.AbstractEventLoop:
    if _loop is None:
        return start()
    return _loop


def run_coro(coro, timeout: float = 30.0):
    """在常驻 loop 上执行协程并等待结果。"""
    loop = get_loop()
    future = asyncio.run_coroutine_threadsafe(coro, loop)
    return future.result(timeout=timeout)


def stop() -> None:
    global _loop, _thread
    if _loop is not None:
        loop = _loop
        _loop = None
        try:
            loop.call_soon_threadsafe(loop.stop)
        except Exception:
            pass
        _thread = None
