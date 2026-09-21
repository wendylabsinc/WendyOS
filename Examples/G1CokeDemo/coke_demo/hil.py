"""Lockstep simulation inference over HTTP. This service has no robot I/O.

Each request contains a JSON header and, on new camera frames, the exact
little-endian float32 image tensor. Simulation time drives policy state.
The most recent reply is cached so a lost HTTP reply cannot advance the GRU
twice. A service restart requires a new simulation episode.
"""
from __future__ import annotations

import hashlib
import http.client
import json
import math
import secrets
import socket
import struct
import threading
import time
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

import numpy as np

from runtime.exact_policy import EXPECTED_CHECKPOINT_SHA256
from .ros_bridge import validate_target

SCHEMA = "wendy.coke.simulation-inference.v1"
IMAGE_SHAPE = (1, 5, 240, 320)
IMAGE_BYTES = 5 * 240 * 320 * 4
MAX_HEADER = 16384
MAX_BODY = IMAGE_BYTES + MAX_HEADER + 4
DT_NS = 25_000_000


def json_bytes(value):
    return json.dumps(value, separators=(",", ":"), allow_nan=False).encode()


def encode_packet(header, image=None):
    raw = json_bytes(header)
    if len(raw) > MAX_HEADER:
        raise ValueError("HIL header is too large")
    if image is None:
        pixels = b""
    else:
        array = np.asarray(image, dtype="<f4")
        if array.shape != IMAGE_SHAPE or not np.isfinite(array).all():
            raise ValueError("HIL image must be finite float32 [1,5,240,320]")
        pixels = array.tobytes()
    return struct.pack("!I", len(raw)) + raw + pixels


def decode_packet(body):
    if not 4 <= len(body) <= MAX_BODY:
        raise ValueError("Invalid HIL packet length")
    size = struct.unpack("!I", body[:4])[0]
    if not 0 < size <= MAX_HEADER or 4 + size > len(body):
        raise ValueError("Invalid HIL header length")
    header = json.loads(body[4:4 + size])
    if not isinstance(header, dict):
        raise ValueError("HIL header must be an object")
    pixels = body[4 + size:]
    if len(pixels) not in {0, IMAGE_BYTES}:
        raise ValueError("Invalid HIL image length")
    image = None
    if pixels:
        image = np.frombuffer(pixels, dtype="<f4").reshape(IMAGE_SHAPE).copy()
        if not np.isfinite(image).all() or np.any(image < 0) or np.any(image > 1):
            raise ValueError("HIL image channels must be finite and within [0,1]")
    return header, image


def vector(value, length, name):
    with np.errstate(over="ignore", invalid="ignore"):
        result = np.asarray(value, dtype=np.float32)
    if result.shape != (length,) or not np.isfinite(result).all():
        raise ValueError(f"{name} must contain {length} finite values")
    return result


class SimulationPolicy:
    """Use the same inputs and reference timing as the built-in scene policy."""

    def __init__(self, bundle: Path, device="cuda", control_device="cpu"):
        import torch
        from runtime.exact_policy import CachedReferenceResidualPolicy, ExactVisionEncoder
        from runtime.observation_adapter import LiveJointFeatureAdapter

        if torch.device(device).type == "cuda" and not torch.cuda.is_available():
            raise RuntimeError("CUDA is unavailable. Check the Wendy GPU entitlement and CUDA build.")
        self.policy = CachedReferenceResidualPolicy(bundle, device=device, control_device=control_device)
        self.names = self.policy.contract["sensor_input_contract"]["joint_names"]
        self.devices = {"vision": str(self.policy.vision_device), "control": str(self.policy.device)}
        self.encode = ExactVisionEncoder(self.policy._vision_model, device, output_device=self.policy.device)
        self.joints = LiveJointFeatureAdapter(self.names)
        self.reset()

    def reset(self):
        self.policy.reset_episode()
        self.joints.reset_episode(started_at_ns=1_000_000_000)
        self.generation = self.policy.vision.status()["episode_generation"]
        self.embedding = None

    def propose(self, header, image):
        import torch
        from runtime.async_vision import VisionSnapshot
        from runtime.observation_adapter import OrderedJointState

        now = 1_000_000_000 + header["step"] * DT_NS
        captured = 1_000_000_000 + header["camera_frame"] * DT_NS
        started = time.perf_counter()
        if image is not None:
            self.embedding = self.encode(torch.from_numpy(image))
        features = self.joints.pack(
            OrderedJointState(tuple(self.names), header["q43"], header["dq43"], now),
            camera_captured_at_ns=captured, control_at_ns=now,
        )
        snapshot = VisionSnapshot(self.generation, header["camera_frame"] + 1,
                                  header["camera_frame"], "coke-scene", captured, now, 0., self.embedding)
        proposal = self.policy.propose(
            joint_features=features, reference_full=header["reference"],
            reference_velocity=header["reference_velocity"], control_at_ns=now,
            vision_snapshot=snapshot,
        )
        return {"target_q_43": proposal["target_q_43"],
                "compute_ms": (time.perf_counter() - started) * 1000}

    def close(self):
        self.policy.close()


class InferenceSession:
    """Serialize recurrent state and reject stale, skipped or changed retries."""

    def __init__(self, predictor):
        self.predictor = predictor
        self.lock = threading.RLock()
        self.session = None
        self.seen_sessions = set()
        self.next_step = 0
        self.last_digest = self.last_reply = None
        self.failed = False

    def health(self):
        with self.lock:
            return {"schema": SCHEMA, "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
                    "joint_names": self.predictor.names, "devices": self.predictor.devices,
                    "physical_commands_sent": 0, "next_step": self.next_step,
                    "episode_failed": self.failed}

    def reset(self, body):
        session = body.get("session")
        if not isinstance(session, str) or len(session) != 32 or any(c not in "0123456789abcdef" for c in session):
            raise ValueError("Invalid HIL session identity")
        if body.get("schema") != SCHEMA or body.get("checkpoint_sha256") != EXPECTED_CHECKPOINT_SHA256:
            raise ValueError("HIL protocol or checkpoint differs")
        if body.get("joint_names") != list(self.predictor.names):
            raise ValueError("HIL joint order differs from checkpoint")
        with self.lock:
            if session != self.session:
                if session in self.seen_sessions:
                    raise ValueError("Cannot reactivate an old HIL session")
                self.failed = True
                self.predictor.reset()
                self.session = session
                self.seen_sessions.add(session)
                self.next_step = 0
                self.last_digest = self.last_reply = None
                self.failed = False
            return {"schema": SCHEMA, "session": session}

    def propose(self, body):
        digest = hashlib.sha256(body).digest()
        header, image = decode_packet(body)
        step, camera = header.get("step"), header.get("camera_frame")
        if type(step) is not int or not 0 <= step < 7518:
            raise ValueError("HIL step must be within 0..7517")
        if type(camera) is not int or camera != step - step % 2:
            raise ValueError("HIL camera must match the 20 Hz simulation exposure")
        if (image is not None) != (step % 2 == 0):
            raise ValueError("HIL requires an image exactly on each new camera frame")
        for name, size in (("q43", 43), ("dq43", 43), ("reference", 43), ("reference_velocity", 15)):
            header[name] = vector(header.get(name), size, name)
        with self.lock:
            if self.session is None or header.get("session") != self.session:
                raise ValueError("HIL session changed. Reset the simulation.")
            if self.failed:
                raise ValueError("HIL episode failed. Reset the simulation.")
            if step == self.next_step - 1 and digest == self.last_digest:
                return self.last_reply
            if step != self.next_step:
                raise ValueError("Stale, skipped or changed HIL step")
            try:
                result = self.predictor.propose(header, image)
                target = vector(result["target_q_43"], 43, "target")
                compute_ms = float(result["compute_ms"])
                if not math.isfinite(compute_ms) or compute_ms < 0:
                    raise ValueError("Invalid HIL compute duration")
                reply = {"schema": SCHEMA, "session": self.session, "step": step,
                         "camera_frame": camera, "target_q_43": target.tolist(),
                         "compute_ms": compute_ms}
            except Exception:
                # The GRU or acceleration history may already have advanced.
                self.failed = True
                raise
            self.next_step += 1
            self.last_digest, self.last_reply = digest, reply
            return reply


def make_inference_server(runtime, host="127.0.0.1", port=8098, token=""):
    from .access import is_loopback
    if token or not is_loopback(host):
        if len(token) < 16 or not token.isascii() or any(ord(c) < 33 or ord(c) > 126 for c in token):
            raise ValueError("COKE_HIL_TOKEN must contain at least 16 printable ASCII characters without whitespace")
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        timeout = 10

        def log_message(self, *_):
            pass

        def reply(self, status, body):
            raw = json_bytes(body)
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Cache-Control", "no-store")
            if self.close_connection:
                self.send_header("Connection", "close")
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                # The client disconnected; there is no response left to send.
                return

        def authorized(self):
            if token and not secrets.compare_digest(self.headers.get("Authorization", ""), "Bearer " + token):
                self.close_connection = True
                self.reply(403, {"error": "Invalid HIL token"})
                return False
            return True

        def do_GET(self):
            if self.authorized():
                self.reply(200, runtime.health()) if self.path == "/health" else self.reply(404, {"error": "Not found"})

        def do_POST(self):
            if not self.authorized():
                return
            try:
                if self.path not in {"/reset", "/step"}:
                    raise ValueError("Unknown HIL endpoint")
                maximum = MAX_HEADER if self.path == "/reset" else MAX_BODY
                length = int(self.headers.get("Content-Length", "0"))
                if self.headers.get("Transfer-Encoding") or not 0 < length <= maximum:
                    raise ValueError("Invalid HIL request length")
                body = self.rfile.read(length)
                if len(body) != length:
                    raise ValueError("Truncated HIL request")
                encoding = self.headers.get("Content-Encoding", "identity")
                if encoding == "deflate":
                    decoder = zlib.decompressobj()
                    body = decoder.decompress(body, maximum + 1)
                    if len(body) > maximum or not decoder.eof or decoder.unused_data:
                        raise ValueError("Invalid or oversized compressed HIL request")
                elif encoding != "identity":
                    raise ValueError("Unsupported HIL content encoding")
                if self.path == "/reset":
                    value = json.loads(body)
                    if not isinstance(value, dict):
                        raise ValueError("Reset request must be an object")
                    result = runtime.reset(value)
                else:
                    result = runtime.propose(body)
                self.reply(200, result)
            except Exception as exc:
                self.close_connection = True
                self.reply(409, {"error": f"{type(exc).__name__}: {exc}"})

    return ThreadingHTTPServer((host, port), Handler)


class RemoteSimulationPolicy:
    def __init__(self, url, names, bounds, *, timeout=5., token=""):
        parsed = urlsplit(url)
        if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.path not in {"", "/"} or parsed.query or parsed.fragment or parsed.username or parsed.password:
            raise ValueError("COKE_POLICY_URL must be an HTTP(S) origin")
        if not math.isfinite(timeout) or not 0 < timeout <= 60:
            raise ValueError("HIL timeout must be within (0,60] seconds")
        self.address = parsed
        self.names, self.bounds = list(names), bounds
        self.timeout, self.token = timeout, token
        self.connection = None
        self.devices = None
        self.reset()

    def reset(self):
        self.session = secrets.token_hex(16)
        self.ready = False
        self.next_step = 0

    def close(self):
        if self.connection is not None:
            self.connection.close()
            self.connection = None

    def request(self, path, body=None):
        headers = {"Content-Type": "application/octet-stream"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        if path == "/step" and body is not None and len(body) > MAX_HEADER:
            compressed = zlib.compress(body, level=1)
            if len(compressed) < len(body):
                body = compressed
                headers["Content-Encoding"] = "deflate"
        for attempt in range(2):
            try:
                if self.connection is None:
                    cls = http.client.HTTPSConnection if self.address.scheme == "https" else http.client.HTTPConnection
                    self.connection = cls(self.address.hostname, self.address.port, timeout=self.timeout)
                    self.connection.connect()
                    self.connection.sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
                self.connection.request("GET" if body is None else "POST", path, body, headers)
                response = self.connection.getresponse()
                raw = response.read(MAX_HEADER + 1)
                if len(raw) > MAX_HEADER:
                    raise ValueError("HIL reply is too large")
                value = json.loads(raw)
                if not isinstance(value, dict):
                    raise ValueError("HIL reply must be an object")
                if response.status != 200:
                    raise RuntimeError(value.get("error", f"HIL HTTP {response.status}"))
                return value
            except (OSError, http.client.HTTPException) as exc:
                self.close()
                if attempt:
                    raise RuntimeError(f"Jetson inference connection failed: {exc}. Reset the simulation to retry.") from exc
            except Exception:
                self.close()
                raise

    def propose(self, observation, reference, reference_velocity):
        step, camera = observation["frame"], observation["camera_frame"]
        if step != self.next_step:
            raise ValueError("Simulation and remote policy steps differ")
        if not self.ready:
            health = self.request("/health")
            if health.get("schema") != SCHEMA or health.get("checkpoint_sha256") != EXPECTED_CHECKPOINT_SHA256 or health.get("joint_names") != self.names:
                raise ValueError("Remote inference protocol, checkpoint or joint order differs")
            self.devices = health["devices"]
            reply = self.request("/reset", json_bytes({
                "schema": SCHEMA, "session": self.session, "joint_names": self.names,
                "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
            }))
            if reply.get("session") != self.session or reply.get("schema") != SCHEMA:
                raise ValueError("Remote inference reset identity differs")
            self.ready = True
        body = encode_packet({
            "session": self.session, "step": step, "camera_frame": camera,
            "q43": np.asarray(observation["q43"]).tolist(),
            "dq43": np.asarray(observation["dq43"]).tolist(),
            "reference": np.asarray(reference).tolist(),
            "reference_velocity": np.asarray(reference_velocity).tolist(),
        }, observation["image"] if step % 2 == 0 else None)
        reply = self.request("/step", body)
        if reply.get("schema") != SCHEMA or reply.get("session") != self.session or reply.get("step") != step or reply.get("camera_frame") != camera:
            raise ValueError("Remote inference reply belongs to a different observation")
        target = validate_target(self.names, reply.get("target_q_43"), self.names, self.bounds)
        duration = float(reply["compute_ms"])
        if not math.isfinite(duration) or duration < 0:
            raise ValueError("Remote inference duration is invalid")
        self.next_step += 1
        return target, duration


def serve_inference(bundle, *, host="127.0.0.1", port=8098, device="cuda", control_device="cpu", token=""):
    predictor = SimulationPolicy(bundle, device, control_device)
    try:
        server = make_inference_server(InferenceSession(predictor), host, port, token)
        print(f"Coke simulation inference: http://{host}:{server.server_port} {predictor.devices}", flush=True)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            # Ctrl+C requests shutdown; the finally blocks close server and runtime.
            pass
        finally:
            server.server_close()
    finally:
        predictor.close()
