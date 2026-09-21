"""Isolated acceptance helpers; importing this module does not initialize DDS."""

import json
import math
from pathlib import Path
import re
import threading
import time
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener


SDK_COMMIT = "65691c8a8bc53b98d3976dba4dbf9d5d20b2e7f5"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def require_isolated_network():
    interfaces = sorted(path.name for path in Path("/sys/class/net").iterdir())
    require(interfaces == ["lo"], f"native acceptance requires an isolated loopback-only Docker namespace, found {interfaces}")


def require_test_environment(api):
    if api.vm_name is None:
        require_isolated_network()
    return api.status()  # No DDS participant is created by this identity check.


def wait_until(predicate, message, timeout=10.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.025)
    raise AssertionError(message)


def yaw(state):
    w, x, y, z = state["quaternion_wxyz"]
    return math.atan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z))


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *_):
        raise ValueError("simulator redirects are forbidden")


class SimulatorAPI:
    def __init__(self, url, *, vm_name=None, source_digest=None):
        parsed = urlsplit(url)
        require(parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "localhost", "::1"},
                "only loopback HTTP simulator URLs are accepted")
        require(not parsed.username and not parsed.password and parsed.path in {"", "/"}
                and not parsed.query and not parsed.fragment, "invalid simulator URL")
        self.base = url.rstrip("/")
        require(vm_name is None or bool(re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]*", vm_name)),
                "managed VM name must be explicit and nonempty")
        require(source_digest is None or (vm_name is not None and
                bool(re.fullmatch(r"sha256:[0-9a-f]{64}", source_digest))),
                "source digest must be a SHA-256 pin for an explicit managed VM")
        self.vm_name, self.source_digest = vm_name, source_digest
        self.opener = build_opener(ProxyHandler({}), NoRedirect())

    def _request(self, path, body=None):
        request = Request(self.base + path, data=None if body is None else json.dumps(body).encode(),
                          headers={} if body is None else {"Content-Type": "application/json"})
        try:
            with self.opener.open(request, timeout=3.0) as response:
                return response.status, json.load(response)
        except HTTPError as error:
            return error.code, json.load(error)

    def status(self):
        code, value = self._request("/api/status")
        require(code == 200 and isinstance(value, dict), f"simulator status failed: {code}, {value}")
        require(value.get("simulation") is True and value.get("robot") == "g1"
                and type(value.get("profile_version")) is int and value["profile_version"] == 1,
                "endpoint did not identify itself as the Wendy G1 simulation")
        if self.vm_name is not None:
            require(value.get("vm_name") == self.vm_name and value.get("robot_kind") == "g1"
                    and value.get("dds_isolation") == "udp-rtps-loopback"
                    and value.get("clock_mode") == "device"
                    and value.get("policy_bundle") == "g1-29dof-velocity-v0-4960b847-v1",
                    "endpoint does not match the named managed G1 VM and its DDS isolation profile")
            digest = value.get("source_digest")
            require(isinstance(digest, str) and bool(re.fullmatch(r"sha256:[0-9a-f]{64}", digest)),
                    "managed virtual robot did not declare a pinned source digest")
            require(self.source_digest is None or self.source_digest == digest,
                    "managed virtual robot source changed or differs from the requested pin")
            self.source_digest = digest
        return value

    def post(self, path, body=None, expected=200):
        require(path in {"/api/reset", "/api/arm_ros", "/api/disarm_ros"}, "HTTP is limited to virtual lifecycle and explicit grants")
        self.status()
        code, value = self._request(path, {} if body is None else body)
        require(code == expected, f"{path}: expected HTTP{expected}, got {code}: {value}")
        return value


def sources(api):
    return {item["publisher_gid"]: item for item in api.status()["ros_commands"]["sources"]}


def grant_new_source(api, before, kind):
    def candidate():
        found = [gid for gid, item in sources(api).items() if gid not in before and item["kind"] == kind
                 and item["age_ms"] < 300 and not item["requires_restart"]]
        require(len(found) <= 1, "multiple new publisher identities appeared; refusing an ambiguous grant")
        return found[0] if found else None
    gid = wait_until(candidate, f"new {kind} SDK publisher was not discovered")
    api.post("/api/arm_ros", {"publisher_gid": gid})
    wait_until(lambda: api.status()["ros_commands"]["owner"] == gid, "publisher grant did not become active")
    return gid


class Stream:
    def __init__(self, send, period):
        self.send, self.period = send, period
        self.stopped = threading.Event()
        self.lock = threading.Lock()
        self.value = None
        self.error = None
        self.count = 0
        self.max_gap = 0.0
        self.thread = threading.Thread(target=self._run, daemon=True)

    def start(self, value):
        self.value = value
        self.thread.start()
        return self

    def set(self, value):
        with self.lock:
            self.value = value

    def _run(self):
        last = None
        while not self.stopped.is_set():
            started = time.monotonic()
            try:
                with self.lock:
                    value = self.value
                self.send(value)
                self.count += 1
                if last is not None:
                    self.max_gap = max(self.max_gap, started - last)
                last = started
            except Exception as error:
                self.error = str(error)
                self.stopped.set()
                return
            self.stopped.wait(max(0, self.period - (time.monotonic() - started)))

    def stop(self):
        self.stopped.set()
        self.thread.join(timeout=3)
        require(not self.thread.is_alive(), "command stream failed to stop")


def sdk_crc():
    """Use the SDK's own portable CRC implementation, with the omission exposed.

    The pinned archive omits the shared libraries that its Linux constructor
    requires. Select its existing Python branch only during construction; no
    source, packing rules, or CRC algorithm is replaced.
    """
    from unittest.mock import patch
    from unitree_sdk2py.utils.crc import CRC
    with patch("unitree_sdk2py.utils.crc.platform.system", return_value="PythonFallback"):
        instance = CRC()
    return instance
