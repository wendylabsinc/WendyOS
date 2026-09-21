#!/usr/bin/env python3
"""Exercise an identified local Go2 simulation and save measured HTTP telemetry.

Uses only the standard library. The default run lasts ten minutes, refreshes
velocity commands at 20 Hz, and never acquires control before checking the
endpoint's explicit simulation identity. This is an HTTP/runtime benchmark;
it does not establish ROS compatibility or physical-robot performance.
"""

from __future__ import annotations

import argparse
from dataclasses import dataclass
from datetime import datetime, timezone
import ipaddress
import json
import math
from pathlib import Path
import statistics
import sys
import threading
import time
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener


SEQUENCE = (
    ("stand", (0.0, 0.0, 0.0)),
    ("forward", (0.3, 0.0, 0.0)),
    ("backward", (-0.3, 0.0, 0.0)),
    ("right", (0.0, -0.2, 0.0)),
    ("left", (0.0, 0.2, 0.0)),
    ("turn_left", (0.0, 0.0, 0.5)),
    ("turn_right", (0.0, 0.0, -0.5)),
)
COMMAND_PERIOD = 0.05


@dataclass
class Config:
    url: str = "http://127.0.0.1:8890"
    duration: float = 600.0
    segment_seconds: float = 6.0
    sample_hz: float = 2.0
    timeout: float = 2.0
    reset: bool = False
    allow_remote: bool = False


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise ValueError(f"Refusing an HTTP redirect from the simulation endpoint: {code}")


def validate_url(url: str, allow_remote: bool = False) -> str:
    parsed = urlsplit(url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise ValueError("A complete HTTP(S) simulation URL is required")
    if parsed.username or parsed.password or parsed.query or parsed.fragment:
        raise ValueError("Simulation URL must not contain credentials, query, or fragment")
    if parsed.path not in {"", "/"}:
        raise ValueError("Simulation URL must address the server root")
    if parsed.port is not None and not 0 < parsed.port < 65536:
        raise ValueError("Invalid simulation port")
    try:
        loopback = ipaddress.ip_address(parsed.hostname).is_loopback
    except ValueError:
        loopback = parsed.hostname.lower() == "localhost"
    if not loopback and not allow_remote:
        raise ValueError("Non-loopback URLs require --allow-remote and still require simulation identity")
    return url.rstrip("/")


class Client:
    def __init__(self, config: Config):
        self.url = validate_url(config.url, config.allow_remote)
        self.timeout = config.timeout

    def request(self, path: str, body: dict | None = None) -> dict:
        data = json.dumps(body, allow_nan=False).encode() if body is not None else None
        request = Request(self.url + path, data=data, headers={"Content-Type": "application/json"})
        # Do not route localhost control through ambient proxy settings. Each
        # call has its own opener because command and status requests overlap.
        opener = build_opener(ProxyHandler({}), NoRedirects())
        with opener.open(request, timeout=self.timeout) as response:
            payload = response.read(2 * 1024 * 1024 + 1)
        if len(payload) > 2 * 1024 * 1024:
            raise ValueError("Simulation response exceeds 2 MiB")
        result = json.loads(payload)
        if not isinstance(result, dict):
            raise ValueError("Simulation response must be a JSON object")
        return result


def require_identity(status: dict) -> None:
    if (status.get("simulation") is not True or status.get("robot") != "go2"
            or type(status.get("profile_version")) is not int or status["profile_version"] != 1):
        raise ValueError("Endpoint is not the supported Go2 simulation profile")


def finite_vector(value, length: int, label: str) -> list[float]:
    if not isinstance(value, list) or len(value) != length:
        raise ValueError(f"Invalid {label} shape")
    if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in value):
        raise ValueError(f"Non-finite or invalid {label}")
    return value


def yaw(quaternion: list[float]) -> float:
    w, x, y, z = quaternion
    return math.atan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z))


def angle_delta(after: float, before: float) -> float:
    return math.atan2(math.sin(after - before), math.cos(after - before))


def telemetry(status: dict, expected_epoch: int) -> dict:
    require_identity(status)
    if status.get("error"):
        raise RuntimeError(f"Runtime error: {status['error']}")
    if status.get("mode") not in {"standing", "moving"}:
        raise RuntimeError(f"Simulation entered {status.get('mode')!r} mode")
    if not status.get("ready"):
        raise RuntimeError("Simulation is no longer ready")
    if status.get("epoch") != expected_epoch:
        raise RuntimeError("Simulation epoch changed during benchmark")
    position = finite_vector(status.get("position"), 3, "position")
    quaternion = finite_vector(status.get("quaternion_wxyz"), 4, "quaternion")
    joints = status.get("joints", {})
    q = finite_vector(joints.get("q"), 12, "joint positions")
    dq = finite_vector(joints.get("dq"), 12, "joint velocities")
    torque = finite_vector(joints.get("torque"), 12, "joint torques")
    contacts = status.get("ncontact")
    if type(contacts) is not int or contacts < 0:
        raise ValueError("Missing or invalid ncontact telemetry")
    simulation_time = status.get("time")
    if isinstance(simulation_time, bool) or not isinstance(simulation_time, (int, float)) or not math.isfinite(simulation_time):
        raise ValueError("Missing or invalid simulation time")
    return {
        "simulation_seconds": simulation_time,
        "mode": status["mode"],
        "position": position,
        "yaw_radians": yaw(quaternion),
        "ncontact": contacts,
        "joint_q": q,
        "joint_speed_abs_max": max(map(abs, dq)),
        "joint_torque_abs_max": max(map(abs, torque)),
        "metrics": status.get("metrics", {}),
    }


def measured_interval(before: dict, after: dict) -> dict:
    """Rates use differences between sampled cumulative counters, not uptime averages."""
    first, last = before["metrics"], after["metrics"]
    wall = last["wall_seconds"] - first["wall_seconds"]
    simulated = after["simulation_seconds"] - before["simulation_seconds"]
    if wall <= 0 or simulated < 0:
        raise ValueError("Simulation clocks did not advance monotonically")
    result = {"wall_seconds": wall, "simulation_seconds": simulated, "real_time_factor": simulated / wall}
    for key, rate in (("physics_steps", "physics_hz"), ("policy_updates", "policy_hz"),
                      ("camera_frames", "camera_fps")):
        count = last[key] - first[key]
        if count < 0:
            raise ValueError(f"Simulation counter reset: {key}")
        result[key] = count
        result[rate] = count / wall
    if "overruns" in first and "overruns" in last:
        result["overruns"] = last["overruns"] - first["overruns"]
    if "cpu_seconds" in first and "cpu_seconds" in last:
        cpu = last["cpu_seconds"] - first["cpu_seconds"]
        if cpu < 0:
            raise ValueError("Process CPU counter reset")
        result["process_cpu_seconds"] = cpu
        result["mean_process_cpu_cores"] = cpu / wall
    return result


def distribution(values: list[float]) -> dict:
    if not values:
        return {"count": 0, "mean": None, "p50": None, "p95": None, "max": None}
    ordered = sorted(values)
    def percentile(fraction):
        index = (len(ordered) - 1) * fraction
        lo, hi = math.floor(index), math.ceil(index)
        return ordered[lo] + (ordered[hi] - ordered[lo]) * (index - lo)
    return {"count": len(values), "mean": statistics.fmean(values), "p50": percentile(.5),
            "p95": percentile(.95), "max": max(values)}


def segment_result(name: str, command: tuple, samples: list[dict]) -> dict:
    first, last = samples[0], samples[-1]
    dx = last["position"][0] - first["position"][0]
    dy = last["position"][1] - first["position"][1]
    heading = first["yaw_radians"]
    body = [math.cos(heading) * dx + math.sin(heading) * dy,
            -math.sin(heading) * dx + math.cos(heading) * dy]
    turned = sum(angle_delta(b["yaw_radians"], a["yaw_radians"]) for a, b in zip(samples, samples[1:]))
    interval = measured_interval(first, last)
    commanded = [v * interval["simulation_seconds"] for v in command]
    observed = [*body, turned]
    qdelta = [b - a for a, b in zip(first["joint_q"], last["joint_q"])]
    return {
        "name": name, "command": list(command), "interval": interval,
        "displacement_in_initial_body_frame_m": body, "yaw_delta_radians": turned,
        "command_integral_over_sim_time": commanded,
        "motion_minus_command_integral": [a - b for a, b in zip(observed, commanded)],
        "signed_motion": [math.copysign(1, c) * value if c else None for c, value in zip(command, observed)],
        "minimum_contacts": min(s["ncontact"] for s in samples),
        "maximum_contacts": max(s["ncontact"] for s in samples),
        "joint_q_endpoint_rms_delta": math.sqrt(statistics.fmean(v * v for v in qdelta)),
        "samples": len(samples),
    }


class CommandPump:
    def __init__(self, client, token):
        self.client, self.token = client, token
        self.command = (0.0, 0.0, 0.0)
        self.lock = threading.Lock()
        self.done = threading.Event()
        self.error = None
        self.latencies = []
        self.intervals = []
        self.thread = threading.Thread(target=self._run, daemon=True)

    def set_command(self, command):
        with self.lock:
            self.command = command

    def _run(self):
        deadline = time.monotonic()
        last_sent = None
        try:
            while not self.done.is_set():
                with self.lock:
                    command = self.command
                sent = time.monotonic()
                if last_sent is not None:
                    self.intervals.append((sent - last_sent) * 1000)
                last_sent = sent
                result = self.client.request("/api/command", {"token": self.token, "velocity": list(command)})
                self.latencies.append((time.monotonic() - sent) * 1000)
                if result.get("accepted") is not True:
                    raise RuntimeError("Simulation did not accept the velocity command")
                deadline += COMMAND_PERIOD
                if deadline < time.monotonic():
                    deadline = time.monotonic()
                self.done.wait(max(0.0, deadline - time.monotonic()))
        except Exception as error:
            self.error = str(error)
            self.done.set()

    def close(self, timeout):
        self.done.set()
        self.thread.join(timeout=timeout + 1)
        if self.thread.is_alive():
            raise RuntimeError("Command request did not finish before cleanup")


def run(config: Config, *, client=None) -> dict:
    validate_url(config.url, config.allow_remote)
    for name in ("duration", "segment_seconds", "sample_hz", "timeout"):
        value = getattr(config, name)
        if not math.isfinite(value) or value <= 0:
            raise ValueError(f"{name} must be finite and positive")
    client = client or Client(config)
    report = {
        "schema_version": 1, "started_at": datetime.now(timezone.utc).isoformat(),
        "url": config.url, "requested_duration_seconds": config.duration,
        "command_hz_target": 1 / COMMAND_PERIOD, "command_timeout_seconds": 0.2,
        "segment_seconds": config.segment_seconds, "reset_requested": config.reset,
        "scope": "HTTP commands to the Go2 simulation; no ROS or hardware result is implied",
        "outcome": "failed", "errors": [], "cleanup_errors": [], "segments": [], "samples": [],
    }
    token = None
    pump = None
    initial = final = None
    try:
        health = client.request("/api/health")
        require_identity(health)
        if not health.get("ready"):
            raise RuntimeError("Simulation is not ready")
        if config.reset:
            client.request("/api/reset", {})
            ready_deadline = time.monotonic() + max(10, config.timeout * 5)
            while True:
                health = client.request("/api/status")
                require_identity(health)
                if health.get("error") or health.get("mode") in {"fallen", "fault", "paused", "damping"}:
                    raise RuntimeError("Simulation failed to recover after reset")
                if health.get("ready"):
                    break
                if time.monotonic() >= ready_deadline:
                    raise RuntimeError("Timed out waiting for reset simulation readiness")
                time.sleep(0.05)
        arm = client.request("/api/arm", {})
        token = arm.get("token")
        if not isinstance(token, str) or not token:
            raise ValueError("Simulation returned no control grant")
        epoch = arm.get("epoch")
        initial = telemetry(client.request("/api/status"), epoch)
        final = initial
        # Reject runtimes missing the cumulative counters before commanding.
        for name in ("wall_seconds", "camera_frames", "physics_steps", "policy_updates"):
            if name not in initial["metrics"]:
                raise ValueError(f"Runtime is missing benchmark counter {name}")
            value = initial["metrics"][name]
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
                raise ValueError(f"Runtime returned an invalid benchmark counter {name}")
        pump = CommandPump(client, token)
        pump.thread.start()
        started = time.monotonic()
        finished = started + config.duration
        next_progress = started + 60
        segment_index = 0
        report["samples"].append({"elapsed_seconds": 0.0, "segment": 0, **initial})
        while time.monotonic() < finished:
            name, command = SEQUENCE[segment_index % len(SEQUENCE)]
            pump.set_command(command)
            segment_end = min(finished, time.monotonic() + config.segment_seconds)
            samples = [final]
            while time.monotonic() < segment_end:
                if pump.done.wait(min(1 / config.sample_hz, max(0, segment_end - time.monotonic()))):
                    raise RuntimeError(f"Command stream failed: {pump.error or 'stopped'}")
                final = telemetry(client.request("/api/status"), epoch)
                samples.append(final)
                report["samples"].append({"elapsed_seconds": time.monotonic() - started,
                                          "segment": segment_index, **final})
                if time.monotonic() >= next_progress:
                    print(json.dumps({"event": "benchmark_progress", "elapsed_seconds": time.monotonic() - started,
                                      "mode": final["mode"], "position": final["position"]}), file=sys.stderr, flush=True)
                    next_progress += 60
            if len(samples) > 1:
                report["segments"].append(segment_result(name, command, samples))
            segment_index += 1
        if pump.error:
            raise RuntimeError(f"Command stream failed: {pump.error}")
        report["outcome"] = "completed"
    except KeyboardInterrupt:
        report["outcome"] = "interrupted"
        report["errors"].append("Benchmark interrupted")
    except Exception as error:
        report["errors"].append(str(error))
    finally:
        if pump is not None:
            try:
                pump.close(config.timeout)
            except Exception as error:
                report["cleanup_errors"].append(str(error))
            report["command_http_round_trip_ms"] = distribution(pump.latencies)
            report["command_send_interval_ms"] = distribution(pump.intervals)
            report["command_gaps_over_200ms"] = sum(gap >= 200 for gap in pump.intervals)
        if token:
            for endpoint in ("/api/stop", "/api/release"):
                try:
                    client.request(endpoint, {"token": token})
                except Exception as error:
                    report["cleanup_errors"].append(f"{endpoint}: {error}")
        if initial is not None and final is not None and final is not initial:
            try:
                report["measured_interval"] = measured_interval(initial, final)
            except ValueError as error:
                report["errors"].append(str(error))
        if final is not None:
            report["final_sample"] = final
            report["runtime_trailing_window_metrics"] = {
                key: value for key, value in final["metrics"].items() if key.endswith("_p95_ms")
            }
        rss = [s["metrics"]["rss_bytes"] for s in report["samples"] if "rss_bytes" in s["metrics"]]
        report["sampled_process_peak_rss_bytes"] = distribution(rss)
        report["rss_semantics"] = "Runtime rss_bytes is ru_maxrss: process lifetime peak, not current RSS"
        if "measured_interval" in report:
            interval = report["measured_interval"]
            command_p95 = final["metrics"].get("command_p95_ms")
            report["performance_targets"] = {
                "window_at_least_600_seconds": interval["wall_seconds"] >= 600,
                "real_time_factor_at_least_0_95": interval["real_time_factor"] >= .95,
                "policy_hz_at_least_47_5": interval["policy_hz"] >= 47.5,
                "runtime_trailing_command_p95_under_100ms": command_p95 is not None and command_p95 < 100,
                "no_command_refresh_gaps_over_200ms": report.get("command_gaps_over_200ms", 0) == 0,
            }
            report["target_notes"] = (
                "Camera FPS measures the simulated robot camera; browser rendering runs independently "
                "and is not measured by this HTTP benchmark. Runtime latency p95 may include "
                "pre-run samples; HTTP latency statistics contain only this run. A short smoke run and "
                "a run without representative guest application load do not complete the VM acceptance gate."
            )
        if report["errors"] or report["cleanup_errors"]:
            if report["outcome"] == "completed":
                report["outcome"] = "failed"
        report["finished_at"] = datetime.now(timezone.utc).isoformat()
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=Config.url)
    parser.add_argument("--duration", type=float, default=Config.duration)
    parser.add_argument("--segment-seconds", type=float, default=Config.segment_seconds)
    parser.add_argument("--sample-hz", type=float, default=Config.sample_hz)
    parser.add_argument("--timeout", type=float, default=Config.timeout)
    parser.add_argument("--reset", action="store_true", help="Explicitly reset the world before acquiring control")
    parser.add_argument("--allow-remote", action="store_true", help="Permit a non-loopback URL after simulation identity validation")
    parser.add_argument("--output", type=Path, required=True, help="Destination JSON report")
    args = parser.parse_args()
    try:
        report = run(Config(**{key: value for key, value in vars(args).items() if key != "output"}))
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(report, indent=2, allow_nan=False) + "\n")
    except (OSError, ValueError) as error:
        print(f"Benchmark failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps({"outcome": report["outcome"], "output": str(args.output.resolve()),
                      "measured_interval": report.get("measured_interval"), "errors": report["errors"],
                      "cleanup_errors": report["cleanup_errors"]}))
    return 0 if report["outcome"] == "completed" else 1


if __name__ == "__main__":
    sys.exit(main())
