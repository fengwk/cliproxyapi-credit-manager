package main

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestRequestLenGuardRejectsBeforeReading pins the ABI-level size decision: a
// caller can pass a small pointer with a huge length without allocating it, so
// the length alone decides and the oversized branch never dereferences the
// buffer. 2^31-1 is the last length that still fits the C.int conversion.
func TestRequestLenGuardRejectsBeforeReading(t *testing.T) {
	for _, tc := range []struct {
		length uint64
		tooBig bool
	}{
		{length: 0, tooBig: false},
		{length: 1, tooBig: false},
		{length: maxBufferLen, tooBig: false},
		{length: maxBufferLen + 1, tooBig: true},
		{length: 1 << 32, tooBig: true},
		{length: 1<<64 - 1, tooBig: true},
	} {
		if got := requestLenTooLarge(tc.length); got != tc.tooBig {
			t.Fatalf("requestLenTooLarge(%d) = %v, want %v", tc.length, got, tc.tooBig)
		}
	}
}

// TestShimRejectionFailsClosedForBeforeInterception pins the ABI answer the shim
// sends when it refuses a call before dispatch (oversized request or a panic).
// A before-auth interception must stay fail-closed: anything the host reads as
// "the hook failed" makes it continue the request unmetered, so the answer is a
// successful envelope carrying the plugin's Terminate.
func TestShimRejectionFailsClosedForBeforeInterception(t *testing.T) {
	for _, message := range []string{"request exceeds maximum size", "internal panic"} {
		raw, code := shimRejection(pluginabi.MethodRequestInterceptBefore, message)
		if code != 0 {
			t.Fatalf("before-auth rejection for %q returned rc = %d, want 0", message, code)
		}
		var envelope pluginabi.Envelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatalf("before-auth rejection for %q is not an envelope: %s", message, raw)
		}
		if !envelope.OK {
			t.Fatalf("before-auth rejection for %q must not be an error envelope: %s", message, raw)
		}
		var response pluginapi.RequestInterceptResponse
		if err := json.Unmarshal(envelope.Result, &response); err != nil {
			t.Fatalf("decode before-auth rejection result: %v", err)
		}
		if !response.Terminate {
			t.Fatalf("before-auth rejection for %q must terminate the request: %s", message, raw)
		}
		if response.StatusCode != 503 {
			t.Fatalf("before-auth rejection for %q status = %d, want 503", message, response.StatusCode)
		}
		var body map[string]any
		if err := json.Unmarshal(response.ResponseBody, &body); err != nil {
			t.Fatalf("before-auth rejection body is not JSON: %s", response.ResponseBody)
		}
		if _, ok := body["error"]; !ok {
			t.Fatalf("before-auth rejection body carries no error: %s", response.ResponseBody)
		}
	}
}

// TestShimRejectionKeepsOrdinaryErrors pins that every other method keeps the
// error envelope the host accepts as a normal plugin response, so unrelated
// RPCs are not turned into a terminating HTTP response.
func TestShimRejectionKeepsOrdinaryErrors(t *testing.T) {
	raw, code := shimRejection(pluginabi.MethodUsageHandle, "internal panic")
	if code != 1 {
		t.Fatalf("ordinary rejection returned rc = %d, want 1", code)
	}
	var envelope pluginabi.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("ordinary rejection is not an envelope: %s", raw)
	}
	// This is exactly the host's isPluginErrorEnvelope predicate: rc != 0 plus
	// an error envelope is still delivered to the caller as a plugin answer.
	if envelope.OK || envelope.Error == nil {
		t.Fatalf("ordinary rejection must be an error envelope: %s", raw)
	}
	if envelope.Error.Message != "internal panic" {
		t.Fatalf("ordinary rejection message = %q", envelope.Error.Message)
	}
}
