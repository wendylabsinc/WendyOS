"""Wall-paced physics, browser pose snapshots, and a real sensor-camera renderer."""

from collections import deque
import io
import json
import os
from pathlib import Path
import resource
import sys
import threading
import time

import mujoco
import numpy as np
from PIL import Image

from .simulation import Simulation, TIMESTEP
from .scene import BrowserScene


def timing_summary(values):
    """Small, bounded wall-time summary; callers never expose raw samples."""
    ordered = sorted(values)
    if not ordered:
        return {"samples": 0, "mean": None, "p95": None, "max": None}
    index = (len(ordered) - 1) * 0.95
    lower = int(index)
    p95 = ordered[lower] + (ordered[min(lower + 1, len(ordered) - 1)] - ordered[lower]) * (index - lower)
    return {"samples": len(ordered), "mean": sum(ordered) / len(ordered),
            "p95": p95, "max": ordered[-1]}


class Runtime:
    def __init__(self, *, render=True, simulation=None, ros=False):
        self.source_digest = os.environ.get("GO2_SOURCE_DIGEST", "")
        self.world = os.environ.get("GO2_WORLD", "indoor")
        if self.world != "indoor":
            raise ValueError("this Go2 profile supports the indoor world")
        self.seed = int(os.environ.get("GO2_SEED", "0"))
        if not 0 <= self.seed <= 0xffffffff:
            raise ValueError("GO2_SEED must be an unsigned 32-bit integer")
        self.policy_bundle = json.loads((Path(__file__).resolve().parents[1] /
                                         "assets.lock.json").read_text())["bundle"]
        if simulation is None:
            from .sensors import instrumented_model
            from .simulation import DEFAULT_ASSETS
            simulation = Simulation(model=instrumented_model(DEFAULT_ASSETS))
        self.sim = simulation or Simulation()
        self.visual_detail = os.environ.get("GO2_VISUAL_DETAIL", "full")
        self.visual_model = self.sim.model
        if self.visual_detail == "balanced":
            from .sensors import instrumented_model
            visual_mesh_dir = self.sim.assets / "visuals"
            if not visual_mesh_dir.is_dir():
                raise RuntimeError("balanced visual assets missing; run tools/build_visuals.py")
            sandbox = mujoco.mj_name2id(self.sim.model, mujoco.mjtObj.mjOBJ_GEOM, "east_wall") >= 0
            self.visual_model = instrumented_model(self.sim.assets, sandbox=sandbox,
                                                  visual_mesh_dir=visual_mesh_dir)
        elif self.visual_detail != "full":
            raise ValueError("GO2_VISUAL_DETAIL must be full or balanced")
        self.scene = BrowserScene(self.sim.model, visual_model=self.visual_model)
        # Lifecycle operations take this before lock. ROS may publish without
        # holding the physics lock, while reset/pause waits for that sample.
        self.observation_lock = threading.RLock()
        self.lock = threading.RLock()
        self.observation_generation = 0
        self.sensor_generation = 0
        self.sensor_settings = {"lidar_enabled": True, "camera_enabled": True, "lidar_dropout": 0.0}
        self.camera_frame = None
        self.camera_jpeg = None
        self.camera_frames = 0
        self.camera_demand = 0.0
        self.stop_event = threading.Event()
        self.render = render
        # Software OpenGL is needed only for actual camera sensor exposures.
        if render:
            self.visual_model.vis.quality.offsamples = 0
        self.errors = {}
        self.started = time.monotonic()
        self.last_physics = self.started
        self.physics_steps = 0
        self.overruns = 0
        self.step_ms = deque(maxlen=30000)
        self.render_ms = deque(maxlen=9000)
        # Completed render cycles only. The HTTP status summarizes at most 300
        # samples per stage, outside the physics lock. These are wall times.
        self.render_stage_ms = deque(maxlen=300)
        self.command_latency_ms = deque(maxlen=30000)
        self.pending_command = None
        self.threads = []
        self.ros_commands = self.ros_bridge = None
        from .browser_lidar import BrowserLidar
        self.browser_lidar = BrowserLidar(self, ros=ros)
        if ros:
            from .commands import ROSCommands
            from .ros import ROSBridge
            self.ros_commands = ROSCommands(
                self, os.environ.get("GO2_COMMAND_SOCKET", "/run/wendy-go2/commands.sock"),
                auto_control=os.environ.get("GO2_AUTO_APP_CONTROL") == "1")
            self.ros_bridge = ROSBridge(self)

    @property
    def error(self):
        return "; ".join(f"{kind}: {message}" for kind, message in self.errors.copy().items()) or None

    def ensure_running(self):
        if self.stop_event.is_set() or not self.threads or not self.threads[0].is_alive():
            raise RuntimeError("physics runtime is not running")
        if "render" in self.errors:
            raise RuntimeError("renderer failed; restart the simulator")
        if "ros" in self.errors:
            raise RuntimeError("ROS bridge failed; restart the simulator")

    def reset(self):
        with self.observation_lock, self.lock:
            self.ensure_running()
            if self.ros_commands:
                self.ros_commands.revoke()
            self.sim.reset()
            self.observation_generation += 1
            self.camera_frame = None
            self.camera_jpeg = None
            self.pending_command = None
            self.command_latency_ms.clear()
            self.sim.policy.inference_ms.clear()
            self.errors.pop("physics", None)

    def pause(self):
        with self.observation_lock, self.lock:
            if self.ros_commands:
                self.ros_commands.revoke()
            self.sim.pause()
            self.observation_generation += 1
            self.camera_frame = None
            self.camera_jpeg = None

    def resume(self):
        with self.observation_lock, self.lock:
            self.sim.resume()
            self.observation_generation += 1
            self.camera_frame = None
            self.camera_jpeg = None

    def move_obstacle(self, position):
        if (not isinstance(position, list) or len(position) != 2 or
                any(isinstance(value, bool) or not isinstance(value, (float, int)) or
                    not np.isfinite(value) or abs(value) > 5.3 for value in position)):
            raise ValueError("obstacle position must contain x/y coordinates inside the room")
        with self.observation_lock, self.lock:
            self.ensure_running()
            body = mujoco.mj_name2id(self.sim.model, mujoco.mjtObj.mjOBJ_BODY, "sandbox_obstacle")
            if body < 0:
                raise ValueError("this world has no movable obstacle")
            if np.linalg.norm(np.array(position) - self.sim.data.qpos[:2]) < 1.0:
                raise ValueError("place the obstacle at least one metre from the robot")
            mocap = self.sim.model.body_mocapid[body]
            self.sim.data.mocap_pos[mocap] = [*position, 0.4]
            mujoco.mj_forward(self.sim.model, self.sim.data)
            self.observation_generation += 1
            self.camera_frame = self.camera_jpeg = None
            return {"position": [*position, 0.4]}

    def configure_sensors(self, changes):
        """Apply a fault scenario without changing physics or control ownership."""
        if not isinstance(changes, dict) or set(changes) - set(self.sensor_settings):
            raise ValueError("sensor settings accept only lidar_enabled, camera_enabled and lidar_dropout")
        for key in ("lidar_enabled", "camera_enabled"):
            if key in changes and type(changes[key]) is not bool:
                raise ValueError(f"{key} must be a boolean")
        if "lidar_dropout" in changes:
            value = changes["lidar_dropout"]
            if (isinstance(value, bool) or not isinstance(value, (int, float))
                    or not 0 <= value <= 1 or not np.isfinite(value)):
                raise ValueError("lidar_dropout must be a finite probability from zero to one")
        with self.observation_lock, self.lock:
            self.ensure_running()
            settings = {**self.sensor_settings, **changes}
            settings["lidar_dropout"] = float(settings["lidar_dropout"])
            changed = settings != self.sensor_settings
            if changed:
                self.sensor_settings = settings
                self.sensor_generation += 1
                self.observation_generation += 1
                self.camera_frame = self.camera_jpeg = None
            return {"sensor_settings": self.sensor_settings.copy(),
                    "generation": self.sensor_generation, "changed": changed}

    def start(self):
        if self.threads or self.stop_event.is_set():
            raise RuntimeError("runtime cannot be started twice")
        targets = [self._physics]
        if self.render:
            targets.append(self._render)
        for target in targets:
            thread = threading.Thread(target=target, daemon=True)
            self.threads.append(thread)
            thread.start()
        if self.ros_commands:
            self.ros_commands.start()
            self.ros_bridge.start()

    def close(self):
        self.pause()
        self.stop_event.set()
        if self.ros_bridge:
            self.ros_bridge.close()
            self.ros_commands.close()
        for thread in self.threads:
            thread.join(timeout=5)

    def _physics(self):
        deadline = time.monotonic()
        while not self.stop_event.is_set():
            if "physics" in self.errors:
                self.stop_event.wait(0.05)
                deadline = time.monotonic()
                continue
            try:
                started = time.perf_counter()
                with self.lock:
                    policy_tick = self.sim._steps % self.sim.decimation == 0
                    paused = self.sim.mode == "paused"
                    self.sim.step()
                    if not paused:
                        self.physics_steps += 1
                        self.last_physics = time.monotonic()
                        if self.ros_bridge and self.sim.mode != "fault":
                            self.ros_bridge.snapshots.capture(self)
                    if policy_tick and not paused and self.pending_command is not None:
                        self.command_latency_ms.append((time.monotonic() - self.pending_command) * 1000)
                        self.pending_command = None
                self.step_ms.append((time.perf_counter() - started) * 1000)
                deadline += TIMESTEP
                remaining = deadline - time.monotonic()
                if remaining < -0.1:
                    self.overruns += 1
                    deadline = time.monotonic()
                if remaining > 0:
                    self.stop_event.wait(remaining)
            except Exception as exc:
                with self.lock:
                    self.errors["physics"] = str(exc)
                    self.sim.pause()
                    self.sim.mode = "fault"

    def _render(self):
        renderer = None
        try:
            # OpenGL contexts must be created/used/destroyed on this thread.
            model = self.visual_model
            robot_camera = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_CAMERA, "front")
            if robot_camera < 0:
                return
            renderer = mujoco.Renderer(model, height=360, width=640)
            data = mujoco.MjData(model)
            options = mujoco.MjvOption()
            options.geomgroup[3] = False
            from .camera import CameraFrame
            while not self.stop_event.is_set():
                started = time.perf_counter()
                thread_started = time.thread_time()
                stages = {}
                wanted = self.camera_wanted()
                with self.lock:
                    stages["capture_lock_wait"] = (time.perf_counter() - started) * 1000
                    data.qpos[:] = self.sim.data.qpos
                    data.qvel[:] = self.sim.data.qvel
                    data.time = self.sim.data.time
                    data.mocap_pos[:] = self.sim.data.mocap_pos
                    data.mocap_quat[:] = self.sim.data.mocap_quat
                    epoch = self.sim.epoch
                    generation = self.observation_generation
                    wall_timestamp_ns = time.time_ns()
                    emit_camera = (wanted and self.sensor_settings["camera_enabled"]
                                   and self.sim.mode not in {"paused", "fault"})
                stages["capture_lock"] = (time.perf_counter() - started) * 1000
                if not emit_camera:
                    self.stop_event.wait(1 / 15)
                    continue
                if not (np.isfinite(data.qpos).all() and np.isfinite(data.qvel).all()
                        and np.isfinite(data.mocap_pos).all() and np.isfinite(data.mocap_quat).all()):
                    self.stop_event.wait(1 / 15)
                    continue
                stage_started = time.perf_counter()
                mujoco.mj_forward(model, data)
                stages["mj_forward"] = (time.perf_counter() - stage_started) * 1000
                stage_started = time.perf_counter()
                renderer.update_scene(data, camera=robot_camera, scene_option=options)
                renderer.scene.flags[mujoco.mjtRndFlag.mjRND_SHADOW] = False
                renderer.scene.flags[mujoco.mjtRndFlag.mjRND_REFLECTION] = False
                stages["camera_scene"] = (time.perf_counter() - stage_started) * 1000
                stage_started = time.perf_counter()
                rgb = renderer.render()
                stages["camera_render"] = (time.perf_counter() - stage_started) * 1000
                stage_started = time.perf_counter()
                frame = CameraFrame(epoch, generation, float(data.time), wall_timestamp_ns,
                                    rgb)
                stages["camera_copy"] = (time.perf_counter() - stage_started) * 1000
                stage_started = time.perf_counter()
                camera_buffer = io.BytesIO()
                Image.fromarray(frame.rgb).save(camera_buffer, format="JPEG", quality=80)
                stages["camera_jpeg"] = (time.perf_counter() - stage_started) * 1000
                # A lifecycle operation may finish during software rendering.
                # Never make that old exposure available to a ROS publisher.
                stage_started = time.perf_counter()
                with self.observation_lock, self.lock:
                    stages["publish_fence_wait"] = (time.perf_counter() - stage_started) * 1000
                    if (epoch == self.sim.epoch and generation == self.observation_generation
                            and self.sim.mode not in {"paused", "fault"}
                            and self.sensor_settings["camera_enabled"]):
                        self.camera_frame = frame
                        self.camera_jpeg = camera_buffer.getvalue()
                        self.camera_frames += 1
                stages["publish_fence"] = (time.perf_counter() - stage_started) * 1000
                stages["work_total"] = (time.perf_counter() - started) * 1000
                # Excludes Mesa worker threads; compare with wall work_total
                # to identify time waiting for GL workers, locks, or the GIL.
                stages["work_total_thread_cpu"] = (time.thread_time() - thread_started) * 1000
                self.render_stage_ms.append(stages)
                self.render_ms.append((time.perf_counter() - started) * 1000)
                self.stop_event.wait(max(0, 1 / 15 - (time.perf_counter() - started)))
        except Exception as exc:
            self.errors["render"] = str(exc)
        finally:
            if renderer is not None:
                renderer.close()

    def camera_wanted(self):
        """Render the sensor camera only while something consumes it: a recent
        browser /camera.jpg request or a live ROS image subscriber. Software
        OpenGL is orders slower than a device GPU, so an always-on exposure loop
        would run flat-out and starve wall-paced physics."""
        if time.monotonic() - self.camera_demand < 2.0:
            return True
        node = getattr(self.ros_bridge, "slow_node", None)
        try:
            return bool(node and node.image_pub.get_subscription_count())
        except Exception:  # ponytail: any rmw hiccup keeps the camera live
            return True

    def command(self, values, token):
        with self.lock:
            self.ensure_running()
            self.sim.command_velocity(*values, token=token)
            if self.pending_command is None:
                self.pending_command = time.monotonic()

    def status(self):
        elapsed = max(time.monotonic() - self.started, 0.001)
        with self.lock:
            result = self.sim.snapshot()
            result["sensor_settings"] = self.sensor_settings.copy()
            result["sensor_generation"] = self.sensor_generation
            inference = list(self.sim.policy.inference_ms)
            policy_updates = self.sim.policy.updates
            if self.ros_commands:
                result["ros_commands"] = self.ros_commands.status()
                result["ros"] = self.ros_bridge.status()
        # Failed physics may contain NaNs. Keep diagnostics valid JSON even then.
        def finite(value):
            if isinstance(value, float) and not np.isfinite(value):
                return None
            if isinstance(value, list):
                return [finite(item) for item in value]
            if isinstance(value, dict):
                return {key: finite(item) for key, item in value.items()}
            return value
        result = finite(result)
        percentile = lambda values: float(np.percentile(list(values), 95)) if values else None
        healthy = (self.error is None and not self.stop_event.is_set() and
                   bool(self.threads) and self.threads[0].is_alive())
        usage = resource.getrusage(resource.RUSAGE_SELF)
        rss_bytes = usage.ru_maxrss * (1 if sys.platform == "darwin" else 1024)
        render_stages = list(self.render_stage_ms)
        render_stage_summary = {
            stage: timing_summary(record[stage] for record in render_stages if stage in record)
            for stage in {stage for record in render_stages for stage in record}
        }
        result.update({
            "robot": "go2", "simulation": True, "profile_version": 1,
            "robot_kind": "go2", "source_digest": self.source_digest,
            "vm_name": os.environ.get("GO2_VM_NAME", ""),
            "dds_isolation": os.environ.get("GO2_ISOLATION_STATUS", "unmanaged"),
            "policy_bundle": self.policy_bundle, "world": self.world,
            "clock_mode": "device", "seed": self.seed,
            "visual_detail": self.visual_detail,
            "healthy": healthy,
            "ready": healthy and result["mode"] in {"standing", "moving"} and result["time"] > 0.5 and
                     time.monotonic() - self.last_physics < 0.5,
            "error": self.error,
            "metrics": {
                "wall_seconds": elapsed,
                "real_time_factor": self.physics_steps * TIMESTEP / elapsed,
                "scene_states": self.scene.samples,
                "camera_frames": self.camera_frames,
                "camera_fps": self.camera_frames / elapsed,
                "physics_steps": self.physics_steps,
                "physics_age_ms": (time.monotonic() - self.last_physics) * 1000,
                "policy_updates": policy_updates,
                "physics_timestep_seconds": TIMESTEP,
                "rss_bytes": rss_bytes,
                "cpu_seconds": usage.ru_utime + usage.ru_stime,
                "overruns": self.overruns,
                "policy_p95_ms": percentile(inference),
                "physics_p95_ms": percentile(self.step_ms),
                "render_p95_ms": percentile(self.render_ms),
                "render_stage_ms": render_stage_summary,
                "command_p95_ms": percentile(self.command_latency_ms),
            },
        })
        if self.ros_bridge:
            result["ready"] = (result["ready"] and result["ros"]["running"] and result["ros"]["fresh"] and
                               all(result["ros"]["samples"].get(topic, 0) > 0
                                   for topic in ("imu", "joints", "odom", "scan")))
        return result
