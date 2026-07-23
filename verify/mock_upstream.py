#!/usr/bin/env python3
"""
Mock upstream for AxonHub ISSUE-001 verification.
Pure Python stdlib (no pip deps). Emits OpenAI-compatible /v1/chat/completions
responses with controllable behaviour selected by the request's "model" field.

Scenarios (choose via model name):
  mock-normal        : normal streaming — role-only first delta, then content
  mock-empty-sse     : streaming that only sends a role delta + [DONE], no content
  mock-slow-first    : waits N seconds before the FIRST event (tests first-event timeout)
  mock-heartbeat     : sends SSE comment heartbeats then content (tests non-content first bytes)
  mock-500           : returns HTTP 500 immediately (tests failover)
  mock-normal-2      : same as normal, different id — use as the "second" channel
  mock-abort         : sends role + partial content, then abruptly drops the TCP
                       connection with NO finish and NO [DONE] (upstream mid-stream abort)

Run: python3 mock_upstream.py [port]   (default 8091)
"""
import json
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 8091
SLOW_SECONDS = 8  # longer than a typical first-event timeout so failover triggers


def sse(data: dict) -> bytes:
    return ("data: " + json.dumps(data) + "\n\n").encode()


def chunk(cid: str, model: str, delta: dict, finish=None) -> dict:
    return {
        "id": cid, "object": "chat.completion.chunk", "created": int(time.time()),
        "model": model,
        "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
    }


def usage_chunk(cid: str, model: str, prompt: int, completion: int, cached: int = 0) -> dict:
    # Final OpenAI-style streaming usage chunk: empty choices + a usage object.
    # AxonHub captures this to populate usage_log (tokens -> cost). Assumption #4.
    return {
        "id": cid, "object": "chat.completion.chunk", "created": int(time.time()),
        "model": model,
        "choices": [],
        "usage": {
            "prompt_tokens": prompt,
            "completion_tokens": completion,
            "total_tokens": prompt + completion,
            "prompt_tokens_details": {"cached_tokens": cached},
        },
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass  # quiet

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(body or b"{}")
        except Exception:
            req = {}
        model = req.get("model", "mock-normal")
        cid = "chatcmpl-mock-" + model

        # Per-channel behaviour override via the upstream Authorization header
        # (AxonHub forwards each channel's credential apiKey). A channel whose
        # credential contains "slow" behaves like mock-slow-first regardless of
        # the requested model — used to build the internal-failover test
        # (assumption #2 path B): slow channel A first-event-times-out, AxonHub
        # fails over to fast channel B.
        auth = self.headers.get("Authorization", "")
        if "slow" in auth:
            model = "mock-slow-first"
            cid = "chatcmpl-mock-slow-channel"

        if model == "mock-500":
            self.send_response(500)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(json.dumps({"error": {"message": "mock upstream 500"}}).encode())
            return

        # streaming SSE
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()

        try:
            if model == "mock-slow-first":
                time.sleep(SLOW_SECONDS)
                self.wfile.write(sse(chunk(cid, model, {"role": "assistant"})))
                self.wfile.write(sse(chunk(cid, model, {"content": "late"})))
                self.wfile.write(sse(chunk(cid, model, {}, finish="stop")))
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return

            if model == "mock-abort":
                # upstream sends role + a bit of content, then abruptly drops the
                # connection mid-stream: no finish_reason, no [DONE], no usage.
                self.wfile.write(sse(chunk(cid, model, {"role": "assistant"})))
                self.wfile.write(sse(chunk(cid, model, {"content": "partial"})))
                self.wfile.flush()
                time.sleep(0.2)
                try:
                    # force an abrupt TCP close so AxonHub sees a truncated stream
                    self.connection.shutdown(1)  # SHUT_WR
                    self.connection.close()
                except Exception:
                    pass
                self.close_connection = True
                return

            if model == "mock-heartbeat":
                # SSE comment heartbeats carry no JSON content
                for _ in range(3):
                    self.wfile.write(b": heartbeat\n\n")
                    self.wfile.flush()
                    time.sleep(0.2)
                self.wfile.write(sse(chunk(cid, model, {"role": "assistant"})))
                self.wfile.write(sse(chunk(cid, model, {"content": "hi"})))
                self.wfile.write(sse(chunk(cid, model, {}, finish="stop")))
                self.wfile.write(sse(usage_chunk(cid, model, 9, 1)))
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return

            # role-only first delta, THEN content — this is the key case for
            # assumption #3 (is "first token" the role delta or the content?)
            self.wfile.write(sse(chunk(cid, model, {"role": "assistant"})))
            self.wfile.flush()
            time.sleep(0.5)  # measurable gap between role delta and first content

            if model == "mock-empty-sse":
                # no content at all, straight to finish
                self.wfile.write(sse(chunk(cid, model, {}, finish="stop")))
                self.wfile.write(b"data: [DONE]\n\n")
                self.wfile.flush()
                return

            for tok in ["Hello", " from", " ", model]:
                self.wfile.write(sse(chunk(cid, model, {"content": tok})))
                self.wfile.flush()
                time.sleep(0.1)
            self.wfile.write(sse(chunk(cid, model, {}, finish="stop")))
            # 4 content tokens above; declare 6 prompt tokens, 2 of them cached.
            self.wfile.write(sse(usage_chunk(cid, model, 6, 4, cached=2)))
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            # client (AxonHub) cancelled — expected in cancel tests
            pass


if __name__ == "__main__":
    print(f"mock upstream listening on :{PORT}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
