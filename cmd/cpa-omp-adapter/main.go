package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct { void* ptr; size_t len; } cliproxy_buffer;
typedef struct { uint32_t abi_version; void* host_ctx; void* call; void* free_buffer; } cliproxy_host_api;
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

	"cpa-omp-adapter/internal/adapter"
)

var plugin = &adapter.RPC{Manager: adapter.NewManager()}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, api *C.cliproxy_plugin_api) C.int {
	if host == nil || api == nil || host.abi_version != 1 {
		return 1
	}
	api.abi_version = 1
	api.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	api.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	api.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, size C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			b, _ := json.Marshal(adapter.Envelope{OK: false, Error: &adapter.RPCError{Code: "plugin_error", Message: "插件内部错误"}})
			putResponse(response, b)
			code = 1
		}
	}()
	if method == nil || size > C.size_t(64<<20) {
		b, _ := json.Marshal(adapter.Envelope{OK: false, Error: &adapter.RPCError{Code: "invalid_request", Message: "请求无效或过大"}})
		putResponse(response, b)
		return 1
	}
	var body []byte
	if request != nil && size > 0 {
		body = C.GoBytes(unsafe.Pointer(request), C.int(size))
	}
	out, err := plugin.Handle(C.GoString(method), body)
	if err != nil {
		out, _ = json.Marshal(adapter.Envelope{OK: false, Error: &adapter.RPCError{Code: "plugin_error", Message: err.Error()}})
		putResponse(response, out)
		return 1
	}
	putResponse(response, out)
	return 0
}

func putResponse(response *C.cliproxy_buffer, body []byte) {
	if len(body) == 0 {
		return
	}
	response.ptr = C.CBytes(body)
	response.len = C.size_t(len(body))
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() { plugin.Manager.Stop() }
