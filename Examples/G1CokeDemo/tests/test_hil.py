from contextlib import contextmanager
from http.client import HTTPConnection
from pathlib import Path
import threading
import time
import zlib

import numpy as np
import pytest

from coke_demo.hil import (
    DT_NS, EXPECTED_CHECKPOINT_SHA256, IMAGE_SHAPE, MAX_BODY, SCHEMA,
    InferenceSession, RemoteSimulationPolicy, SimulationPolicy,
    decode_packet, encode_packet, make_inference_server,
)

NAMES = [f"joint_{i}" for i in range(43)]
BOUNDS = np.tile([-10., 10.], (43, 1))
ROOT = Path(__file__).resolve().parents[1]


class Predictor:
    names = NAMES
    devices = {"vision": "cpu", "control": "cpu"}

    def __init__(self):
        self.calls = self.resets = 0
        self.images = []

    def reset(self):
        self.resets += 1

    def propose(self, header, image):
        self.calls += 1
        self.images.append(image)
        return {"target_q_43": header["q43"].tolist(), "compute_ms": .1}


def reset_body(session="a" * 32, names=NAMES):
    return {"session": session, "schema": SCHEMA, "joint_names": names,
            "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256}


def observation(step=0):
    return {"frame": step, "camera_frame": step - step % 2,
            "q43": np.arange(43) / 100, "dq43": np.zeros(43),
            "image": np.full(IMAGE_SHAPE, .25, dtype=np.float32)}


def packet(step=0, session="a" * 32, **changes):
    obs = observation(step)
    header = {"session": session, "step": step, "camera_frame": obs["camera_frame"],
              "q43": obs["q43"].tolist(), "dq43": obs["dq43"].tolist(),
              "reference": [0.] * 43, "reference_velocity": [0.] * 15}
    header.update(changes)
    return encode_packet(header, obs["image"] if step % 2 == 0 else None)


@contextmanager
def running_server(runtime=None, token=""):
    runtime = runtime or InferenceSession(Predictor())
    server = make_inference_server(runtime, port=0, token=token)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield runtime, f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_packet_preserves_exact_tensor_and_rejects_truncation():
    body = packet()
    header, image = decode_packet(body)
    assert header["step"] == 0
    np.testing.assert_array_equal(image, observation()["image"])
    for invalid in (b"", body[:3], body[:-1], b"\xff" * 4 + body, body + b"x"):
        with pytest.raises(ValueError):
            decode_packet(invalid)


def test_retries_advance_policy_once_and_old_sessions_cannot_return():
    runtime = InferenceSession(Predictor())
    runtime.reset(reset_body())
    first = runtime.propose(packet())
    assert runtime.propose(packet()) == first
    assert runtime.predictor.calls == 1
    runtime.reset(reset_body())  # Lost reset reply is also safe to retry.
    assert runtime.next_step == 1
    for invalid in (packet(2), packet(0, q43=[1.] * 43), packet(session="b" * 32)):
        with pytest.raises(ValueError):
            runtime.propose(invalid)
    runtime.propose(packet(1))
    assert runtime.predictor.images[-1] is None
    runtime.reset(reset_body("b" * 32))
    assert runtime.next_step == 0
    with pytest.raises(ValueError, match="old HIL session"):
        runtime.reset(reset_body())
    with pytest.raises(ValueError, match="session changed"):
        runtime.propose(packet(0))
    runtime.propose(packet(0, session="b" * 32))


@pytest.mark.parametrize("changes", [
    {"step": True}, {"step": 7518}, {"camera_frame": 1},
    {"q43": [0.] * 42}, {"dq43": [1e100] * 43},
    {"reference_velocity": []},
])
def test_invalid_observation_does_not_advance(changes):
    runtime = InferenceSession(Predictor())
    runtime.reset(reset_body())
    with pytest.raises(ValueError):
        runtime.propose(packet(**changes))
    assert runtime.next_step == runtime.predictor.calls == 0


def test_inference_failure_requires_reset_even_when_retrying_same_step():
    predictor = Predictor()
    runtime = InferenceSession(predictor)
    runtime.reset(reset_body())
    original = predictor.propose

    def fail(*args):
        original(*args)
        raise RuntimeError("failed after changing hidden state")

    predictor.propose = fail
    with pytest.raises(RuntimeError):
        runtime.propose(packet())
    predictor.propose = original
    with pytest.raises(ValueError, match="episode failed"):
        runtime.propose(packet())
    assert predictor.calls == 1
    runtime.reset(reset_body("b" * 32))
    runtime.propose(packet(session="b" * 32))


def test_real_http_lost_reply_is_retried_without_second_inference(monkeypatch):
    with running_server() as (runtime, url):
        client = RemoteSimulationPolicy(url, NAMES, BOUNDS)
        original = HTTPConnection.getresponse
        dropped = False

        def lose_step_reply(connection):
            nonlocal dropped
            response = original(connection)
            if runtime.next_step == 1 and not dropped:
                dropped = True
                response.read()  # Server completed the step, but caller never got it.
                raise ConnectionResetError("reply lost")
            return response

        monkeypatch.setattr(HTTPConnection, "getresponse", lose_step_reply)
        try:
            for step in range(3):
                target, _ = client.propose(observation(step), np.zeros(43), np.zeros(15))
                np.testing.assert_allclose(target, observation()["q43"])
            assert dropped and runtime.predictor.calls == runtime.next_step == 3
            client.reset()
            client.propose(observation(), np.zeros(43), np.zeros(15))
            assert runtime.next_step == 1 and runtime.predictor.resets == 2
        finally:
            client.close()


def test_http_limits_authentication_and_checkpoint_handshake():
    with running_server(token="test-token-123456") as (runtime, url):
        client = RemoteSimulationPolicy(url, NAMES, BOUNDS)
        try:
            with pytest.raises(RuntimeError, match="token"):
                client.propose(observation(), np.zeros(43), np.zeros(15))
            assert runtime.predictor.calls == 0
            client.token = "test-token-123456"
            client.names = NAMES[::-1]
            with pytest.raises(ValueError, match="joint order"):
                client.propose(observation(), np.zeros(43), np.zeros(15))
            assert runtime.predictor.resets == 0
            client.request("/health")
            client.connection.request("POST", "/step", headers={
                "Authorization": "Bearer test-token-123456", "Content-Length": str(MAX_BODY + 1)})
            response = client.connection.getresponse()
            assert response.status == 409
            assert response.getheader("Connection") == "close"
            response.read()
        finally:
            client.close()


def test_restart_rejects_inflight_episode():
    with running_server() as (runtime, url):
        client = RemoteSimulationPolicy(url, NAMES, BOUNDS)
        try:
            client.propose(observation(), np.zeros(43), np.zeros(15))
            runtime.session = None  # A replacement process has no previous session.
            with pytest.raises(RuntimeError, match="session changed"):
                client.propose(observation(1), np.zeros(43), np.zeros(15))
            assert client.next_step == 1
            client.reset()
            client.propose(observation(), np.zeros(43), np.zeros(15))
        finally:
            client.close()


def test_network_latency_does_not_skip_policy_steps():
    predictor = Predictor()
    original = predictor.propose

    def delayed(header, image):
        time.sleep(.04)  # Longer than one 25 ms control interval.
        return original(header, image)

    predictor.propose = delayed
    with running_server(InferenceSession(predictor)) as (runtime, url):
        client = RemoteSimulationPolicy(url, NAMES, BOUNDS, timeout=1.)
        try:
            for step in range(4):
                client.propose(observation(step), np.zeros(43), np.zeros(15))
            assert runtime.next_step == predictor.calls == 4
        finally:
            client.close()


def test_compressed_request_has_a_decompressed_size_limit():
    with running_server() as (runtime, url):
        client = RemoteSimulationPolicy(url, NAMES, BOUNDS)
        try:
            client.request("/health")
            client.connection.request("POST", "/step", zlib.compress(b"x" * (MAX_BODY + 1)),
                                      {"Content-Encoding": "deflate"})
            response = client.connection.getresponse()
            assert response.status == 409
            assert b"oversized" in response.read()
            assert runtime.predictor.calls == 0
        finally:
            client.close()


@pytest.mark.parametrize("change", [
    {"step": 4}, {"session": "old"}, {"camera_frame": 2},
    {"target_q_43": [100.] * 43}, {"target_q_43": [float("nan")] * 43},
])
def test_bad_reply_cannot_advance_simulation_client(change):
    client = RemoteSimulationPolicy("http://localhost:8098", NAMES, BOUNDS)
    client.ready = True
    client.request = lambda *_: {"schema": SCHEMA, "session": client.session,
        "step": 0, "camera_frame": 0, "target_q_43": [0.] * 43, "compute_ms": 1., **change}
    with pytest.raises(ValueError):
        client.propose(observation(), np.zeros(43), np.zeros(15))
    assert client.next_step == 0


def test_remote_cpu_matches_existing_scene_policy_with_camera_reuse():
    """Compare against the existing inference sequence, not another RPC encoder."""
    if not (ROOT / "bundle/checkpoint.pt").exists():
        pytest.skip("prepared assets required")
    import torch
    from runtime.async_vision import VisionSnapshot
    from runtime.exact_policy import CachedReferenceResidualPolicy
    from runtime.observation_adapter import LiveJointFeatureAdapter, OrderedJointState

    predictor = SimulationPolicy(ROOT / "bundle", "cpu", "cpu")
    local = CachedReferenceResidualPolicy(ROOT / "bundle", device="cpu")
    names = predictor.names
    with np.load(ROOT / "expert/rollout.npz", allow_pickle=False) as trace:
        references = trace["trace"][:, 87:].copy()
    owned = np.asarray(local.contract["owned_indices"], dtype=int)
    velocities = np.gradient(references[:, owned], .025, axis=0).astype(np.float32)
    joints = LiveJointFeatureAdapter(names)
    try:
        with running_server(InferenceSession(predictor)) as (_, url):
            client = RemoteSimulationPolicy(url, names, np.tile([-100., 100.], (43, 1)))
            try:
                # Repeat after reset to check GRU, image cache and filtered acceleration.
                for episode in range(2):
                    client.reset()
                    local.reset_episode()
                    joints.reset_episode(started_at_ns=1_000_000_000)
                    generation = local.vision.status()["episode_generation"]
                    for step in range(4):
                        obs = observation(step)
                        obs["q43"] = references[step]
                        obs["dq43"] = np.full(43, step * .001)
                        obs["image"].fill(.1 + (step // 2) * .1)
                        now = 1_000_000_000 + step * DT_NS
                        captured = 1_000_000_000 + obs["camera_frame"] * DT_NS
                        if step % 2 == 0:
                            with torch.inference_mode():
                                embedding = local.model.encoder.vision(torch.from_numpy(obs["image"]))
                        features = joints.pack(OrderedJointState(tuple(names), obs["q43"], obs["dq43"], now),
                                               camera_captured_at_ns=captured, control_at_ns=now)
                        snapshot = VisionSnapshot(generation, obs["camera_frame"] + 1,
                                                  obs["camera_frame"], "coke-scene", captured, now, 0., embedding)
                        expected = local.propose(joint_features=features, reference_full=references[step].astype(np.float32),
                            reference_velocity=velocities[step], control_at_ns=now, vision_snapshot=snapshot)
                        actual, _ = client.propose(obs, references[step], velocities[step])
                        np.testing.assert_allclose(actual, expected["target_q_43"], atol=1e-7, rtol=1e-6)
            finally:
                client.close()
    finally:
        local.close()
        predictor.close()
