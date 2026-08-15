from astrbot.api.web import request
from astrbot.api import logger


async def api_get_emojis(category: str = None):
    """返回分类表情（含 quart 全局 request 与 api.web request 两种访问方式）。"""
    from quart import request as qreq

    pack = request.query.get("pack_id", "") or qreq.args.get("pack_id", "")
    return {"status": "ok", "category": category, "pack_id": pack, "via": "web"}


async def api_echo():
    data = await request.json({})
    return {"status": "ok", "echo": data}
