"""DDS-free exact policy service for the G1 physical control process.

Unitree's high-rate Python DDS callbacks run in the physical process. Keeping
Torch inference here gives it a separate GIL while the physical process remains
the sole owner and publisher of motor commands.
"""
from __future__ import annotations

import json
import os
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler
from coke_demo.http_server import DrainingHTTPServer
from pathlib import Path
from typing import Any

from .contracts import DEFAULT_MAXIMUM_CAMERA_AGE_S, INFERENCE_SCHEMA
from .exact_policy import CachedReferenceResidualPolicy, EXPECTED_CHECKPOINT_SHA256
from .observation_adapter import ExactEpisodeObservationAdapter, OrderedJointState
from .physical_policy import SegmentationCameraPump


BUNDLE = Path(os.environ.get("G1_POLICY_BUNDLE", "/bundle"))
PORT = int(os.environ.get("G1_INFERENCE_PORT", "8097"))
MAXIMUM_CAMERA_AGE_S = float(os.environ.get("G1_MAXIMUM_CAMERA_AGE_S", str(DEFAULT_MAXIMUM_CAMERA_AGE_S)))


class InferenceRuntime:
    motion_capability = False

    def __init__(self) -> None:
        policy = CachedReferenceResidualPolicy(
            BUNDLE,
            device=os.environ.get("G1_POLICY_DEVICE", "cuda"),
            control_device=os.environ.get("G1_POLICY_CONTROL_DEVICE", "cpu"),
            maximum_camera_age_s=MAXIMUM_CAMERA_AGE_S,
        )
        self.episode = ExactEpisodeObservationAdapter(policy, BUNDLE)
        self.camera = SegmentationCameraPump(self.episode)
        self.camera.start()
        self.lock = threading.RLock()
        self.session_id: str | None = None
        self.active = False
        self.session_last_seen = time.monotonic()
        self.proposals = 0
        self.last_error: str | None = None

    def preflight(self) -> dict[str, Any]:
        return self.camera.preflight()

    def reset(self, session_id: str, started_at_ns: int) -> dict[str, Any]:
        with self.lock:
            self._expire_session()
            if self.active and self.session_id != session_id:
                raise RuntimeError("another inference session is active")
            result = self.episode.reset_episode(started_at_ns=started_at_ns)
            self.session_id = session_id
            self.session_last_seen = time.monotonic()
            self.active = False
            self.proposals = 0
            self.last_error = None
            return result

    def activate(self, session_id: str) -> dict[str, Any]:
        with self.lock:
            self._require_session(session_id)
            self.camera.activate()
            self.active = True
            return {"active": True, "session_id": session_id}

    def deactivate(self, session_id: str) -> dict[str, Any]:
        with self.lock:
            if self.session_id not in {None, session_id}:
                raise RuntimeError("inference session identity changed")
            self.camera.deactivate()
            self.active = False
            self.session_id = None
            return {"active": False, "session_id": session_id}

    def propose(self, body: dict[str, Any]) -> dict[str, Any]:
        with self.lock:
            session_id = str(body["session_id"])
            self._require_session(session_id)
            if not self.active:
                raise RuntimeError("inference session is not active")
            try:
                proposal = self.episode.propose(
                    OrderedJointState(
                        self.episode.joint_names,
                        body["q_43"],
                        body["dq_43"],
                        int(body["sampled_at_ns"]),
                    ),
                    control_at_ns=int(body["control_at_ns"]),
                )
                self.proposals += 1
                self.last_error = None
                # This 131-float diagnostic is reconstructable from the live
                # state and needlessly taxes the 40 Hz IPC response.
                proposal.pop("joint_features_131", None)
                return proposal
            except Exception as exc:
                self.last_error = f"{type(exc).__name__}: {exc}"
                raise

    def _expire_session(self) -> None:
        # Proposals renew the lease. Status polling cannot retain an abandoned owner.
        # The initial lease also covers the bounded entry ramp and settling period.
        if self.session_id is not None and time.monotonic() - self.session_last_seen > 120:
            self.camera.deactivate()
            self.active = False
            self.session_id = None

    def _require_session(self, session_id: str) -> None:
        self._expire_session()
        if len(session_id) < 16 or self.session_id != session_id:
            raise RuntimeError("inference session is unavailable")
        self.session_last_seen = time.monotonic()

    def status(self) -> dict[str, Any]:
        with self.lock:
            self._expire_session()
            return {
                "schema": INFERENCE_SCHEMA,
                "joint_names": list(self.episode.joint_names),
                "healthy": True,
                "motion_capability": False,
                "physical_commands_sent": 0,
                "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
                "vision_device": str(self.episode.policy.vision_device),
                "control_device": str(self.episode.policy.device),
                "active": self.active,
                "proposals": self.proposals,
                "last_error": self.last_error,
                "camera": self.camera.status(),
            }

    def close(self) -> None:
        self.camera.close()
        self.episode.policy.close()


def make_server(runtime: InferenceRuntime) -> DrainingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        timeout = 10
        protocol_version = "HTTP/1.1"

        def setup(self) -> None:
            super().setup()
            self.request.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

        def log_message(self, *_args: Any) -> None:
            return

        def do_GET(self) -> None:
            if self.path.split("?", 1)[0] != "/health":
                return self.reply(404, {"error": "not found"})
            self.reply(200, runtime.status())

        def do_POST(self) -> None:
            path = self.path.split("?", 1)[0]
            if path not in {"/preflight", "/reset", "/activate", "/deactivate", "/propose"}:
                self.close_connection = True
                return self.reply(404, {"error": "not found"})
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length <= 0 or length > 65536:
                    raise ValueError("invalid request length")
                body = json.loads(self.rfile.read(length))
                if path == "/preflight":
                    value = runtime.preflight()
                elif path == "/reset":
                    value = runtime.reset(str(body["session_id"]), int(body["started_at_ns"]))
                elif path == "/activate":
                    value = runtime.activate(str(body["session_id"]))
                elif path == "/deactivate":
                    value = runtime.deactivate(str(body["session_id"]))
                else:
                    value = runtime.propose(body)
                self.reply(200, value)
            except Exception as exc:
                self.close_connection = True
                self.reply(409, {"error": f"{type(exc).__name__}: {exc}"})

        def reply(self, status: int, value: dict[str, Any]) -> None:
            body = json.dumps(value, separators=(",", ":"), allow_nan=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            if self.server.closing:
                self.close_connection = True
            if self.close_connection:
                self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)

    return DrainingHTTPServer(("127.0.0.1", PORT), Handler)


def main() -> None:
    runtime = InferenceRuntime()
    server = make_server(runtime)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        runtime.close()


if __name__ == "__main__":
    main()
