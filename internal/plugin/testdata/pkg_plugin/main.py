from astrbot.api.event import filter, AstrMessageEvent
from astrbot.api.star import Context, Star, register

from .backend.util import greet


@register("pkg_plugin", "包式插件", "相对导入测试", "0.1.0")
class PkgPlugin(Star):
    @filter.command("pkghello")
    async def hello(self, event: AstrMessageEvent):
        yield event.plain_result(greet())
