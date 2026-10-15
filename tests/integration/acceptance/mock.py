"""Loopback OpenAI/Gemini-compatible upstream mock.

The mock is fully synthetic and deterministic. It records every upstream call so
a test can assert the real host actually retried or did not call upstream. It can
be scripted per upstream credential key so retry tests can answer 429 on the
first credential and 200 on the second while the client only ever issued one
request.
"""

from __future__ import annotations

import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Simplest genuinely complete OpenAI usage: input 10, output 4, total 14. With a
# price of 1 microUSD per token this is exactly 14 microUSD of cost.
DEFAULT_USAGE = {"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14}


def wire(value) -> bytes:
    return json.dumps(value, separators=(",", ":")).encode()


def free_port() -> int:
    import socket

    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


class Script:
    """Per-upstream-credential response script.

    status: HTTP status to answer.
    usage: usage object mixed into a success response.
    delay: seconds to sleep before answering (for cancel tests).
    body: optional raw JSON body to answer instead of a generated completion.
    """

    def __init__(self, status=200, usage=None, delay=0.0, body=None, text="synthetic answer", omit_usage=False):
        self.status = status
        self.usage = dict(usage) if usage is not None else dict(DEFAULT_USAGE)
        self.delay = delay
        self.body = body
        self.text = text
        self.omit_usage = omit_usage


class UpstreamMock:
    def __init__(self, upstream_keys, scripts=None):
        self.upstream_keys = list(upstream_keys)
        # scripts maps an upstream credential key to a Script. The default script
        # applies to any key without a specific one.
        self.scripts = dict(scripts or {})
        self.default_script = Script()
        self.calls = []
        # When fail_first > 0, the next N upstream calls answer fail_status before
        # any per-key script, so credential failover can be exercised.
        self.fail_first = 0
        self.fail_status = 429
        self._lock = threading.Lock()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), self._handler())
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.server.server_port}"

    def start(self) -> None:
        self.thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def calls_for(self, path_contains=None):
        with self._lock:
            rows = list(self.calls)
        if path_contains is None:
            return rows
        return [row for row in rows if path_contains in row["path"]]

    def count(self, path_contains=None) -> int:
        return len(self.calls_for(path_contains))

    def reset(self) -> None:
        with self._lock:
            self.calls = []

    def set_script(self, upstream_key, script: Script) -> None:
        with self._lock:
            self.scripts[upstream_key] = script

    def _script_for(self, upstream_key):
        with self._lock:
            return self.scripts.get(upstream_key, self.default_script)

    def _handler(self):
        mock = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *args):  # silence stderr noise
                pass

            def _read_body(self):
                length = int(self.headers.get("Content-Length", 0) or 0)
                raw = self.rfile.read(length) if length else b""
                try:
                    return json.loads(raw) if raw else {}
                except ValueError:
                    return {}

            def _upstream_key(self, gemini):
                if gemini:
                    return self.headers.get("x-goog-api-key")
                auth = self.headers.get("Authorization", "")
                if auth.lower().startswith("bearer "):
                    return auth[7:]
                return None

            def _record(self, path, body, gemini):
                row = {
                    "path": path,
                    "upstream_key": self._upstream_key(gemini),
                    "body": body,
                    "stream": bool(body.get("stream")) or ":streamGenerateContent" in path,
                    "at": time.time(),
                }
                with mock._lock:
                    mock.calls.append(row)
                return row

            def _send(self, status, payload, content_type="application/json", extra_headers=None):
                raw = payload if isinstance(payload, (bytes, bytearray)) else wire(payload)
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", content_type)
                    self.send_header("Content-Length", str(len(raw)))
                    for name, value in (extra_headers or {}).items():
                        self.send_header(name, value)
                    self.end_headers()
                    self.wfile.write(raw)
                except (BrokenPipeError, ConnectionResetError, OSError):
                    # The client cancelled; nothing to answer.
                    pass

            def do_POST(self):
                path = self.path
                gemini = path.startswith("/v1beta")
                body = self._read_body()
                row = self._record(path, body, gemini)
                with mock._lock:
                    if mock.fail_first > 0:
                        mock.fail_first -= 1
                        forced = mock.fail_status
                    else:
                        forced = 0
                if forced:
                    self._send(forced, {"error": {"message": f"synthetic forced {forced}",
                                                  "type": "synthetic_error", "code": forced}})
                    return
                script = mock._script_for(row["upstream_key"])
                if script.delay:
                    time.sleep(script.delay)
                if script.status != 200:
                    error = {
                        "error": {
                            "message": f"synthetic upstream status {script.status}",
                            "type": "synthetic_error",
                            "code": script.status,
                        }
                    }
                    self._send(script.status, error)
                    return
                model = body.get("model") or "synthetic-model"
                if gemini:
                    self._send_gemini(script, model, path)
                else:
                    self._send_openai(script, model, bool(body.get("stream")))

            def _openai_completion(self, script, model):
                payload = {
                    "id": "chatcmpl-synthetic",
                    "object": "chat.completion",
                    "created": int(time.time()),
                    "model": model,
                    "choices": [
                        {
                            "index": 0,
                            "message": {"role": "assistant", "content": script.text},
                            "finish_reason": "stop",
                        }
                    ],
                }
                if not script.omit_usage:
                    payload["usage"] = script.usage
                return payload

            def _send_openai(self, script, model, stream):
                if script.body is not None:
                    self._send(200, script.body)
                    return
                payload = self._openai_completion(script, model)
                if not stream:
                    self._send(200, payload)
                    return
                chunks = [
                    {
                        "id": "chatcmpl-synthetic",
                        "object": "chat.completion.chunk",
                        "model": model,
                        "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}],
                    },
                    {
                        "id": "chatcmpl-synthetic",
                        "object": "chat.completion.chunk",
                        "model": model,
                        "choices": [{"index": 0, "delta": {"content": script.text}, "finish_reason": None}],
                    },
                    {
                        "id": "chatcmpl-synthetic",
                        "object": "chat.completion.chunk",
                        "model": model,
                        "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}],
                        "usage": script.usage,
                    },
                ]
                out = b"".join(b"data: " + wire(chunk) + b"\n\n" for chunk in chunks) + b"data: [DONE]\n\n"
                self._send(200, out, content_type="text/event-stream")

            def _send_gemini(self, script, model, path):
                if "streamGenerateContent" in path:
                    payload = {
                        "candidates": [
                            {"content": {"role": "model", "parts": [{"text": script.text}]},
                             "finishReason": "STOP", "index": 0}
                        ],
                        "usageMetadata": {
                            "promptTokenCount": script.usage.get("prompt_tokens", 0),
                            "candidatesTokenCount": script.usage.get("completion_tokens", 0),
                            "totalTokenCount": script.usage.get("total_tokens", 0),
                        },
                        "modelVersion": model,
                    }
                    self._send(200, b"data: " + wire(payload) + b"\n\n", content_type="text/event-stream")
                    return
                payload = {
                    "candidates": [
                        {"content": {"role": "model", "parts": [{"text": script.text}]},
                         "finishReason": "STOP", "index": 0}
                    ],
                    "usageMetadata": {
                        "promptTokenCount": script.usage.get("prompt_tokens", 0),
                        "candidatesTokenCount": script.usage.get("completion_tokens", 0),
                        "totalTokenCount": script.usage.get("total_tokens", 0),
                    },
                    "modelVersion": model,
                }
                self._send(200, payload)

        return Handler
