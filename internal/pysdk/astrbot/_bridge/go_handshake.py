"""go-plugin 握手协议（经典模式，非 mux）。

宿主 go-plugin v1.6.2 客户端启动插件子进程时注入：
  ASTRBOT_PLUGIN_MAGIC_COOKIE=astrbot-go-plugin-1
  PLUGIN_MIN_PORT / PLUGIN_MAX_PORT
  PLUGIN_PROTOCOL_VERSIONS=1
（宿主未配置 GRPCBrokerMultiplex，故无 PLUGIN_MULTIPLEX_GRPC。）

插件侧：验证 cookie → 监听 TCP → stdout 打印握手行
  "1|<appVer>|tcp|127.0.0.1:<port>|grpc|"
→ 之后 stdout 归 go-plugin 的 stdio 捕获（不得再直接打印）。
"""
from __future__ import annotations

import os
import socket
import sys

MAGIC_COOKIE_KEY = "ASTRBOT_PLUGIN_MAGIC_COOKIE"
MAGIC_COOKIE_VALUE = "astrbot-go-plugin-1"
CORE_PROTOCOL_VERSION = 1
APP_PROTOCOL_VERSION = 1
ENV_MULTIPLEX_GRPC = "PLUGIN_MULTIPLEX_GRPC"


def _bind_tcp() -> socket.socket:
    min_port = _parse_port("PLUGIN_MIN_PORT")
    max_port = _parse_port("PLUGIN_MAX_PORT")
    if min_port and max_port:
        for port in range(min_port, max_port + 1):
            s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
            try:
                s.bind(("127.0.0.1", port))
                return s
            except OSError:
                s.close()
        raise OSError(
            f"go-plugin: 无法在 PLUGIN_MIN_PORT={min_port}..PLUGIN_MAX_PORT={max_port} 范围内绑定端口"
        )
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.bind(("127.0.0.1", 0))
    return s


def _parse_port(env: str) -> int:
    v = os.environ.get(env, "")
    if not v:
        return 0
    try:
        return max(0, int(v))
    except (ValueError, TypeError):
        return 0


def check_magic_cookie() -> None:
    """校验 magic cookie；env 未设置时跳过（本地调试/测试模式）。"""
    key = os.environ.get(MAGIC_COOKIE_KEY)
    if key is not None and key != MAGIC_COOKIE_VALUE:
        print(
            f"go-plugin: invalid magic cookie value (expected {MAGIC_COOKIE_VALUE!r})",
            file=sys.stderr,
        )
        sys.exit(1)


def check_no_multiplex() -> None:
    """宿主若启用 gRPC broker mux，Python SDK 无法支持（拒绝启动）。"""
    if os.environ.get(ENV_MULTIPLEX_GRPC, "") == "true":
        print(
            "go-plugin: PLUGIN_MULTIPLEX_GRPC is not supported by the Python SDK",
            file=sys.stderr,
        )
        sys.exit(1)


def handshake_listener() -> socket.socket:
    check_magic_cookie()
    check_no_multiplex()
    sock = _bind_tcp()
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    sock.listen(64)
    return sock


def port_range() -> tuple[int, int]:
    """返回 (PLUGIN_MIN_PORT, PLUGIN_MAX_PORT)，未设置时为 (0, 0)。"""
    return _parse_port("PLUGIN_MIN_PORT"), _parse_port("PLUGIN_MAX_PORT")


def print_handshake_line(host: str, port: int) -> None:
    protocol_line = (
        f"{CORE_PROTOCOL_VERSION}|{APP_PROTOCOL_VERSION}|tcp|{host}:{port}|grpc|"
    )
    # 握手行之后的 stdout 内容会被 go-plugin 客户端捕获并转发（stdio），
    # 但首行之前的任何输出都会污染协议——所有 print 必须在这之后。
    sys.stdout.write(protocol_line + "\n")
    sys.stdout.flush()
