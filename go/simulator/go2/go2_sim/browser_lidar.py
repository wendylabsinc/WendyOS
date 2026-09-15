"""Shared browser lidar observations from actual, coherently captured rays.

ROS supplies its exact cloud and mounting pose when enabled. Standalone browser
sessions share one demand-driven 10 Hz sampler; additional clients do not cast
additional rays. Kinematics, raycasts and serialization run outside physics lock.
"""

import threading
import time

import mujoco
import numpy as np

from .lidar import Lidar
from .scene import compact_json, numbers, xyzw
from .sensors import PhysicsSampler


PERIOD = 0.1
MAX_AGE = 0.5


class BrowserLidar:
    def __init__(self, runtime, *, ros=False):
        self.runtime = runtime
        self.ros = ros
        self.available = mujoco.mj_name2id(runtime.sim.model, mujoco.mjtObj.mjOBJ_SITE, "lidar") >= 0
        self.lock = threading.Lock()
        self.record = None
        self.sampler = self.lidar = None
        self.lidar_identity = None
        self.samples = 0

    def prepare(self, result, sampler, generation):
        """Convert the very same sensor-space cloud published to ROS."""
        origin = sampler.data.site_xpos[sampler.lidar_id].copy()
        rotation = sampler.data.site_xmat[sampler.lidar_id].reshape(3, 3)
        points = result["xyz"] @ rotation.T + origin
        if not (np.isfinite(points).all() and np.isfinite(origin).all()
                and np.isfinite(rotation).all() and np.isfinite(result["time"])):
            return None
        quaternion = np.empty(4)
        mujoco.mju_mat2Quat(quaternion, rotation.reshape(-1))
        return {
            "scene_id": self.runtime.scene.description["id"],
            "epoch": result["epoch"], "generation": generation,
            "time": result["time"], "mode": sampler.mode,
            "enabled": True, "available": True, "fresh": True,
            "origin": numbers(origin), "quaternion": xyzw(quaternion),
            "points": np.round(points, 5).reshape(-1).tolist(),
            "captured_at": time.monotonic() - max(0, (time.time_ns() - result["wall_timestamp_ns"]) / 1e9),
        }

    def _current(self, record):
        runtime = self.runtime
        return (record is not None and record["epoch"] == runtime.sim.epoch
                and record["generation"] == runtime.observation_generation
                and runtime.sensor_settings["lidar_enabled"]
                and runtime.sim.mode not in {"paused", "fault"}
                and not runtime.stop_event.is_set())

    def publish(self, record):
        """Fence a prepared ROS cloud without retaining mutable sensor arrays."""
        with self.lock, self.runtime.lock:
            if self._current(record):
                self.record = record
                self.samples += 1

    def _sample(self):
        runtime = self.runtime
        if self.sampler is None:
            self.sampler = PhysicsSampler(runtime.sim)
        sampler = self.sampler
        with runtime.lock:
            sim = runtime.sim
            if (sim.mode in {"paused", "fault"} or runtime.stop_event.is_set()
                    or not runtime.sensor_settings["lidar_enabled"]
                    or not np.isfinite(sim.data.qpos).all()
                    or not np.isfinite(sim.data.qvel).all()
                    or not np.isfinite(sim.data.mocap_pos).all()
                    or not np.isfinite(sim.data.mocap_quat).all()
                    or not np.isfinite(sim.data.time)):
                return
            generation = runtime.observation_generation
            if (self.record is not None and self.record["epoch"] == sim.epoch
                    and self.record["generation"] == generation
                    and self.record["time"] == float(sim.data.time)):
                return  # Frozen physics cannot create a fresh sensor timestamp.
            lidar_identity = (sim.epoch, runtime.sensor_generation)
            dropout = runtime.sensor_settings["lidar_dropout"]
            mujoco.mj_getState(sampler.model, sim.data, sampler.integration_state, sampler.signature)
            sampler.epoch, sampler.mode = sim.epoch, sim.mode
            sampler.wall_timestamp_ns = time.time_ns()
        # Only the integration copy above touches live physics. Forward dynamics
        # and all five rings use this worker's private data and ray-only model.
        mujoco.mj_setState(sampler.model, sampler.data, sampler.integration_state, sampler.signature)
        mujoco.mj_forward(sampler.model, sampler.data)
        if lidar_identity != self.lidar_identity:
            self.lidar = Lidar(seed=runtime.seed, dropout=dropout)
            self.lidar_identity = lidar_identity
        result = self.lidar.sample(sampler)
        record = self.prepare(result, sampler, generation)
        with runtime.lock:
            if self._current(record):
                self.record = record
                self.samples += 1

    def state_json(self):
        runtime = self.runtime
        with self.lock:
            with runtime.lock:
                active = (self.available and runtime.sensor_settings["lidar_enabled"]
                          and runtime.sim.mode not in {"paused", "fault"}
                          and not runtime.stop_event.is_set())
                current = self._current(self.record)
            now = time.monotonic()
            if (active and not self.ros
                    and (not current or now - self.record["captured_at"] >= PERIOD)):
                self._sample()
            with runtime.lock:
                current = self._current(self.record)
                age = time.monotonic() - self.record["captured_at"] if current else None
                if current and 0 <= age <= MAX_AGE:
                    result = {key: value for key, value in self.record.items() if key != "captured_at"}
                    result["age_ms"] = round(age * 1000, 1)
                else:
                    sim_time = float(runtime.sim.data.time)
                    result = {
                        "scene_id": runtime.scene.description["id"], "epoch": runtime.sim.epoch,
                        "generation": runtime.observation_generation,
                        "time": sim_time if np.isfinite(sim_time) else None,
                        "mode": runtime.sim.mode, "enabled": runtime.sensor_settings["lidar_enabled"],
                        "available": self.available, "fresh": False, "age_ms": None,
                        "origin": None, "quaternion": None, "points": [],
                    }
            return compact_json(result)
