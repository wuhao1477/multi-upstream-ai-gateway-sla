#!/usr/bin/env python3
"""模拟一个 NewAPI 系上游站点，供 P1 采集链路端到端验收。

不是"假装成功的桩"：字段形态严格照 04 §3.1 的实测结果构造 ——
· /api/status 给 quota_per_unit（逐站不同，采集器不得写死）
· 鉴权要求 Authorization **且**用户 ID 头（只带前者必须 401）
· 用户 ID 头名故意用二开的 `Veloera-User`，验证 fan-out 能命中
· 额度以 quota 整数给出（需按 quota_per_unit 归一为美元）
· expired_time = -1 表示永不过期
· model_limits_enabled=false 的那把 Key 带一个 model_limits 残留值，
  验证采集器不会把它当权限用

用法：python3 verify/mock_newapi.py [port]
"""
import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

QUOTA_PER_UNIT = 500000  # 与 upstream-d.invalid 实测一致；采集器须从 /api/status 读
USER_ID_HEADER = "Veloera-User"  # 故意用二开头名

STATUS = {
    "success": True,
    "data": {
        "version": "v1.0.0-rc.19",
        "quota_per_unit": QUOTA_PER_UNIT,
        "turnstile_check": False,
        "checkin_enabled": True,
    },
}

SELF = {
    "success": True,
    "data": {"id": 42, "username": "collector", "quota": 1_000_000,
             "used_quota": 250_000, "request_count": 8123, "group": "vip"},
}

TOKENS = {
    "success": True,
    "data": [
        {"id": 7, "name": "主力 Key", "remain_quota": 500_000,
         "used_quota": 100_000, "unlimited_quota": False,
         "expired_time": -1, "group": "vip",
         "model_limits_enabled": True, "model_limits": "gpt-5.5,gpt-4o",
         "request_count": 123},
        {"id": 8, "name": "备用 Key", "remain_quota": 250_000,
         "used_quota": 0, "unlimited_quota": False,
         "expired_time": -1, "group": "default",
         # 残留值：enabled=false 时不该被当成权限限制
         "model_limits_enabled": False, "model_limits": "stale-should-be-ignored",
         "request_count": 0},
    ],
}

PRICING = {
    "success": True,
    "data": {
        "model_ratio": {
            "gpt-5.5": 15, "gpt-4o": 5, "claude-4-opus": 30,
            "claude-4-sonnet": 6, "gemini-3-pro": 4, "deepseek-v4": 0.5,
            "qwen-3-max": 1.2, "llama-4-405b": 2.5,
        },
        "completion_ratio": {"gpt-5.5": 3, "claude-4-opus": 5, "gpt-4o": 4},
        "cache_ratio": {"gpt-5.5": 0.5, "gpt-4o": 0.5},
        "group_ratio": {"default": 1.0, "vip": 0.8, "svip": 0.6},
    },
}


class Handler(BaseHTTPRequestHandler):
    def _send(self, obj, code=200):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _authed(self):
        """04 §3.1：必须同时带 Authorization 与用户 ID 头，只带前者 401。"""
        if not self.headers.get("Authorization"):
            return False
        return bool(self.headers.get(USER_ID_HEADER))

    def do_GET(self):
        path = self.path.split("?")[0]
        if path == "/api/status":  # 公开端点，无需鉴权
            return self._send(STATUS)
        if path == "/api/pricing":  # 公开
            return self._send(PRICING)
        if not self._authed():
            return self._send({"success": False, "message": "unauthorized"}, 401)
        if path == "/api/user/self":
            return self._send(SELF)
        if path == "/api/token":
            return self._send(TOKENS)
        self._send({"success": False, "message": "not found"}, 404)

    def log_message(self, fmt, *args):
        sys.stderr.write("[mock-newapi] " + (fmt % args) + "\n")


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 18099
    print(f"mock NewAPI 站点监听 :{port}（用户 ID 头 = {USER_ID_HEADER}）",
          file=sys.stderr)
    HTTPServer(("127.0.0.1", port), Handler).serve_forever()
