from dataclasses import dataclass


@dataclass
class PlatformMetadata:
    name: str
    """平台的名称，即平台的类型，如 aiocqhttp, discord, slack"""
    description: str
    """平台的描述"""
    id: str
    """平台的唯一标识符，用于配置中识别特定平台"""

    default_config_tmpl: dict | None = None
    adapter_display_name: str | None = None
    logo_path: str | None = None
    support_streaming_message: bool = True
    support_proactive_message: bool = True
    module_path: str | None = None
    i18n_resources: dict[str, dict] | None = None
    config_metadata: dict | None = None
