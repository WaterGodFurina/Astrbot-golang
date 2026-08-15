from astrbot.core.config.astrbot_config import AstrBotConfig
from astrbot.core.provider.func_tool_manager import (
    FuncTool,
    FunctionToolManager,
    ToolSet,
)

llm_tool = __import__(
    "astrbot.core.star.register.star_handler", fromlist=["register_llm_tool"]
).register_llm_tool

agent = __import__(
    "astrbot.core.star.register.star_handler", fromlist=["register_agent"]
).register_agent


def _plugin_logger():
    import logging
    import sys

    module_name = sys._getframe(1).f_globals.get("__name__", "")
    return logging.getLogger(f"astrbot.plugin.{module_name}")


class _PluginContextLogger:
    """把 astrbot.api.logger 调用路由到调用方插件 logger。"""

    def __getattr__(self, item: str):
        import logging
        import sys

        module_name = sys._getframe(1).f_globals.get("__name__", "")
        return getattr(logging.getLogger(f"astrbot.plugin.{module_name}"), item)


logger = _PluginContextLogger()

html_renderer = None  # Go 宿主无 HTML 渲染

__all__ = [
    "AstrBotConfig",
    "FuncTool",
    "FunctionToolManager",
    "ToolSet",
    "agent",
    "html_renderer",
    "llm_tool",
    "logger",
]
