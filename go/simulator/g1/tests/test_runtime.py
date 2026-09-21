"""HTTP control and runtime integration using an actual headless G1 world."""

import http.client
from http.server import ThreadingHTTPServer
import gzip
import json
import threading
import time

import mujoco
import pytest

from g1_sim.runtime import Runtime
from g1_sim.server import Handler


def wait_until(predicate, *, timeout=2.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("timed out waiting for the live runtime")


@pytest.fixture
def endpoint():
    runtime = Runtime(render=False)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.runtime = runtime
    serving = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01}, daemon=True)
    runtime.start()
    serving.start()

    def request(path, body=None, *, headers=None, raw=False):
        connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=2.0)
        try:
            connection.request(
                "GET" if body is None else "POST", path,
                body=None if body is None else json.dumps(body),
                headers={**({} if body is None else {"Content-Type": "application/json"}), **(headers or {})},
            )
            response = connection.getresponse()
            if raw:
                return response.status, response.read(), dict(response.getheaders())
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    try:
        wait_until(lambda: request("/api/health")[0] == 200)
        yield runtime, request
    finally:
        server.shutdown()
        serving.join(timeout=2.0)
        runtime.close()
        server.server_close()


def test_http_pause_and_reset_revoke_a_continuing_command_publisher(endpoint):
    runtime, request = endpoint
    code, grant = request("/api/arm", {})
    assert code == 200
    old_token = grant["token"]
    done = threading.Event()
    results = []
    errors = []

    def publish():
        try:
            while not done.is_set():
                result = request("/api/command", {"token": old_token, "velocity": [0.35, 0.0, 0.0]})
                results.append(result[0])
                done.wait(0.02)
        except Exception as exc:
            errors.append(exc)

    publisher = threading.Thread(target=publish, daemon=True)
    publisher.start()
    try:
        wait_until(lambda: results.count(200) >= 3)
        assert request("/api/pause", {})[0] == 200
        paused = request("/api/status")[1]
        wait_until(lambda: results.count(403) >= 3)
        still_paused = request("/api/status")[1]
        assert still_paused["mode"] == "paused"
        assert still_paused["time"] == paused["time"]
        assert still_paused["command"] == pytest.approx([0.0, 0.0, 0.0])
        assert not still_paused["armed"]
        assert request("/api/health")[0] == 503
        assert still_paused["healthy"]
        assert not still_paused["ready"]

        denied_before_resume = results.count(403)
        assert request("/api/resume", {})[0] == 200
        wait_until(lambda: results.count(403) >= denied_before_resume + 3)
        resumed = request("/api/status")[1]
        assert resumed["time"] > paused["time"]
        assert resumed["command"] == pytest.approx([0.0, 0.0, 0.0])
        assert not resumed["armed"]

        assert request("/api/reset", {})[0] == 200
        reset = request("/api/status")[1]
        denied_before_reset = results.count(403)
        wait_until(lambda: results.count(403) >= denied_before_reset + 3)
        assert reset["epoch"] != paused["epoch"]
        assert reset["command"] == pytest.approx([0.0, 0.0, 0.0])
        assert not reset["armed"]

        code, replacement = request("/api/arm", {})
        assert code == 200
        assert replacement["token"] != old_token
        assert request("/api/command", {"token": replacement["token"], "velocity": [-0.35, 0.0, 0.0]})[0] == 200
        # A stale sender cannot overwrite the replacement owner's target.
        assert request("/api/command", {"token": old_token, "velocity": [0.35, 0.0, 0.0]})[0] == 403
        assert request("/api/status")[1]["command"] == pytest.approx([-0.35, 0.0, 0.0])
        assert not errors
        assert runtime.error is None
    finally:
        done.set()
        publisher.join(timeout=2.0)
        assert not publisher.is_alive()


def test_http_stopped_publisher_expires_without_an_explicit_stop_request(endpoint):
    _, request = endpoint
    code, grant = request("/api/arm", {})
    assert code == 200
    code, _ = request("/api/command", {"token": grant["token"], "velocity": [0.35, 0.0, 0.0]})
    assert code == 200
    assert request("/api/status")[1]["command"] == pytest.approx([0.35, 0.0, 0.0])
    wait_until(lambda: request("/api/status")[1]["command"] == [0.0, 0.0, 0.0], timeout=1.0)


def test_http_physics_fault_is_reported_and_reset_recovers_the_live_thread(endpoint):
    runtime, request = endpoint
    _, grant = request("/api/arm", {})
    assert request("/api/command", {"token": grant["token"], "velocity": [0.35, 0.0, 0.0]})[0] == 200
    epoch = request("/api/status")[1]["epoch"]
    last_pose = request("/api/scene/state")[1]
    # Corrupt a physical state value under the production synchronization lock.
    # The actual physics loop must recognize the fault and remain recoverable.
    with runtime.lock:
        runtime.sim.data.qpos[0] = float("nan")
    wait_until(lambda: runtime.error is not None)

    code, fault = request("/api/health")
    assert code == 503
    assert fault["mode"] == "fault"
    assert not fault["ready"]
    assert not fault["healthy"]
    assert not fault["armed"]
    assert fault["position"][0] is None, "invalid physics must still produce valid JSON diagnostics"
    assert fault["command"] == pytest.approx([0.0, 0.0, 0.0])
    code, fault_pose = request("/api/scene/state")
    assert code == 200 and fault_pose["valid"] is False and fault_pose["mode"] == "fault"
    assert fault_pose["positions"] == last_pose["positions"]
    assert fault_pose["quaternions"] == last_pose["quaternions"]
    assert runtime.threads[0].is_alive()
    assert request("/api/resume", {})[0] == 503
    assert request("/api/command", {"token": grant["token"], "velocity": [0.35, 0.0, 0.0]})[0] == 503

    code, reset = request("/api/reset", {})
    assert code == 200
    assert reset["epoch"] != epoch
    reset_pose = request("/api/scene/state")[1]
    assert reset_pose["epoch"] == reset["epoch"] and reset_pose["valid"]
    wait_until(lambda: request("/api/health")[0] == 200)
    healthy = request("/api/status")[1]
    assert healthy["error"] is None
    assert not healthy["armed"]
    assert healthy["time"] > 0.5
    assert healthy["command"] == pytest.approx([0.0, 0.0, 0.0])
    assert request("/api/command", {"token": grant["token"], "velocity": [0.35, 0.0, 0.0]})[0] == 403
    code, replacement = request("/api/arm", {})
    assert code == 200
    assert replacement["token"] != grant["token"]


def test_http_fallen_robot_is_not_ready_and_requires_reset(endpoint):
    runtime, request = endpoint
    _, grant = request("/api/arm", {})
    with runtime.lock:
        runtime.sim.data.qpos[3:7] = [2 ** -0.5, 2 ** -0.5, 0.0, 0.0]
        mujoco.mj_forward(runtime.sim.model, runtime.sim.data)
    wait_until(lambda: request("/api/status")[1]["mode"] == "fallen")
    code, fallen = request("/api/health")
    assert code == 503
    assert fallen["healthy"], "a fall does not terminate the physics runtime"
    assert not fallen["ready"]
    assert not fallen["armed"]
    assert request("/api/arm", {})[0] == 400
    assert request("/api/command", {"token": grant["token"], "velocity": [0.35, 0.0, 0.0]})[0] == 403
    assert request("/api/reset", {})[0] == 200
    wait_until(lambda: request("/api/health")[0] == 200)


def test_http_closed_runtime_cannot_report_ready_or_accept_control(endpoint):
    runtime, request = endpoint
    _, grant = request("/api/arm", {})
    runtime.close()
    code, closed = request("/api/health")
    assert code == 503
    assert not closed["healthy"]
    assert not closed["ready"]
    assert not any(thread.is_alive() for thread in runtime.threads)
    assert request("/api/command", {"token": grant["token"], "velocity": [0.35, 0.0, 0.0]})[0] == 503
    assert request("/api/resume", {})[0] == 503
    assert request("/api/reset", {})[0] == 503


def test_sensor_settings_are_strict_atomic_and_do_not_change_control_ownership(endpoint):
    runtime, request = endpoint
    initial = request("/api/status")[1]
    expected = {"lidar_enabled": True, "camera_enabled": True, "lidar_dropout": 0.0}
    assert initial["sensor_settings"] == expected
    generation = runtime.observation_generation
    retained_frame = object()
    runtime.camera_frame = retained_frame
    for changes in ({}, expected):
        code, result = request("/api/sensors", changes)
        assert code == 200 and not result["changed"] and result["generation"] == 0
        assert runtime.observation_generation == generation
        assert runtime.camera_frame is retained_frame
    for invalid in ([], {"unknown": True}, {"lidar_enabled": 0}, {"camera_enabled": "false"},
                    {"lidar_dropout": True}, {"lidar_dropout": None}, {"lidar_dropout": "0.5"},
                    {"lidar_dropout": -0.1}, {"lidar_dropout": 1.1}, {"lidar_dropout": float("nan")},
                    {"lidar_enabled": False, "camera_enabled": 1}):
        assert request("/api/sensors", invalid)[0] == 400
        assert runtime.sensor_settings == expected
        assert runtime.observation_generation == generation
        assert runtime.camera_frame is retained_frame
    assert request("/api/sensors", {"lidar_enabled": False})[0] == 200
    changed = request("/api/status")[1]
    assert changed["sensor_settings"] == {**expected, "lidar_enabled": False}
    assert changed["sensor_generation"] == 1 and runtime.observation_generation == generation + 1
    assert changed["epoch"] == initial["epoch"] and not changed["armed"]
    assert runtime.camera_frame is None
    _, grant = request("/api/arm", {})
    assert request("/api/sensors", {"lidar_dropout": 0.5, "camera_enabled": False})[0] == 200
    assert runtime.sim.owner == grant["token"] and runtime.sim.epoch == initial["epoch"]
    assert request("/api/reset", {})[0] == 200
    assert request("/api/status")[1]["sensor_settings"] == {
        "lidar_enabled": False, "camera_enabled": False, "lidar_dropout": 0.5}


def test_http_browser_scene_is_static_compressed_and_excludes_robot_collision_proxies(endpoint):
    runtime, request = endpoint
    code, scene = request("/api/scene")
    assert code == 200 and scene["version"] == 1
    assert len(scene["meshes"]) == runtime.sim.model.nmesh
    names = {geom["name"] for geom in scene["geoms"]}
    assert {"floor", "east_wall", "west_wall", "north_wall", "south_wall", "obstacle"} <= names
    assert "base_box" not in names and "base_cyl1" not in names
    code, compressed, headers = request("/api/scene", headers={"Accept-Encoding": "gzip"}, raw=True)
    assert code == 200 and headers["Content-Encoding"] == "gzip"
    assert headers["Vary"] == "Accept-Encoding"
    assert json.loads(gzip.decompress(compressed)) == scene
    assert len(compressed) < len(runtime.scene.json) / 2
    for encoding in ("gzip;q=0", "gzip;q=invalid"):
        code, payload, headers = request("/api/scene", headers={"Accept-Encoding": encoding}, raw=True)
        assert code == 200 and "Content-Encoding" not in headers
        assert payload == runtime.scene.json
    assert request("/frame.jpg")[0] == 410
    assert request("/camera.jpg")[0] == 503
    assert request("/vendor/../../runtime.py")[0] == 404
    assert request("/vendor/arbitrary.js")[0] == 404
    assert request("/api/health")[0] == 200, "headless browser rendering needs no server JPEG"


def test_http_scene_lifecycle_and_movable_obstacle_update_without_reloading_meshes(endpoint):
    runtime, request = endpoint
    scene = request("/api/scene")[1]
    initial = request("/api/scene/state")[1]
    assert initial["scene_id"] == scene["id"] and initial["valid"]
    assert len(initial["positions"]) == len(scene["bodies"]) * 3
    assert len(initial["quaternions"]) == len(scene["bodies"]) * 4
    assert request("/api/pause", {})[0] == 200
    paused = request("/api/scene/state")[1]
    assert paused["mode"] == "paused" and paused["generation"] > initial["generation"]
    assert request("/api/scene/state")[1] == paused, "reconnecting clients see the same paused state"
    assert request("/api/obstacle", {"position": [3, 2]})[0] == 200
    moved = request("/api/scene/state")[1]
    body = runtime.sim.model.body("sandbox_obstacle").id
    assert moved["positions"][body * 3:body * 3 + 3] == [3, 2, 0.4]
    assert moved["generation"] > paused["generation"] and moved["scene_id"] == scene["id"]
    assert request("/api/reset", {})[0] == 200
    reset = request("/api/scene/state")[1]
    assert reset["epoch"] > initial["epoch"] and reset["generation"] > moved["generation"]
    assert reset["positions"][body * 3:body * 3 + 3] == [2.5, 1.5, 0.4]
    assert reset["scene_id"] == scene["id"] and reset["valid"]
