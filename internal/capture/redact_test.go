package capture

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
)

// TestRedactHeadersKeepsStructureAndRedactsSecrets verifies that sensitive header
// values are replaced case-insensitively while unrelated headers survive, so a
// captured snapshot still shows that credentials were present.
func TestRedactHeadersKeepsStructureAndRedactsSecrets(t *testing.T) {
	in := http.Header{
		"Authorization":  {"Bearer sk-secret"},
		"x-api-key":      {"key-one", "key-two"},
		"X-Goog-Api-Key": {"google-secret"},
		"Cookie":         {"session=abc"},
		"Set-Cookie":     {"session=abc; HttpOnly"},
		"Content-Type":   {"application/json"},
		"X-Request-Id":   {"req-1"},
	}
	out := redactHeaders(in)

	for name, values := range map[string][]string{
		"Authorization":  {redactedValue},
		"x-api-key":      {redactedValue, redactedValue},
		"X-Goog-Api-Key": {redactedValue},
		"Cookie":         {redactedValue},
		"Set-Cookie":     {redactedValue},
	} {
		got := out[name]
		if len(got) != len(values) {
			t.Fatalf("header %q has %d values, want %d", name, len(got), len(values))
		}
		for i := range got {
			if got[i] != values[i] {
				t.Fatalf("header %q value %d = %q, want %q", name, i, got[i], values[i])
			}
		}
	}
	if got := out.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want unchanged", got)
	}
	if got := out.Get("X-Request-Id"); got != "req-1" {
		t.Fatalf("X-Request-Id = %q, want unchanged", got)
	}
	// The caller's header map must not be mutated.
	if got := in.Get("Authorization"); got != "Bearer sk-secret" {
		t.Fatalf("caller header mutated: %q", got)
	}
}

// TestRedactJSONBodyRecursiveSecrets verifies recursive redaction of obvious
// secret keys in nested objects and arrays.
func TestRedactJSONBodyRecursiveSecrets(t *testing.T) {
	raw := []byte(`{"model":"gpt","API_KEY":"k1","messages":[{"role":"user","content":"hi",` +
		`"tool":{"refresh_token":"r1","nested":{"password":"p1","ok":"keep"}}}],` +
		`"authorization":"a1","cookie":"c1","token":"t1","access_token":"at1","apikey":"k2","plain":"v"}`)
	out, changed, err := redactJSONBody(raw)
	if err != nil {
		t.Fatalf("redactJSONBody error: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("redacted body is not JSON: %v", err)
	}
	for _, key := range []string{"API_KEY", "authorization", "cookie", "token", "access_token", "apikey"} {
		if got := decoded[key]; got != redactedValue {
			t.Fatalf("key %q = %v, want %q", key, got, redactedValue)
		}
	}
	if got := decoded["model"]; got != "gpt" {
		t.Fatalf("model = %v, want gpt", got)
	}
	if got := decoded["plain"]; got != "v" {
		t.Fatalf("plain = %v, want v", got)
	}
	msg := decoded["messages"].([]any)[0].(map[string]any)
	tool := msg["tool"].(map[string]any)
	if tool["refresh_token"] != redactedValue || tool["nested"].(map[string]any)["password"] != redactedValue {
		t.Fatalf("nested secrets not redacted: %v", tool)
	}
	if tool["nested"].(map[string]any)["ok"] != "keep" {
		t.Fatalf("nested non-secret changed: %v", tool)
	}
}

// TestRedactJSONBodyPreservesUnchangedAndIntegers verifies that bodies without
// secrets keep their exact bytes (no reformatting, no lost 64-bit integers) and
// that redaction does not round 64-bit integer text.
func TestRedactJSONBodyPreservesUnchangedAndIntegers(t *testing.T) {
	unchanged := []byte(`{"model":"gpt","n":12345678901234567890,"f":1.25e-10,"list":[1,2,3]}`)
	out, changed, err := redactJSONBody(unchanged)
	if err != nil {
		t.Fatalf("redactJSONBody error: %v", err)
	}
	if changed || string(out) != string(unchanged) {
		t.Fatalf("body without secrets changed: %q -> %q", unchanged, out)
	}

	withSecret := []byte(`{"api_key":"secret","n":12345678901234567890,"f":1.5}`)
	out, changed, err = redactJSONBody(withSecret)
	if err != nil {
		t.Fatalf("redactJSONBody error: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	var decoded map[string]json.Number
	if err := json.Unmarshal(out, &decoded); err == nil {
		t.Fatalf("redacted secret remained a number: %v", decoded)
	}
	var generic map[string]any
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	if err := decoder.Decode(&generic); err != nil {
		t.Fatalf("redacted body is not JSON: %v", err)
	}
	for key, want := range map[string]string{"n": "12345678901234567890", "f": "1.5"} {
		number, ok := generic[key].(json.Number)
		if !ok {
			t.Fatalf("%s = %#v, want json.Number", key, generic[key])
		}
		if got := number.String(); got != want {
			t.Fatalf("%s literal = %q, want %q", key, got, want)
		}
	}
	if got := generic["api_key"]; got != redactedValue {
		t.Fatalf("api_key = %v, want %q", got, redactedValue)
	}
}

// TestRedactJSONBodyNonJSON verifies that non-JSON bodies are returned untouched
// so that plain text and binary diagnostics are preserved exactly.
func TestRedactJSONBodyNonJSON(t *testing.T) {
	for _, raw := range [][]byte{
		nil,
		[]byte(""),
		[]byte("token=secret"),
		[]byte("{\"api_key\":\"x\"} trailing"),
		[]byte(`"just a string"`),
		[]byte{0x00, 0xff, 0x10},
	} {
		out, changed, err := redactJSONBody(raw)
		if err != nil {
			t.Fatalf("redactJSONBody(%q) error: %v", raw, err)
		}
		if changed {
			t.Fatalf("redactJSONBody(%q) changed = true, want false", raw)
		}
		if string(out) != string(raw) {
			t.Fatalf("redactJSONBody(%q) = %q, want unchanged", raw, out)
		}
	}
}

// TestRedactJSONBodyExtendedSecretKeys verifies that obvious secret fields are
// recognized in snake_case, kebab-case and camelCase spellings, recursively
// through arrays and objects, without losing 64-bit integer text.
func TestRedactJSONBodyExtendedSecretKeys(t *testing.T) {
	raw := []byte(`{"client_secret":"c1","clientSecret":"c2","client-secret":"c3","CLIENT-SECRET":"c4",` +
		`"auth_token":"a1","authToken":"a2","AUTH-TOKEN":"a3",` +
		`"id_token":"i1","idToken":"i2",` +
		`"private_key":"p1","privateKey":"p2","PRIVATE-KEY":"p3",` +
		`"secret_key":"s1","secretKey":"s2","secret-key":"s3",` +
		`"tools":[{"nested":{"id-token":"deep"}},{"plain":"keep"}],` +
		`"mirror":["clientSecret","value"],` +
		`"n":12345678901234567890,"model":"gpt"}`)
	out, changed, err := redactJSONBody(raw)
	if err != nil {
		t.Fatalf("redactJSONBody error: %v", err)
	}
	if !changed {
		t.Fatal("changed = false, want true")
	}
	decoder := json.NewDecoder(bytes.NewReader(out))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("redacted body is not JSON: %v", err)
	}
	for _, key := range []string{
		"client_secret", "clientSecret", "client-secret", "CLIENT-SECRET",
		"auth_token", "authToken", "AUTH-TOKEN",
		"id_token", "idToken",
		"private_key", "privateKey", "PRIVATE-KEY",
		"secret_key", "secretKey", "secret-key",
	} {
		if got := decoded[key]; got != redactedValue {
			t.Fatalf("key %q = %#v, want %q", key, got, redactedValue)
		}
	}
	tools := decoded["tools"].([]any)
	nested := tools[0].(map[string]any)["nested"].(map[string]any)
	if nested["id-token"] != redactedValue {
		t.Fatalf("nested id-token = %#v, want redacted", nested["id-token"])
	}
	if tools[1].(map[string]any)["plain"] != "keep" {
		t.Fatalf("non-secret array entry changed: %#v", tools[1])
	}
	// A secret *value* is not a secret key: it is data, not a leaked credential.
	mirror := decoded["mirror"].([]any)
	if mirror[0] != "clientSecret" || mirror[1] != "value" {
		t.Fatalf("array values changed: %#v", mirror)
	}
	number, ok := decoded["n"].(json.Number)
	if !ok || number.String() != "12345678901234567890" {
		t.Fatalf("64-bit integer text lost: %#v", decoded["n"])
	}
	if decoded["model"] != "gpt" {
		t.Fatalf("model = %#v, want gpt", decoded["model"])
	}
}

// TestEncodeHeadersDropsOversizedSets verifies that the header limit is applied
// before serializing so that one huge header cannot consume the body budget.
func TestEncodeHeadersDropsOversizedSets(t *testing.T) {
	small := encodeHeaders(http.Header{"X-Request-Id": {"req-1"}})
	if len(small) == 0 {
		t.Fatal("small header set was dropped")
	}
	big := http.Header{}
	value := make([]byte, headerLimit)
	for i := range value {
		value[i] = 'a'
	}
	big.Set("X-Big", string(value))
	if got := encodeHeaders(big); got != nil {
		t.Fatalf("oversized header set encoded to %d bytes, want dropped", len(got))
	}
}

// TestRawHeaderSizeBoundsHighCardinalityHeaders verifies that header sets with
// huge numbers of tiny values are rejected without walking or cloning them,
// while ordinary header sets keep working.
func TestRawHeaderSizeBoundsHighCardinalityHeaders(t *testing.T) {
	// A single header with a million empty values has almost no content, so an
	// estimate that only counts content would clone a million strings.
	oneMillionValues := http.Header{"X-Wide": make([]string, 1_000_000)}
	manyKeys := make(http.Header, 100000)
	for i := 0; i < 100000; i++ {
		manyKeys["X-"+strconv.Itoa(i)] = []string{""}
	}
	for name, headers := range map[string]http.Header{
		"one key with a million values": oneMillionValues,
		"many keys with tiny values":    manyKeys,
	} {
		if got := rawHeaderSize(headers, rawHeaderLimit); got <= rawHeaderLimit {
			t.Fatalf("%s: rawHeaderSize = %d, want an early result above %d", name, got, rawHeaderLimit)
		}
		allocs := testing.AllocsPerRun(1, func() {
			if encoded := encodeHeaders(headers); encoded != nil {
				t.Fatalf("%s: high cardinality header set encoded to %d bytes, want dropped", name, len(encoded))
			}
		})
		if allocs > 64 {
			t.Fatalf("%s: rejecting the header set allocated %.0f objects; it must not be cloned", name, allocs)
		}
	}

	normal := http.Header{
		"Authorization": {"Bearer secret"},
		"X-Request-Id":  {"req-1"},
	}
	size := rawHeaderSize(normal, rawHeaderLimit)
	if size <= 0 || size > rawHeaderLimit {
		t.Fatalf("rawHeaderSize(normal) = %d, want a small positive size", size)
	}
	if encoded := encodeHeaders(normal); len(encoded) == 0 {
		t.Fatal("ordinary header set was dropped")
	}
	wide := http.Header{}
	for i := 0; i < 200; i++ {
		wide["X-"+strconv.Itoa(i)] = []string{"value", "value"}
	}
	if encoded := encodeHeaders(wide); len(encoded) == 0 {
		t.Fatal("wide but small header set was dropped")
	}
}
