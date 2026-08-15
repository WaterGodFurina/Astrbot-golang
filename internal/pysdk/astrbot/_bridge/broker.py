"""go-plugin GRPCBroker（插件侧 server 端）。

宿主作为 gRPC client 调插件 server 的 `plugin.GRPCBroker/StartStream`
（bidi stream）。宿主 `Accept(id)` 时把 `ConnInfo{service_id, network,
address}` Send 过来；插件本地 `dial(id)` 从收到的 ConnInfo 中取地址连接。
（经典模式，宿主未启用 mux，无需处理 knock。）
"""
from __future__ import annotations

import logging
import queue
import threading

import grpc

from .gen import goplugin_pb2, goplugin_pb2_grpc

logger = logging.getLogger("astrbot.broker")

DIAL_TIMEOUT = 8.0


class GRPCBrokerServicer(goplugin_pb2_grpc.GRPCBrokerServicer):
    def __init__(self) -> None:
        # service_id -> (network, address)
        self._conns: dict[int, tuple[str, str]] = {}
        self._cv = threading.Condition()

    def StartStream(self, request_iterator, context):
        # 收到宿主 Accept 发来的 ConnInfo → 存入表，通知 dial 等待者。
        for info in request_iterator:
            with self._cv:
                self._conns[info.service_id] = (info.network, info.address)
                self._cv.notify_all()
            logger.debug(
                f"broker: host offered service_id={info.service_id} {info.network} {info.address}"
            )
        return iter(())

    def dial(self, service_id: int, timeout: float = DIAL_TIMEOUT) -> grpc.Channel:
        deadline = threading.Event()
        waited = 0.0
        while True:
            with self._cv:
                if service_id in self._conns:
                    network, address = self._conns[service_id]
                    break
            import time

            time.sleep(0.05)
            waited += 0.05
            if waited >= timeout:
                raise TimeoutError(
                    f"broker: timeout waiting for connection info (service_id={service_id})"
                )
        if network not in ("tcp", "unix"):
            raise ValueError(f"broker: unsupported network {network!r}")
        if network == "unix":
            return grpc.insecure_channel(
                f"unix:{address}",
                options=[("grpc.max_receive_message_length", 128 * 1024 * 1024)],
            )
        return grpc.insecure_channel(
            address,
            options=[("grpc.max_receive_message_length", 128 * 1024 * 1024)],
        )


_broker_servicer: GRPCBrokerServicer | None = None


def get_broker() -> GRPCBrokerServicer:
    global _broker_servicer
    if _broker_servicer is None:
        _broker_servicer = GRPCBrokerServicer()
    return _broker_servicer
