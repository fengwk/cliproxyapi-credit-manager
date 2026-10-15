// Package accounting normalizes the token counters delivered by the
// CLIProxyAPI SDK into the canonical, mutually exclusive buckets owned by the
// credit store.
//
// It performs no I/O and depends only on SDK and credit types, so the
// normalization contract stays unit-testable on its own.
//
// The adapter-cohort repair below is adapted from
// github.com/fengwk/cliproxyapi-observer/internal/observer/adapter_accounting.go
// (MIT License, Copyright (c) 2026 fengwk). The narrow compatibility scope,
// the "unique complete candidate" rule and the rejection of malformed counters
// are preserved verbatim in behavior.
package accounting

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/credit"
)

// The compatibility is deliberately narrow: only these exact upstream
// providers, the shared native executor implementation, and their own model
// namespaces are reinterpreted. Every other record keeps whatever accounting
// the SDK derived.
const (
	// adapterExecutor is the single executor implementation shared by the
	// native plugin adapters whose raw callback counters may be repaired.
	adapterExecutor = "executorAdapter"

	openCodeGoProvider    = "opencode-go"
	openCodeGoModelPrefix = "opencode-go/"

	commandCodeProvider    = "commandcode"
	commandCodeModelPrefix = "commandcode/"
)

// Result is the normalized token accounting for one usage record.
type Result struct {
	// Tokens holds the canonical credit buckets. Input is uncached input and
	// Output excludes reasoning, matching the credit pricing contract.
	Tokens credit.Tokens
	// Quality is the SDK accounting quality string: complete, inconsistent or
	// unclassified. Only complete accounting may be billed as exact.
	Quality string
	// Complete reports whether every raw counter was classified exactly.
	Complete bool
	// Accounted reports whether any token counter was observed at all. A
	// record with no usage is not a zero-usage record and must not settle.
	Accounted bool
}

// Normalize converts raw usage counters into canonical credit buckets.
//
// Records from the exact commandcode/opencode-go adapter cohorts are first
// reinterpreted with the strict unique-and-complete repair; every other record
// is normalized with the standard SDK method for its provider and executor.
// When normalization cannot confidently classify the counters the quality is
// reported as the SDK's unclassified state instead of a fabricated zero.
func Normalize(record pluginapi.UsageRecord) Result {
	if breakdown, ok := recoverAdapterBreakdown(record.Provider, record.ExecutorType, record.Model, record.Detail); ok {
		return resultFromBreakdown(breakdown)
	}
	detail := sdkDetail(record.Detail)
	breakdown := usage.EnsureTokenBreakdownForProvider(detail, record.Provider, record.ExecutorType).TokenBreakdown
	return resultFromBreakdown(breakdown)
}

// sdkDetail projects the plugin usage payload onto the SDK detail shape.
func sdkDetail(detail pluginapi.UsageDetail) usage.Detail {
	return usage.Detail{
		InputTokens:         detail.InputTokens,
		OutputTokens:        detail.OutputTokens,
		ReasoningTokens:     detail.ReasoningTokens,
		CachedTokens:        detail.CachedTokens,
		CacheReadTokens:     detail.CacheReadTokens,
		CacheCreationTokens: detail.CacheCreationTokens,
		TotalTokens:         detail.TotalTokens,
	}
}

// resultFromBreakdown maps a canonical breakdown onto credit buckets. A
// breakdown that does not satisfy the v2 invariants is reported as
// unclassified with no tokens, so an invalid record is never billed as zero.
func resultFromBreakdown(breakdown usage.TokenBreakdown) Result {
	if !breakdown.Valid() {
		return Result{Quality: string(usage.TokenAccountingQualityUnclassified)}
	}
	tokens := credit.Tokens{
		Input:      breakdown.Input.UncachedTokens,
		CacheRead:  breakdown.Input.CacheReadTokens,
		CacheWrite: breakdown.Input.CacheWriteTokens,
		Output:     breakdown.Output.NonReasoningTokens,
		Reasoning:  breakdown.Output.ReasoningTokens,
		Total:      breakdown.TotalTokens,
	}
	return Result{
		Tokens:   tokens,
		Quality:  string(breakdown.Quality),
		Complete: breakdown.Quality == usage.TokenAccountingQualityComplete,
		Accounted: tokens.Total > 0 || tokens.Input > 0 || tokens.CacheRead > 0 ||
			tokens.CacheWrite > 0 || tokens.Output > 0 || tokens.Reasoning > 0,
	}
}

// isAdapterCompatTarget reports whether a usage record belongs to one of the
// exact adapter cohorts whose canonical accounting may be reconstructed from
// raw counters. The executor must be the shared adapter implementation and the
// model must carry a non-empty native id inside the cohort's own namespace; a
// bare "<provider>/" prefix has no native id and is not a model.
func isAdapterCompatTarget(provider, executor, model string) bool {
	if executor != adapterExecutor {
		return false
	}
	switch provider {
	case openCodeGoProvider:
		return hasNativeModelID(model, openCodeGoModelPrefix)
	case commandCodeProvider:
		return hasNativeModelID(model, commandCodeModelPrefix)
	default:
		return false
	}
}

// hasNativeModelID reports whether model lives inside prefix and carries a
// non-empty native id after it.
func hasNativeModelID(model, prefix string) bool {
	return strings.HasPrefix(model, prefix) && len(model) > len(prefix)
}

// recoverAdapterBreakdown rebuilds a canonical, mutually exclusive token
// breakdown from the raw callback counters of a native adapter record.
//
// The host parses these providers' output with OpenAI, OpenAI Responses or
// Anthropic Messages semantics but the plugin cannot see which one was used,
// so both candidate normalizations are tried:
//
//   - subset:      cache inside input, reasoning inside output (OpenAI/Responses)
//   - independent: input excludes cache, raw output still contains reasoning
//     (Anthropic Messages)
//
// A candidate is accepted only when it is Valid and complete, and the record is
// recovered only when the raw counters uniquely determine the result: a single
// valid candidate, or several identical ones. The raw callback total stays
// authoritative, so the SDK's unknown-provider lower bound is never used here.
func recoverAdapterBreakdown(provider, executor, model string, raw pluginapi.UsageDetail) (usage.TokenBreakdown, bool) {
	if !isAdapterCompatTarget(provider, executor, model) {
		return usage.TokenBreakdown{}, false
	}
	// Every raw counter must be non-negative; a negative value is malformed and
	// must never be repaired into fabricated billing.
	for _, v := range []int64{
		raw.InputTokens, raw.OutputTokens, raw.ReasoningTokens, raw.CachedTokens,
		raw.CacheReadTokens, raw.CacheCreationTokens, raw.TotalTokens,
	} {
		if v < 0 {
			return usage.TokenBreakdown{}, false
		}
	}
	// A record with no preserved raw usage has nothing to recover; repairing it
	// could only overwrite whatever the row holds, so leave it alone.
	if raw.InputTokens == 0 && raw.OutputTokens == 0 && raw.ReasoningTokens == 0 &&
		raw.CachedTokens == 0 && raw.CacheReadTokens == 0 && raw.CacheCreationTokens == 0 &&
		raw.TotalTokens == 0 {
		return usage.TokenBreakdown{}, false
	}
	// Reasoning is a subset of the raw output for both registered families.
	if raw.ReasoningTokens > raw.OutputTokens {
		return usage.TokenBreakdown{}, false
	}
	// Non-zero usage must carry a positive authoritative raw total.
	nonZero := raw.InputTokens > 0 || raw.OutputTokens > 0 || raw.ReasoningTokens > 0 ||
		raw.CacheReadTokens > 0 || raw.CacheCreationTokens > 0 || raw.CachedTokens > 0
	if nonZero && raw.TotalTokens <= 0 {
		return usage.TokenBreakdown{}, false
	}
	// CachedTokens aliases an explicit cache component and is never additive:
	// it must equal CacheReadTokens, or CacheCreationTokens when the read is
	// zero. A record carrying only the alias is rejected.
	if raw.CachedTokens > 0 &&
		!(raw.CachedTokens == raw.CacheReadTokens ||
			(raw.CacheReadTokens == 0 && raw.CachedTokens == raw.CacheCreationTokens)) {
		return usage.TokenBreakdown{}, false
	}

	candidates := make([]usage.TokenBreakdown, 0, 2)
	subset := usage.NewSubsetTokenBreakdown(
		raw.InputTokens, raw.CacheReadTokens, raw.CacheCreationTokens,
		raw.OutputTokens, raw.ReasoningTokens, raw.TotalTokens)
	if subset.Valid() && subset.Quality == usage.TokenAccountingQualityComplete {
		candidates = append(candidates, subset)
	}
	independent := usage.NewIndependentTokenBreakdown(
		raw.InputTokens, raw.CacheReadTokens, raw.CacheCreationTokens,
		raw.OutputTokens-raw.ReasoningTokens, raw.ReasoningTokens, raw.TotalTokens)
	if independent.Valid() && independent.Quality == usage.TokenAccountingQualityComplete {
		candidates = append(candidates, independent)
	}
	if len(candidates) == 0 {
		return usage.TokenBreakdown{}, false
	}
	chosen := candidates[0]
	for _, c := range candidates[1:] {
		if c != chosen {
			// Two complete interpretations that disagree cannot be resolved
			// without guessing; keep the record unclassified.
			return usage.TokenBreakdown{}, false
		}
	}
	return chosen, true
}
