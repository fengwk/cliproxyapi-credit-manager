"""Shared acceptance context: mock upstream, isolated host, API clients."""

from __future__ import annotations

import json
from pathlib import Path

from .api import Client, Manager
from .host import Host, HostConfig
from .mock import UpstreamMock, free_port


class Context:
    def __init__(self, artifacts: Path, cpa: Path, plugin: Path, plugin_id="credit-manager"):
        self.artifacts = Path(artifacts)
        self.cpa = Path(cpa)
        self.plugin = Path(plugin)
        self.plugin_id = plugin_id
        self.management_secret = "synthetic-management-guard"
        self.original_client_key = "synthetic-original-cpa-client"
        self.openai_upstream_keys = ["synthetic-openai-upstream-a", "synthetic-openai-upstream-b"]
        self.gemini_upstream_keys = ["synthetic-gemini-upstream"]
        self.mock = UpstreamMock(self.openai_upstream_keys + self.gemini_upstream_keys)
        self.host = None
        self.manager = None
        self.client = None
        self.host_root = None
        self._host_seq = 0

    # -- lifecycle ---------------------------------------------------------
    def build_host(self, *, root, enable_plugins=True, plugin_config=None, extra_plugin_yaml="", client_keys=None, request_retry=3):
        config = HostConfig(
            root=Path(root),
            plugin_library=self.plugin,
            upstream_base_url=self.mock.base_url,
            management_secret=self.management_secret,
            client_keys=client_keys if client_keys is not None else [self.original_client_key],
            openai_models=[{"name": "synthetic-chat", "alias": "synthetic-chat"},
                           {"name": "synthetic-free", "alias": "synthetic-free"},
                           {"name": "synthetic-unpriced", "alias": "synthetic-unpriced"},
                           {"name": "synthetic-upstream-name", "alias": "synthetic-alias"}],
            gemini_models=[{"name": "gemini-2.5-flash", "alias": "gemini-2.5-flash"}],
            plugin_id=self.plugin_id,
            plugin_config=plugin_config or {"data_dir": str(Path(root) / "data")},
            plugins_dir=self.artifacts / "plugins",
            extra_plugin_yaml=extra_plugin_yaml,
            enable_plugins=enable_plugins,
            request_retry=request_retry,
            openai_upstream_keys=self.openai_upstream_keys,
            gemini_upstream_keys=self.gemini_upstream_keys,
        )
        self.host_config = config
        self.host = Host(self.cpa, config)
        self.host_root = Path(root)
        return self.host

    def wire_clients(self):
        self.manager = Manager(self.host.config.base_url, self.management_secret, self.plugin_id)
        self.client = Client(self.host.config.base_url)

    def recreate_host(self, name, *, root=None, enable_plugins=True, plugin_config=None, extra_plugin_yaml="", client_keys=None, request_retry=3):
        """Stop any running host and boot a fresh isolated one.

        Each case gets a clean credit database and configuration unless it passes
        an explicit root/plugin_config (used by restart/persistence cases).
        """
        if self.host is not None:
            self.host.stop()
            self.host = None
        self._host_seq += 1
        if root is None:
            safe = "".join(ch if ch.isalnum() or ch in "-." else "-" for ch in name)
            root = self.artifacts / f"host-{self._host_seq:02d}-{safe}"
        if plugin_config is None:
            plugin_config = {"data_dir": str(Path(root) / "data")}
        self.build_host(
            root=root,
            enable_plugins=enable_plugins,
            plugin_config=plugin_config,
            extra_plugin_yaml=extra_plugin_yaml,
            client_keys=client_keys,
            request_retry=request_retry,
        )
        self.host.start()
        self.wire_clients()
        self.host.plugin_registration()
        return self.host

    # -- helpers -----------------------------------------------------------
    def new_key(self, **spec):
        body = {
            "label": spec.pop("label", "synthetic"),
            "enabled": spec.pop("enabled", True),
            "quota_micro_usd": spec.pop("quota_micro_usd", 0),
            "daily_micro_usd": spec.pop("daily_micro_usd", 0),
            "weekly_micro_usd": spec.pop("weekly_micro_usd", 0),
            "monthly_micro_usd": spec.pop("monthly_micro_usd", 0),
            "max_concurrent": spec.pop("max_concurrent", 0),
            "allowed_models": spec.pop("allowed_models", []),
        }
        body.update(spec)
        response = self.manager.create_key(body)
        if response.status != 201:
            raise RuntimeError(f"key creation failed: {response}")
        data = response.json()
        return data["item"], data["key"]

    def set_price(self, model, input_rate=1_000_000, output_rate=1_000_000, cache_read=1_000_000, cache_write=1_000_000, reasoning=None):
        rates = {
            "input": input_rate,
            "output": output_rate,
            "cache_read": cache_read,
            "cache_write": cache_write,
        }
        if reasoning is not None:
            rates["reasoning"] = reasoning
        response = self.manager.put_price({"model": model, "rates": rates})
        if response.status != 200:
            raise RuntimeError(f"price upsert failed: {response}")
        return response

    def usage_rows(self, **query):
        response = self.manager.usage(**query)
        if response.status != 200:
            raise RuntimeError(f"usage query failed: {response}")
        return response.json()

    def find_request(self, key_id=None, model=None, search=None):
        """Return the newest matching request item from the usage report."""
        query = {"page": 1, "page_size": 50}
        if key_id:
            query["key_id"] = key_id
        if model:
            query["model"] = model
        if search:
            query["search"] = search
        report = self.usage_rows(**query)
        items = report.get("items") or []
        return items[0] if items else None

    @staticmethod
    def wait_until(fn, timeout=15, interval=0.1):
        """Poll fn until it returns something truthy or the timeout elapses."""
        import time as _time

        deadline = _time.monotonic() + timeout
        result = None
        while _time.monotonic() < deadline:
            result = fn()
            if result:
                return result
            _time.sleep(interval)
        return result


def load_specs(module_names, only=""):
    """Import case modules and collect (name, expects, callable) specs."""
    specs = []
    for module_name in module_names:
        module = __import__(f"acceptance.cases.{module_name}", fromlist=["CASES"])
        for spec in module.CASES:
            if only and only not in spec["name"]:
                continue
            specs.append(spec)
    return specs
