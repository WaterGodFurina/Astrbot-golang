from typing import TypeVar

SUPPORTED_VALUE_TYPES = int | float | str | bytes | bool | dict | list | None
_VT = TypeVar("_VT")


class PluginKVStoreMixin:
    """为插件提供键值存储功能的 Mixin 类（Go 宿主兼容运行时）。

    键值数据直接存到插件数据目录的 kv_store.json（宿主未提供 KV RPC）。
    """

    plugin_id: str

    def _kv_path(self) -> str:
        import json
        import os

        data_dir = os.environ.get(
            "ASTRBOT_PLUGIN_DATA_DIR",
            os.path.join(os.environ.get("ASTRBOT_DATA_PATH", os.getcwd()), "kv"),
        )
        path = os.path.join(data_dir, "kv_store.json")
        os.makedirs(os.path.dirname(path), exist_ok=True)
        return path

    def _kv_load(self) -> dict:
        import json
        import os

        path = self._kv_path()
        if not os.path.exists(path):
            return {}
        try:
            with open(path, encoding="utf-8") as f:
                data = json.load(f)
                return data if isinstance(data, dict) else {}
        except Exception:
            return {}

    def _kv_save(self, data: dict) -> None:
        import json

        with open(self._kv_path(), "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)

    async def put_kv_data(self, key: str, value) -> None:
        data = self._kv_load()
        data[key] = value
        self._kv_save(data)

    async def get_kv_data(self, key: str, default=None):
        return self._kv_load().get(key, default)

    async def delete_kv_data(self, key: str) -> None:
        data = self._kv_load()
        if key in data:
            del data[key]
            self._kv_save(data)
