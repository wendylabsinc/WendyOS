"""Bounded, single-use observations captured on actual physics steps.

The physics worker only copies integration state. MuJoCo forward dynamics and
ROS serialization run on the observation worker. Capture timestamps survive
scheduling delays; no state is duplicated or stamped again to fill a rate gap.
"""

from collections import deque
from dataclasses import dataclass
import threading
import time

import mujoco
import numpy as np


@dataclass(frozen=True)
class PhysicsSnapshot:
    integration_state: np.ndarray
    epoch: int
    generation: int
    mode: str
    control_mode: str
    wall_timestamp_ns: int
    monotonic_ns: int


class SnapshotQueue:
    def __init__(self, simulation, *, capacity=10, max_age_ns=20_000_000,
                 monotonic_ns=time.monotonic_ns, wall_ns=time.time_ns):
        self.model = simulation.model
        self.signature = mujoco.mjtState.mjSTATE_INTEGRATION
        self.size = mujoco.mj_stateSize(self.model, self.signature)
        self.capacity, self.max_age_ns = capacity, max_age_ns
        self.monotonic_ns, self.wall_ns = monotonic_ns, wall_ns
        self.queue = deque()
        self.lock = threading.Lock()
        self.captured = self.consumed = self.overflow = self.expired = 0

    def capture(self, runtime):
        """Called immediately after an advancing step, with runtime.lock held."""
        state = np.empty(self.size)
        mujoco.mj_getState(self.model, runtime.sim.data, state, self.signature)
        snapshot = PhysicsSnapshot(
            state, runtime.sim.epoch, runtime.observation_generation,
            runtime.sim.mode, runtime.sim.control_mode, self.wall_ns(), self.monotonic_ns())
        with self.lock:
            if len(self.queue) >= self.capacity:
                self.queue.popleft()
                self.overflow += 1
            self.queue.append(snapshot)
            self.captured += 1

    def take(self):
        with self.lock:
            now = self.monotonic_ns()
            while self.queue:
                snapshot = self.queue.popleft()
                age = now - snapshot.monotonic_ns
                if age < 0 or age > self.max_age_ns:
                    self.expired += 1
                    continue
                self.consumed += 1
                return snapshot
        return None

    def status(self):
        with self.lock:
            return {"captured": self.captured, "consumed": self.consumed,
                    "overflow": self.overflow, "expired": self.expired,
                    "pending": len(self.queue), "capacity": self.capacity,
                    "max_age_ms": self.max_age_ns / 1e6}


def load_snapshot(sampler, snapshot):
    """Recompute coherent derived values on the consumer's private MjData."""
    sampler.epoch = snapshot.epoch
    sampler.mode = snapshot.mode
    sampler.control_mode = snapshot.control_mode
    sampler.wall_timestamp_ns = snapshot.wall_timestamp_ns
    mujoco.mj_setState(sampler.model, sampler.data, snapshot.integration_state, sampler.signature)
    mujoco.mj_forward(sampler.model, sampler.data)
