"""Direct native ABI probe.

The host listing does not expose the negotiated schema version or capability
set, so this case loads the real plugin shared library and drives its documented
C ABI (``cliproxy_plugin_init`` + ``cliproxyPluginCall``) with ctypes. The
credit-manager makes no host callbacks -- it is passive, with no executor,
router, scheduler or model-execution bridge -- so a host API with ABI 1 and no
function pointers is sufficient. This verifies ABI 1 / schema 6 negotiation,
that an undefined or older schema is refused, that no
executor/router/scheduler capability is declared, and that registering
issues no host model-execution callback.
"""

from __future__ import annotations

import base64
import ctypes
import json
from pathlib import Path

from ..report import check, check_eq

CASES = []


def case(name, expects):
    def wrap(fn):
        CASES.append({"name": name, "expects": expects, "fn": fn})
        return fn
    return wrap


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("len", ctypes.c_size_t)]


HOST_CALL = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_void_p, ctypes.c_char_p, ctypes.c_void_p,
                             ctypes.c_size_t, ctypes.POINTER(Buffer))
HOST_FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)


class HostAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("host_ctx", ctypes.c_void_p),
                ("call", HOST_CALL), ("free_buffer", HOST_FREE)]


PLUGIN_CALL = ctypes.CFUNCTYPE(ctypes.c_int, ctypes.c_char_p, ctypes.c_void_p, ctypes.c_size_t,
                               ctypes.POINTER(Buffer))
PLUGIN_FREE = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)
PLUGIN_SHUTDOWN = ctypes.CFUNCTYPE(None)


class PluginAPI(ctypes.Structure):
    _fields_ = [("abi_version", ctypes.c_uint32), ("call", PLUGIN_CALL),
                ("free_buffer", PLUGIN_FREE), ("shutdown", PLUGIN_SHUTDOWN)]


def _noop_free(*_args):
    return None


class NativePlugin:
    def __init__(self, library: Path):
        self.lib = ctypes.CDLL(str(library))
        self.lib.cliproxy_plugin_init.restype = ctypes.c_int
        self.lib.cliproxy_plugin_init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
        # Record every host callback the plugin makes. A passive plugin must issue
        # none: in particular no host model-execution bridge may exist.
        self.host_calls = []

        def record_host_call(_ctx, method, _payload, _length, _out):
            self.host_calls.append(method.decode(errors="replace") if method else "")
            return 1

        self._call_keepalive = HOST_CALL(record_host_call)
        self._free_keepalive = HOST_FREE(_noop_free)
        self.host = HostAPI(abi_version=1, host_ctx=None,
                            call=self._call_keepalive, free_buffer=self._free_keepalive)
        self.api = PluginAPI()

    def init(self) -> int:
        return self.lib.cliproxy_plugin_init(ctypes.byref(self.host), ctypes.byref(self.api))

    def call(self, method: str, body=None):
        payload = b""
        if body is not None:
            payload = json.dumps(body).encode()
        buf = Buffer()
        ptr = ctypes.cast(ctypes.create_string_buffer(payload), ctypes.c_void_p) if payload else None
        rc = self.api.call(method.encode(), ptr, len(payload), ctypes.byref(buf))
        data = b""
        if buf.ptr:
            data = ctypes.string_at(buf.ptr, buf.len)
            self.api.free_buffer(buf.ptr, buf.len)
        return rc, data

    def shutdown(self):
        try:
            self.api.shutdown()
        except Exception:  # noqa: BLE001
            pass


@case("native.abi_probe", "real .so negotiates ABI1/schema6, refuses an undefined/older schema and declares only passive capabilities")
def abi_probe(ctx):
    plugin = NativePlugin(ctx.plugin)
    rc = plugin.init()
    check_eq(rc, 0, "cliproxy_plugin_init return code")
    check_eq(plugin.api.abi_version, 1, "plugin-reported ABI version")

    data_dir = ctx.artifacts / "abi-probe-data"
    data_dir.mkdir(parents=True, exist_ok=True)
    config_yaml = f"data_dir: {data_dir}\n".encode()
    register_body = {"config_yaml": base64.b64encode(config_yaml).decode(), "schema_version": 6}
    rc, raw = plugin.call("plugin.register", register_body)
    check_eq(rc, 0, "plugin.register rc")
    envelope = json.loads(raw)
    check(envelope.get("ok"), f"register envelope not ok: {envelope}")
    result = envelope["result"]
    check_eq(result.get("schema_version"), 6, "negotiated schema version")
    caps = result.get("capabilities", {})
    for flag in ("frontend_auth_provider", "frontend_auth_provider_exclusive",
                 "request_interceptor", "request_lifecycle_plugin", "usage_plugin", "management_api"):
        check(caps.get(flag) is True, f"capability {flag} true required: {caps}")
    for absent in ("executor", "scheduler", "scheduler_across_priorities", "model_router", "model_provider",
                   "model_registrar", "request_translator", "request_normalizer", "response_translator",
                   "response_interceptor", "response_stream_interceptor", "thinking_applier", "quota_provider",
                   "auth_provider", "command_line_plugin", "websocket_response_observer"):
        check(not caps.get(absent), f"capability {absent} must be absent: {caps}")

    rc, raw = plugin.call("frontend_auth.identifier", {})
    ident = json.loads(raw)["result"]["identifier"]
    check_eq(ident, "credit-manager", "frontend auth provider identifier")

    # A host whose RPC schema is missing, zero, or predates the current contract
    # must be refused: the plugin implements exactly one schema and never
    # negotiates down to an older or undefined one.
    for label, body in (
        ("missing", {"config_yaml": ""}),
        ("zero", {"config_yaml": "", "schema_version": 0}),
        ("one", {"config_yaml": "", "schema_version": 1}),
        ("five", {"config_yaml": "", "schema_version": 5}),
    ):
        rc, raw = plugin.call("plugin.register", body)
        refused = json.loads(raw)
        check(not refused.get("ok") and refused.get("error", {}).get("code") == "unsupported_host",
              f"{label} host schema must be refused with unsupported_host: {refused}")

    # A filename-derived identity outside the one published id must stop
    # admissions, not merely ignore its accounting notifications.
    rc, raw = plugin.call("management.register", {
        "ResourceBasePath": "/v0/resource/plugins/credit-manager",
    })
    refused = json.loads(raw)
    check(not refused.get("ok") and refused.get("error", {}).get("code") == "unsupported_plugin_id",
          f"a renamed library must be refused: {refused}")
    rc, raw = plugin.call("request.intercept_before", {})
    stopped = json.loads(raw)
    check(stopped.get("ok"), f"interceptor must return a termination envelope: {stopped}")
    termination = stopped["result"]
    check(termination.get("Terminate") is True and termination.get("StatusCode") == 503,
          f"a renamed library must not execute unbillable requests: {stopped}")

    plugin.shutdown()
    # No host model-execution bridge: the plugin never invokes a host callback at
    # any point of its lifecycle, and in particular nothing named like a model
    # execution. A warmup-era build would call `host.model.execute` here.
    model_calls = [name for name in plugin.host_calls if "model" in name.lower()]
    check_eq(model_calls, [], f"no host model-execution callback may be issued: {plugin.host_calls}")
    check_eq(plugin.host_calls, [], f"a passive plugin issues no host callback at all: {plugin.host_calls}")
    return {"abi_version": plugin.api.abi_version, "schema_version": result.get("schema_version"),
            "capabilities": caps, "identifier": ident, "host_calls": len(plugin.host_calls)}
