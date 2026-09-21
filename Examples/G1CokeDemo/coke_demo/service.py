"""Wendy operator app for the ROS-connected Coke manipulation scene."""
from __future__ import annotations

import gzip
import io
import json
import mimetypes
import queue
import secrets
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

import numpy as np
import torch
from PIL import Image

from runtime.async_vision import VisionSnapshot
from runtime.exact_policy import CachedReferenceResidualPolicy, EXPECTED_CHECKPOINT_SHA256
from runtime.observation_adapter import LiveJointFeatureAdapter, OrderedJointState


class DemoRuntime:
    def __init__(self, root: Path, *, ros_enabled=True, policy_url=None, policy_timeout=5., policy_token=""):
        self.root = root
        self.ros_enabled = ros_enabled
        self.policy_url = policy_url
        self.policy_timeout = policy_timeout
        self.policy_token = policy_token
        self.lock = threading.RLock()
        self.commands = queue.Queue(maxsize=8)
        self.shutdown = threading.Event()
        self.token = secrets.token_urlsafe(32)
        self.pending_transition = None
        self.trajectory_steps = {"policy": 7518, "hil": 7518, "expert": 2400, "ros": 2400}
        self.scene_json = None
        self.pose = None
        self.camera_jpeg = None
        self.mask_jpeg = None
        self.state = {
            "phase": "loading", "mode": "policy", "step": 0, "total_steps": 7518,
            "trajectory_steps": self.trajectory_steps.copy(),
            "simulation_seconds": 0, "control_hz": 40, "physics_hz": 1000,
            "checkpoint": "reference-residual-gru-u002525", "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
            "scene": "attempt-000001", "fixed_base": True, "joint_count": 43,
            "mask_source": "MuJoCo visible can pixels", "physical_commands_sent": 0,
            "ros_enabled": ros_enabled, "ros": None, "last_error": None, "restart_required": False,
            "hil_enabled": bool(policy_url), "hil": None,
            "metrics": {}, "inference_ms": None, "can_visible": False,
        }

    def status(self):
        with self.lock:
            return json.loads(json.dumps(self.state, allow_nan=False))

    def request(self, action, body):
        if not isinstance(body, dict):
            raise ValueError("Request must be an object")
        with self.lock:
            if self.state["restart_required"]:
                raise ValueError("Worker failed; restart the application before sending commands")
            phase = self.state["phase"]
            if phase == "loading":
                raise ValueError("Scene is still loading")
            if action == "run":
                if phase in {"running", "starting", "resetting"} or self.pending_transition:
                    raise ValueError("A run is already active")
                mode = body.get("mode", "policy")
                if mode not in self.trajectory_steps:
                    raise ValueError("Mode must be policy, hil, expert, or ros")
                if mode == "hil" and not self.policy_url:
                    raise ValueError("Jetson policy requires COKE_POLICY_URL or --policy-url")
                if mode == "ros" and not self.ros_enabled:
                    raise ValueError("ROS control requires the VM ROS runtime")
                maximum = self.trajectory_steps[mode]
                steps = body.get("steps", maximum)
                if type(steps) is not int or not 1 <= steps <= maximum:
                    raise ValueError(f"Steps must be an integer within 1..{maximum}")
                body = {"mode": mode, "steps": steps}
            elif action == "resume":
                if phase != "paused":
                    raise ValueError("Only a paused run can resume")
            elif action not in {"pause", "reset"}:
                raise ValueError("Unknown command")
            try:
                self.commands.put_nowait((action, body))
            except queue.Full as exc:
                raise ValueError("Command queue is busy") from exc
            if action in {"run", "resume"}:
                self.pending_transition = action
                self.state["phase"] = "starting"
            elif action == "reset":
                self.pending_transition = action
                self.state["phase"] = "resetting"
            return {"accepted": True, "action": action}

    def _update(self, **values):
        with self.lock:
            self.state.update(values)

    def run(self):
        # The scene and its OpenGL context stay on this thread for their entire
        # lifetime, including on macOS. HTTP and ROS callbacks only queue work.
        scene = policy = bridge = remote = None
        try:
            from .scene import CokeScene
            scene = CokeScene(self.root / "expert")
            policy = CachedReferenceResidualPolicy(self.root / "bundle", device="cpu")
            sensor_names = policy.contract["sensor_input_contract"]["joint_names"]
            if scene.names != sensor_names:
                raise ValueError("Scene joint names differ from the frozen checkpoint")
            joints = LiveJointFeatureAdapter(sensor_names)
            if self.policy_url:
                from .hil import RemoteSimulationPolicy
                remote = RemoteSimulationPolicy(self.policy_url, scene.names, scene.target_bounds,
                                                timeout=self.policy_timeout, token=self.policy_token)
            if self.ros_enabled:
                from .ros_bridge import ROSBridge
                bridge = ROSBridge(scene.names, camera_fovy=float(scene.model.cam_fovy[scene.model.camera("robot_rgbd").id]), joint_bounds=scene.target_bounds)
                bridge.start()
            owned = np.asarray(policy.contract["owned_indices"], dtype=int)
            references = scene.raw_references
            velocities = np.gradient(references[:, owned], .025, axis=0).astype(np.float32)
            self.trajectory_steps["policy"] = len(references)
            self.trajectory_steps["hil"] = len(references)
            self._update(trajectory_steps=self.trajectory_steps.copy())
            description = scene.scene_description()
            with self.lock:
                self.scene_json = gzip.compress(json.dumps(description, separators=(",", ":"), allow_nan=False).encode(), mtime=0)
            mode, phase, limit = "policy", "idle", len(references)
            embedding, embedded_frame = None, -1
            last_image, epoch_offset = None, 1_000_000_000
            generation = 0
            wall_started = time.monotonic()

            def reset():
                nonlocal embedding, embedded_frame, last_image, generation, epoch_offset, wall_started
                scene.reset()
                policy.reset_episode()
                epoch_offset += 100_000_000_000
                joints.reset_episode(started_at_ns=epoch_offset)
                generation = policy.vision.status()["episode_generation"]
                embedding, embedded_frame, last_image = None, -1, None
                wall_started = time.monotonic()
                if bridge:
                    bridge.clear_commands()
                if remote:
                    remote.reset()
                self._update(hil=None)

            reset()
            self._update(phase="idle")
            while not self.shutdown.is_set():
                tick = time.monotonic()
                if bridge:
                    while (event := bridge.take_event()) is not None:
                        try:
                            self.request(event, {})
                        except ValueError as exc:
                            self._update(last_error=str(exc))
                while True:
                    try:
                        action, body = self.commands.get_nowait()
                    except queue.Empty:
                        break
                    if action == "run":
                        reset()
                        mode, limit, phase = body["mode"], body["steps"], "running"
                        self._update(last_error=None, inference_ms=None)
                    elif action == "reset":
                        reset()
                        phase = "idle"
                        self._update(last_error=None, inference_ms=None)
                    elif action == "pause" and phase == "running":
                        phase = "paused"
                    elif action == "resume" and phase == "paused":
                        phase = "running"
                    with self.lock:
                        self.pending_transition = None
                        self.state["phase"] = phase
                observation = scene.observation()
                if bridge:
                    bridge.publish(observation, {"phase": phase, "mode": mode})
                camera_key = (scene.epoch, observation["camera_frame"])
                if camera_key != last_image:
                    rgb_bytes, mask_bytes = io.BytesIO(), io.BytesIO()
                    Image.fromarray(observation["rgb_u8"]).save(rgb_bytes, format="JPEG", quality=85)
                    Image.fromarray(observation["mask"].astype(np.uint8) * 255).save(mask_bytes, format="JPEG")
                    with self.lock:
                        self.camera_jpeg, self.mask_jpeg = rgb_bytes.getvalue(), mask_bytes.getvalue()
                    last_image = camera_key
                if phase == "running":
                    try:
                        if mode == "policy":
                            started = time.perf_counter()
                            now = epoch_offset + round(observation["sim_time"] * 1e9)
                            captured = epoch_offset + round(observation["camera_sim_time"] * 1e9)
                            if embedded_frame != observation["camera_frame"]:
                                with torch.inference_mode():
                                    embedding = policy.model.encoder.vision(torch.from_numpy(observation["image"]))
                                embedded_frame = observation["camera_frame"]
                            features = joints.pack(
                                OrderedJointState(tuple(scene.names), observation["q43"], observation["dq43"], now),
                                camera_captured_at_ns=captured, control_at_ns=now,
                            )
                            snapshot = VisionSnapshot(generation, embedded_frame + 1, embedded_frame, "coke-scene", captured, now, 0., embedding)
                            proposal = policy.propose(
                                joint_features=features,
                                reference_full=references[scene.frame].astype(np.float32),
                                reference_velocity=velocities[scene.frame], control_at_ns=now,
                                vision_snapshot=snapshot,
                            )
                            target = proposal["target_q_43"]
                            self._update(inference_ms=(time.perf_counter() - started) * 1000)
                        elif mode == "hil":
                            started = time.perf_counter()
                            target, compute_ms = remote.propose(observation, references[scene.frame], velocities[scene.frame])
                            elapsed = (time.perf_counter() - started) * 1000
                            self._update(inference_ms=elapsed, hil={"devices": remote.devices,
                                         "compute_ms": compute_ms, "round_trip_ms": elapsed})
                        elif mode == "expert":
                            target = scene.references[scene.frame]
                        else:
                            target = bridge.take_command(timeout=.025)
                        if target is not None:
                            if bridge and mode != "ros":
                                # Motor targets really traverse DDS, including
                                # in the bundled policy and reference demos.
                                bridge.clear_commands()
                                bridge.send_target(target)
                                received = bridge.take_command(timeout=1.0)
                                if received is None:
                                    raise RuntimeError("ROS 2 target was not received within one second")
                                if not np.allclose(received, target, atol=1e-10, rtol=0):
                                    raise RuntimeError("Another ROS publisher changed the active demo target")
                                target = received
                            scene.step(target)
                            if scene.frame >= limit:
                                phase = "completed"
                    except Exception as exc:
                        phase = "error"
                        self._update(last_error=f"{type(exc).__name__}: {exc}")
                with self.lock:
                    self.pose = scene.scene_state()
                    self.state.update(
                        phase=phase, mode=mode, step=scene.frame, total_steps=limit,
                        epoch=scene.epoch, simulation_seconds=scene.frame / 40,
                        wall_seconds=time.monotonic() - wall_started,
                        metrics=scene.metrics(), can_visible=observation["detection_valid"],
                        joint_names=scene.names, joints=observation["q43"].tolist(),
                        ros=bridge.status() if bridge else None,
                    )
                self.shutdown.wait(max(0, (.025 if phase == "running" else .1) - (time.monotonic() - tick)))
        except Exception as exc:
            self._update(phase="error", restart_required=True, last_error=f"{type(exc).__name__}: {exc}")
            # Keep the operator page available to explain initialization faults.
            self.shutdown.wait()
        finally:
            if remote:
                remote.close()
            if bridge:
                bridge.close()
            if policy:
                policy.close()
            if scene:
                scene.close()


def make_server(runtime: DemoRuntime, host="127.0.0.1", port=8892):
    from coke_demo.access import require_loopback
    require_loopback(host)
    class Handler(BaseHTTPRequestHandler):
        # Reuse browser connections for the 30 Hz pose stream instead of
        # opening a new VM-forwarded TCP connection for every request.
        protocol_version = "HTTP/1.1"
        timeout = 15

        def log_message(self, *_):
            pass

        def reply(self, code, body, content_type="application/json", *, compressed=False):
            raw = body if isinstance(body, bytes) else json.dumps(body, allow_nan=False).encode()
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(raw)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            if self.close_connection:
                self.send_header("Connection", "close")
            if compressed:
                self.send_header("Content-Encoding", "gzip")
            self.end_headers()
            try:
                self.wfile.write(raw)
            except (BrokenPipeError, ConnectionResetError):
                # The client disconnected; there is no response left to send.
                pass

        def do_GET(self):
            path = urlsplit(self.path).path
            if path == "/api/status":
                return self.reply(200, runtime.status())
            if path == "/api/session":
                return self.reply(200, {"token": runtime.token})
            if path == "/healthz":
                status = runtime.status()
                return self.reply(200 if status["phase"] not in {"loading", "error"} else 503, status)
            if path == "/api/scene":
                with runtime.lock:
                    data = runtime.scene_json
                return self.reply(200, data, compressed=True) if data else self.reply(503, {"error": "Scene loading"})
            if path == "/api/scene/state":
                with runtime.lock:
                    data = runtime.pose
                return self.reply(200, data) if data else self.reply(503, {"error": "Scene loading"})
            if path in {"/camera.jpg", "/mask.jpg"}:
                with runtime.lock:
                    data = runtime.camera_jpeg if path == "/camera.jpg" else runtime.mask_jpeg
                return self.reply(200, data, "image/jpeg") if data else self.reply(503, {"error": "Camera loading"})
            relative = "index.html" if path == "/" else path.lstrip("/")
            public = (runtime.root / "ui").resolve()
            file = (public / relative).resolve()
            if not file.is_relative_to(public) or not file.is_file():
                return self.reply(404, {"error": "Not found"})
            return self.reply(200, file.read_bytes(), mimetypes.guess_type(file.name)[0] or "application/octet-stream")

        def do_POST(self):
            if not secrets.compare_digest(self.headers.get("X-Coke-Token", ""), runtime.token):
                self.close_connection = True  # The request body is unread.
                return self.reply(403, {"error": "Reload the operator page before sending commands"})
            action = urlsplit(self.path).path.removeprefix("/api/")
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 < length <= 4096:
                    raise ValueError("Invalid request length")
                body = json.loads(self.rfile.read(length))
                return self.reply(202, runtime.request(action, body))
            except (ValueError, TypeError) as exc:
                self.close_connection = True
                return self.reply(409, {"error": str(exc)})

    return ThreadingHTTPServer((host, port), Handler)


def serve(root: Path, *, host="127.0.0.1", port=8892, ros_enabled=True,
          policy_url=None, policy_timeout=5., policy_token=""):
    runtime = DemoRuntime(root, ros_enabled=ros_enabled, policy_url=policy_url,
                          policy_timeout=policy_timeout, policy_token=policy_token)
    server = make_server(runtime, host, port)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    print(f"Coke demo: http://{host}:{server.server_port}", flush=True)
    try:
        runtime.run()
    except KeyboardInterrupt:
        # Ctrl+C requests shutdown; finally closes the server and worker.
        pass
    finally:
        runtime.shutdown.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)
