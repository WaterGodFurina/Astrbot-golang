"""HostService 客户端 + 宿主桥（插件 → 宿主反向调用）。"""
from __future__ import annotations

import json
import logging
import threading

import grpc

from astrbot._bridge import loop
from astrbot._bridge.broker import get_broker
from astrbot._bridge.gen import plugin_pb2, plugin_pb2_grpc
from astrbot._bridge.serialize import component_to_json

logger = logging.getLogger("astrbot.host")

HOST_SERVICE_APP_ID = 9000

# 宿主 accept 9000 后，ConnInfo 只在约 5s 窗口内有效（go-plugin timeoutWait）；
# 与 Go SDK 一致，serve 后立即循环预连接并缓存。
PRECONNECT_ATTEMPTS = 20
PRECONNECT_INTERVAL = 0.25


class HostBridge:
    """宿主能力代理（经 broker Dial 9000 连接宿主 HostService）。"""

    def __init__(self) -> None:
        self._channel: grpc.Channel | None = None
        self._stub: plugin_pb2_grpc.HostServiceStub | None = None
        self._lock = threading.Lock()
        self.plugin_name: str = ""
        self.plugin_id: str = ""

    def connect(self, service_id: int = HOST_SERVICE_APP_ID) -> bool:
        try:
            channel = get_broker().dial(service_id)
            stub = plugin_pb2_grpc.HostServiceStub(channel)
            with self._lock:
                self._channel = channel
                self._stub = stub
            return True
        except Exception as e:
            logger.debug(f"HostService connect 失败: {e}")
            return False

    def ensure_connected(self) -> bool:
        with self._lock:
            if self._stub is not None:
                return True
        return self.connect()

    def preconnect(self) -> None:
        for _ in range(PRECONNECT_ATTEMPTS):
            if self.connect():
                logger.info("HostService 已连接（预连接）")
                return
            import time

            time.sleep(PRECONNECT_INTERVAL)
        logger.warning("HostService 预连接失败（5s 窗口内未收到 ConnInfo），插件将失去反向调用能力")

    def get_config(self, plugin_name: str) -> dict:
        if not self.ensure_connected():
            return {}
        resp = self._stub.GetConfig(
            plugin_pb2.GetConfigRequest(plugin_name=plugin_name or self.plugin_name),
            timeout=30,
        )
        if not resp.config_json:
            return {}
        data = json.loads(resp.config_json)
        return data if isinstance(data, dict) else {}

    def set_config(self, plugin_name: str, cfg: dict) -> bool:
        if not self.ensure_connected():
            return False
        self._stub.SetConfig(
            plugin_pb2.SetConfigRequest(
                plugin_name=plugin_name or self.plugin_name,
                config_json=json.dumps(cfg).encode(),
            ),
            timeout=30,
        )
        return True

    def chat_llm(self, prompt: str, system_prompt: str = "", image_urls: list[str] | None = None) -> str:
        if not self.ensure_connected():
            raise RuntimeError("宿主桥未就绪（ChatLLM 不可用）")
        resp = self._stub.ChatLLM(
            plugin_pb2.ChatLLMRequest(
                prompt=prompt,
                system_prompt=system_prompt,
                image_urls=image_urls or [],
            ),
            timeout=180,
        )
        return resp.text

    def react(self, platform: str, session_id: str, message_id: str, emoji: str) -> bool:
        if not self.ensure_connected():
            return False
        self._stub.React(
            plugin_pb2.ReactRequest(
                platform=platform,
                session_id=session_id,
                message_id=message_id,
                emoji=emoji,
            ),
            timeout=30,
        )
        return True

    def text_to_image(self, text: str, template_name: str = "") -> bytes:
        """宿主 t2i 渲染文本为图片，返回 PNG 字节。"""
        if not self.ensure_connected():
            raise RuntimeError("宿主桥未就绪（TextToImage 不可用）")
        resp = self._stub.TextToImage(
            plugin_pb2.TextToImageRequest(text=text, template_name=template_name),
            timeout=120,
        )
        import base64

        return base64.b64decode(resp.image_base64)

    def send_message(self, session, chain) -> bool:
        """发送消息链。session 为 MessageSession 或 MessageChain。"""
        from astrbot.core.platform.message_session import MessageSession

        if not self.ensure_connected():
            raise RuntimeError("宿主桥未就绪（SendMessage 不可用）")
        if isinstance(session, str):
            session = MessageSession.from_str(session)
        chain_json = [component_to_json(c) for c in chain.chain]
        self._stub.SendMessage(
            plugin_pb2.SendMessageRequest(
                platform=session.platform_id,
                session_id=session.session_id,
                chain_json=json.dumps(chain_json).encode(),
            ),
            timeout=30,
        )
        return True

    def call_action(self, platform: str, api: str, params: dict) -> dict:
        if not self.ensure_connected():
            raise RuntimeError("宿主桥未就绪（CallAction 不可用）")
        resp = self._stub.CallAction(
            plugin_pb2.CallActionRequest(
                platform=platform,
                api=api,
                params_json=json.dumps(params or {}).encode(),
            ),
            timeout=30,
        )
        if not resp.result_json:
            return {}
        data = json.loads(resp.result_json)
        return data if isinstance(data, dict) else {"data": data}

    async def call_action_async(self, platform: str, api: str, params: dict) -> dict:
        return self.call_action(platform, api, params)

    def recall_message(self, platform: str, message_id: str) -> bool:
        if not self.ensure_connected():
            return False
        self._stub.RecallMessage(
            plugin_pb2.RecallMessageRequest(platform=platform, message_id=message_id),
            timeout=30,
        )
        return True

    async def react_message(self, event, emoji: str) -> bool:
        """给事件消息添加表情回应（宿主 React RPC）。"""
        from astrbot.core.platform.message_type import MessageType

        msg_id = getattr(event.message_obj, "message_id", "") if event.message_obj else ""
        if not msg_id:
            return False
        session = event.session
        return self.react(session.platform_id, session.session_id, msg_id, emoji)


_bridge: HostBridge | None = None


def get_bridge() -> HostBridge:
    global _bridge
    if _bridge is None:
        _bridge = HostBridge()
    return _bridge
