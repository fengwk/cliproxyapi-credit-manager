package plugin

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Source identifies an upstream account, not a trusted execution origin.
func TestCustomerUsageSourceCannotExemptBilling(t *testing.T) {
	for _, source := range []string{"internal", "warmup", "plugin_host_model_callback", " INTERNAL "} {
		t.Run(source, func(t *testing.T) {
			h := newHarness(t)
			if response := h.intercept(h.request("request-source", "trace-source")); response.Terminate {
				t.Fatalf("reserve: %+v", response)
			}
			h.usage(pluginapi.UsageRecord{
				RequestID: "execution-source", TraceID: "trace-source", Provider: "openai",
				Model: testModel, APIKey: h.key.Principal, Source: source,
				AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
				Detail:         pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
			})
			h.complete("request-source", "trace-source", pluginapi.RequestCompletionSucceeded)
			key, err := h.store().GetKey(context.Background(), h.key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if key.SpentMicroUSD != 20 || key.HeldMicroUSD != 0 || h.m.LastError() != "" {
				t.Fatalf("source %q: spent=%d held=%d health=%q", source,
					key.SpentMicroUSD, key.HeldMicroUSD, h.m.LastError())
			}
		})
	}
}

// Omitted timestamps must remain omitted until the ledger canonicalizes replays.
func TestNativeUsageWithoutTimestampReplaysIdempotently(t *testing.T) {
	h := newHarness(t)
	if response := h.intercept(h.request("request-replay", "trace-replay")); response.Terminate {
		t.Fatalf("reserve: %+v", response)
	}
	record := pluginapi.UsageRecord{
		RequestID: "execution-replay", TraceID: "trace-replay", Provider: "openai",
		Model: testModel, APIKey: h.key.Principal,
		AccessProvider: "plugin:cliproxyapi-credit-manager:" + authProviderID,
		Detail:         pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
	h.usage(record)
	h.usage(record)
	if message := h.m.LastError(); message != "" {
		t.Fatalf("identical replay degraded health: %s", message)
	}
}
