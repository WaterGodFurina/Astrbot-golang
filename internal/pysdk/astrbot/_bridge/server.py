"""Python 插件 gRPC 入口。

用法（宿主启动）：
    python3 -m astrbot._bridge.server <plugin_dir>
环境：
    PYTHONPATH 须包含本 SDK 包根目录（astrbot/ 的父目录）
    ASTRBOT_PLUGIN_DATA_DIR = 插件数据目录（cwd 通常已是）
"""
from __future__ import annotations

import logging
import os
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor

import grpc

HOST_SERVICE_APP_ID = 9000

logger = logging.getLogger("astrbot")


def _setup_logging() -> None:
    level = os.environ.get("ASTRBOT_PLUGIN_LOG_LEVEL", "INFO").upper()
    handler = logging.StreamHandler(sys.stderr)
    handler.setFormatter(
        logging.Formatter("[%(asctime)s] [%(levelname)s] %(name)s: %(message)s")
    )
    root = logging.getLogger()
    root.handlers.clear()
    root.addHandler(handler)
    root.setLevel(getattr(logging, level, logging.INFO))


def main() -> int:
    _setup_logging()
    if len(sys.argv) < 2:
        print("usage: python3 -m astrbot._bridge.server <plugin_dir>", file=sys.stderr)
        return 2
    plugin_dir = os.path.abspath(sys.argv[1])

    # 1) go-plugin 握手（必须先于任何 stdout 输出/插件 import）
    from astrbot._bridge import go_handshake

    go_handshake.check_magic_cookie()
    go_handshake.check_no_multiplex()

    # 2) 常驻 event loop
    from astrbot._bridge import loop as event_loop

    event_loop.start()

    # 3) 插件加载（握手行之后！import 期间的 print 不会污染协议）
    from astrbot._bridge.dispatch import PluginServiceServicer
    from astrbot._bridge.loader import load_plugin
    from astrbot.core.star.context import Context, set_host_bridge

    context = Context()
    metadata = load_plugin(plugin_dir, context)
    if metadata is None:
        print("插件加载失败：未找到 Star 类", file=sys.stderr)
        return 1

    plugin_name = metadata.name or os.path.basename(plugin_dir)
    plugin_version = metadata.version or ""
    plugin_desc = metadata.desc or ""
    plugin_author = metadata.author or ""

    servicer = PluginServiceServicer(plugin_name, plugin_version, plugin_desc, plugin_author, plugin_dir)
    servicer.inst = metadata.star_cls
    servicer.web_apis = list(getattr(context, "_web_apis", []))
    context.plugin_name = plugin_name
    context.plugin_id = metadata.plugin_id
    try:
        setattr(metadata.star_cls_type, "plugin_id", metadata.plugin_id)
    except Exception:
        pass

    # 4) 宿主桥（HostService 反向调用）预连接
    from astrbot._bridge.host import HostBridge

    bridge = HostBridge()
    bridge.plugin_name = plugin_name
    bridge.plugin_id = metadata.plugin_id
    set_host_bridge(bridge)
    threading.Thread(target=bridge.preconnect, daemon=True).start()

    # 5) gRPC server：PluginService + plugin.GRPCBroker
    from astrbot._bridge.broker import get_broker
    from astrbot._bridge.gen import goplugin_pb2_grpc, plugin_pb2_grpc

    server = grpc.server(
        ThreadPoolExecutor(max_workers=16),
        options=[
            ("grpc.max_send_message_length", 128 * 1024 * 1024),
            ("grpc.max_receive_message_length", 128 * 1024 * 1024),
        ],
    )
    plugin_pb2_grpc.add_PluginServiceServicer_to_server(servicer, server)
    goplugin_pb2_grpc.add_GRPCBrokerServicer_to_server(get_broker(), server)

    # 宿主通过 TCP 连入（grpc-python 只支持非阻塞 serve）
    bound_port = None
    min_port, max_port = go_handshake.port_range()
    if min_port and max_port:
        for port in range(min_port, max_port + 1):
            try:
                bound_port = server.add_insecure_port(f"127.0.0.1:{port}")
                break
            except Exception:
                continue
        if bound_port is None:
            print(
                f"go-plugin: 无法在 PLUGIN_MIN_PORT={min_port}..PLUGIN_MAX_PORT={max_port} 范围内绑定端口",
                file=sys.stderr,
            )
            return 1
    else:
        bound_port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    go_handshake.print_handshake_line("127.0.0.1", bound_port)
    logger.info(f"插件 {plugin_name} v{plugin_version} 服务已启动，等待宿主连接")

    try:
        while True:
            time.sleep(3600)
    except (KeyboardInterrupt, SystemExit):
        pass
    finally:
        server.stop(0)
        event_loop.stop()
    return 0


if __name__ == "__main__":
    sys.exit(main())
