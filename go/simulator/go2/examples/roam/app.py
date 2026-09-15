#!/usr/bin/env python3
"""Local Go2 roaming sample. Uses public HTTP observations and leased commands.

Run ros_app.py instead when deploying through Wendy to a ROS-enabled simulator.
Neither transport imports the simulator or modifies its physics state.
"""

import argparse
import json
import math
from pathlib import Path
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.error import HTTPError, URLError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

from controller import RoamController


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, *_args, **_kwargs):
        return None


class SimulatorClient:
    def __init__(self, url, timeout=0.25):
        parsed = urlsplit(url)
        if (parsed.scheme != "http" or parsed.hostname not in {"localhost", "127.0.0.1", "::1"}
                or parsed.username or parsed.password or parsed.path not in {"", "/"}
                or parsed.query or parsed.fragment):
            raise ValueError("Use a loopback simulator URL, such as http://127.0.0.1:8898")
        _ = parsed.port  # Validate the port before any request.
        self.url = url.rstrip("/")
        self.timeout = timeout
        self.opener = build_opener(ProxyHandler({}), NoRedirects())

    def request(self, path, body=None):
        payload = None if body is None else json.dumps(body, allow_nan=False).encode()
        request = Request(self.url + path, data=payload,
                          headers={"Content-Type": "application/json"})
        try:
            with self.opener.open(request, timeout=self.timeout) as response:
                raw = response.read(1_048_577)
                if len(raw) > 1_048_576:
                    raise RuntimeError("Simulator response is too large")
                result = json.loads(raw)
                if not isinstance(result, dict):
                    raise RuntimeError("Simulator response must be an object")
                return result
        except HTTPError as error:
            try:
                detail = json.loads(error.read(4096)).get("error", str(error.code))
            except (ValueError, AttributeError):
                detail = str(error.code)
            raise RuntimeError(f"Simulator rejected the request: {detail}") from error
        except (URLError, TimeoutError, OSError, ValueError) as error:
            raise RuntimeError(f"Simulator connection failed: {error}") from error

    def get(self, path):
        return self.request(path)

    def post(self, path, body=None):
        return self.request(path, {} if body is None else body)


def finite_vector(value, size):
    if (not isinstance(value, list) or len(value) != size or
            any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in value)):
        raise RuntimeError("Invalid simulator observation")
    return value


def horizontal_scan(sample):
    """Recover the real horizontal ring from world-space cloud points.

    The virtual Go2 has 360 azimuth samples, five elevation rings and a 12m
    range. Undo the captured lidar mounting pose; ground hits from the other
    rings must not be mistaken for obstacles. Missing rays stay unknown.
    """
    origin = finite_vector(sample.get("origin"), 3)
    x, y, z, w = finite_vector(sample.get("quaternion"), 4)
    norm = math.sqrt(x*x + y*y + z*z + w*w)
    if abs(norm - 1) > 0.01:
        raise RuntimeError("Invalid lidar orientation")
    x, y, z, w = (v / norm for v in (x, y, z, w))
    points = sample.get("points")
    if not isinstance(points, list) or len(points) > 5400 or len(points) % 3:
        raise RuntimeError("Invalid lidar point cloud")
    ranges = [math.inf] * 360
    increment = math.tau / 360
    for i in range(0, len(points), 3):
        point = finite_vector(points[i:i+3], 3)
        dx, dy, dz = (a - b for a, b in zip(point, origin))
        lx = (1-2*(y*y+z*z))*dx + 2*(x*y+w*z)*dy + 2*(x*z-w*y)*dz
        ly = 2*(x*y-w*z)*dx + (1-2*(x*x+z*z))*dy + 2*(y*z+w*x)*dz
        lz = 2*(x*z+w*y)*dx + 2*(y*z-w*x)*dy + (1-2*(x*x+y*y))*dz
        distance = math.hypot(lx, ly)
        if not 0.1 <= distance <= 12 or abs(lz) > max(0.001, distance * 0.015):
            continue
        index = round((math.atan2(ly, lx) + math.pi) / increment) % 360
        ranges[index] = min(ranges[index], distance)
    return ranges


class RoamApp:
    def __init__(self, client, *, period=0.05, sensor_period=0.1):
        self.client = client
        self.period, self.sensor_period = period, sensor_period
        self.lock = threading.RLock()
        self.controller = RoamController()
        self._token = None
        self._identity = None
        self._epoch = None
        self.last_pose_time = self.last_scan_time = None
        self.active = False
        self.error = None
        self.started = None
        self.distance = 0.0
        self.last_position = None
        self.closed = threading.Event()
        self.worker = threading.Thread(target=self._loop, name="go2-roamer", daemon=True)
        self.worker.start()

    def _check_status(self, state):
        if (state.get("simulation") is not True or state.get("robot_kind") != "go2"
                or state.get("profile_version") != 1):
            raise RuntimeError("This sample only controls the virtual Go2 simulator")
        identity = (state.get("source_digest"), state.get("policy_bundle"), state.get("world"))
        if self._identity is not None and identity != self._identity:
            raise RuntimeError("Simulator changed. Start again to grant control to the new runtime.")
        if (state.get("healthy") is not True or state.get("ready") is not True
                or state.get("mode") not in {"standing", "moving"}):
            raise RuntimeError(f"Simulator is {state.get('mode', 'unavailable')}. Resume or reset it before starting.")
        epoch = state.get("epoch")
        if isinstance(epoch, bool) or not isinstance(epoch, int):
            raise RuntimeError("Simulator has no valid world epoch")
        if self._epoch is not None and epoch != self._epoch:
            raise RuntimeError("World reset. Start again to grant control for the new world.")
        return identity, epoch

    def _observe(self, state=None):
        received = time.monotonic()
        state = state or self.client.get("/api/status")
        identity, epoch = self._check_status(state)
        if self.active and state.get("armed") is not True:
            raise RuntimeError("Robot control was released. Start again to navigate.")
        sample = self.client.get("/api/scene/lidar")
        if (sample.get("epoch") != epoch or sample.get("enabled") is not True
                or sample.get("available") is not True or sample.get("fresh") is not True
                or sample.get("mode") not in {"standing", "moving"}):
            raise RuntimeError("Lidar is disabled, paused or stale. Restore fresh scans and press Start.")
        scan_time = sample.get("time")
        pose_time = state.get("time")
        age = sample.get("age_ms")
        physics_age = state.get("metrics", {}).get("physics_age_ms")
        for value in (scan_time, pose_time, age, physics_age):
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
                raise RuntimeError("Missing observation timestamps")
        if not 0 <= age < 300 or not 0 <= physics_age < 300:
            raise RuntimeError("Robot observations are stale. Press Start after they recover.")
        if ((self.last_pose_time is not None and pose_time < self.last_pose_time)
                or (self.last_scan_time is not None and scan_time < self.last_scan_time)):
            raise RuntimeError("Observation clock restarted. Press Start to begin a new session.")
        if self.last_pose_time is None or pose_time > self.last_pose_time:
            position = finite_vector(state.get("position"), 3)
            w, x, y, z = finite_vector(state.get("quaternion_wxyz"), 4)
            heading = math.atan2(2*(w*z+x*y), 1-2*(y*y+z*z))
            if not self.controller.observe_pose(*position[:2], heading, received - physics_age/1000):
                raise RuntimeError("Invalid robot pose")
            if self.last_position is not None:
                self.distance += math.dist(position[:2], self.last_position)
            self.last_position = position[:2]
            self.last_pose_time = pose_time
        if self.last_scan_time is None or scan_time > self.last_scan_time:
            accepted = self.controller.observe_scan(horizontal_scan(sample), angle_min=-math.pi,
                angle_increment=math.tau/360, range_min=0.1, range_max=12.0, now=received-age/1000)
            if not accepted:
                raise RuntimeError("Lidar scan is unusable. Restore sensor returns and press Start.")
            self.last_scan_time = scan_time
        return identity, epoch

    def start(self):
        with self.lock:
            if self.closed.is_set():
                raise RuntimeError("Sample app is closed")
            if self.active:
                return self.status()
            self.error = None
            self._identity = self._epoch = None
            self.last_scan_time = self.last_pose_time = None
            self.last_position = None
            self.distance = 0.0
            self.controller = RoamController()
            state = self.client.get("/api/status")
            self._check_status(state)
            if state.get("armed"):
                raise RuntimeError("Another controller owns the robot. Release controls in its original sandbox tab, or Pause then Resume to transfer control, then Start here.")
            identity, epoch = self._observe(state)
            if not self.controller.start(time.monotonic()):
                raise RuntimeError("Waiting for enough fresh lidar returns to navigate")
            try:
                grant = self.client.post("/api/arm")
            except Exception:
                self.controller.stop("control_not_granted")
                raise
            token = grant.get("token")
            if not isinstance(token, str) or not token:
                self.controller.stop("invalid_grant")
                raise RuntimeError("Simulator returned an invalid control grant")
            self._token = token
            if grant.get("epoch") != epoch:
                self._stop("world_changed")
                raise RuntimeError("World changed while acquiring control. Press Start again.")
            self._identity, self._epoch = identity, epoch
            self.active = True
            self.started = time.monotonic()
            return self.status()

    def _stop(self, reason):
        self.active = False
        self.controller.stop(reason)
        token, self._token = self._token, None
        if token:
            # Zero immediately, then give ownership back. The simulator's own
            # 200ms lease also stops motion if either request cannot arrive.
            for path, body in (("/api/command", {"token": token, "velocity": [0, 0, 0]}),
                               ("/api/release", {"token": token})):
                try:
                    self.client.post(path, body)
                except Exception as error:
                    if self.error is None:
                        self.error = str(error)
                    if path == "/api/release":
                        self.error += " If the disconnected app still owns the robot, pause then resume the sandbox to release it."

    def stop(self, reason="stopped"):
        with self.lock:
            self._stop(reason)
            return self.status()

    def status(self):
        with self.lock:
            return {"active": self.active, "error": self.error, "simulator_url": self.client.url,
                    "distance_m": round(self.distance, 2),
                    "elapsed_seconds": round(time.monotonic()-self.started, 1) if self.active and self.started else 0,
                    "controller": self.controller.status(time.monotonic())}

    def _loop(self):
        next_sensor = 0.0
        while not self.closed.is_set():
            started = time.monotonic()
            with self.lock:
                if self.active:
                    try:
                        if started >= next_sensor:
                            self._observe()
                            next_sensor = started + self.sensor_period
                        observation = self.controller.status(time.monotonic())
                        if observation["pose_age"] is None or observation["pose_age"] >= 0.35:
                            self.controller.stop("stale_pose")
                        linear, angular = self.controller.tick(time.monotonic())
                        # Controller freshness failures latch a stop; never
                        # obtain a new grant or resume an old task implicitly.
                        if not self.controller.status(time.monotonic())["active"]:
                            self._stop(self.controller.status(time.monotonic())["reason"])
                        else:
                            self.client.post("/api/command", {"token": self._token,
                                                             "velocity": [linear, 0, angular]})
                    except Exception as error:
                        self.error = str(error)
                        self._stop("stopped_on_error")
            self.closed.wait(max(0, self.period-(time.monotonic()-started)))

    def close(self):
        self.closed.set()
        self.stop()
        self.worker.join(timeout=2)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, code, data, mime="application/json"):
        payload = json.dumps(data, allow_nan=False).encode() if mime == "application/json" else data
        self.send_response(code)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        try:
            path = urlsplit(self.path).path
            if path == "/":
                self.send(200, Path(__file__).with_name("index.html").read_bytes(), "text/html; charset=utf-8")
            elif path == "/api/status":
                self.send(200, self.server.app.status())
            else:
                self.send(404, {"error": "not found"})
        except (BrokenPipeError, ConnectionResetError):
            # Client disconnected mid-response; nothing left to write.
            pass

    def do_POST(self):
        try:
            origin = self.headers.get("Origin")
            if origin and urlsplit(origin).netloc != self.headers.get("Host"):
                self.send(403, {"error": "cross-origin control is disabled"})
                return
            if int(self.headers.get("Content-Length", "0")) > 4096:
                self.send(413, {"error": "request too large"})
                return
            if self.path == "/api/start":
                self.send(200, self.server.app.start())
            elif self.path == "/api/stop":
                self.send(200, self.server.app.stop())
            else:
                self.send(404, {"error": "not found"})
        except (ValueError, RuntimeError) as error:
            self.send(409, {"error": str(error)})
        except (BrokenPipeError, ConnectionResetError):
            # Client disconnected mid-response; nothing left to write.
            pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--simulator", default="http://127.0.0.1:8898")
    parser.add_argument("--port", type=int, default=8901)
    args = parser.parse_args()
    app = RoamApp(SimulatorClient(args.simulator))
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    server.app = app
    def shutdown(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGINT, shutdown)
    signal.signal(signal.SIGTERM, shutdown)
    print(f"Go2 roaming sample: http://127.0.0.1:{server.server_port} (press Start to navigate)", flush=True)
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        app.close()
        server.server_close()


if __name__ == "__main__":
    main()
