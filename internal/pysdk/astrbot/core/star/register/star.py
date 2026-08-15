import warnings

from astrbot.core.star.star import StarMetadata, star_map


def register_star(
    name: str,
    author: str,
    desc: str,
    version: str,
    repo: str | None = None,
):
    """注册一个插件(Star)。"""

    def decorator(cls):
        if not star_map.get(cls.__module__):
            metadata = StarMetadata(
                name=name,
                author=author,
                desc=desc,
                version=version,
                repo=repo,
            )
            star_map[cls.__module__] = metadata
        else:
            star_map[cls.__module__].name = name
            star_map[cls.__module__].author = author
            star_map[cls.__module__].desc = desc
            star_map[cls.__module__].version = version
            star_map[cls.__module__].repo = repo
        return cls

    return decorator
