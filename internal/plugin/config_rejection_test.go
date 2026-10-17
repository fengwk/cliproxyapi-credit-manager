package plugin

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// A direct lifecycle rejection must name the invalid field and leave the live
// manager untouched. Host-level file reload may unload the library separately.
func TestRetiredConfigurationNamesFieldAndPreservesLiveManager(t *testing.T) {
	for _, setting := range []string{"warmup", "max_pending_requests"} {
		t.Run(setting, func(t *testing.T) {
			h := newHarness(t)
			raw := append(h.yaml(), []byte(setting+": null\n")...)
			envelope := h.call(pluginabi.MethodPluginReconfigure, lifecycleRequest{
				ConfigYAML: raw, SchemaVersion: pluginabi.SchemaVersion,
			})
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != "invalid_config" ||
				!strings.Contains(envelope.Error.Message, setting) {
				t.Fatalf("configuration rejection must name %s: %+v", setting, envelope)
			}
			if got := h.intercept(h.request("after-rejection", "trace-after-rejection")); got.Terminate {
				t.Fatalf("direct rejected configuration changed live admission: %+v", got)
			}
			if got := h.m.LastError(); got != "" {
				t.Fatalf("invalid candidate poisoned live health: %s", got)
			}
		})
	}
}
