"""Provider 占位（Go 宿主兼容运行时）。"""


class Provider:
    """占位：宿主未开放 provider 直连，插件请使用 context.llm_generate。"""

    def __init__(self, *args, **kwargs):
        pass


class ProviderMetaData:
    def __init__(self, *args, **kwargs):
        pass
