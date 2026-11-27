"""One live MuJoCo scene for robot actuation and bounded multimodal capture.

Only the Actuator thread steps physics. Rendering copies that state and never
steps its own trajectory. A slow renderer cannot hold the command watchdog.
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
import json
import math
from pathlib import Path
import random
import threading
import time

from sim.actuator import Actuator, Rejected, read_token
from sim.run import PackageError, gas_value, load_package
from sim.sensors import HEIGHT, WIDTH, audio_wav, calibration, parameters, png_rgb

MAX_SNAPSHOT_BYTES = 1 << 20
MAX_SNAPSHOT_AGE = 1.0


def live_package(path):
    package = load_package(path)
    expected = {"cameras": ["camera-a", "camera-b"], "width": WIDTH, "height": HEIGHT,
                "gas_binding": "robot", "audio_binding": "robot", "audio_rate_hz": 8000,
                "audio_duration_ms": 250, "alarm_frequency_hz": 880,
                "calibration_revision": "synthetic-live-sensors-v1"}
    if package.get("live_sensors") != expected:
        raise PackageError("unsupported live sensor bindings")
    return package


class LiveScene:
    def __init__(self, package, *, run_id="live-run", seed=7, interval_ms=200, allow_test_faults=False,
                 clock_domain=None, clock_anchor_unix_ns=None):
        parameters(1, interval_ms, seed, run_id, True, ())
        if interval_ms < 100:
            raise PackageError("live rendering interval must be at least 100ms")
        if ((clock_domain is None) != (clock_anchor_unix_ns is None)
                or clock_domain is not None and (clock_domain != "simulation:" + run_id
                    or type(clock_anchor_unix_ns) is not int or not 0 < clock_anchor_unix_ns < 2**63)):
            raise PackageError("clock requires its run domain and a fixed positive nanosecond anchor")
        self.actuator = Actuator(package, allow_test_faults=allow_test_faults)
        self.package, self.run_id, self.seed = package, run_id, seed
        self.interval = interval_ms / 1000
        self.allow_test_faults = allow_test_faults
        self.done = threading.Event()
        self.render_paused = threading.Event()
        self.cache_lock = threading.Lock()
        self.latest = None
        self.capture_monotonic = 0.0
        self.render_error = None
        self.thread = None
        self.sequence = 0
        self.last_published_sim_ns = -1
        self.clock_identity = None if clock_domain is None else {
            "schema_version": "wendy.sim.clock.v1", "run_id": run_id,
            "boot_id": self.actuator.boot_id, "package_sha256": package["_digest"],
            "frame": {"id": "office-floor", "revision": package["frame_revision"]},
            "clock_domain": clock_domain, "anchor_unix_ns": clock_anchor_unix_ns,
        }
        self.clock_sequence = 0
        self.clock_last_sim_ns = -1
        self.clock_last_capture_ns = 0
        self.clock_fault = False

    def start(self):
        self.actuator.start()
        self.thread = threading.Thread(target=self._render_loop, name="live-render", daemon=True)
        self.thread.start()

    def close(self):
        self.done.set()
        # Stop physics before waiting for a renderer which could have stalled.
        self.actuator.close()
        if self.thread:
            self.thread.join(timeout=3)

    def _copy_state(self, target):
        with self.actuator.lock:
            if self.actuator.thread is None or not self.actuator.thread.is_alive() or self.actuator.done.is_set():
                raise PackageError("live physics is unavailable")
            # Copy only the independent simulation state used by this one-axis
            # model. mj_forward computes camera transforms outside this lock.
            target.qpos[:] = self.actuator.data.qpos
            target.qvel[:] = self.actuator.data.qvel
            target.ctrl[:] = self.actuator.data.ctrl
            target.time = self.actuator.data.time
            state = self.actuator._state()
            # This is the wall instant of the coherent state copy, distinct
            # from simulation-relative time and the last physics-step instant.
            state["sample_unix_ms"] = int(time.time() * 1000)
            captured = time.monotonic()
        self.actuator.mj.mj_forward(self.actuator.model, target)
        return state, captured

    def _snapshot(self, renderer, data):
        state, captured = self._copy_state(data)
        sample_ns = round(state["simulation_time_s"] * 1_000_000_000)
        if sample_ns < self.last_published_sim_ns:
            raise PackageError("live physics clock regressed")
        # Rendering and physics run independently. Under load the render
        # thread can copy the same physics step again. Do not assign a new
        # sequence or refresh the cache's capture time for that old state.
        if sample_ns == self.last_published_sim_ns:
            return None, captured
        if sample_ns < 250_000_000:
            return None, captured
        sequence = self.sequence + 1
        observations, media = [], []
        for sensor_id in ("camera-a", "camera-b", "gas-1", "audio-1"):
            if sensor_id == "gas-1" and not state["sensors"]["gas"]:
                continue
            kind = "camera" if sensor_id.startswith("camera") else "gas" if sensor_id == "gas-1" else "audio"
            sample = {"schema_version": "wendy.sensor.sample.v1", "id": f"{self.run_id}/{sensor_id}/{sequence}",
                      "site_id": self.package["site_id"], "frame": dict(state["frame"]),
                      "sensor_id": sensor_id, "kind": kind, "sequence": sequence, "sample_ns": sample_ns,
                      "run_id": self.run_id, "scenario_version": self.package["package_revision"], "seed": self.seed,
                      "package_sha256": self.package["_digest"], "uncertainty_m": .01,
                      "calibration_revision": self.package["live_sensors"]["calibration_revision"]}
            blob = None
            if kind == "camera":
                sample["sensor_model"] = f"mujoco-{self.actuator.mj.__version__}-live-rgb-pinhole"
                camera_id = self.actuator.mj.mj_name2id(self.actuator.model, self.actuator.mj.mjtObj.mjOBJ_CAMERA, sensor_id)
                if camera_id < 0:
                    raise PackageError("live camera binding missing")
                sample["position_m"], sample["camera"] = calibration(self.actuator.model, data, camera_id)
                renderer.update_scene(data, camera=sensor_id)
                blob, content_type = png_rgb(renderer.render()), "image/png"
            elif kind == "gas":
                sample["sensor_model"] = "analytic-gaussian-fixture-v1"
                sample["position_m"] = list(state["position_m"])
                sample["gas"] = {"value_ppm": gas_value(self.package["gas_model"], state["position_m"], random.Random((self.seed << 32) | sequence)),
                                 "uncertainty_ppm": self.package["gas_model"]["noise_stddev_ppm"], "species": "synthetic-test-gas"}
            else:
                sample["sensor_model"] = "generated-880hz-tone-v1"
                sample["position_m"] = list(state["position_m"])
                sample["audio"] = {"sample_rate_hz": 8000, "channels": 1, "duration_ns": 250_000_000}
                blob, content_type = audio_wav(self.seed, sequence, True), "audio/wav"
            if blob is not None:
                digest = hashlib.sha256(blob).hexdigest()
                sample["evidence"] = {"sha256": digest, "content_type": content_type, "size": len(blob)}
                media.append({"sha256": digest, "data": base64.b64encode(blob).decode("ascii")})
            observations.append(sample)
        snapshot = {"schema_version": "wendy.sim.live.v1", "run_id": self.run_id,
                    "boot_id": state["boot_id"], "sequence": sequence, "sample_unix_ms": state["sample_unix_ms"],
                    "simulation_time_ns": sample_ns, "state": state, "observations": observations, "media": media}
        return snapshot, captured

    def _render_loop(self):
        try:
            data = self.actuator.mj.MjData(self.actuator.model)
            # GL context construction, rendering and teardown stay on this thread.
            with self.actuator.mj.Renderer(self.actuator.model, height=HEIGHT, width=WIDTH) as renderer:
                while not self.done.is_set():
                    started = time.monotonic()
                    if self.render_paused.is_set():
                        self.done.wait(.02)
                        continue
                    snapshot, captured = self._snapshot(renderer, data)
                    if snapshot is not None:
                        raw = json.dumps(snapshot, allow_nan=False, separators=(",", ":")).encode()
                        if len(raw) > MAX_SNAPSHOT_BYTES:
                            raise PackageError("live snapshot exceeds 1MiB")
                        # A fault which arrived during rendering invalidates the
                        # copied prerequisites; do not publish the older state.
                        with self.actuator.lock:
                            valid = snapshot["state"]["sensors"] == self.actuator.sensors
                            if valid and time.monotonic() - captured <= MAX_SNAPSHOT_AGE and not self.actuator.done.is_set():
                                with self.cache_lock:
                                    self.latest, self.capture_monotonic = raw, captured
                                    self.sequence = snapshot["sequence"]
                                    self.last_published_sim_ns = snapshot["simulation_time_ns"]
                    self.done.wait(max(0, self.interval - (time.monotonic() - started)))
        except Exception as exc:
            with self.cache_lock:
                self.render_error = str(exc)
                self.latest = None

    def samples(self):
        with self.cache_lock:
            if self.actuator.done.is_set() or self.latest is None or time.monotonic() - self.capture_monotonic > MAX_SNAPSHOT_AGE:
                raise Rejected("live capture unavailable or stale", 503)
            return self.latest

    def control_state(self):
        # Cheap state reads carry immutable scene identity without requiring a
        # rendered snapshot. Older clients permit extra diagnostic fields.
        return {**self.actuator.state(), "run_id": self.run_id}

    def source_clock(self, nonce):
        # This is a read-only observation of source time, not an actuator lease
        # or evidence that a rendered sample is fresh. A paused scene can keep
        # the same source instant while these nonce-bound observations advance.
        if not isinstance(nonce, str) or len(nonce) != 64 or any(c not in "0123456789abcdef" for c in nonce):
            raise Rejected("a 32-byte hexadecimal clock nonce is required")
        with self.actuator.lock:
            identity = self.clock_identity
            if identity is None or self.clock_fault:
                raise Rejected("source clock not commissioned or faulted", 503)
            if self.actuator.thread is None or not self.actuator.thread.is_alive() or self.actuator.done.is_set():
                raise Rejected("source physics unavailable", 503)
            seconds = float(self.actuator.data.time)
            captured = time.time_ns()
            if (not math.isfinite(seconds) or seconds < 0 or seconds >= (2**63 - identity["anchor_unix_ns"]) / 1e9
                    or captured <= 0 or self.clock_sequence >= 2**53 - 1
                    or self.actuator.boot_id != identity["boot_id"] or self.run_id != identity["run_id"]
                    or self.package["_digest"] != identity["package_sha256"]
                    or self.package["frame_revision"] != identity["frame"]["revision"]):
                self.clock_fault = True
                raise Rejected("source clock identity or range fault", 503)
            sim_ns = round(seconds * 1_000_000_000)
            if (sim_ns > 2**63 - 1 - identity["anchor_unix_ns"]
                    or sim_ns < self.clock_last_sim_ns or captured < self.clock_last_capture_ns):
                self.clock_fault = True
                raise Rejected("source clock regressed", 503)
            self.clock_sequence += 1
            self.clock_last_sim_ns, self.clock_last_capture_ns = sim_ns, captured
            return {**identity, "frame": dict(identity["frame"]), "simulation_time_ns": sim_ns,
                    "observed_unix_ns": captured, "observation_sequence": self.clock_sequence, "nonce": nonce}

    def request(self, path, body):
        if path == "/v1/render-pause":
            if not self.allow_test_faults:
                raise Rejected("test faults are disabled", 403)
            if not isinstance(body, dict) or set(body) != {"paused"} or type(body["paused"]) is not bool:
                raise Rejected("invalid render fault")
            if body["paused"]:
                self.render_paused.set()
            else:
                self.render_paused.clear()
            return {"paused": self.render_paused.is_set()}
        result = self.actuator.request(path, body)
        if path == "/v1/fault":
            with self.cache_lock:
                self.latest = None
        return result


def make_server(address, scene, actuator_token, sensor_token):
    if actuator_token == sensor_token:
        raise PackageError("sensor and actuator tokens must differ")

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            pass

        def setup(self):
            super().setup()
            self.connection.settimeout(2)

        def respond(self, status, body):
            raw = body if isinstance(body, bytes) else json.dumps(body, allow_nan=False, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(raw)

        def authorized(self, read=False):
            supplied = self.headers.get("Authorization", "").encode()
            act = hmac.compare_digest(supplied, ("Bearer " + actuator_token).encode())
            sensor = hmac.compare_digest(supplied, ("Bearer " + sensor_token).encode())
            if not act and not (read and sensor):
                self.respond(401, {"error": "unauthorized"})
                return False
            return True

        def do_GET(self):
            if not self.authorized(self.path in ("/v1/samples", "/v1/clock")):
                return
            try:
                if self.path == "/v1/samples":
                    self.respond(200, scene.samples())
                elif self.path == "/v1/state":
                    self.respond(200, scene.control_state())
                elif self.path == "/v1/clock":
                    self.respond(200, scene.source_clock(self.headers.get("X-Wendy-Clock-Nonce")))
                else:
                    self.respond(404, {"error": "unknown endpoint"})
            except Rejected as exc:
                self.respond(exc.status, {"error": str(exc)})

        def do_POST(self):
            if not self.authorized():
                return
            try:
                if self.headers.get("Transfer-Encoding"):
                    raise Rejected("chunked bodies are unsupported")
                length = int(self.headers.get("Content-Length", "-1"))
                if not 0 < length <= 2048:
                    raise Rejected("body must contain 1-2048 bytes", 413)
                def reject_constant(value):
                    raise Rejected("nonfinite JSON number")
                def unique_fields(pairs):
                    obj = {}
                    for key, value in pairs:
                        if key in obj:
                            raise Rejected("duplicate JSON field")
                        obj[key] = value
                    return obj
                body = json.loads(self.rfile.read(length), parse_constant=reject_constant, object_pairs_hook=unique_fields)
                self.respond(200, scene.request(self.path, body))
            except Rejected as exc:
                self.respond(exc.status, {"error": str(exc)})
            except (ValueError, TypeError, UnicodeError):
                self.respond(400, {"error": "invalid JSON request"})

    return ThreadingHTTPServer(address, Handler)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path)
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--sensor-token-file", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--seed", type=int, default=7)
    parser.add_argument("--interval-ms", type=int, default=200)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8091)
    parser.add_argument("--allow-network-simulation", action="store_true")
    parser.add_argument("--allow-test-faults", action="store_true")
    parser.add_argument("--clock-domain", help="explicit simulation:<run-id> clock domain")
    parser.add_argument("--clock-anchor-unix-ns", type=int, help="fixed commissioned simulation epoch; never inferred from wall time")
    args = parser.parse_args(argv)
    try:
        if not ipaddress.ip_address(args.host).is_loopback and not args.allow_network_simulation:
            raise PackageError("non-loopback binding requires --allow-network-simulation")
        scene = LiveScene(live_package(args.package), run_id=args.run_id, seed=args.seed, interval_ms=args.interval_ms,
                          allow_test_faults=args.allow_test_faults, clock_domain=args.clock_domain,
                          clock_anchor_unix_ns=args.clock_anchor_unix_ns)
        server = make_server((args.host, args.port), scene, read_token(args.token_file), read_token(args.sensor_token_file))
    except (PackageError, ValueError, OSError) as exc:
        parser.error(str(exc))
    scene.start()
    print(json.dumps({"adapter": "synthetic-live-mujoco", "run_id": args.run_id, "listen": list(server.server_address)}), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        scene.close()
        server.server_close()


if __name__ == "__main__":
    main()
