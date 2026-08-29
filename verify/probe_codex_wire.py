#!/usr/bin/env python3
"""T1 spike 的必答项之一：Codex 请求的模型名到底从哪来？

⚠️ 这里起的是一台**记录用的服务端**，被测对象是**真实 Codex CLI 发出的字节**。
按 CLAUDE.md §1 的例外判据（真依赖能不能按需产出这个输入）：要观测客户端发什么，
就必须有个东西收下它并记下来 —— 真上游收得下，但不会把收到的原始请求给你。
所以假的那一端**就是被测输入的观测点**，不是被测依赖。

起一个只记录不干活的服务端，把真实 Codex CLI 指向它，记录：
  Q1 它是否调用 GET /v1/models
  Q2 它在 POST 体里填的 model 是什么
  Q3 它是否发 If-None-Match（关系到 x-models-etag 能否自生成）

结论直接决定 03 §4「/v1/models 由网关合成」是否会让 Codex 404 打不通。
"""
import json
import sys
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

LOG = []
PORT = 8899


def sse(event: str, data: dict) -> bytes:
    return f"event: {event}\ndata: {json.dumps(data)}\n\n".encode()


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _record(self, body=None):
        LOG.append({
            "method": self.command,
            "path": self.path,
            "headers": {k.lower(): v for k, v in self.headers.items()},
            "body_model": (body or {}).get("model"),
            "body_keys": sorted((body or {}).keys()) or None,
        })

    def do_GET(self):
        self._record()
        if self.path.startswith("/v1/models"):
            payload = json.dumps({"object": "list", "data": []}).encode()
            self.send_response(200)
            self.send_header("content-type", "application/json")
            self.send_header("content-length", str(len(payload)))
            self.send_header("x-models-etag", "mock-etag-1")
            self.end_headers()
            self.wfile.write(payload)
        else:
            self.send_response(404)
            self.send_header("content-length", "0")
            self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("content-length", 0) or 0)
        raw = self.rfile.read(n) if n else b""
        try:
            body = json.loads(raw or b"{}")
        except Exception:
            body = {}
        self._record(body)

        self.send_response(200)
        self.send_header("content-type", "text/event-stream")
        self.send_header("cache-control", "no-cache")
        self.send_header("connection", "close")
        self.end_headers()
        rid, mid = "resp_mock", "msg_mock"
        m = body.get("model", "unknown")
        base = {"id": rid, "object": "response", "created_at": 0, "model": m,
                "output": [], "usage": None}
        self.wfile.write(sse("response.created",
                             {"type": "response.created",
                              "response": dict(base, status="in_progress")}))
        self.wfile.write(sse("response.output_item.added",
                             {"type": "response.output_item.added", "output_index": 0,
                              "item": {"id": mid, "type": "message", "status": "in_progress",
                                       "role": "assistant", "content": []}}))
        self.wfile.write(sse("response.content_part.added",
                             {"type": "response.content_part.added", "item_id": mid,
                              "output_index": 0, "content_index": 0,
                              "part": {"type": "output_text", "text": "", "annotations": []}}))
        self.wfile.write(sse("response.output_text.delta",
                             {"type": "response.output_text.delta", "item_id": mid,
                              "output_index": 0, "content_index": 0, "delta": "ok"}))
        self.wfile.write(sse("response.output_text.done",
                             {"type": "response.output_text.done", "item_id": mid,
                              "output_index": 0, "content_index": 0, "text": "ok"}))
        self.wfile.write(sse("response.content_part.done",
                             {"type": "response.content_part.done", "item_id": mid,
                              "output_index": 0, "content_index": 0,
                              "part": {"type": "output_text", "text": "ok", "annotations": []}}))
        self.wfile.write(sse("response.output_item.done",
                             {"type": "response.output_item.done", "output_index": 0,
                              "item": {"id": mid, "type": "message", "status": "completed",
                                       "role": "assistant",
                                       "content": [{"type": "output_text", "text": "ok",
                                                    "annotations": []}]}}))
        done = dict(base, status="completed",
                    output=[{"id": mid, "type": "message", "status": "completed",
                             "role": "assistant",
                             "content": [{"type": "output_text", "text": "ok",
                                          "annotations": []}]}],
                    usage={"input_tokens": 1, "output_tokens": 1, "total_tokens": 2})
        self.wfile.write(sse("response.completed",
                             {"type": "response.completed", "response": done}))

    def log_message(self, *a):
        pass


if __name__ == "__main__":
    srv = HTTPServer(("127.0.0.1", PORT), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    sys.stderr.write(f"recorder listening on :{PORT}\n")
    try:
        threading.Event().wait(float(sys.argv[1]) if len(sys.argv) > 1 else 120)
    finally:
        print(json.dumps(LOG, ensure_ascii=False, indent=2))
