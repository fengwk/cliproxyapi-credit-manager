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
    """Client for the plugin's management API through the host.

    Two identifiers are involved and they are NOT the same string:

    * The plugin's *declared* management route prefix, which the plugin fixes at
      ``/v0/management/plugins/credit-manager`` and which serves the financial
      ``/api/*`` routes.
    * The host's plugin *instance* id (the loaded library id, i.e. the
      acceptance ``--plugin-id``, default ``cliproxyapi-credit-manager``), which
      names the instance in the host config and therefore names the host's
      generic ``/v0/management/plugins/<id>/config`` route.

    The default instance id is the released library id, which the host derives
    from the plugin library filename, so the config route uses that id while the
    financial routes stay on the plugin's fixed prefix.
    """

    def __init__(self, base_url, secret, plugin_id="cliproxyapi-credit-manager", base=MANAGER_BASE):
        self.base_url = base_url
        self.secret = secret
        self.plugin_id = plugin_id          # host plugin instance id (library id)
        self.instance_id = plugin_id
        self.base = base                    # plugin-declared route prefix for /api/*
        self.config_base = f"/v0/management/plugins/{plugin_id}"

    def _hdr(self, extra=None, with_auth=True):
        headers = {}
        if with_auth:
            headers["Authorization"] = f"Bearer {self.secret}"
        headers.update(extra or {})
        return headers

    def call(self, method, suffix, body=None, query=None, headers=None, with_auth=True, timeout=20, base=None):
        path = (base or self.base) + suffix
        if query:
            path += "?" + urlencode(query)
        return Response(
            *http(method, self.base_url + path, body=body, headers=self._hdr(headers, with_auth), timeout=timeout)
        )

    # Convenience wrappers -------------------------------------------------
    def config_call(self, method, body=None, timeout=20):
        """Call the host's generic plugin config route for this instance.

        This is a host route keyed by the plugin instance id, not by the
        plugin's declared management route prefix.
        """
        return self.call(method, "/config", body=body, timeout=timeout, base=self.config_base)

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

    # Extended management surface (aligned features) ------------------------
    def callers(self):
        return self.call("GET", "/api/callers")

    def create_caller(self, spec):
        return self.call("POST", "/api/callers", spec)

    def update_caller(self, spec):
        return self.call("PUT", "/api/callers", spec)

    def audit(self, **query):
        return self.call("GET", "/api/audit", query=query)

    def overview(self, **query):
        return self.call("GET", "/api/overview", query=query)

    def model_catalog(self, **query):
        return self.call("GET", "/api/model-catalog", query=query)

    def exchange_rate(self, **query):
        return self.call("GET", "/api/exchange-rate", query=query)

    def pricing_rules(self):
        return self.call("GET", "/api/pricing-rules")

    def put_pricing_rule(self, rule):
        return self.call("PUT", "/api/pricing-rules", rule)

    def delete_pricing_rule(self, rule_id):
        return self.call("POST", "/api/delete-price-rule", {"id": rule_id})

    def price_preview(self, **query):
        return self.call("GET", "/api/price-preview", query=query)

    def reveal(self, key_id):
        return self.call("POST", "/api/reveal", {"id": key_id})

    def delete_key(self, key_id, reason):
        return self.call("POST", "/api/delete-key", {"id": key_id, "reason": reason})

    def reset_spend(self, spec):
        return self.call("POST", "/api/reset-spend", spec)


class Client:
    """Client-facing proxy API (OpenAI + Gemini routes)."""

    def __init__(self, base_url):
        self.base_url = base_url

    def post(self, path, body, headers=None, timeout=30, raw=False):
        payload = body if raw else json.dumps(body).encode()
        return Response(*http("POST", self.base_url + path, body=payload, headers=headers, timeout=timeout))

    def get(self, path, headers=None, timeout=30):
        return Response(*http("GET", self.base_url + path, headers=headers, timeout=timeout))

    def chat(self, key, model, stream=False, content="synthetic", max_tokens=None, location="bearer", extra=None, timeout=30):
        body = {
            "model": model,
            "messages": [{"role": "user", "content": content}],
            "stream": stream,
        }
        if max_tokens is not None:
            body["max_tokens"] = max_tokens
        if extra:
            body.update(extra)
        return self.post("/v1/chat/completions", body, headers=auth_headers(key, location), timeout=timeout)

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
    if location in ("x-goog-api-key", "goog"):
        return {"x-goog-api-key": key}
    if location == "x-api-key":
        return {"x-api-key": key}
    if location == "none":
        return {}
    raise ValueError(f"unknown auth location {location}")
