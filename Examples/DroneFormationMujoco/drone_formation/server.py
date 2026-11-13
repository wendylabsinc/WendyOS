"""Dependency-free HTTP server: the viewer, a live state stream and health checks."""

from __future__ import annotations

import json
import os
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse

from .scene import SceneAsset
from .simulation import Simulation


HOST = os.environ.get("DRONE_FORMATION_HOST", "0.0.0.0")
PORT = int(os.environ.get("DRONE_FORMATION_PORT", "8879"))
STATIC_ROOT = Path(__file__).resolve().parent / "static"
STATE_HZ = 60
WIND_EVERY = 4  # send the wind grid with every fourth state (15 Hz)
STATIC_FILES = {
    "/": ("index.html", "text/html; charset=utf-8"),
    "/viewer.js": ("viewer.js", "text/javascript; charset=utf-8"),
    # Fetched by drone_formation.assets from a pinned three.js commit.
    "/three/three.module.js": ("three/three.module.js", "text/javascript; charset=utf-8"),
    "/three/three.core.js": ("three/three.core.js", "text/javascript; charset=utf-8"),
    "/three/addons/controls/OrbitControls.js": ("three/addons/controls/OrbitControls.js", "text/javascript; charset=utf-8"),
}


class DemoServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address: tuple[str, int], simulation: Simulation) -> None:
        self.simulation = simulation
        self.scene = SceneAsset(simulation.model)
        self.stopping = threading.Event()
        super().__init__(address, DemoHandler)


class DemoHandler(BaseHTTPRequestHandler):
    server: DemoServer
    protocol_version = "HTTP/1.1"

    def _headers(self, status: int, content_type: str, length: int | None = None,
                 extra: dict[str, str] | None = None) -> None:
        self.send_response(status)
        self.send_header("Content-Type", content_type)
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Content-Type-Options", "nosniff")
        for key, value in (extra or {}).items():
            self.send_header(key, value)
        if length is not None:
            self.send_header("Content-Length", str(length))
        self.end_headers()

    def _json(self, payload: dict, status: int = 200) -> None:
        body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self._headers(status, "application/json", len(body))
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802
        path = urlparse(self.path).path
        simulation = self.server.simulation
        if path in STATIC_FILES:
            name, content_type = STATIC_FILES[path]
            body = (STATIC_ROOT / name).read_bytes()
            self._headers(200, content_type, len(body))
            self.wfile.write(body)
            return
        if path in ("/api/health", "/api/status"):
            payload = simulation.status()
            self._json(payload, 200 if payload["ready"] else 503)
            return
        if path == "/api/scene":
            if "gzip" in self.headers.get("Accept-Encoding", ""):
                self._headers(200, "application/json", len(self.server.scene.gzip), {"Content-Encoding": "gzip"})
                self.wfile.write(self.server.scene.gzip)
            else:
                self._headers(200, "application/json", len(self.server.scene.json))
                self.wfile.write(self.server.scene.json)
            return
        if path == "/api/state":
            with simulation.lock:
                payload = simulation.snapshot()
            self._json(payload)
            return
        if path == "/api/stream":
            self._stream()
            return
        self.send_error(404)

    def do_POST(self) -> None:  # noqa: N802
        if urlparse(self.path).path == "/api/reset":
            simulation = self.server.simulation
            with simulation.lock:
                simulation.reset()
            self._json({"ok": True})
            return
        self.send_error(404)

    def _stream(self) -> None:
        """Server-sent events: drone poses at 60 Hz, the wind grid at 15 Hz."""
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-store")
        self.send_header("X-Accel-Buffering", "no")
        self.end_headers()
        simulation = self.server.simulation
        period = 1.0 / STATE_HZ
        frame = 0
        next_due = time.monotonic()
        try:
            while not self.server.stopping.is_set():
                with simulation.lock:
                    state = simulation.snapshot()
                    wind = simulation.wind_grid() if frame % WIND_EVERY == 0 else None
                chunks = [b"event: state\ndata: ", json.dumps(state, separators=(",", ":")).encode(), b"\n\n"]
                if wind is not None:
                    chunks += [b"event: wind\ndata: ", json.dumps(wind, separators=(",", ":")).encode(), b"\n\n"]
                self.wfile.write(b"".join(chunks))
                self.wfile.flush()
                frame += 1
                next_due += period
                delay = next_due - time.monotonic()
                if delay > 0:
                    time.sleep(delay)
                else:
                    next_due = time.monotonic()
        except (BrokenPipeError, ConnectionResetError, ConnectionAbortedError):
            return

    def log_message(self, fmt: str, *args: object) -> None:
        if os.environ.get("DRONE_FORMATION_HTTP_LOG", "0") == "1":
            super().log_message(fmt, *args)


def main() -> None:
    # DRONE_FORMATION_SPEED > 1 fast-forwards the show (handy while tweaking the viewer).
    simulation = Simulation(realtime_speed=float(os.environ.get("DRONE_FORMATION_SPEED", "1")))
    simulation.start()
    server = DemoServer((HOST, PORT), simulation)

    def shutdown(_signum: int, _frame: object) -> None:
        if server.stopping.is_set():
            return
        server.stopping.set()
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    status = simulation.status()
    print(f"MuJoCo {status['mujocoVersion']}: {status['drones']} Crazyflies, {status['physicsHz']} Hz physics", flush=True)
    print(f"Viewer on port {PORT}", flush=True)
    try:
        server.serve_forever(poll_interval=0.25)
    finally:
        server.server_close()
        simulation.close()


if __name__ == "__main__":
    main()
