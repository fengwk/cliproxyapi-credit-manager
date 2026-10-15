package accounting

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func complete(provider, executor, model string, raw pluginapi.UsageDetail) bool {
	result := Normalize(pluginapi.UsageRecord{Provider: provider, ExecutorType: executor, Model: model, Detail: raw})
	return result.Complete && result.Quality == string(usage.TokenAccountingQualityComplete)
}

// TestSubsetProviderExcludesCacheAndReasoning checks the canonical bucket
// contract for an OpenAI-style record: cache is subtracted from input and
// reasoning from output, so the credit store sees non-overlapping buckets.
func TestSubsetProviderExcludesCacheAndReasoning(t *testing.T) {
	result := Normalize(pluginapi.UsageRecord{
		Provider:     "openai",
		ExecutorType: "openaiExecutor",
		Model:        "gpt-4o",
		Detail: pluginapi.UsageDetail{
			InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 200, ReasoningTokens: 100, TotalTokens: 1500,
		},
	})
	if !result.Complete {
		t.Fatalf("quality = %q, want complete", result.Quality)
	}
	want := struct{ input, cacheRead, output, reasoning, total int64 }{800, 200, 400, 100, 1500}
	if result.Tokens.Input != want.input || result.Tokens.CacheRead != want.cacheRead ||
		result.Tokens.Output != want.output || result.Tokens.Reasoning != want.reasoning || result.Tokens.Total != want.total {
		t.Fatalf("tokens = %+v, want input=%d cacheRead=%d output=%d reasoning=%d total=%d",
			result.Tokens, want.input, want.cacheRead, want.output, want.reasoning, want.total)
	}
	if !result.Accounted {
		t.Fatal("nonzero usage must be marked accounted")
	}
}

// TestIndependentProviderKeepsSeparateReasoning checks Anthropic-style buckets
// where uncached input, cache, ordinary output and reasoning are all separate.
func TestIndependentProviderKeepsSeparateReasoning(t *testing.T) {
	result := Normalize(pluginapi.UsageRecord{
		Provider:     "claude",
		ExecutorType: "claudeExecutor",
		Model:        "claude-sonnet",
		Detail: pluginapi.UsageDetail{
			InputTokens: 1000, OutputTokens: 500, ReasoningTokens: 120,
			CacheReadTokens: 300, CacheCreationTokens: 50, TotalTokens: 1970,
		},
	})
	if !result.Complete {
		t.Fatalf("quality = %q, want complete", result.Quality)
	}
	if result.Tokens.Input != 1000 || result.Tokens.Output != 500 {
		t.Fatalf("tokens = %+v, want input=1000 output=500", result.Tokens)
	}
	if result.Tokens.CacheRead != 300 || result.Tokens.CacheWrite != 50 || result.Tokens.Reasoning != 120 {
		t.Fatalf("tokens = %+v, want cacheRead=300 cacheWrite=50 reasoning=120", result.Tokens)
	}
}

// TestAdapterCohortRepairsCompleteCounters verifies that the exact
// commandcode/opencode-go adapter cohort is reconstructed from raw counters.
func TestAdapterCohortRepairsCompleteCounters(t *testing.T) {
	for _, provider := range []string{"opencode-go", "commandcode"} {
		if !complete(provider, adapterExecutor, provider+"/native-model", pluginapi.UsageDetail{
			InputTokens: 900, OutputTokens: 300, ReasoningTokens: 0, CacheReadTokens: 0, TotalTokens: 1200,
		}) {
			t.Fatalf("provider %s: adapter cohort record was not repaired to complete", provider)
		}
	}
}

// TestAdapterCohortRejectsMalformedCounters verifies that a malformed adapter
// record falls back to the SDK normalizer and never fabricates a complete bill.
func TestAdapterCohortRejectsMalformedCounters(t *testing.T) {
	cases := map[string]pluginapi.UsageDetail{
		"negative":              {InputTokens: -1, OutputTokens: 10, TotalTokens: 9},
		"reasoning-over":        {InputTokens: 10, OutputTokens: 10, ReasoningTokens: 20, TotalTokens: 20},
		"nonzero-without-total": {InputTokens: 10, OutputTokens: 10},
		"cache-alias-only":      {CachedTokens: 30, TotalTokens: 30},
	}
	for name, detail := range cases {
		result := Normalize(pluginapi.UsageRecord{Provider: "opencode-go", ExecutorType: adapterExecutor, Model: "opencode-go/x", Detail: detail})
		if result.Complete {
			t.Fatalf("%s: malformed counters were reported as complete: %+v", name, result.Tokens)
		}
		if result.Quality != string(usage.TokenAccountingQualityUnclassified) {
			t.Fatalf("%s: quality = %q, want unclassified", name, result.Quality)
		}
	}
}

// TestAdapterCohortRequiresNativeModelID verifies that a bare provider prefix
// without a native model id is not treated as an adapter cohort.
func TestAdapterCohortRequiresNativeModelID(t *testing.T) {
	record := pluginapi.UsageRecord{
		Provider: "opencode-go", ExecutorType: adapterExecutor, Model: "opencode-go/",
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	}
	if _, ok := recoverAdapterBreakdown(record.Provider, record.ExecutorType, record.Model, record.Detail); ok {
		t.Fatal("bare provider prefix must not be repaired as an adapter cohort")
	}
}

// TestEmptyUsageIsNotAccountedButValid checks that a record with no counters is
// still a valid complete zero and is never marked as accounted.
func TestEmptyUsageIsNotAccountedButValid(t *testing.T) {
	result := Normalize(pluginapi.UsageRecord{
		Provider: "gemini", ExecutorType: "geminiExecutor", Model: "gemini-2.0",
		Detail: pluginapi.UsageDetail{},
	})
	if result.Accounted {
		t.Fatal("a record with no counters must not be marked accounted")
	}
	if !result.Complete || result.Tokens.Total != 0 {
		t.Fatalf("empty usage = %+v, want complete zero", result)
	}
}

// TestUnknownProviderStaysUnclassified checks that an unknown provider is not
// guessed into complete accounting.
func TestUnknownProviderStaysUnclassified(t *testing.T) {
	result := Normalize(pluginapi.UsageRecord{
		Provider: "custom", ExecutorType: "customExecutor", Model: "custom-model",
		Detail: pluginapi.UsageDetail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
	})
	if result.Complete {
		t.Fatalf("unknown provider must not be complete: %+v", result.Tokens)
	}
	if result.Quality != string(usage.TokenAccountingQualityUnclassified) {
		t.Fatalf("quality = %q, want unclassified", result.Quality)
	}
}
