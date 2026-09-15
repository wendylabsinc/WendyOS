"""Small local sandbox endpoint; the physics runtime outlives browser clients."""

import json
import os
from pathlib import Path
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

from .runtime import Runtime


STATIC_ASSETS = {"/viewer.js": "viewer.js", "/vendor/three.module.js": "vendor/three.module.js",
                 "/vendor/three.core.js": "vendor/three.core.js",
                 "/vendor/OrbitControls.js": "vendor/OrbitControls.js"}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    @property
    def runtime(self):
        return self.server.runtime

    def send(self, code, value, mime="application/json", *, encoded=False, headers=None):
        payload = json.dumps(value, allow_nan=False).encode() if mime == "application/json" and not encoded else value
        self.send_response(code)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        for key, value in (headers or {}).items():
            self.send_header(key, value)
        self.end_headers()
        self.wfile.write(payload)

    def do_GET(self):
        try:
            path = urlsplit(self.path).path
            if path in {"/api/status", "/api/health"}:
                status = self.runtime.status()
                self.send(200 if path == "/api/status" or status["ready"] else 503, status)
            elif path == "/api/profile":
                profile = Path(__file__).resolve().parents[1] / "compatibility.json"
                self.send(200, json.loads(profile.read_text()))
            elif path == "/api/scene":
                scene = self.runtime.scene
                compressed = False
                for encoding in self.headers.get("Accept-Encoding", "").split(","):
                    parts = [part.strip() for part in encoding.split(";")]
                    if parts[0] == "gzip":
                        try:
                            compressed = all(float(part[2:]) > 0 for part in parts[1:] if part.startswith("q="))
                        except ValueError:
                            compressed = False
                headers = {"Vary": "Accept-Encoding", "ETag": '"' + scene.description["id"] + '"'}
                if compressed:
                    headers["Content-Encoding"] = "gzip"
                self.send(200, scene.gzip if compressed else scene.json, encoded=True, headers=headers)
            elif path == "/api/scene/state":
                self.send(200, self.runtime.scene.state_json(self.runtime), encoded=True)
            elif path == "/frame.jpg":
                self.send(410, {"error": "observer images were replaced by /api/scene and /api/scene/state"})
            elif path == "/camera.jpg":
                jpeg = self.runtime.camera_jpeg
                if jpeg is None:
                    self.send(503, {"error": "waiting for first frame"})
                else:
                    self.send(200, jpeg, "image/jpeg")
            elif path == "/":
                self.send(200, Path(__file__).with_name("index.html").read_bytes(), "text/html; charset=utf-8")
            elif path in STATIC_ASSETS:
                asset = Path(__file__).parent / STATIC_ASSETS[path]
                if asset.is_file():
                    self.send(200, asset.read_bytes(), "text/javascript; charset=utf-8")
                else:
                    self.send(404, {"error": "not found"})
            else:
                self.send(404, {"error": "not found"})
        except RuntimeError as exc:
            self.send(503, {"error": str(exc)})
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_POST(self):
        try:
            origin = self.headers.get("Origin")
            if origin and urlsplit(origin).netloc != self.headers.get("Host"):
                self.send(403, {"error": "cross-origin control is disabled"})
                return
            size = int(self.headers.get("Content-Length", "0"))
            if size < 0 or size > 4096:
                self.send(413, {"error": "request too large"})
                return
            body = json.loads(self.rfile.read(size) or b"{}")
            if not isinstance(body, dict):
                raise ValueError("request must be an object")
            path = urlsplit(self.path).path
            with self.runtime.observation_lock, self.runtime.lock:
                self.runtime.ensure_running()
                if self.runtime.error is not None and path != "/api/reset":
                    raise RuntimeError("simulator fault; reset the world before resuming control")
                sim = self.runtime.sim
                token = body.get("token")
                if path == "/api/arm":
                    result = {"token": sim.arm(), "epoch": sim.epoch}
                elif path == "/api/arm_ros":
                    if self.runtime.ros_commands is None:
                        raise ValueError("ROS command ingress is disabled")
                    result = self.runtime.ros_commands.grant(body.get("publisher_gid"))
                elif path == "/api/disarm_ros":
                    commands = self.runtime.ros_commands
                    if commands is None:
                        raise ValueError("ROS command ingress is disabled")
                    if commands.token is not None and commands.token == sim.owner:
                        sim.release(commands.token)
                    commands.revoke()
                    result = {"released": True, "requires_publisher_restart": True}
                elif path == "/api/command":
                    velocity = body.get("velocity")
                    if not isinstance(velocity, list) or len(velocity) != 3:
                        raise ValueError("velocity must contain three numbers")
                    self.runtime.command(velocity, token)
                    result = {"accepted": True}
                elif path == "/api/stop":
                    sim.stop(token)
                    result = {"stopped": True}
                elif path == "/api/release":
                    sim.release(token)
                    result = {"released": True}
                elif path == "/api/pause":
                    self.runtime.pause()
                    result = {"paused": True}
                elif path == "/api/resume":
                    self.runtime.resume()
                    result = {"resumed": True, "requires_rearm": True}
                elif path == "/api/reset":
                    self.runtime.reset()
                    result = {"epoch": sim.epoch, "requires_rearm": True}
                elif path == "/api/obstacle":
                    result = self.runtime.move_obstacle(body.get("position"))
                elif path == "/api/sensors":
                    result = self.runtime.configure_sensors(body)
                else:
                    self.send(404, {"error": "not found"})
                    return
            self.send(200, result)
        except PermissionError as exc:
            self.send(403, {"error": str(exc)})
        except (ValueError, TypeError) as exc:
            self.send(400, {"error": str(exc)})
        except RuntimeError as exc:
            self.send(503, {"error": str(exc)})
        except (BrokenPipeError, ConnectionResetError):
            pass


def main():
    runtime = Runtime(render=os.environ.get("G1_RENDER", "1") != "0",
                      ros=os.environ.get("G1_ROS", "0") == "1")
    server = ThreadingHTTPServer((os.environ.get("G1_HOST", "127.0.0.1"),
                                  int(os.environ.get("G1_PORT", "8890"))), Handler)
    server.runtime = runtime
    runtime.start()
    def shutdown(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    print(json.dumps({"event": "g1_started", "port": server.server_port}), flush=True)
    try:
        server.serve_forever(poll_interval=0.2)
    finally:
        runtime.close()
        server.server_close()


if __name__ == "__main__":
    main()
