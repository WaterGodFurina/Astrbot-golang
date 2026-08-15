"""旧式插件配置 API 兼容（参考 Python astrbot.core.star.config）。"""

from astrbot.core.config.astrbot_config import AstrBotConfig


def load_config(namespace: str) -> dict | bool:
    """从配置文件中加载配置。"""
    return AstrBotConfig.load_config(namespace)


def put_config(namespace: str, name: str, key: str, value, description: str) -> None:
    """将配置项写入以 namespace 为名字的配置文件。"""
    return AstrBotConfig.put_config(namespace, name, key, value, description)


def update_config(namespace: str, key: str, value) -> None:
    """更新配置文件中的配置项。"""
    return AstrBotConfig.update_config(namespace, key, value)
