"""Live environment transactions using actual pinned MuJoCo robots and HTTP."""
import base64
import json
import threading
import time
from types import SimpleNamespace
from http.server import ThreadingHTTPServer
from http.client import HTTPConnection

import mujoco
import numpy as np
import pytest

from go2_sim.runtime import Runtime
from go2_sim.project import apply_project, decode_project, MAX_REQUEST
from go2_sim.sensors import PhysicsSampler
from go2_sim.observations import SnapshotQueue
from go2_sim.server import Handler

KIND = "go2"


def manifest():
    return {"version": 1, "profile": KIND, "geoms": [
        {"name": "crate", "type": "box", "size": [0.3, 0.4, 0.5], "position": [3, -2, 0.5]}]}


def envelope(runtime, scene=None, assets=None):
    files = {"scene.json": json.dumps(manifest() if scene is None else scene).encode(), **(assets or {})}
    return {"vm_name": "project-test", "source_digest": runtime.source_digest,
            "project_id": "a" * 32, "base_revision": runtime.project_status["revision"],
            "files": {name: base64.b64encode(content).decode() for name, content in files.items()}}


@pytest.fixture
def live(monkeypatch):
    monkeypatch.setenv(KIND.upper() + "_VM_NAME", "project-test")
    monkeypatch.setenv(KIND.upper() + "_SOURCE_DIGEST", "test-pinned-source")
    monkeypatch.setenv(KIND.upper() + "_VISUAL_DETAIL", "full")
    runtime = Runtime(render=False)
    runtime.start()
    try:
        yield runtime
    finally:
        runtime.close()


def test_commit_refreshes_scene_and_samplers_without_robot_interface_changes(live):
    old_model, old_scene = live.sim.model, live.scene
    sampler = PhysicsSampler(live.sim)
    queue = SnapshotQueue(live.sim)
    token = live.sim.arm()
    old_epoch = live.sim.epoch
    result = apply_project(live, envelope(live), KIND)
    assert result["revision"] and result["error"] is None
    assert live.sim.epoch == old_epoch + 1 and live.sim.mode == "paused" and live.sim.owner is None
    assert [getattr(live.sim.model, name) for name in ("nq", "nv", "nu", "nbody", "nmocap")] == [
        getattr(old_model, name) for name in ("nq", "nv", "nu", "nbody", "nmocap")]
    assert live.scene.description["id"] != old_scene.description["id"]
    assert any(geom["name"] == "project_crate" for geom in live.scene.description["geoms"])
    state = json.loads(live.scene.state_json(live))
    assert state["scene_id"] == live.scene.description["id"] and state["epoch"] == live.sim.epoch
    assert live.status()["source_digest"] == "test-pinned-source"
    with pytest.raises(PermissionError):
        live.sim.command_velocity(0, 0, 0, token=token)
    with live.lock:
        live.sim.resume()
        assert sampler.capture(live)
        assert sampler.model is live.sim.model and sampler.sim is live.sim
        queue.capture(live)
        assert queue.model is live.sim.model
    assert np.isfinite(sampler.state()["position"]).all()
    if hasattr(live, "browser_lidar"):
        cloud = json.loads(live.browser_lidar.state_json())
        assert cloud["scene_id"] == live.scene.description["id"]


def test_obj_mesh_compiles_and_reaches_browser(live):
    scene = manifest()
    scene["geoms"][0].update(type="mesh", size=[0.4, 0.4, 0.4], mesh="assets/crate.obj")
    obj = b"v 0 0 0\nv 1 0 0\nv 0 1 0\nv 0 0 1\nf 1 3 2\nf 1 2 4\nf 1 4 3\nf 2 3 4\n"
    apply_project(live, envelope(live, scene, {"assets/crate.obj": obj}), KIND)
    geom = next(g for g in live.scene.description["geoms"] if g["name"] == "project_crate")
    assert geom["type"] == "mesh" and geom["mesh"] >= 0


@pytest.mark.parametrize("mutation", [
    lambda scene: scene.update(controller="untrusted"),
    lambda scene: scene.update(profile="other"),
    lambda scene: scene["geoms"][0].update(position=[0, 0, 0.5]),
    lambda scene: scene["geoms"][0].update(position=[float("nan"), 2, 0]),
    lambda scene: scene["geoms"][0].update(type="mesh", mesh="../secret.obj"),
    lambda scene: scene["geoms"][0].update(type="mesh", mesh="assets/missing.obj"),
    lambda scene: scene["geoms"][0].update(quaternion=[0, 0, 0, 0]),
    lambda scene: scene["geoms"][0].update(size=[-1, 1, 1]),
])
def test_invalid_edit_retains_last_working_world(live, mutation):
    apply_project(live, envelope(live), KIND)
    previous, scene, epoch = live.sim, live.scene, live.sim.epoch
    changed = manifest()
    mutation(changed)
    with pytest.raises(ValueError, match="previous scene retained"):
        apply_project(live, envelope(live, changed), KIND)
    assert live.sim is previous and live.scene is scene and live.sim.epoch == epoch
    assert live.project_status["error"] and live.error is None and live.status()["healthy"]


def test_idempotency_revision_conflict_and_runtime_identity(live):
    body = envelope(live)
    apply_project(live, body, KIND)
    epoch = live.sim.epoch
    with pytest.raises(ValueError, match="revision changed"):
        apply_project(live, body, KIND)
    apply_project(live, envelope(live), KIND)
    assert live.sim.epoch == epoch
    for key, value in (("project_id", "b"*32), ("vm_name", "wrong"), ("source_digest", "wrong")):
        changed = envelope(live)
        changed[key] = value
        with pytest.raises(ValueError):
            apply_project(live, changed, KIND)
    report = envelope(live)
    report.pop("files")
    report["error"] = "scene.json is incomplete"
    apply_project(live, report, KIND)
    assert live.project_status["error"] == "scene.json is incomplete" and live.sim.epoch == epoch
    report.pop("error")
    report.update(heartbeat=True, clear_error=True)
    apply_project(live, report, KIND)
    assert live.project_status["error"] is None


def test_decode_rejects_external_obj_references_and_resource_limits():
    scene = manifest()
    scene["geoms"][0].update(type="mesh", mesh="assets/crate.obj", size=[1, 1, 1])
    for content in (b"mtllib /etc/passwd", b"v inf 0 0", b"v 0 0 0\nf 1 2 3", b"x"*(2*1024*1024+1)):
        files = {"scene.json": json.dumps(scene).encode(), "assets/crate.obj": content}
        with pytest.raises(ValueError):
            decode_project({n: base64.b64encode(c).decode() for n,c in files.items()}, KIND)
    with pytest.raises(ValueError):
        decode_project({"scene.json": "not-base64"}, KIND)


def test_http_transaction_errors_and_busy_rejection(live):
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.runtime = live
    thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
    thread.start()
    def request(body, headers=None):
        connection = HTTPConnection("127.0.0.1", server.server_port, timeout=10)
        try:
            connection.request("POST", "/api/project", json.dumps(body), {"Content-Type": "application/json", **(headers or {})})
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()
    try:
        assert request(envelope(live), {"Origin": "http://foreign.example"})[0] == 403
        assert request(envelope(live), {"Content-Length": str(MAX_REQUEST+1)})[0] == 413
        assert request(envelope(live))[0] == 200
        assert request(envelope(live, {"bad": True}))[0] == 400
        with live.project_lock:
            assert request(envelope(live))[0] == 503
        assert live.error is None
    finally:
        server.shutdown()
        thread.join(2)
        server.server_close()


def test_camera_worker_recreates_model_buffers_and_fences_old_exposures(live, monkeypatch):
    created, closed = [], []
    class Renderer:
        def __init__(self, model, **kwargs):
            self.model = model
            self.scene = SimpleNamespace(flags={})
            created.append(model)
        def update_scene(self, *args, **kwargs):
            pass
        def render(self):
            return np.zeros((360, 640, 3), dtype=np.uint8)
        def close(self):
            closed.append(self.model)
    monkeypatch.setattr(mujoco, "Renderer", Renderer)
    live.camera_demand = time.monotonic() + 10
    worker = threading.Thread(target=live._render, daemon=True)
    live.threads.append(worker)
    worker.start()
    deadline = time.monotonic() + 2
    while not created and time.monotonic() < deadline:
        time.sleep(0.01)
    old = live.sim.model
    apply_project(live, envelope(live), KIND)
    deadline = time.monotonic() + 2
    while len(created) < 2 and time.monotonic() < deadline:
        time.sleep(0.01)
    assert len(created) >= 2 and old in closed
    assert created[-1] is live.visual_model and live.error is None
    assert live.camera_frame is None
    live.resume()
    deadline = time.monotonic() + 2
    while live.camera_frame is None and time.monotonic() < deadline:
        time.sleep(0.01)
    assert live.camera_frame.epoch == live.sim.epoch
    assert live.camera_frame.generation == live.observation_generation
