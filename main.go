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

// The host hands the plugin a process-lifetime host API during init. It stays
// valid until the host unloads this library, which is also the bound on every
// outbound host call this plugin makes.
static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"fmt"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"

	"github.com/fengwk/cliproxyapi-credit-manager/internal/plugin"
)

func main() {}

// maxBufferLen bounds buffer lengths before size_t->C.int conversion. Values
// >= 2^31 truncate negative and C.GoBytes would panic outside any recover.
const maxBufferLen = 1<<31 - 1

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
	// The host API is captured so the plugin can issue its own host callbacks
	// (the optional background warmup). The host identifies the calling plugin
	// from this context automatically, so a background call can never borrow a
	// customer request's identity or callback id.
	C.store_host_api(host)
	manager.SetHostCall(callHost)
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
	defer func() {
		if recovered := recover(); recovered != nil {
			writeResponse(response, errorEnvelope("plugin_error", "internal panic"))
			rc = 1
		}
	}()
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var req []byte
	if request != nil && requestLen > 0 {
		if uint64(requestLen) > maxBufferLen {
			writeResponse(response, errorEnvelope("plugin_error", "request exceeds maximum size"))
			return 0
		}
		req = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := manager.HandleCall(C.GoString(method), req)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
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

// callHost bridges the plugin's host-callback interface into the stored C host
// API. Error text carries only the method name and return code: no request or
// response material is ever echoed into an error.
func callHost(method string, payload []byte) ([]byte, error) {
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var request *C.uint8_t
	if len(payload) > 0 {
		request = (*C.uint8_t)(C.CBytes(payload))
		defer C.free(unsafe.Pointer(request))
	}
	var response C.cliproxy_buffer
	if rc := C.call_host_api(cMethod, request, C.size_t(len(payload)), &response); rc != 0 {
		return nil, fmt.Errorf("host callback %s failed (rc=%d)", method, int(rc))
	}
	if response.ptr == nil || response.len == 0 {
		return nil, fmt.Errorf("host callback %s returned an empty response", method)
	}
	if response.len > maxBufferLen {
		C.free_host_buffer(response.ptr, response.len)
		return nil, fmt.Errorf("host callback %s returned an oversized response", method)
	}
	out := C.GoBytes(unsafe.Pointer(response.ptr), C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)
	return out, nil
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
