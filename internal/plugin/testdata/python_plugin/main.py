from astrbot.api.event import filter, AstrMessageEvent
from astrbot.api.star import Context, Star, register
from astrbot.api import logger

import json


@register("test_pyplugin", "测试Python插件", "冒烟测试用", "0.1.0")
class TestPlugin(Star):
    def __init__(self, context: Context, config: dict = None):
        super().__init__(context, config)
        self.counter = 0

    async def initialize(self):
        logger.info("TestPlugin initialize()")
        self._init_web_apis()

    @filter.command("pyhello")
    async def hello(self, event: AstrMessageEvent):
        self.counter += 1
        yield event.plain_result(f"Hello from Python! count={self.counter}")

    @filter.command("pysend")
    async def pysend(self, event: AstrMessageEvent):
        """主动发送（不设置 Result）：宿主必须据此跳过 LLM（_has_send_oper）。"""
        from astrbot.api.event import MessageChain
        from astrbot.api.message_components import Plain

        await event.send(MessageChain([Plain("已发送")]))
        return None

    @filter.command("pycfg")
    async def cfg(self, event: AstrMessageEvent):
        cfg = self.context.get_config() if self.context else {}
        # 敏感键脱敏：完整配置可能包含 provider 的 api_key，不能原样发到聊天会话。
        sensitive = ("key", "token", "secret", "password")
        safe = {
            k: ("***" if any(s in str(k).lower() for s in sensitive) else v)
            for k, v in dict(cfg).items()
        }
        yield event.plain_result("config=" + json.dumps(safe, ensure_ascii=False))

    @filter.command("pyadd")
    async def add(self, event: AstrMessageEvent, a: int, b: int):
        yield event.plain_result(f"{a} + {b} = {a+b}")

    @filter.regex(r"^pyecho\s+(.+)$")
    async def echo(self, event: AstrMessageEvent):
        import re

        m = re.search(r"^pyecho\s+(.+)$", event.get_message_str())
        yield event.plain_result(f"echo: {m.group(1)}")

    @filter.llm_tool(name="py_add_tool")
    async def add_tool(self, event: AstrMessageEvent, a: int, b: int) -> str:
        """加法工具。

        Args:
            a (int): 第一个数
            b (int): 第二个数
        """
        return str(a + b)

    @filter.on_llm_request()
    async def llm_req(self, event: AstrMessageEvent, req):
        req.system_prompt += "【由Python插件注入】"

    def _init_web_apis(self):
        import importlib

        api_mod = importlib.import_module("web_api")
        api_echo = api_mod.api_echo
        api_get_emojis = api_mod.api_get_emojis

        self.context.register_web_api(
            "/test_pyplugin/emoji/<category>", api_get_emojis, ["GET"], "获取表情"
        )
        self.context.register_web_api("/test_pyplugin/echo", api_echo, ["POST"], "回显")

    @filter.command("pyt2i")
    async def t2i_cmd(self, event: AstrMessageEvent):
        img = await self.text_to_image("Python SDK t2i 测试", return_url=False)
        yield event.plain_result(f"t2i_len={len(img)}")
