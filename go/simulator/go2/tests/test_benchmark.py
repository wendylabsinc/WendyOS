"""Benchmark-driver contract tests; synthetic fixtures are never benchmark artifacts."""

import importlib.util
import math
from pathlib import Path
import sys
import threading
import time

import pytest


spec = importlib.util.spec_from_file_location("go2_benchmark", Path(__file__).parents[1] / "tools/benchmark.py")
benchmark = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = benchmark
spec.loader.exec_module(benchmark)


def sample(wall, sim, *, heading=0.0, position=None):
    return {
        "simulation_seconds": sim,
        "position": position or [0.0, 0.0, .34],
        "yaw_radians": heading,
        "ncontact": 4,
        "joint_q": [0.0] * 12,
        "metrics": {"wall_seconds": wall, "physics_steps": wall * 500,
                    "policy_updates": wall * 50, "camera_frames": wall * 15, "scene_states": 0,
                    "cpu_seconds": wall * 1.5, "overruns": 0,
                    # These lifetime averages deliberately disagree with the interval.
                    "camera_fps": 1.0, "real_time_factor": .1},
    }


def test_rates_are_computed_from_interval_not_lifetime_averages():
    result = benchmark.measured_interval(sample(1000, 25), sample(1010, 35))
    assert result["wall_seconds"] == 10
    assert result["real_time_factor"] == 1
    assert result["camera_fps"] == 15
    assert result["policy_hz"] == 50
    assert result["physics_steps"] == 5000
    assert result["mean_process_cpu_cores"] == 1.5


def test_counter_reset_cannot_look_like_successful_throughput():
    first, last = sample(1000, 25), sample(1010, 35)
    last["metrics"]["camera_frames"] = 1
    with pytest.raises(ValueError, match="counter reset"):
        benchmark.measured_interval(first, last)


def test_segment_displacement_uses_initial_body_frame_and_unwraps_yaw():
    first = sample(1000, 25, heading=math.pi / 2)
    last = sample(1002, 27, heading=math.pi / 2, position=[0.0, .6, .34])
    segment = benchmark.segment_result("forward", (.3, 0, 0), [first, last])
    assert segment["displacement_in_initial_body_frame_m"] == pytest.approx([.6, 0])
    assert segment["signed_motion"][0] == pytest.approx(.6)
    first["yaw_radians"], last["yaw_radians"] = 3.1, -3.1
    segment = benchmark.segment_result("turn_left", (0, 0, .5), [first, last])
    assert segment["yaw_delta_radians"] == pytest.approx(2 * math.pi - 6.2)


@pytest.mark.parametrize("url", ["http://192.168.123.161:8890", "file:///tmp/sim", "http://user:pass@localhost:8890", "http://localhost:8890/?x=1"])
def test_default_url_guard_rejects_unintended_targets(url):
    with pytest.raises(ValueError):
        benchmark.validate_url(url)


class FixtureClient:
    def __init__(self, *, identified=True, reject=False, fall=False):
        self.started = time.monotonic()
        self.identified, self.reject, self.fall = identified, reject, fall
        self.calls = []
        self.lock = threading.Lock()
        self.status_calls = 0

    def request(self, path, body=None):
        with self.lock:
            self.calls.append((path, body))
            if path == "/api/arm":
                return {"token": "test-only-fixture-token", "epoch": 1}
            if path == "/api/command":
                return {"accepted": not self.reject}
            if path in {"/api/stop", "/api/release"}:
                return {"ok": True}
            self.status_calls += path == "/api/status"
            elapsed = time.monotonic() - self.started
            state = sample(1000 + elapsed, 25 + elapsed)
            return {
                "robot": "go2", "simulation": self.identified, "profile_version": 1,
                "ready": True, "epoch": 1, "mode": "fallen" if self.fall and self.status_calls > 1 else "standing",
                "time": state["simulation_seconds"], "position": state["position"],
                "quaternion_wxyz": [1.0, 0.0, 0.0, 0.0], "ncontact": 4,
                "joints": {"q": [0.0] * 12, "dq": [0.0] * 12, "torque": [0.0] * 12},
                "metrics": {**state["metrics"], "rss_bytes": 1024, "command_p95_ms": 3.0},
            }


def test_unidentified_endpoint_receives_no_control_requests():
    client = FixtureClient(identified=False)
    report = benchmark.run(benchmark.Config(duration=.12, sample_hz=50), client=client)
    assert report["outcome"] == "failed"
    assert [path for path, _ in client.calls] == ["/api/health"]


@pytest.mark.parametrize("failure", [None, "reject", "fall"])
def test_control_is_stopped_and_released_on_completion_or_failure(failure):
    client = FixtureClient(reject=failure == "reject", fall=failure == "fall")
    config = benchmark.Config(duration=.12, segment_seconds=.08, sample_hz=50)
    report = benchmark.run(config, client=client)
    paths = [path for path, _ in client.calls]
    assert paths.count("/api/arm") == 1
    assert paths[-2:] == ["/api/stop", "/api/release"]
    assert "/api/command" in paths
    assert report["outcome"] == ("completed" if failure is None else "failed")
    if failure is None:
        assert report["command_http_round_trip_ms"]["count"] >= 2
        assert report["measured_interval"]["camera_fps"] == pytest.approx(15)
        assert "render_fps_at_least_14" not in report["performance_targets"]
        assert not report["performance_targets"]["window_at_least_600_seconds"]
