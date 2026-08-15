"""AstrBot Go 宿主兼容运行时（Python SDK）。

插件在 Go 宿主（Astrbot-golang）中以 gRPC 子进程方式运行时 import 本包；
在 Python AstrBot 本体中 import 真正的 astrbot 包。两套运行时间名同构，
插件代码无需任何修改。
"""
import logging

__version__ = "4.27.3-compat"

logger = logging.getLogger("astrbot")

# 全局 logging 配置（供插件直接使用 logging 或 astrbot.api.logger）
if not logging.getLogger().handlers:
    logging.basicConfig(level=logging.INFO)
