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

    # -- lifecycle ---------------------------------------------------------
    def build_host(self, *, root, enable_plugins=True, plugin_config=None, extra_plugin_yaml="", client_keys=None, request_retry=3):
        config = HostConfig(
            root=Path(root),
            plugin_library=self.plugin,
            upstream_base_url=self.mock.base_url,
            management_secret=self.management_secret,
            client_keys=client_keys if client_keys is not None else [self.original_client_key],
            openai_models=[{"name": "synthetic-chat", "alias": "synthetic-chat"}],
            gemini_models=[{"name": "gemini-2.5-flash", "alias": "gemini-2.5-flash"}],
            plugin_id=self.plugin_id,
            plugin_config=plugin_config or {"data_dir": str(Path(root) / "data")},
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
