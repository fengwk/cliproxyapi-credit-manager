"""HTTP helpers for the plugin management API and the proxy client API."""

from __future__ import annotations

import json
from urllib.parse import urlencode

from .host import http

MANAGER_BASE = "/v0/management/plugins/credit-manager"


class Response:
    def __init__(self, status, raw):
        self.status = status
        self.raw = raw

    @property
    def text(self):
        try:
            return self.raw.decode()
        except Exception:  # noqa: BLE001
            return repr(self.raw)

    def json(self):
        return json.loads(self.raw)

    def __repr__(self):
        return f"Response(status={self.status}, body={self.text[:200]!r})"


class Manager:
    """Client for the plugin's management API through the host."""

    def __init__(self, base_url, secret, plugin_id="credit-manager", base=MANAGER_BASE):
        self.base_url = base_url
        self.secret = secret
        self.plugin_id = plugin_id
        self.base = base

    def _hdr(self, extra=None, with_auth=True):
        headers = {}
        if with_auth:
            headers["Authorization"] = f"Bearer {self.secret}"
        headers.update(extra or {})
        return headers

    def call(self, method, suffix, body=None, query=None, headers=None, with_auth=True, timeout=20):
        path = self.base + suffix
        if query:
            path += "?" + urlencode(query)
        return Response(
            *http(method, self.base_url + path, body=body, headers=self._hdr(headers, with_auth), timeout=timeout)
        )

    # Convenience wrappers -------------------------------------------------
    def health(self):
        return self.call("GET", "/api/health")

    def keys(self):
        return self.call("GET", "/api/keys")

    def create_key(self, spec):
        return self.call("POST", "/api/keys", spec)

    def update_key(self, key_id, spec):
        return self.call("PUT", "/api/keys", {"id": key_id, "spec": spec})

    def rotate(self, key_id, key=None):
        body = {"id": key_id}
        if key is not None:
            body["key"] = key
        return self.call("POST", "/api/rotate", body)

    def prices(self):
        return self.call("GET", "/api/prices")

    def put_price(self, price):
        return self.call("PUT", "/api/prices", price)

    def usage(self, **query):
        return self.call("GET", "/api/usage", query=query)

    def capture(self, request_id):
        return self.call("GET", "/api/capture", query={"id": request_id})

    def resolve(self, request_id, action, amount_micro_usd=0, reason="synthetic"):
        return self.call(
            "POST",
            "/api/resolve",
            {"id": request_id, "action": action, "amount_micro_usd": amount_micro_usd, "reason": reason},
        )


class Client:
    """Client-facing proxy API (OpenAI + Gemini routes)."""

    def __init__(self, base_url):
        self.base_url = base_url

    def post(self, path, body, headers=None, timeout=30, raw=False):
        payload = body if raw else json.dumps(body).encode()
        return Response(*http("POST", self.base_url + path, body=payload, headers=headers, timeout=timeout))

    def get(self, path, headers=None, timeout=30):
        return Response(*http("GET", self.base_url + path, headers=headers, timeout=timeout))

    def chat(self, key, model, stream=False, content="synthetic", max_tokens=None, location="bearer", extra=None):
        body = {
            "model": model,
            "messages": [{"role": "user", "content": content}],
            "stream": stream,
        }
        if max_tokens is not None:
            body["max_tokens"] = max_tokens
        if extra:
            body.update(extra)
        return self.post("/v1/chat/completions", body, headers=auth_headers(key, location))

    def gemini(self, key, model, action="generateContent", location="goog", body=None, extra=None):
        payload = body or {"contents": [{"role": "user", "parts": [{"text": "synthetic"}]}]}
        payload = dict(payload)
        if extra:
            payload.update(extra)
        suffix = f"/v1beta/models/{model}:{action}"
        if action == "streamGenerateContent":
            suffix += "?alt=sse"
        return self.post(suffix, payload, headers=auth_headers(key, location))


def auth_headers(key, location="bearer"):
    if location == "bearer":
        return {"Authorization": f"Bearer {key}"}
    if location == "x-goog-api-key":
        return {"x-goog-api-key": key}
    if location == "x-api-key":
        return {"x-api-key": key}
    if location == "none":
        return {}
    raise ValueError(f"unknown auth location {location}")
