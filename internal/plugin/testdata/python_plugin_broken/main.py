# 启动即失败的测试插件：import 一个不存在的模块，触发 ModuleNotFoundError，
# 宿主侧应通过 [ASTRBOT] STARTUP_ERROR 协议把它报告为 phase=plugin_import
# 的清晰错误（而不是笼统的握手失败）。
from astrbot.api.star import Star, register

import nonexistent_module_xyz  # noqa: F401
