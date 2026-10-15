package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// PluginError is a dispatch failure carrying an envelope code, a sanitized
// message and an HTTP status for the host. Messages must never contain
// database paths, request payloads or credentials.
type PluginError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *PluginError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type envelopeResult struct {
	OK    bool           `json:"ok"`
	Error *envelopeError `json:"error,omitempty"`
}

func errorResult(code, message string, status int) envelopeResult {
	return envelopeResult{OK: false, Error: &envelopeError{Code: code, Message: message, HTTPStatus: status}}
}

// errorEnvelope maps a handler error to a host envelope. Only plugin-owned
// codes, generic messages and statuses are surfaced; arbitrary error text is
// discarded so no database path or payload can leak through the host.
func errorEnvelope(err error) ([]byte, error) {
	if pluginErr, ok := err.(*PluginError); ok {
		return mustEnvelope(errorResult(pluginErr.Code, pluginErr.Message, pluginErr.HTTPStatus))
	}
	return mustEnvelope(errorResult("plugin_error", "internal error", http.StatusInternalServerError))
}

func mustEnvelope(result envelopeResult) ([]byte, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"internal error"}}`), nil
	}
	return raw, nil
}

// okEnvelope serializes a successful RPC envelope around a result value.
func okEnvelope(value any) ([]byte, error) {
	var result json.RawMessage
	if value != nil {
		encoded, errMarshal := json.Marshal(value)
		if errMarshal != nil {
			return nil, fmt.Errorf("encode response")
		}
		result = encoded
	}
	if len(result) == 0 {
		result = json.RawMessage("{}")
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}
