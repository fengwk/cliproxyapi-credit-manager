package capture

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// redactedValue replaces sensitive header values and obvious JSON secret fields.
const redactedValue = "[redacted]"

// errRedactionFailed is returned when a body that was detected as JSON cannot be
// re-encoded after redaction. It is defensive: redaction of a value decoded with
// json.Number cannot fail in practice.
var errRedactionFailed = errors.New("json redaction failed")

// sensitiveHeaders holds the header names whose values are always redacted.
// Matching is case-insensitive on the trimmed name.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"api-key":             true,
	"x-goog-api-key":      true,
	"cookie":              true,
	"set-cookie":          true,
	"x-auth-token":        true,
	"x-access-token":      true,
}

// sensitiveJSONKeys holds the normalized JSON object keys whose values are
// always redacted. Keys are normalized with normalizeJSONKey, so snake_case,
// kebab-case and camelCase spellings of the same secret share one entry.
var sensitiveJSONKeys = map[string]bool{
	"key":           true,
	"apikey":        true,
	"token":         true,
	"accesstoken":   true,
	"refreshtoken":  true,
	"authtoken":     true,
	"idtoken":       true,
	"password":      true,
	"authorization": true,
	"cookie":        true,
	"clientsecret":  true,
	"privatekey":    true,
	"secretkey":     true,
}

// isSensitiveHeader reports whether a header value must be redacted.
func isSensitiveHeader(name string) bool {
	return sensitiveHeaders[strings.ToLower(strings.TrimSpace(name))]
}

// isSensitiveJSONKey reports whether an object value must be redacted.
func isSensitiveJSONKey(key string) bool {
	return sensitiveJSONKeys[normalizeJSONKey(key)]
}

// normalizeJSONKey lowercases a key and drops separators so that "clientSecret",
// "client_secret" and "client-secret" are recognized as the same secret field.
func normalizeJSONKey(key string) string {
	simple := true
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '-' || c == '_' || c == ' ' || (c >= 'A' && c <= 'Z') {
			simple = false
			break
		}
	}
	if simple {
		return key
	}
	var builder strings.Builder
	builder.Grow(len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		switch {
		case c == '-' || c == '_' || c == ' ':
			continue
		case c >= 'A' && c <= 'Z':
			builder.WriteByte(c + ('a' - 'A'))
		default:
			builder.WriteByte(c)
		}
	}
	return builder.String()
}

// redactHeaders returns a deep copy of h with sensitive values replaced. The
// caller's header map is never modified.
func redactHeaders(h http.Header) http.Header {
	if len(h) == 0 {
		return nil
	}
	out := make(http.Header, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		if isSensitiveHeader(k) {
			for i := range cp {
				cp[i] = redactedValue
			}
		} else {
			copy(cp, vs)
		}
		out[k] = cp
	}
	return out
}

// rawHeaderSize estimates how much memory a cloned header set would occupy,
// including a fixed bookkeeping cost per header name and per value.
//
// It returns as soon as the estimate exceeds limit, so a header map with
// millions of entries is never walked in full and never cloned. Callers treat
// any result above limit as "too large to keep".
func rawHeaderSize(h http.Header, limit int) int {
	size := 0
	for k, vs := range h {
		size += minHeaderStructureCost + len(k)
		for _, v := range vs {
			size += minHeaderStructureCost + len(v)
		}
		if size > limit {
			return size
		}
	}
	return size
}

// looksLikeJSON reports whether b starts with a JSON object or array.
func looksLikeJSON(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

// redactJSONBody redacts obvious secret fields of a JSON body recursively.
//
// The returned body is the original byte slice (and changed is false) whenever
// the body is not a single JSON document, contains no sensitive field, or
// cannot be re-encoded. Bodies are therefore never reformatted unless a secret
// was actually removed. Numbers are decoded with json.Number so that 64-bit
// integer text survives redaction unchanged.
func redactJSONBody(raw []byte) (out []byte, changed bool, err error) {
	if !looksLikeJSON(raw) {
		return raw, false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return raw, false, nil
	}
	// Trailing content means this is not a single JSON document; keep it as is.
	if _, err := dec.Token(); err != io.EOF {
		return raw, false, nil
	}
	redacted, changed := redactJSONValue(value)
	if !changed {
		return raw, false, nil
	}
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return nil, false, errRedactionFailed
	}
	return encoded, true, nil
}

// redactJSONValue walks a decoded JSON value and replaces the values of
// sensitive object keys.
func redactJSONValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for k, v := range typed {
			if isSensitiveJSONKey(k) {
				typed[k] = redactedValue
				changed = true
				continue
			}
			replaced, c := redactJSONValue(v)
			if c {
				typed[k] = replaced
				changed = true
			}
		}
		return typed, changed
	case []any:
		changed := false
		for i, v := range typed {
			replaced, c := redactJSONValue(v)
			if c {
				typed[i] = replaced
				changed = true
			}
		}
		return typed, changed
	default:
		return value, false
	}
}
