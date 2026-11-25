"""Independent synthetic MuJoCo actuator, for isolated simulation only."""

from __future__ import annotations

import argparse
import hmac
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ipaddress
import json
import math
from pathlib import Path
import random
import stat
import threading
import time
import uuid

from sim.run import PackageError, compile_model, gas_value, load_package, number


class Rejected(ValueError):
    def __init__(self, message, status=400):
        super().__init__(message)
        self.status = status


class Actuator:
    """Physics and watchdog run independently of the command producer."""

    def __init__(self, package, *, allow_test_faults=False):
        import mujoco

        self.mj = mujoco
        self.package = package
        self.model = compile_model(package, mujoco)
        self.data = mujoco.MjData(self.model)
        self.body = mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_BODY, package["mujoco"]["body"])
        self.actuator = mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_ACTUATOR, package["mujoco"]["actuator"])
        if (self.body <= 0 or self.actuator < 0 or self.model.nv != 1 or self.model.nu != 1
                or package["mujoco"].get("control_mode") != "velocity"):
            raise PackageError("actuator requires the one-axis mission fixture")
        if not 0 < self.model.opt.timestep <= .02:
            raise PackageError("actuator timestep must be at most 20ms")
        self.lock = threading.Lock()
        self.done = threading.Event()
        self.boot_id = str(uuid.uuid4())
        self.session = str(uuid.uuid4())
        self.sequence = 0
        self.deadline = 0.0
        self.command_not_after_ms = 0
        self.bounded_clock_fault = False
        self.bounded_last_wall_ms = int(time.time() * 1000)
        self.bounded_last_mono = time.monotonic()
        self.velocity = 0.0
        self.watchdog_stopped = True
        self.sensors = {"gas": True, "local-obstacles": True}
        self.allow_test_faults = allow_test_faults
        self.sample_ms = int(time.time() * 1000)
        self.thread = None
        self.mj.mj_forward(self.model, self.data)

    def start(self):
        self.thread = threading.Thread(target=self._run, name="mujoco-physics", daemon=True)
        self.thread.start()

    def close(self):
        self.done.set()
        if self.thread:
            self.thread.join(timeout=2)
        with self.lock:
            self._brake()

    def _brake(self):
        # An ideal simulated brake directly removes velocity. This does not model
        # physical braking distance, contact safety, or a real robot controller.
        self.velocity = 0.0
        self.deadline = 0.0
        self.command_not_after_ms = 0
        self.data.ctrl[self.actuator] = 0
        self.data.qvel[:] = 0
        self.watchdog_stopped = True
        self.mj.mj_forward(self.model, self.data)

    def _bounded_clock(self):
        # Wall time is an extra simulation-only deadline constraint. Monotonic
        # time still limits holdover; clock rollback latches bounded commands
        # off until process restart, without changing autonomous command rules.
        mono = time.monotonic()
        wall_ms = int(time.time() * 1000)
        if wall_ms < self.bounded_last_wall_ms or mono < self.bounded_last_mono:
            self.bounded_clock_fault = True
        self.bounded_last_wall_ms = wall_ms
        self.bounded_last_mono = mono
        return mono, wall_ms

    def _run(self):
        while not self.done.is_set():
            started = time.monotonic()
            with self.lock:
                clock_mono, clock_wall = self._bounded_clock()
                bounded_expired = self.command_not_after_ms and (
                    self.bounded_clock_fault or clock_wall >= self.command_not_after_ms)
                if clock_mono >= self.deadline or bounded_expired or not all(self.sensors.values()):
                    self._brake()
                self.data.ctrl[self.actuator] = self.velocity
                self.mj.mj_step(self.model, self.data)
                self.mj.mj_forward(self.model, self.data)
                if not all(math.isfinite(float(v)) for v in (*self.data.qpos, *self.data.qvel)):
                    self.done.set()
                    self._brake()
                    break
                self.sample_ms = int(time.time() * 1000)
            # Never replay elapsed wall time as catch-up motion after a stall.
            self.done.wait(max(0, float(self.model.opt.timestep) - (time.monotonic() - started)))

    def _state(self):
        x, y, z = map(float, self.data.xpos[self.body])
        position = [x, z, -y]
        return {
            "schema_version": "wendy.sim.actuator.v0", "synthetic": True,
            "boot_id": self.boot_id, "session": self.session, "sequence": self.sequence,
            "site_id": self.package["site_id"],
            "frame": {"id": "office-floor", "revision": self.package["frame_revision"]},
            "position_m": position, "velocity_mps": [float(self.data.qvel[0]), 0., 0.],
            "sample_unix_ms": self.sample_ms, "simulation_time_s": float(self.data.time),
            "sensors": dict(self.sensors), "obstacle_clear": self.sensors["local-obstacles"],
            "gas_ppm": gas_value(self.package["gas_model"], position, random.Random(0)),
            "energy": 1.0, "commanded_velocity_mps": self.velocity,
            "lease_remaining_ms": max(0, int((self.deadline - time.monotonic()) * 1000)),
            "watchdog_stopped": self.watchdog_stopped,
            "provenance": {"mode": "simulated", "package_sha256": self.package["_digest"],
                           "robot_model": "single-axis-puck-ideal-brake", "gas_model": "analytic-gaussian-fixture"},
        }

    def state(self):
        with self.lock:
            return self._state()

    def request(self, path, body):
        if not isinstance(body, dict):
            raise Rejected("body must be an object")
        expected = {
            "/v1/session": set(), "/v1/command": {"session", "sequence", "velocity_mps", "lease_ms"},
            "/v1/command/bounded": {"session", "sequence", "velocity_mps", "lease_ms", "not_after_unix_ms"},
            "/v1/stop": {"session", "sequence"}, "/v1/fault": {"sensor", "available"},
        }
        if path not in expected:
            raise Rejected("unknown endpoint", 404)
        if set(body) != expected[path]:
            raise Rejected("missing or unknown fields")
        with self.lock:
            if path == "/v1/session":
                self._brake()
                self.session = str(uuid.uuid4())
                self.sequence = 0
            elif path == "/v1/fault":
                if not self.allow_test_faults:
                    raise Rejected("test faults are disabled", 403)
                if (not isinstance(body["sensor"], str) or body["sensor"] not in self.sensors
                        or type(body["available"]) is not bool):
                    raise Rejected("invalid sensor fault")
                self.sensors[body["sensor"]] = body["available"]
                if not body["available"]:
                    self._brake()
            else:
                if body["session"] != self.session:
                    raise Rejected("stale writer session", 409)
                seq = body["sequence"]
                if type(seq) is not int or not self.sequence < seq <= 2**53 - 1:
                    raise Rejected("sequence must strictly increase", 409)
                if path == "/v1/stop":
                    self._brake()
                else:
                    try:
                        velocity = number(body["velocity_mps"], "velocity_mps", -.5, .5)
                    except PackageError as exc:
                        raise Rejected(str(exc)) from exc
                    lease = body["lease_ms"]
                    if type(lease) is not int or not 1 <= lease <= 500:
                        raise Rejected("lease_ms must be an integer between 1 and 500")
                    if not all(self.sensors.values()) or self.done.is_set():
                        raise Rejected("local sensors or physics unavailable", 409)
                    command_mono = time.monotonic()
                    not_after_ms = 0
                    if path == "/v1/command/bounded":
                        command_mono, command_wall = self._bounded_clock()
                        not_after_ms = body["not_after_unix_ms"]
                        if (type(not_after_ms) is not int or self.bounded_clock_fault
                                or not 0 < not_after_ms - command_wall <= 500):
                            raise Rejected("bounded command deadline expired or clock invalid", 409)
                        lease = min(lease, not_after_ms - command_wall)
                    self.velocity = float(velocity)
                    self.deadline = command_mono + lease / 1000
                    self.command_not_after_ms = not_after_ms
                    self.watchdog_stopped = False
                self.sequence = seq
            return self._state()


def read_token(path):
    path = Path(path)
    metadata = path.stat()
    if not stat.S_ISREG(metadata.st_mode) or metadata.st_mode & 0o077:
        raise PackageError("token file must be a private regular file, mode 0600 or 0400")
    with path.open("rb") as source:
        token = source.read(257)
    if len(token) > 256:
        raise PackageError("token exceeds 256 bytes")
    token = token.decode("ascii").strip()
    if len(token) < 32 or not all(c.isalnum() or c in "-_" for c in token):
        raise PackageError("token must have 32-256 ASCII letters, digits, hyphens or underscores")
    return token


def make_server(address, actuator, token):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format, *args):
            pass

        def setup(self):
            super().setup()
            self.connection.settimeout(2)

        def respond(self, status, body):
            raw = json.dumps(body, allow_nan=False, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(raw)

        def authorized(self):
            supplied = self.headers.get("Authorization", "")
            if not hmac.compare_digest(supplied.encode(), ("Bearer " + token).encode()):
                self.respond(401, {"error": "unauthorized"})
                return False
            return True

        def do_GET(self):
            if self.authorized():
                if self.path != "/v1/state":
                    self.respond(404, {"error": "unknown endpoint"})
                else:
                    self.respond(200, actuator.state())

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
                    raise Rejected("non-finite JSON number")
                def unique_fields(pairs):
                    result = {}
                    for key, value in pairs:
                        if key in result:
                            raise Rejected("duplicate JSON field")
                        result[key] = value
                    return result
                body = json.loads(self.rfile.read(length), parse_constant=reject_constant,
                                  object_pairs_hook=unique_fields)
                self.respond(200, actuator.request(self.path, body))
            except Rejected as exc:
                self.respond(exc.status, {"error": str(exc)})
            except (ValueError, TypeError, UnicodeError):
                self.respond(400, {"error": "invalid JSON request"})

    return ThreadingHTTPServer(address, Handler)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("package", type=Path)
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=8091)
    parser.add_argument("--allow-network-simulation", action="store_true")
    parser.add_argument("--allow-test-faults", action="store_true")
    args = parser.parse_args(argv)
    try:
        if not ipaddress.ip_address(args.host).is_loopback and not args.allow_network_simulation:
            raise PackageError("non-loopback binding requires --allow-network-simulation on an isolated test network")
        token = read_token(args.token_file)
        actuator = Actuator(load_package(args.package), allow_test_faults=args.allow_test_faults)
        server = make_server((args.host, args.port), actuator, token)
    except (PackageError, ValueError, OSError) as exc:
        parser.error(str(exc))
    actuator.start()
    print(json.dumps({"adapter": "synthetic-mujoco", "listen": list(server.server_address)}), flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        actuator.close()
        server.server_close()


if __name__ == "__main__":
    main()
