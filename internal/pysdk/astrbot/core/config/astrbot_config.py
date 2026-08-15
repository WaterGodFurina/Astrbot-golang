"""AstrBot 配置（Go 宿主兼容运行时）。"""
from __future__ import annotations

import json
import os
from typing import Any


def get_astrbot_data_path() -> str:
    """返回 AstrBot 数据目录（由宿主注入 ASTRBOT_DATA_PATH，缺省为 cwd）。"""
    return os.environ.get("ASTRBOT_DATA_PATH", os.getcwd())


class AstrBotConfig(dict):
    """插件配置对象（dict 子类 + 属性访问）。"""

    def __getattr__(self, item):
        if item in self:
            return self[item]
        raise AttributeError(item)

    def __setattr__(self, key, value) -> None:
        self[key] = value

    def __delattr__(self, key) -> None:
        if key in self:
            del self[key]

    def get(self, key: str, default=None):
        return super().get(key, default)

    @staticmethod
    def load_config(namespace: str) -> dict | bool:
        """从配置文件加载配置（兼容旧 API）。"""
        path = os.path.join(
            get_astrbot_data_path(), "config", f"{namespace}.json"
        )
        if not os.path.exists(path):
            return False
        with open(path, encoding="utf-8-sig") as f:
            data = json.load(f)
            ret = {}
            for k in data:
                if isinstance(data[k], dict) and "value" in data[k]:
                    ret[k] = data[k]["value"]
                else:
                    ret[k] = data[k]
            return ret

    @staticmethod
    def put_config(namespace: str, name: str, key: str, value, description: str) -> None:
        """写入配置项（兼容旧 API）。"""
        if namespace == "":
            raise ValueError("namespace 不能为空。")
        config_dir = os.path.join(get_astrbot_data_path(), "config")
        os.makedirs(config_dir, exist_ok=True)
        path = os.path.join(config_dir, f"{namespace}.json")
        if not os.path.exists(path):
            with open(path, "w", encoding="utf-8-sig") as f:
                f.write("{}")
        with open(path, encoding="utf-8-sig") as f:
            d = json.load(f)
        assert isinstance(d, dict)
        if key not in d:
            d[key] = {
                "config_type": "item",
                "name": name,
                "description": description,
                "path": key,
                "value": value,
                "val_type": type(value).__name__,
            }
            with open(path, "w", encoding="utf-8-sig") as f:
                json.dump(d, f, indent=2, ensure_ascii=False)
                f.flush()

    @staticmethod
    def update_config(namespace: str, key: str, value) -> None:
        """更新配置项（兼容旧 API）。"""
        path = os.path.join(get_astrbot_data_path(), "config", f"{namespace}.json")
        if not os.path.exists(path):
            raise FileNotFoundError(f"配置文件 {namespace}.json 不存在。")
        with open(path, encoding="utf-8-sig") as f:
            d = json.load(f)
        assert isinstance(d, dict)
        if key not in d:
            raise KeyError(f"配置项 {key} 不存在。")
        d[key]["value"] = value
        with open(path, "w", encoding="utf-8-sig") as f:
            json.dump(d, f, indent=2, ensure_ascii=False)
            f.flush()
