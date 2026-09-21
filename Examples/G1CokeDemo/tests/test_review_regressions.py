import http.client
import threading
from types import SimpleNamespace
from unittest.mock import Mock

import numpy as np
import pytest

from coke_demo.access import require_loopback
from coke_demo.hil import make_inference_server
from runtime.async_vision import AsyncVisionBuffer, VisionFrame, VisionUnavailable
from runtime.inference_client import InferenceClient, RemoteCamera
from runtime.physical_policy import IntegratedPhysicalPolicyRunner, PolicyStopRequested
from runtime.shadow_service import readiness_status


@pytest.mark.parametrize("host", ["0.0.0.0", "::", "localhost", "192.0.2.1"])
def test_control_listener_rejects_nonliteral_or_remote_hosts(host):
    with pytest.raises(ValueError, match="loopback"):
        require_loopback(host)


def test_network_hil_requires_token_before_opening_socket():
    with pytest.raises(ValueError, match="COKE_HIL_TOKEN"):
        make_inference_server(None, host="0.0.0.0", port=0)


def test_lost_proposal_reply_is_not_retried():
    client = InferenceClient("http://127.0.0.1:8098")
    connection = Mock()
    connection.getresponse.side_effect = http.client.RemoteDisconnected()
    client.connection = connection
    with pytest.raises(http.client.RemoteDisconnected):
        client.request("POST", "/propose", {"sampled_at_ns": 42})
    assert connection.request.call_count == 1
    assert client.connection is None


def test_failed_deactivation_keeps_session_for_cleanup_retry():
    client = SimpleNamespace(session_id="session", request=Mock(side_effect=OSError))
    with pytest.raises(OSError):
        RemoteCamera(client).deactivate()
    assert client.session_id == "session"
    client.request.side_effect = None
    RemoteCamera(client).deactivate()
    assert client.session_id is None


def test_stream_change_discards_cached_and_inflight_embeddings():
    encoding = threading.Event()
    release = threading.Event()
    def encode(payload):
        if payload == "old-pending":
            encoding.set()
            assert release.wait(2)
        return payload
    buffer = AsyncVisionBuffer(encode, maximum_capture_age_s=1, clock_ns=lambda: 100)
    buffer.start()
    try:
        buffer.submit(VisionFrame(1, "old", 100, "old-ready"))
        with buffer._condition:
            assert buffer._condition.wait_for(lambda: buffer._processed == 1, 2)
        old = buffer.latest()
        buffer.submit(VisionFrame(2, "old", 100, "old-pending"))
        assert encoding.wait(2)
        buffer.submit(VisionFrame(0, "new", 100, "new"))
        with pytest.raises(VisionUnavailable):
            buffer.latest()
        with pytest.raises(VisionUnavailable):
            buffer.admit(old, control_at_ns=100)
        release.set()
        with buffer._condition:
            assert buffer._condition.wait_for(lambda: buffer._processed == 2, 2)
        assert buffer.latest().embedding == "new"
    finally:
        release.set()
        buffer.close()


def test_stop_during_entry_prevents_next_motion_command():
    runner = IntegratedPhysicalPolicyRunner.__new__(IntegratedPhysicalPolicyRunner)
    runner.stop_requested = threading.Event()
    runner.stop_requested.set()
    runner._publish_with_timing_retries = Mock()
    with pytest.raises(PolicyStopRequested):
        runner._publish_entry_tick("owner", 1, np.zeros(43), 1., 0.)
    runner._publish_with_timing_retries.assert_not_called()


def test_frozen_camera_is_not_ready():
    state = {"healthy": True, "identity": {}, "provider": {
        "exact_frame_synchronized": True, "target_mask_valid": True}}
    status = readiness_status(state, {"latest_capture_age_ms": 1000},
                              observed_at_unix_ns=1, revision="test")
    assert not status["perception"]["stream_synchronized"]
    assert not status["perception"]["target_mask_valid"]


@pytest.mark.parametrize("field,value", [
    ("schema", "other"), ("checkpoint_sha256", "0" * 64),
    ("joint_names", ["right", "left"]),
])
def test_remote_inference_rejects_wrong_identity(monkeypatch, tmp_path, field, value):
    from runtime import inference_client
    from runtime.contracts import INFERENCE_SCHEMA, EXPECTED_CHECKPOINT_SHA256
    status = {"healthy": True, "motion_capability": False, "schema": INFERENCE_SCHEMA,
              "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256, "joint_names": ["left", "right"]}
    client = Mock()
    client.request.return_value = status
    monkeypatch.setattr(inference_client, "InferenceClient", lambda url: client)
    np.savez(tmp_path / "reference-contract.npz", joint_names=["left", "right"],
             reference_joint_targets_43=np.zeros((1, 43)))
    episode, _ = inference_client.remote_components("http://127.0.0.1:8097", tmp_path)
    assert episode.joint_names == ("left", "right")
    status[field] = value
    with pytest.raises(RuntimeError, match="identity contract"):
        inference_client.remote_components("http://127.0.0.1:8097", tmp_path)
    client.close.assert_called_once()


@pytest.mark.parametrize("bad_depth", [float("nan"), float("inf"), -float("inf")])
def test_segmentation_packet_rejects_nonfinite_depth(bad_depth):
    import io
    import json
    from runtime.shadow_service import decode_policy_frame
    meta = {"frame_id": 1, "stream_id": "test", "captured_at_unix_ns": 100}
    depth = np.ones((240, 320), dtype=np.float32)
    depth[0, 0] = bad_depth
    packet = io.BytesIO()
    np.savez(packet, color_bgr=np.zeros((240, 320, 3), dtype=np.uint8), depth_m=depth,
             mask=np.zeros((240, 320), dtype=np.uint8), metadata_json=json.dumps(meta))
    with pytest.raises(ValueError, match="finite"):
        decode_policy_frame(packet.getvalue(), meta)


def test_failed_scene_worker_rejects_reset(monkeypatch, tmp_path):
    from coke_demo import scene
    from coke_demo.service import DemoRuntime
    monkeypatch.setattr(scene, "CokeScene", Mock(side_effect=RuntimeError("bad assets")))
    runtime = DemoRuntime(tmp_path, ros_enabled=False)
    runtime.shutdown.set()
    runtime.run()
    assert runtime.status()["restart_required"]
    with pytest.raises(ValueError, match="restart"):
        runtime.request("reset", {})
    assert runtime.commands.empty()
    assert runtime.status()["phase"] == "error"


def test_physical_shutdown_stops_before_server_close_and_always_closes_io(monkeypatch):
    from runtime import physical_policy_service as service
    physical = Mock()
    runner = Mock(stop_requested=threading.Event())
    runner.close.side_effect = RuntimeError("camera cleanup failed")
    server = Mock()
    server.serve_forever.side_effect = KeyboardInterrupt
    def close_server():
        assert runner.stop_requested.is_set()
    server.server_close.side_effect = close_server
    monkeypatch.setattr(service, "PhysicalProbeRuntime", lambda: physical)
    monkeypatch.setattr(service, "make_server", lambda _: server)
    monkeypatch.setattr(IntegratedPhysicalPolicyRunner, "load", lambda *args: runner)
    with pytest.raises(RuntimeError, match="camera cleanup"):
        service.main()
    physical.close.assert_called_once()


def test_abandoned_inference_session_expires_without_exposing_old_id(monkeypatch):
    from runtime import inference_service
    runtime = inference_service.InferenceRuntime.__new__(inference_service.InferenceRuntime)
    runtime.lock = threading.RLock()
    runtime.camera = Mock()
    runtime.episode = Mock()
    runtime.session_id = "old-session-123456"
    runtime.active = True
    runtime.session_last_seen = 10.
    monkeypatch.setattr(inference_service.time, "monotonic", lambda: 20.)
    with pytest.raises(RuntimeError, match="another inference session"):
        runtime.reset("new-session-123456", 1)
    monkeypatch.setattr(inference_service.time, "monotonic", lambda: 131.)
    runtime.reset("new-session-123456", 1)
    runtime.camera.deactivate.assert_called_once()
    assert runtime.session_id == "new-session-123456"
    assert not runtime.active
    assert runtime.session_last_seen == 131.


def test_delayed_reset_keeps_publishing_owned_hold_target():
    runner = IntegratedPhysicalPolicyRunner.__new__(IntegratedPhysicalPolicyRunner)
    release = threading.Event()
    reset_started = threading.Event()
    def reset(**kwargs):
        reset_started.set()
        assert release.wait(2)
        return {"reset": True}
    runner.episode = SimpleNamespace(reset_episode=reset)
    runner.camera = Mock()
    runner.wall_time_ns = lambda: 100
    runner._live_guard_with_timing_retries = Mock()
    steps = []
    target = np.zeros(43)
    def publish(owner, step, held_target, weight, tick):
        assert reset_started.wait(2)
        assert owner == "owner" and held_target is target and weight == 1.
        steps.append(step)
        if len(steps) == 3:
            release.set()
        # Yield to the asynchronous reset without depending on wall-clock latency.
        threading.Event().wait(.001)
        return step + 1, tick + .025
    runner._publish_entry_tick = publish
    result, step, _ = runner._reset_while_holding("owner", {"remote_sequence": 7}, target, 10, 0.)
    assert result == {"reset": True}
    assert len(steps) >= 3 and step == 10 + len(steps)
    runner.camera.activate.assert_called_once()
    runner._live_guard_with_timing_retries.assert_called_with(expected_remote_sequence=7)


def test_operator_stop_releases_ownership_before_remote_deactivation():
    owned = {"value": True}
    physical = SimpleNamespace(motion_enabled=True, command_results={}, command_lock=threading.Lock(),
                               io=SimpleNamespace(status=lambda: {"publishers_armed": owned["value"]}))
    episode = SimpleNamespace(reference=SimpleNamespace(references=range(1)))
    camera = Mock()
    runner = IntegratedPhysicalPolicyRunner(physical, episode, camera)
    runner._log_timing = Mock()
    def enter(owner):
        runner.stop_requested.set()
        return {"goal": np.zeros(43), "actuation_goal": np.zeros(43), "io_step": 1, "next_tick": 0.}
    runner._enter_and_hold = enter
    runner._reset_while_holding = Mock(return_value=({}, 1, 0.))
    def release(*args, **kwargs):
        owned["value"] = False
        return {"completed": True}
    runner._controlled_release = release
    def deactivate():
        assert not owned["value"], "network deactivation ran while publisher watchdog was armed"
    camera.deactivate.side_effect = deactivate
    with pytest.raises(PolicyStopRequested):
        runner.run("test", maximum_policy_steps=1)
    assert not physical.command_lock.locked()
    camera.deactivate.assert_called_once()


def test_inference_error_closes_connection_and_next_proposal_reconnects(monkeypatch):
    from runtime import inference_service
    monkeypatch.setattr(inference_service, "PORT", 0)
    runtime = Mock()
    runtime.propose.side_effect = [VisionUnavailable("waiting"), {"step": 1}]
    server = inference_service.make_server(runtime)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    client = InferenceClient(f"http://127.0.0.1:{server.server_port}")
    client.session_id = "retained-session"
    try:
        with pytest.raises(RuntimeError, match="VisionUnavailable"):
            client.request("POST", "/propose", {})
        assert client.connection is None
        assert client.session_id == "retained-session"
        assert client.request("POST", "/propose", {}) == {"step": 1}
        connection = http.client.HTTPConnection("127.0.0.1", server.server_port)
        try:
            connection.request("POST", "/unknown", body="{}")
            response = connection.getresponse()
            assert response.status == 404
            assert response.getheader("Connection") == "close"
            response.read()
        finally:
            connection.close()
    finally:
        client.close()
        server.shutdown()
        server.server_close()
        worker.join(2)


@pytest.mark.parametrize("token", ["", "x", "short-token", "x" * 16 + "\r", "x" * 16 + "\n", "x" * 16 + "é"])
def test_hil_network_listener_rejects_weak_or_invalid_tokens(token):
    with pytest.raises(ValueError, match="COKE_HIL_TOKEN"):
        make_inference_server(None, host="0.0.0.0", port=0, token=token)


def test_hil_network_listener_accepts_strong_token():
    server = make_inference_server(None, host="0.0.0.0", port=0, token="test-token-123456")
    server.server_close()


@pytest.mark.parametrize("name", ["result.json", "qualification-60s.json", "verification.json", "replay_expert.py"])
def test_replay_rejects_modified_expert_inputs_before_model_load(tmp_path, monkeypatch, name):
    from pathlib import Path
    from coke_demo import simulation
    source = Path(__file__).resolve().parents[1] / "expert"
    for asset in source.iterdir():
        if asset.name == name:
            data = asset.read_bytes()
            (tmp_path / asset.name).write_bytes(bytes([data[0] ^ 1]) + data[1:])
        else:
            (tmp_path / asset.name).symlink_to(asset)
    model_load = Mock(side_effect=AssertionError("model loaded before verification"))
    monkeypatch.setattr(simulation.mujoco.MjModel, "from_binary_path", model_load)
    with pytest.raises(ValueError, match=f"checksum mismatch: {name}"):
        simulation.Simulation(tmp_path)
    model_load.assert_not_called()


@pytest.mark.parametrize("kind", ["physical", "inference"])
def test_service_close_waits_for_active_handlers(monkeypatch, kind):
    from physical_io import service as physical_service
    from runtime import inference_service
    entered, release, closed = threading.Event(), threading.Event(), threading.Event()
    runtime = Mock()
    def health():
        entered.set()
        assert release.wait(5)
        return {"healthy": True}
    runtime.state.side_effect = health
    runtime.status.side_effect = health
    if kind == "physical":
        server = physical_service.make_server(runtime, port=0)
    else:
        monkeypatch.setattr(inference_service, "PORT", 0)
        server = inference_service.make_server(runtime)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=2)
    connection.request("GET", "/health")
    closer = None
    try:
        assert entered.wait(2)
        server.shutdown()
        def close():
            server.server_close()
            closed.set()
        closer = threading.Thread(target=close, daemon=True)
        closer.start()
        assert not closed.wait(.05), "resources could close while a handler is active"
        release.set()
        response = connection.getresponse()
        assert response.status == 200
        if kind == "inference":
            assert response.getheader("Connection") == "close"
        response.read()
        assert closed.wait(2)
    finally:
        release.set()
        connection.close()
        server.shutdown()
        if closer is not None:
            closer.join(2)
        server.server_close()
        worker.join(2)
