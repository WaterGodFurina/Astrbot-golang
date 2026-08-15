"""数据目录解析（对齐 Python 本体 astrbot.core.utils.astrbot_path）。

宿主子进程的 ASTRBOT_DATA_PATH 指向宿主数据目录（data/）。
"""
import os


def get_astrbot_data_path() -> str:
    """返回 AstrBot 数据目录（宿主注入 ASTRBOT_DATA_PATH，缺省为 cwd）。"""
    return os.environ.get("ASTRBOT_DATA_PATH", os.getcwd())


def get_astrbot_plugin_path() -> str:
    return os.path.realpath(os.path.join(get_astrbot_data_path(), "plugins"))


def get_astrbot_config_path() -> str:
    return os.path.join(get_astrbot_data_path(), "config")


def get_astrbot_plugin_data_path() -> str:
    return os.path.realpath(os.path.join(get_astrbot_data_path(), "plugin_data"))


def get_astrbot_webui_path() -> str:
    return os.path.join(get_astrbot_data_path(), "webui")
