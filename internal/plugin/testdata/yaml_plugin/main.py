from astrbot.api.event import filter, AstrMessageEvent
from astrbot.api.star import Context, Star, register

@register("yaml_demo", "YAML演示", "metadata.yaml 演示", "0.1.0")
class YamlDemo(Star):
    @filter.command("yamlhello")
    async def hello(self, event: AstrMessageEvent):
        yield event.plain_result("yaml metadata ok")
