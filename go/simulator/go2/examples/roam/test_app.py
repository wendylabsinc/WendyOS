"""Ownership and lifecycle checks for the standalone sample runner."""

import copy
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
from pathlib import Path
import sys
import threading
import time
from urllib.error import HTTPError
from urllib.request import Request, urlopen

import pytest

sys.path.insert(0, str(Path(__file__).parent))
from app import Handler, RoamApp, SimulatorClient


class FixtureSimulator:
    """A finite, always-clear room fixture; it is never a simulation benchmark."""

    url = "http://127.0.0.1:8899"
    timeout = 0.1

    def __init__(self):
        self.lock = threading.RLock()
        self.started = time.monotonic()
        self.owner = None
        self.epoch = 1
        self.source_digest = "fixture-source"
        self.calls = []
        self.status_overrides = {}
        self.lidar_overrides = {}
        self.arm_epoch = None
        self.freeze = False
        self.frozen_time = None
        self.fail_commands = False

    def _time(self):
        if self.freeze:
            if self.frozen_time is None:
                self.frozen_time = time.monotonic() - self.started + 1
            return self.frozen_time
        return time.monotonic() - self.started + 1

    def get(self, path):
        with self.lock:
            self.calls.append((path, None, time.monotonic()))
            if path in {"/api/status", "/api/health"}:
                return copy.deepcopy({
                    "simulation": True, "robot": "go2", "robot_kind": "go2", "profile_version": 1,
                    "ready": True, "healthy": True, "mode": "standing", "error": None,
                    "armed": self.owner is not None, "epoch": self.epoch, "time": self._time(),
                    "source_digest": self.source_digest, "position": [0, 0, 0.35],
                    "quaternion_wxyz": [1, 0, 0, 0], "control_mode": "sport",
                    "sensor_settings": {"lidar_enabled": True, "camera_enabled": True, "lidar_dropout": 0},
                    "metrics": {"wall_seconds": time.monotonic() - self.started + 100, "physics_age_ms": 0},
                    **self.status_overrides,
                })
            if path == "/api/scene/lidar":
                points = [coordinate for index in range(360)
                          for coordinate in (4 * math.cos(index * math.pi / 180),
                                             4 * math.sin(index * math.pi / 180), 0.35)]
                return copy.deepcopy({
                    "scene_id": "fixture-scene", "epoch": self.epoch, "generation": 0,
                    "time": self._time(), "mode": "standing", "enabled": True,
                    "available": True, "fresh": True, "age_ms": 0,
                    "origin": [0, 0, 0.35], "quaternion": [0, 0, 0, 1], "points": points,
                    **self.lidar_overrides,
                })
            raise AssertionError(f"Unexpected fixture GET {path}")

    def post(self, path, body=None):
        body = body or {}
        with self.lock:
            self.calls.append((path, copy.deepcopy(body), time.monotonic()))
            if path == "/api/arm":
                if self.owner is not None:
                    raise RuntimeError("another command source already owns the robot")
                self.owner = "private-fixture-grant"
                return {"token": self.owner, "epoch": self.arm_epoch or self.epoch}
            if body.get("token") != self.owner or self.owner is None:
                raise RuntimeError("command ownership expired")
            if path == "/api/command":
                if self.fail_commands:
                    raise RuntimeError("simulated command transport failure")
                return {"accepted": True}
            if path == "/api/stop":
                return {"stopped": True}
            if path == "/api/release":
                self.owner = None
                return {"released": True}
            raise AssertionError(f"Unexpected fixture POST {path}")

    def posted(self, path=None):
        with self.lock:
            return [(name, body, at) for name, body, at in self.calls
                    if body is not None and (path is None or name == path)]


def until(predicate, timeout=2):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("runner did not reach the expected state")


@pytest.fixture
def runner():
    client = FixtureSimulator()
    app = RoamApp(client, period=0.02, sensor_period=0.03)
    try:
        yield app, client
    finally:
        app.close()


@pytest.mark.parametrize("changes", [
    {"simulation": False}, {"robot": "g1", "robot_kind": "g1"},
    {"ready": False}, {"mode": "paused"}, {"healthy": False, "error": "physics fault"},
])
def test_start_requires_ready_identified_simulation_before_acquiring_control(runner, changes):
    app, client = runner
    client.status_overrides.update(changes)
    with pytest.raises((RuntimeError, ValueError)):
        app.start()
    assert client.posted() == []


def test_existing_browser_owner_is_never_replaced(runner):
    app, client = runner
    client.owner = "browser-owner"
    with pytest.raises((RuntimeError, ValueError), match="(?i)control|owner|release"):
        app.start()
    assert client.posted() == []
    assert client.owner == "browser-owner"


def test_explicit_start_arms_once_and_stop_releases_only_its_grant(runner):
    app, client = runner
    app.start()
    until(lambda: len(client.posted("/api/command")) >= 4)
    app.start()  # Double clicks must not acquire a second grant.
    assert len(client.posted("/api/arm")) == 1
    assert "private-fixture-grant" not in json.dumps(app.status())
    commands = client.posted("/api/command")
    assert all(len(body["velocity"]) == 3 and all(math.isfinite(v) for v in body["velocity"])
               for _, body, _ in commands)
    assert any(body["velocity"][0] > 0 for _, body, _ in commands)
    app.stop()
    assert client.owner is None
    assert [path for path, _, _ in client.posted()][-2:] == ["/api/command", "/api/release"]
    assert client.posted("/api/command")[-1][1]["velocity"] == [0, 0, 0]
    count = len(client.posted())
    time.sleep(0.12)
    assert len(client.posted()) == count, "Stopped apps must not resume sending commands"


@pytest.mark.parametrize("change", ["epoch", "identity", "ownership", "transport", "frozen"])
def test_interrupted_run_stops_and_never_rearms_automatically(runner, change):
    app, client = runner
    app.start()
    until(lambda: len(client.posted("/api/command")) >= 3)
    with client.lock:
        if change == "epoch":
            client.epoch += 1
        elif change == "identity":
            client.source_digest = "replacement-runtime"
        elif change == "ownership":
            client.owner = "replacement-owner"
        elif change == "transport":
            client.fail_commands = True
        else:
            client.freeze = True
    until(lambda: not app.status()["active"])
    time.sleep(0.15)
    assert len(client.posted("/api/arm")) == 1
    if change == "ownership":
        assert client.owner == "replacement-owner"
    else:
        assert client.owner is None


def test_epoch_change_during_arm_releases_new_grant_without_moving(runner):
    app, client = runner
    client.arm_epoch = 2
    with pytest.raises((RuntimeError, ValueError)):
        app.start()
    assert client.owner is None
    assert all(body["velocity"] == [0, 0, 0] for _, body, _ in client.posted("/api/command"))


@pytest.mark.parametrize("lidar", [
    {"fresh": False}, {"enabled": False}, {"available": False},
    {"age_ms": 1000}, {"points": [float("nan"), 0, 0]}, {"points": []},
])
def test_unusable_lidar_does_not_acquire_control(runner, lidar):
    app, client = runner
    client.lidar_overrides.update(lidar)
    with pytest.raises((RuntimeError, ValueError)):
        app.start()
    assert client.posted() == []


@pytest.mark.parametrize("url", [
    "http://192.168.123.161:8890", "file:///tmp/simulator", "http://user:pass@localhost:8890",
    "http://localhost:8890/path", "http://localhost:8890/?query=yes", "http://localhost:8890/#fragment",
])
def test_client_rejects_nonlocal_or_ambiguous_targets(url):
    with pytest.raises((RuntimeError, ValueError)):
        SimulatorClient(url)


@contextmanager
def local_server(handler, **attributes):
    server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
    for name, value in attributes.items():
        setattr(server, name, value)
    thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_real_http_client_handles_json_and_refuses_redirects_and_oversized_responses(monkeypatch):
    paths = []

    class FixtureHandler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            paths.append(self.path)
            self.send_response(302 if self.path == "/redirect" else 200)
            if self.path == "/redirect":
                self.send_header("Location", "/ok")
            self.end_headers()
            body = ([1, 2] if self.path == "/array" else
                    {"padding": "x" * 1_048_576} if self.path == "/large" else {"value": 42})
            try:
                self.wfile.write(json.dumps(body).encode())
            except (BrokenPipeError, ConnectionResetError):
                # The client disconnected; there is no response left to send.
                pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            self.send_response(200)
            self.end_headers()
            self.wfile.write(json.dumps({"echo": body}).encode())

    with local_server(FixtureHandler) as url:
        monkeypatch.setenv("HTTP_PROXY", "http://127.0.0.1:1")
        monkeypatch.setenv("NO_PROXY", "")
        client = SimulatorClient(url)
        assert client.get("/ok") == {"value": 42}
        assert client.post("/echo", {"velocity": [0.1, 0, 0]}) == {"echo": {"velocity": [0.1, 0, 0]}}
        for path in ("/redirect", "/array", "/large"):
            with pytest.raises(RuntimeError):
                client.get(path)
        assert paths.count("/ok") == 1, "A redirect must never retarget simulation requests"


def test_web_start_stop_uses_the_runner_and_rejects_cross_origin_control(runner):
    app, client = runner
    with local_server(Handler, app=app) as url:
        request = Request(url + "/api/start", data=b"{}", headers={"Origin": "https://unrelated.example"})
        with pytest.raises(HTTPError) as error:
            urlopen(request, timeout=1)
        assert error.value.code == 403
        assert client.posted() == []
        own_origin = {"Origin": url, "Content-Type": "application/json"}
        with urlopen(Request(url + "/api/start", data=b"{}", headers=own_origin), timeout=1) as response:
            assert json.load(response)["active"] is True
        until(lambda: len(client.posted("/api/command")) >= 2)
        with urlopen(url + "/api/status", timeout=1) as response:
            state = json.load(response)
            assert state["active"] is True
            assert "private-fixture-grant" not in json.dumps(state)
        with urlopen(Request(url + "/api/stop", data=b"{}", headers=own_origin), timeout=1) as response:
            assert json.load(response)["active"] is False
        assert client.owner is None
