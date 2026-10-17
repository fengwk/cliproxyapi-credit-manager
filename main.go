// Command cliproxyapi-credit-manager builds the CLIProxyAPI native plugin shared
// library exposing the credit manager. This file is CGO glue only: every RPC
// method is forwarded into internal/plugin, and no inference is executed here.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);
*/
import "C"

import (
	"encoding/json"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/plugin"
)

func main() {}

// maxBufferLen bounds buffer lengths before size_t->C.int conversion. Values
// >= 2^31 truncate negative and C.GoBytes would panic outside any recover.
const maxBufferLen = 1<<31 - 1

// requestLenTooLarge reports whether a length must be refused before any byte
// is read. Callers can pass a small pointer with a huge length, so the length
// is checked on its own: the decision never dereferences the buffer.
func requestLenTooLarge(requestLen uint64) bool {
	return requestLen > maxBufferLen
}

// manager is process-wide and opens no files: the credit and capture stores are
// opened only when the host sends plugin.register / plugin.reconfigure.
var manager = plugin.NewManager()

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil {
		return 1
	}
	if uint32(host.abi_version) != pluginabi.ABIVersion {
		return 1
	}
	// The host API layout is validated above and then deliberately unused: this
	// plugin is passive and makes no outbound host calls, so it never executes a
	// model or issues a background probe. Only the plugin-side vtable is filled.
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) (rc C.int) {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	// methodName is read before dispatch so the panic recovery below can still
	// answer with the fail-closed response for a before-auth interception.
	methodName := ""
	defer func() {
		if recovered := recover(); recovered != nil {
			raw, code := shimRejection(methodName, "internal panic")
			writeResponse(response, raw)
			rc = C.int(code)
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	methodName = C.GoString(method)
	var req []byte
	if request != nil && requestLen > 0 {
		if requestLenTooLarge(uint64(requestLen)) {
			raw, code := shimRejection(methodName, "request exceeds maximum size")
			writeResponse(response, raw)
			return C.int(code)
		}
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := manager.HandleCall(methodName, req)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

// shimRejection is the ABI answer for a call this shim refuses before (or
// instead of) dispatching it, plus the C return code.
//
// A before-auth interception must fail closed: an error envelope or a non-zero
// code makes the host treat the hook as failed and continue the request
// unmetered, so that method is answered with the plugin's own Terminate
// response and a success code. Every other method keeps its error envelope,
// which the host accepts as a normal plugin response (`isPluginErrorEnvelope`).
func shimRejection(method, message string) ([]byte, int) {
	if method == pluginabi.MethodRequestInterceptBefore {
		return plugin.FailClosedInterceptEnvelope(), 0
	}
	return errorEnvelope("plugin_error", message), 1
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	defer func() {
		_ = recover()
	}()
	_, _ = manager.HandleCall(pluginabi.MethodPluginShutdown, nil)
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

func errorEnvelope(code, message string) []byte {
	type envelopeError struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	raw, err := json.Marshal(struct {
		OK    bool           `json:"ok"`
		Error *envelopeError `json:"error"`
	}{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if err != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"internal error"}}`)
	}
	return raw
}
