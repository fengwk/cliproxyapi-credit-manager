"""Launch and stop an isolated real CLIProxyAPI host with the native plugin.

The host runs from a private directory with synthetic configuration only: a
random loopback port, synthetic management secret, synthetic client keys and a
loopback upstream mock. No real profile, credential or database is touched.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import time
from pathlib import Path
from urllib.error import HTTPError, URLError
from urllib.request import ProxyHandler, Request, build_opener

OPENER = build_opener(ProxyHandler({}))


def free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def http(method, url, body=None, headers=None, timeout=20):
    """Minimal HTTP call returning (status, bytes). Never raises on 4xx/5xx."""
    data = None
    hdrs = dict(headers or {})
    if body is not None:
        data = json.dumps(body).encode() if not isinstance(body, (bytes, bytearray)) else bytes(body)
        hdrs.setdefault("Content-Type", "application/json")
    req = Request(url, data=data, headers=hdrs, method=method)
    try:
        with OPENER.open(req, timeout=timeout) as response:
            return response.status, response.read()
    except HTTPError as error:
        return error.code, error.read()
    except URLError as error:
        return 0, str(error).encode()


class HostConfig:
    def __init__(
        self,
        root: Path,
        plugin_library: Path,
        upstream_base_url: str,
        management_secret: str,
        client_keys,
        openai_models,
        gemini_models,
        plugin_id: str = "credit-manager",
        plugin_config: dict | None = None,
        plugins_dir=None,
        extra_plugin_yaml: str = "",
        extra_top_yaml: str = "",
        enable_plugins: bool = True,
        request_retry: int = 3,
        openai_upstream_keys=None,
        gemini_upstream_keys=None,
        session_affinity: bool = False,
        session_affinity_ttl: str = "1h",
    ):
        self.root = Path(root)
        self.plugin_library = Path(plugin_library)
        self.upstream_base_url = upstream_base_url
        self.management_secret = management_secret
        self.client_keys = list(client_keys)
        self.openai_models = list(openai_models)
        self.gemini_models = list(gemini_models)
        self.plugin_id = plugin_id
        self.plugin_config = dict(plugin_config or {})
        self.plugins_dir = Path(plugins_dir) if plugins_dir else None
        self.extra_plugin_yaml = extra_plugin_yaml
        self.extra_top_yaml = extra_top_yaml
        self.enable_plugins = enable_plugins
        self.request_retry = request_retry
        self.session_affinity = session_affinity
        self.session_affinity_ttl = session_affinity_ttl
        self.openai_upstream_keys = list(openai_upstream_keys or [])
        self.gemini_upstream_keys = list(gemini_upstream_keys or [])
        self.port = free_port()

    @property
    def base_url(self) -> str:
        return f"http://127.0.0.1:{self.port}"

    def _plugins_root(self) -> Path:
        return self.plugins_dir or (self.root / "plugins")

    def _plugin_path(self) -> Path:
        return self._plugins_root() / "linux" / "amd64" / f"{self.plugin_id}.so"

    def render(self) -> str:
        lines = []
        lines.append(f'server:')
        lines.append(f'  host: "127.0.0.1"')
        lines.append(f'  port: {self.port}')
        lines.append('management:')
        lines.append('  allow-remote: false')
        lines.append(f'  secret-key: "{self.management_secret}"')
        lines.append('  disable-control-panel: true')
        lines.append('  disable-auto-update-panel: true')
        lines.append('access:')
        lines.append('  api-keys:')
        for key in self.client_keys:
            lines.append(f'    - "{key}"')
        lines.append('routing:')
        # `session-affinity` and its TTL are direct children of `routing`, next
        # to `retry`; they are rendered once here so no second top-level
        # `routing:` block is emitted.
        if self.session_affinity:
            lines.append('  session-affinity: true')
            lines.append(f'  session-affinity-ttl: "{self.session_affinity_ttl}"')
        lines.append('  retry:')
        lines.append(f'    request-retry: {self.request_retry}')
        lines.append('oauth:')
        lines.append(f'  auth-dir: "{self.root / "auths"}"')
        if self.openai_models:
            lines.append('api-keys:')
            lines.append('  openai-compatibility:')
            lines.append('    - name: "synthetic-openai"')
            lines.append(f'      base-url: "{self.upstream_base_url}"')
            lines.append('      models:')
            for model in self.openai_models:
                name = model["name"] if isinstance(model, dict) else model
                alias = model.get("alias", name) if isinstance(model, dict) else name
                lines.append(f'        - name: "{name}"')
                lines.append(f'          alias: "{alias}"')
            lines.append('      keys:')
            for key in self.openai_upstream_keys or ["synthetic-openai-upstream"]:
                lines.append(f'        - api-key: "{key}"')
        if self.gemini_models:
            if not self.openai_models:
                lines.append('api-keys:')
            lines.append('  gemini:')
            lines.append('    - name: "synthetic-gemini"')
            lines.append(f'      base-url: "{self.upstream_base_url}"')
            lines.append('      models:')
            for model in self.gemini_models:
                name = model["name"] if isinstance(model, dict) else model
                alias = model.get("alias", name) if isinstance(model, dict) else name
                lines.append(f'        - name: "{name}"')
                lines.append(f'          alias: "{alias}"')
            lines.append('      keys:')
            for key in self.gemini_upstream_keys or ["synthetic-gemini-upstream"]:
                lines.append(f'        - api-key: "{key}"')
        lines.append('plugins:')
        lines.append(f'  enabled: {str(self.enable_plugins).lower()}')
        lines.append(f'  dir: "{self._plugins_root()}"')
        lines.append('  configs:')
        lines.append(f'    {self.plugin_id}:')
        lines.append('      enabled: true')
        _emit_mapping(lines, 6, self.plugin_config)
        if self.extra_plugin_yaml:
            lines.append(self.extra_plugin_yaml.rstrip("\n"))
        if self.extra_top_yaml:
            lines.append(self.extra_top_yaml.rstrip("\n"))
        return "\n".join(lines) + "\n"

    def write(self) -> Path:
        for name in ("tmp", "auths", "data"):
            (self.root / name).mkdir(parents=True, exist_ok=True)
        dest = self._plugin_path()
        dest.parent.mkdir(parents=True, exist_ok=True)
        if self.enable_plugins and self.plugin_library.exists():
            shutil.copy2(self.plugin_library, dest)
        config_path = self.root / "config.yaml"
        config_path.write_text(self.render())
        return config_path


class Host:
    def __init__(self, binary: Path, config: HostConfig):
        self.binary = Path(binary)
        self.config = config
        self.process: subprocess.Popen | None = None
        self.log_path = config.root / "host.log"
        self._log = None

    def start(self, timeout=45) -> None:
        config_path = self.config.write()
        self._log = self.log_path.open("wb")
        env = {
            "HOME": str(self.config.root),
            "TMPDIR": str(self.config.root / "tmp"),
            "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
            "NO_PROXY": "*",
            "no_proxy": "*",
        }
        self.process = subprocess.Popen(
            [str(self.binary), "--config", str(config_path), "--local-model"],
            cwd=str(self.config.root),
            env=env,
            stdout=self._log,
            stderr=subprocess.STDOUT,
        )
        self._wait_ready(timeout)

    def _wait_ready(self, timeout) -> None:
        deadline = time.monotonic() + timeout
        route = f"{self.config.base_url}/v0/management/plugins"
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise RuntimeError(
                    f"host exited early rc={self.process.returncode}; see {self.log_path}"
                )
            status, raw = http(
                "GET",
                route,
                headers={"Authorization": f"Bearer {self.config.management_secret}"},
                timeout=5,
            )
            if status == 200:
                return
            time.sleep(0.15)
        raise RuntimeError(f"host readiness timeout; see {self.log_path}")

    def plugin_registration(self, timeout=20) -> dict:
        route = f"{self.config.base_url}/v0/management/plugins"
        deadline = time.monotonic() + timeout
        last = {}
        while time.monotonic() < deadline:
            status, raw = self.management("GET", "/v0/management/plugins")
            if status == 200:
                last = json.loads(raw)
                plugins = {entry["id"]: entry for entry in last.get("plugins", [])}
                if self.config.plugin_id in plugins:
                    entry = plugins[self.config.plugin_id]
                    if entry.get("registered"):
                        return last
            time.sleep(0.15)
        return last

    def management(self, method, path, body=None, headers=None, timeout=20):
        hdrs = {"Authorization": f"Bearer {self.config.management_secret}"}
        hdrs.update(headers or {})
        return http(method, self.config.base_url + path, body=body, headers=hdrs, timeout=timeout)

    def stop(self) -> None:
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=5)
        if self._log is not None:
            self._log.close()
            self._log = None

    def __enter__(self):
        self.start()
        return self

    def __exit__(self, *exc):
        self.stop()
        return False


def _yaml_scalar(value):
    if isinstance(value, bool):
        return str(value).lower()
    if isinstance(value, (int, float)):
        return str(value)
    if value is None:
        return "null"
    return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"') + '"'


def _emit_mapping(lines, indent, mapping):
    pad = " " * indent
    for key, value in mapping.items():
        if isinstance(value, dict):
            lines.append(f'{pad}{key}:')
            _emit_mapping(lines, indent + 2, value)
        elif isinstance(value, (list, tuple)):
            lines.append(f'{pad}{key}:')
            _emit_list(lines, indent + 2, value)
        else:
            lines.append(f'{pad}{key}: {_yaml_scalar(value)}')


def _emit_list(lines, indent, items):
    """Emit a YAML sequence, including sequences of mappings (e.g. warmup targets)."""
    pad = " " * indent
    for entry in items:
        if isinstance(entry, dict):
            block = []
            _emit_mapping(block, indent + 2, entry)
            if block:
                block[0] = f'{pad}- ' + block[0].lstrip()
            lines.extend(block)
        elif isinstance(entry, (list, tuple)):
            lines.append(f'{pad}-')
            _emit_list(lines, indent + 2, entry)
        else:
            lines.append(f'{pad}- {_yaml_scalar(entry)}')
