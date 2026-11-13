"""Local HTTP sandbox. The simulator runs independently of browser clients."""

import json
import os
from pathlib import Path
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

from .runtime import Runtime

ASSETS = {"/": ("index.html", "text/html; charset=utf-8"),
          **{f"/{name}": (name, "text/javascript; charset=utf-8")
             for name in ("viewer.js", "robot-model.js", "environment.js", "visuals.js")},
          **{f"/vendor/{name}": (f"vendor/{name}", "text/javascript; charset=utf-8")
             for name in ("three.module.js", "three.core.js", "OrbitControls.js")}}


class SimulatorHTTPServer(ThreadingHTTPServer):
    # Leave room for asset requests and camera connections from several tabs.
    request_queue_size = 128


class Handler(BaseHTTPRequestHandler):
    # Reuse connections for telemetry and assets instead of repeatedly filling
    # libslirp's small host-forward listen queue with new TCP handshakes.
    protocol_version = "HTTP/1.1"

    def setup(self):
        super().setup()
        self.connection.settimeout(5)

    def log_message(self, *_):
        pass

    def send(self, code, data, mime="application/json"):
        payload = json.dumps(data, allow_nan=False).encode() if mime == "application/json" else data
        self.send_response(code)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(payload)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        try:
            self.end_headers()
            self.wfile.write(payload)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_GET(self):
        path = urlsplit(self.path).path
        runtime = self.server.runtime
        if path in ASSETS:
            name, mime = ASSETS[path]
            self.send(200, (Path(__file__).parent / name).read_bytes(), mime)
        elif path in {"/api/status", "/api/health"}:
            status = runtime.status()
            self.send(503 if path == "/api/health" and not status["ready"] else 200, status)
        elif path == "/api/camera/info":
            from .camera import calibration
            self.send(200, calibration())
        elif path in {"/stream/color", "/stream/depth"}:
            channel = path.rsplit("/", 1)[1]
            self.send_response(200)
            self.send_header("Content-Type", "multipart/x-mixed-replace; boundary=frame")
            self.send_header("Cache-Control", "no-store")
            self.send_header("Connection", "close")
            self.close_connection = True
            self.end_headers()
            last_capture = 0
            try:
                while not runtime.closed.wait(.03):
                    with runtime.lock:
                        frame = runtime.camera_frame
                        healthy = not runtime.error
                    if not healthy:
                        break
                    if frame and frame["capture_ns"] != last_capture:
                        last_capture = frame["capture_ns"]
                        payload = frame[channel]
                        self.wfile.write((f"--frame\r\nContent-Type: image/jpeg\r\nContent-Length: {len(payload)}\r\nX-Capture-Ns: {last_capture}\r\n\r\n").encode() + payload + b"\r\n")
                        self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError, TimeoutError):
                pass
        elif path in {"/api/camera/color.jpg", "/api/camera/depth.jpg", "/api/camera/depth.raw"}:
            with runtime.lock:
                frame = runtime.camera_frame
            if not frame or runtime.error or time.time_ns() - frame["capture_ns"] > 2_000_000_000:
                self.send(503, {"error": "no current camera frame"})
            else:
                raw = path.endswith(".raw")
                key = "depth_raw" if raw else "depth" if "depth" in path else "color"
                self.send(200, frame[key], "application/octet-stream" if raw else "image/jpeg")
        elif path == "/api/profile":
            self.send(200, json.loads((Path(__file__).parents[1] / "compatibility.json").read_text()))
        elif path == "/api/scene":
            self.send(200, runtime.sim.scene())
        elif path == "/api/scan":
            with runtime.lock:
                self.send(200, {**runtime.scan, "capture_ns": runtime.scan_ns, "epoch": runtime.epoch,
                                "paused": runtime.paused})
        else:
            self.send(404, {"error": "not found"})

    def do_POST(self):
        runtime = self.server.runtime
        try:
            origin = self.headers.get("Origin")
            if origin and urlsplit(origin).netloc != self.headers.get("Host"):
                self.send(403, {"error": "cross-origin control is disabled"})
                return
            if self.headers.get("Sec-Fetch-Site") == "cross-site":
                self.send(403, {"error": "cross-site control is disabled"})
                return
            length = int(self.headers.get("Content-Length", "0"))
            if not 0 <= length <= 4096:
                self.send(413, {"error": "request is too large"})
                return
            body = json.loads(self.rfile.read(length) or b"{}")
            if not isinstance(body, dict):
                raise ValueError("request must be an object")
            path = urlsplit(self.path).path
            result = {"ok": True}
            with runtime.lock:
                if path == "/api/app/claim":
                    result = runtime.claim_app()
                elif path == "/api/app/command":
                    runtime.app_command(body)
                elif path == "/api/arm":
                    result = runtime.arm(body.get("mode", "browser"))
                elif path == "/api/command":
                    runtime.command(body)
                elif path == "/api/stop":
                    runtime.revoke()
                elif path == "/api/reset":
                    runtime.reset()
                elif path == "/api/pause":
                    if type(body.get("paused")) is not bool:
                        raise ValueError("paused must be a boolean")
                    runtime.revoke()
                    runtime.paused = body["paused"]
                else:
                    self.send(404, {"error": "not found"})
                    return
            self.send(200, result)
        except (ValueError, TypeError, OverflowError) as exc:
            self.send(400, {"error": str(exc)})


def main():
    runtime = Runtime()
    runtime.start()
    server = SimulatorHTTPServer((os.getenv("R2_HOST", "127.0.0.1"), int(os.getenv("R2_PORT", "8890"))), Handler)
    server.runtime = runtime
    def stop(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    print(f"ROSMASTER R2 simulator: http://{server.server_address[0]}:{server.server_address[1]}", flush=True)
    try:
        server.serve_forever()
    finally:
        runtime.close()
        server.server_close()


if __name__ == "__main__":
    main()
