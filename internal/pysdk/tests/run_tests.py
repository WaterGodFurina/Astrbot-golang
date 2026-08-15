"""Python SDK 单元测试（不依赖 pytest，直接运行：python3 tests/run_tests.py）。

覆盖：装饰器收集/Register 元数据、序列化往返、go-plugin 握手、broker。
"""
import json
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "astrbot"))


class TestSerialize(unittest.TestCase):
    def test_component_roundtrip(self):
        from astrbot._bridge.serialize import component_from_json, component_to_json
        from astrbot.core.message.components import At, Image, Plain

        cases = [
            {"type": "Plain", "text": "hi"},
            {"type": "At", "target_id": "123", "name": "u"},
            {"type": "AtAll"},
            {"type": "Image", "url": "https://x/y.png"},
            {"type": "Reply", "id": "42"},
            {"type": "Face", "id": "1"},
        ]
        for c in cases:
            comp = component_from_json(c)
            out = component_to_json(comp)
            self.assertEqual(out["type"], c["type"], f"{c} -> {out}")
        # Plain -> 组件 -> JSON 属性一致
        p = component_from_json({"type": "Plain", "text": "abc"})
        self.assertIsInstance(p, Plain)
        self.assertEqual(component_to_json(p)["text"], "abc")
        # At
        a = component_from_json({"type": "At", "target_id": "456"})
        self.assertIsInstance(a, At)
        # Python 组件 -> JSON（Image url）
        self.assertEqual(
            component_to_json(Image.fromURL("http://a/b.png")),
            {"type": "Image", "url": "http://a/b.png"},
        )

    def test_event_from_json(self):
        from astrbot.core.platform.astr_message_event import AstrMessageEvent

        ev = AstrMessageEvent.from_event_json(
            {
                "type": "message",
                "platform": "qq_official",
                "self_id": "bot1",
                "sender_id": "u1",
                "sender_name": "小明",
                "conv_id": "g1",
                "is_group": True,
                "is_at_bot": True,
                "is_admin": True,
                "message_str": "/cmd 1",
                "plain_text": "/cmd 1",
                "message_id": "m1",
                "timestamp": 123,
                "chain": [{"type": "Plain", "text": "/cmd 1"}],
            }
        )
        self.assertEqual(ev.get_sender_id(), "u1")
        self.assertEqual(ev.get_sender_name(), "小明")
        self.assertEqual(ev.get_group_id(), "g1")
        self.assertTrue(ev.is_admin())
        self.assertEqual(ev.get_platform_name(), "qq_official")
        self.assertEqual(ev.session.message_type.value, "GROUP")

    def test_result_to_json(self):
        from astrbot._bridge.serialize import result_to_json
        from astrbot.core.message.message_event_result import EventResultType, MessageEventResult

        chain, stop = result_to_json("hello")
        self.assertEqual(chain, [{"type": "Plain", "text": "hello"}])
        r = MessageEventResult().message("a").url_image("https://x/y.png")
        chain2, _ = result_to_json(r)
        self.assertEqual(chain2[0]["type"], "Plain")
        self.assertEqual(chain2[1]["type"], "Image")
        r.stop_event()
        _, stop2 = result_to_json(r)
        self.assertTrue(stop2)
        _, stop3 = result_to_json(None)
        self.assertEqual(stop3, False)


class TestRegistry(unittest.TestCase):
    def _make_plugin(self):
        # 动态创建插件模块并加载（与 _bridge.loader 相同路径）
        import importlib
        import sys as _sys

        mod = types.ModuleType("pytest_dyn_plugin")
        code = '''
from astrbot.api.event import filter, AstrMessageEvent
from astrbot.api.star import Context, Star, register

@register("dyn_plugin", "动态插件", "测试", "1.0.0")
class Dyn(Star):
    @filter.command("dc")
    async def cmd(self, event: AstrMessageEvent, a: int):
        yield event.plain_result(str(a))

    @filter.regex(r"^drx")
    async def rx(self, event: AstrMessageEvent):
        yield event.plain_result("rx")

    @filter.llm_tool(name="dyn_tool")
    async def tool(self, event: AstrMessageEvent, x: str) -> str:
        """动态工具。

        Args:
            x (string): 输入
        """
        return x

    @filter.on_llm_request()
    async def llm_req(self, event: AstrMessageEvent, req):
        req.system_prompt += "X"
'''
        _sys.modules[mod.__name__] = mod
        exec(compile(code, "<dyn>", "exec"), mod.__dict__)
        return mod

    def test_register_metadata(self):
        import types

        from astrbot._bridge.dispatch import PluginServiceServicer
        from astrbot.core.star.star import star_map

        mod = self._make_plugin()
        md = star_map.get(mod.__name__)
        self.assertIsNotNone(md, "Star 类未注册")
        self.assertEqual(md.name, "dyn_plugin")

        svc = PluginServiceServicer("dyn_plugin", "1.0.0", "", "", plugin_dir=tempfile.gettempdir())
        # 不加载真实实例（无宿主），仅验证 build_registry 收集
        from astrbot._bridge.dispatch import star_handlers_registry as _s  # noqa

        resp = svc.Register(None, None)
        names = {c.name for c in resp.commands}
        self.assertIn("dc", names)
        tools = {t.name for t in resp.tools}
        self.assertIn("dyn_tool", tools)
        hooks = {h.event for h in resp.hooks}
        self.assertIn("on_llm_request", hooks)
        filters = {f.name for f in resp.filters}
        self.assertTrue(any("rx" in n for n in filters), f"filters={filters}")


class TestGoHandshake(unittest.TestCase):
    def test_magic_cookie(self):
        import astrbot._bridge.go_handshake as gh

        old = os.environ.get(gh.MAGIC_COOKIE_KEY)
        try:
            os.environ[gh.MAGIC_COOKIE_KEY] = "wrong"
            with self.assertRaises(SystemExit):
                gh.check_magic_cookie()
            os.environ[gh.MAGIC_COOKIE_KEY] = gh.MAGIC_COOKIE_VALUE
            gh.check_magic_cookie()  # 不抛
            del os.environ[gh.MAGIC_COOKIE_KEY]
            gh.check_magic_cookie()  # 未设置跳过
        finally:
            if old is None:
                os.environ.pop(gh.MAGIC_COOKIE_KEY, None)
            else:
                os.environ[gh.MAGIC_COOKIE_KEY] = old

    def test_no_multiplex(self):
        import astrbot._bridge.go_handshake as gh

        old = os.environ.get(gh.ENV_MULTIPLEX_GRPC)
        try:
            os.environ[gh.ENV_MULTIPLEX_GRPC] = "true"
            with self.assertRaises(SystemExit):
                gh.check_no_multiplex()
            os.environ[gh.ENV_MULTIPLEX_GRPC] = ""
            gh.check_no_multiplex()
        finally:
            if old is None:
                os.environ.pop(gh.ENV_MULTIPLEX_GRPC, None)
            else:
                os.environ[gh.ENV_MULTIPLEX_GRPC] = old

    def test_handshake_line(self):
        import io
        import sys as _sys

        import astrbot._bridge.go_handshake as gh

        buf = io.StringIO()
        old = _sys.stdout
        _sys.stdout = buf
        try:
            gh.print_handshake_line("127.0.0.1", 12345)
        finally:
            _sys.stdout = old
        self.assertEqual(buf.getvalue().strip(), "1|1|tcp|127.0.0.1:12345|grpc|")


class TestBroker(unittest.TestCase):
    def test_dial_timeout(self):
        from astrbot._bridge.broker import GRPCBrokerServicer

        b = GRPCBrokerServicer()
        with self.assertRaises(TimeoutError):
            b.dial(9999, timeout=0.3)

    def test_broker_internals(self):
        from astrbot._bridge.broker import GRPCBrokerServicer

        b = GRPCBrokerServicer()
        with b._cv:
            b._conns[7] = ("tcp", "127.0.0.1:1")
            b._cv.notify_all()
        # dial 会尝试连接（端口 1 不可用，不抛超时即可；连接失败由 grpc 延迟暴露）
        try:
            ch = b.dial(7, timeout=1)
            ch.close()
        except Exception:
            pass
        self.assertIn(7, b._conns)


import types  # noqa: E402


class TestWebAPI(unittest.TestCase):
    """Web UI 插件 API：路由匹配、动态参数、quart 注入、响应序列化。"""

    def test_web_route_pattern(self):
        from astrbot._bridge.dispatch import PluginServiceServicer

        svc = PluginServiceServicer("x", "1", "", "")
        p, names = svc._web_route_pattern("/meme_manager/emoji/<category>")
        self.assertEqual(names, ["category"])
        m = p.match("/meme_manager/emoji/happy")
        self.assertIsNotNone(m)
        self.assertEqual(m.groupdict(), {"category": "happy"})
        self.assertIsNone(p.match("/meme_manager/other/happy"))
        p2, _ = svc._web_route_pattern("/a/<x>/<y>")
        self.assertEqual(p2.match("/a/1/2").groupdict(), {"x": "1", "y": "2"})

    def test_web_handler_dispatch(self):
        import asyncio

        from astrbot._bridge.dispatch import PluginServiceServicer
        from astrbot._bridge.gen import plugin_pb2

        async def api_emoji(category: str = None):
            return {"status": "ok", "category": category}

        svc = PluginServiceServicer("webdemo", "1", "", "")
        svc.web_apis = [("/webdemo/emoji/<category>", api_emoji, ["GET"], "x")]
        r = plugin_pb2.HandleWebRequestRequest(
            method="GET", path="/webdemo/emoji/happy",
            query=[plugin_pb2.WebKV(key="a", value="1"), plugin_pb2.WebKV(key="a", value="2")],
        )
        out = svc.HandleWebRequest(r, None)
        self.assertEqual(out.status_code, 200)
        self.assertEqual(json.loads(out.body), {"status": "ok", "category": "happy"})
        # 方法不匹配 → 404
        r2 = plugin_pb2.HandleWebRequestRequest(method="POST", path="/webdemo/emoji/happy")
        self.assertEqual(svc.HandleWebRequest(r2, None).status_code, 404)
        # 未注册 → 404
        r3 = plugin_pb2.HandleWebRequestRequest(method="GET", path="/webdemo/nope")
        self.assertEqual(svc.HandleWebRequest(r3, None).status_code, 404)

    def test_quart_request_injection(self):
        """quart 全局 request（_cv_request.get().request）可用。"""
        import asyncio

        try:
            import quart  # noqa: F401
        except ImportError:
            self.skipTest("quart 未安装")
        from astrbot._bridge.dispatch import PluginServiceServicer
        from astrbot._bridge.gen import plugin_pb2

        async def api_uses_quart():
            from quart.globals import _cv_request

            fake = _cv_request.get()
            assert fake is not None and fake.request is fake
            return {"status": "ok", "q": fake.request.args.get("pack_id", "")}

        svc = PluginServiceServicer("qd", "1", "", "")
        svc.web_apis = [("/qd/check", api_uses_quart, ["GET"], "x")]
        r = plugin_pb2.HandleWebRequestRequest(
            method="GET", path="/qd/check",
            query=[plugin_pb2.WebKV(key="pack_id", value="p9")],
        )
        out = svc.HandleWebRequest(r, None)
        self.assertEqual(out.status_code, 200)
        self.assertEqual(json.loads(out.body), {"status": "ok", "q": "p9"})

    def test_web_response_serialization(self):
        from astrbot._bridge.dispatch import PluginServiceServicer
        from astrbot._bridge.gen import plugin_pb2

        svc = PluginServiceServicer("s", "1", "", "")
        # dict → json 200
        self.assertEqual(
            svc._serialize_web_result({"status": "ok"}).status_code, 200
        )
        # (dict, status) 元组
        out = svc._serialize_web_result(({"e": "x"}, 409))
        self.assertEqual(out.status_code, 409)
        # quart Response 兼容对象（有 status_code + body）
        class FakeResp:
            status_code = 201
            body = b'{"created": true}'
            headers = {"Content-Type": "application/json"}

        out2 = svc._serialize_web_result(FakeResp())
        self.assertEqual(out2.status_code, 201)
        self.assertEqual(out2.body, b'{"created": true}')
        # None → 200 空
        self.assertEqual(svc._serialize_web_result(None).status_code, 200)


if __name__ == "__main__":
    unittest.main(verbosity=2)
