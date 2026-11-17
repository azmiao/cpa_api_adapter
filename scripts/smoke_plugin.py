#!/usr/bin/env python3
"""Exercise a compiled plugin through the CPA native ABI."""

import base64
import ctypes
import json
import pathlib
import sys
import tempfile


class Buffer(ctypes.Structure):
    _fields_ = [("ptr", ctypes.c_void_p), ("length", ctypes.c_size_t)]


class HostAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("host_ctx", ctypes.c_void_p),
        ("call", ctypes.c_void_p),
        ("free_buffer", ctypes.c_void_p),
    ]


class PluginAPI(ctypes.Structure):
    _fields_ = [
        ("abi_version", ctypes.c_uint32),
        ("call", ctypes.c_void_p),
        ("free_buffer", ctypes.c_void_p),
        ("shutdown", ctypes.c_void_p),
    ]


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def main():
    if len(sys.argv) != 3:
        raise SystemExit("Usage: python scripts/smoke_plugin.py LIBRARY VERSION")
    library = ctypes.CDLL(str(pathlib.Path(sys.argv[1]).resolve()))
    init = library.cliproxy_plugin_init
    init.argtypes = [ctypes.POINTER(HostAPI), ctypes.POINTER(PluginAPI)]
    init.restype = ctypes.c_int
    host, plugin = HostAPI(1, None, None, None), PluginAPI()
    require(init(ctypes.byref(host), ctypes.byref(plugin)) == 0, "plugin init failed")
    require(plugin.abi_version == 1 and plugin.call and plugin.free_buffer and plugin.shutdown, "invalid ABI table")

    call = ctypes.CFUNCTYPE(
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.POINTER(ctypes.c_uint8),
        ctypes.c_size_t,
        ctypes.POINTER(Buffer),
    )(plugin.call)
    free = ctypes.CFUNCTYPE(None, ctypes.c_void_p, ctypes.c_size_t)(plugin.free_buffer)
    shutdown = ctypes.CFUNCTYPE(None)(plugin.shutdown)

    def invoke(method, request):
        raw = json.dumps(request, ensure_ascii=False).encode("utf-8")
        request_buffer = (ctypes.c_uint8 * len(raw)).from_buffer_copy(raw)
        response = Buffer()
        code = call(method.encode("utf-8"), request_buffer, len(raw), ctypes.byref(response))
        try:
            require(bool(response.ptr), f"{method}: missing response")
            result = json.loads(ctypes.string_at(response.ptr, response.length))
        finally:
            if response.ptr:
                free(response.ptr, response.length)
        require(code == 0 and result.get("ok"), f"{method}: {result}")
        return result["result"]

    with tempfile.TemporaryDirectory(prefix="omp-smoke-") as directory:
        config = {
            "socket_path": str(pathlib.Path(directory) / "adapter.sock"),
            "socket_mode": "0600",
            "upstream": "http://127.0.0.1:1",
            "prefix": "/omp",
        }
        encoded = base64.b64encode(json.dumps(config).encode()).decode()
        try:
            registration = invoke("plugin.register", {"config_yaml": encoded})
            metadata = registration["metadata"]
            require(metadata.get("Version") == sys.argv[2], f"wrong plugin version: {metadata}")
            require(bool(metadata.get("GitHubRepository")), "missing repository metadata")
            require(registration["capabilities"].get("management_api") is True, "missing management capability")

            status_response = invoke("management.handle", {
                "Method": "GET",
                "Path": "/v0/management/plugins/cpa-omp-adapter/status",
            })
            status = json.loads(base64.b64decode(status_response["Body"]))
            require(status_response["StatusCode"] == 200 and status.get("running"), f"listener failed: {status}")

            page_response = invoke("management.handle", {
                "Method": "GET",
                "Path": "/v0/resource/plugins/cpa-omp-adapter/index.html",
            })
            page = base64.b64decode(page_response["Body"]).decode("utf-8")
            require(page_response["StatusCode"] == 200 and "OMP 适配器" in page, "management page unavailable")
        finally:
            invoke("plugin.shutdown", {})
            shutdown()
    print(f"CPA ABI smoke check passed: {sys.argv[1]} ({sys.argv[2]})")


if __name__ == "__main__":
    main()
