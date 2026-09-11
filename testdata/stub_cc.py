#!/usr/bin/env python3
"""Stub Command Code upstream for e2e testing the Go proxy."""
import json
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1]) if len(sys.argv) > 1 else 9701
# 模拟账号套餐（individual-go / individual-pro ...），可用 STUB_PLAN 覆盖
import os
STUB_PLAN = os.environ.get("STUB_PLAN", "individual-go")
# 混合列表：opensource 允许 / premium 拒绝 / opensource 但 go 黑名单
STUB_MODELS = [{"id": "deepseek/deepseek-v4-flash"},
               {"id": "google/gemini-3.5-flash"},
               {"id": "meta/muse-spark-1.2"},
               {"id": "claude-opus-4-8"}]
# 哪些 key 返回 429（测试 failover）
RATE_LIMITED = {"user_ratelimited"}


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stderr.write("[stub] %s %s\n" % (self.command, self.path))

    def _json(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        auth = self.headers.get("Authorization", "")
        key = auth.removeprefix("Bearer ")
        if key in RATE_LIMITED and "billing" not in self.path:
            return self._json(429, {"error": {"message": "rate limited"}})
        if self.path.startswith("/alpha/whoami"):
            return self._json(200, {"org": {"id": "org_1", "login": "stub"}, "user": {"userName": "stub"}})
        if self.path.startswith("/alpha/billing/credits"):
            return self._json(200, {"credits": {"planId": STUB_PLAN, "monthlyCredits": 10,
                                                "purchasedCredits": 0, "freeCredits": 0}})
        if self.path.startswith("/alpha/billing/subscriptions"):
            return self._json(200, {"data": {"planId": STUB_PLAN, "status": "active",
                                             "currentPeriodStart": "2026-09-01T00:00:00Z"}})
        if self.path == "/provider/v1/models":
            return self._json(200, {"data": STUB_MODELS})
        self._json(404, {"error": "not found"})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        auth = self.headers.get("Authorization", "")
        key = auth.removeprefix("Bearer ")
        if self.path in ("/alpha/fingerprint/record", "/alpha/lifecycle-events"):
            return self._json(200, {"ok": True})
        if self.path == "/alpha/generate":
            if key in RATE_LIMITED:
                return self._json(429, {"error": {"message": "rate limited"}})
            if key == "user_dead":
                return self._json(500, {"error": {"message": "upstream broke"}})
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True

            def emit(obj):
                self.wfile.write((json.dumps(obj) + "\n").encode())
                self.wfile.flush()

            emit({"type": "start"})
            emit({"type": "start-step"})
            emit({"type": "text-start", "id": "t1"})
            emit({"type": "text-delta", "id": "t1", "delta": "Hello"})
            emit({"type": "text-delta", "id": "t1", "delta": " from stub"})
            emit({"type": "text-end", "id": "t1"})
            emit({"type": "reasoning-start", "id": "r1"})
            emit({"type": "reasoning-delta", "id": "r1", "delta": "thinking hard"})
            emit({"type": "reasoning-end", "id": "r1"})
            if b'"tools"' in body or b"get_weather" in body:
                emit({"type": "tool-call", "toolCallId": "call_stub_1",
                      "toolName": "get_weather", "input": {"city": "SF"}})
            emit({"type": "finish-step"})
            emit({"type": "finish", "finishReason": "stop",
                  "totalUsage": {"inputTokens": 100, "outputTokens": 25,
                                 "cachedInputTokens": 60,
                                 "inputTokenDetails": {"noCacheTokens": 40,
                                                       "cacheReadTokens": 60}}})
            self.wfile.write(b"0\r\n\r\n")
            return
        self._json(404, {"error": "not found"})


if __name__ == "__main__":
    ThreadingHTTPServer(("127.0.0.1", PORT), H).serve_forever()
