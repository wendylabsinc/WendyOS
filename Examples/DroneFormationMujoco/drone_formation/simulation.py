"""Twelve Crazyflies flying a formation show through changing wind, in MuJoCo.

MuJoCo integrates every airframe (RK4, 500 Hz) under four rotor forces, rotor
drag torques, gravity, floor contact and wind drag. The controller runs at
250 Hz on each drone's simulated sensors.
"""

from __future__ import annotations

import argparse
import base64
import platform
import threading
import time
from collections import deque

import mujoco
import numpy as np

from .choreography import PADS, FlightPlan
from .control import SwarmController, quat_to_matrix
from .model import N_DRONES, PHYSICS_HZ, SENSOR_WIDTH, TIMESTEP, build_model
from .wind import WindField, drag_forces


CONTROL_HZ = 250
CONTROL_DECIMATION = PHYSICS_HZ // CONTROL_HZ

# Wind grid streamed to the browser: 0.5 m spacing over the flight area.
GRID_X = np.linspace(-4.0, 4.0, 17)
GRID_Y = np.linspace(-4.0, 4.0, 17)
GRID_Z = np.array([0.35, 1.0, 1.65, 2.3, 2.95])
GRID_SCALE = 0.05  # m/s per int8 step


class Simulation:
    def __init__(self, *, realtime_speed: float = 1.0) -> None:
        self.plan = FlightPlan()
        self.wind = WindField(duration=self.plan.duration)
        self.model = build_model(PADS)
        self.data = mujoco.MjData(self.model)
        self.controller = SwarmController(N_DRONES)
        self.speed = realtime_speed
        self.body_ids = np.array([mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_BODY, f"cf{i}")
                                  for i in range(N_DRONES)])
        self.floor_geom = mujoco.mj_name2id(self.model, mujoco.mjtObj.mjOBJ_GEOM, "floor")
        self.lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self.epoch = 0
        self.elapsed = 0.0  # simulated seconds since start, across loops
        self.steps = 0
        self.local_wind = np.zeros((N_DRONES, 3))
        self.reference = np.zeros((N_DRONES, 3))
        self.wind_state = self.wind.schedule(0.0)
        self.error_now = np.zeros(N_DRONES)
        self.min_separation = float("inf")
        self.drone_contacts = 0
        self.rtf_window: deque[tuple[float, float]] = deque(maxlen=240)
        self.late_resyncs = 0
        self.started_wall = time.time()
        self.error: str | None = None
        self.reset()

    # --- physics ------------------------------------------------------------------

    def reset(self) -> None:
        mujoco.mj_resetData(self.model, self.data)
        self.controller.reset()
        mujoco.mj_forward(self.model, self.data)
        self.epoch += 1

    def sensors(self) -> tuple[np.ndarray, np.ndarray, np.ndarray, np.ndarray]:
        s = self.data.sensordata.reshape(N_DRONES, SENSOR_WIDTH)
        return s[:, 0:3].copy(), s[:, 3:6].copy(), s[:, 6:10].copy(), s[:, 10:13].copy()

    def _control(self) -> None:
        t = float(self.data.time)
        segment = self.plan.state(t)
        position, velocity, quat, rates = self.sensors()
        rotation = quat_to_matrix(quat)
        target = self.plan.reference(t)
        self.reference = target[0]
        airborne = segment.airborne or segment.name == "Landing"
        integrate = airborne and bool((position[:, 2] > 0.2).all())
        rotors = self.controller.update(1.0 / CONTROL_HZ, position, velocity, rotation, rates, target,
                                        segment.motors, integrate)
        self.data.ctrl[:] = rotors.reshape(-1)

        self.wind_state = self.wind.schedule(t)
        self.local_wind = self.wind.velocity(position, t, self.wind_state)
        # Rotor drag uses the thrust the rotors are producing now, not the command.
        produced = self.data.act.reshape(N_DRONES, 4).sum(axis=1)
        self.data.xfrc_applied[self.body_ids, 0:3] = drag_forces(velocity, self.local_wind, rotation, produced)
        self.data.xfrc_applied[self.body_ids, 3:6] = 0.0

        if segment.airborne:
            self.error_now = np.linalg.norm(position - target[0], axis=1)
        else:
            self.error_now = np.zeros(N_DRONES)
        delta = position[:, None, :] - position[None, :, :]
        distance = np.linalg.norm(delta, axis=2) + np.eye(N_DRONES) * 1e3
        self.min_separation = float(distance.min())

    def step(self) -> None:
        if self.data.time >= self.plan.duration - 1e-9:
            self.reset()
        if self.steps % CONTROL_DECIMATION == 0:
            self._control()
        mujoco.mj_step(self.model, self.data)
        self.steps += 1
        self.elapsed += TIMESTEP
        for contact in self.data.contact[: self.data.ncon]:
            b1 = self.model.geom_bodyid[contact.geom1]
            b2 = self.model.geom_bodyid[contact.geom2]
            if b1 != 0 and b2 != 0:
                self.drone_contacts += 1
        if not np.isfinite(self.data.qpos).all():
            self.error = "non-finite state"
            self.reset()

    def run_headless(self, seconds: float) -> None:
        for _ in range(int(round(seconds * PHYSICS_HZ))):
            self.step()

    # --- real-time loop -----------------------------------------------------------

    def start(self) -> None:
        self._thread = threading.Thread(target=self._loop, name="physics", daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2.0)

    def _loop(self) -> None:
        wall0 = time.monotonic()
        sim0 = self.elapsed
        while not self._stop.is_set():
            wall = time.monotonic()
            due = sim0 + (wall - wall0) * self.speed
            if due - self.elapsed > 0.25:
                # Fell far behind (host stalled): resynchronise instead of racing.
                self.late_resyncs += 1
                wall0, sim0 = wall, self.elapsed
                continue
            if self.elapsed >= due:
                time.sleep(0.0015)
                continue
            with self.lock:
                batch = 0
                while self.elapsed < due and batch < 25:
                    self.step()
                    batch += 1
                self.rtf_window.append((wall, self.elapsed))

    # --- views --------------------------------------------------------------------

    def realtime_factor(self) -> float:
        if len(self.rtf_window) < 2:
            return 0.0
        (w0, s0), (w1, s1) = self.rtf_window[0], self.rtf_window[-1]
        return (s1 - s0) / max(w1 - w0, 1e-6)

    def snapshot(self) -> dict:
        """Everything the viewer needs for one frame. Call with the lock held."""
        data = self.data
        t = float(data.time)
        segment = self.plan.state(t)
        positions = data.xpos[self.body_ids]
        quats = data.xquat[self.body_ids][:, [1, 2, 3, 0]]
        thrust = data.act.reshape(N_DRONES, 4)
        mean = np.asarray(self.wind_state["mean"])
        speeds = np.linalg.norm(self.local_wind, axis=1)
        airborne = segment.airborne
        return {
            "epoch": self.epoch,
            "time": round(t, 4),
            "elapsed": round(self.elapsed, 4),
            "formation": segment.name,
            "positions": np.round(positions, 4).reshape(-1).tolist(),
            "quaternions": np.round(quats, 5).reshape(-1).tolist(),
            "rotors": np.round(thrust, 4).reshape(-1).tolist(),
            "targets": np.round(self.reference, 4).reshape(-1).tolist(),
            "links": self.plan.links(t),
            "wind": {
                "pattern": self.wind_state["name"],
                "heading": round(float(self.wind_state["heading"]), 1),
                "mean": round(float(np.linalg.norm(mean)), 2),
                "atDrones": np.round(self.local_wind, 3).reshape(-1).tolist(),
                "peakAtDrones": round(float(speeds.max()), 2),
            },
            "stats": {
                "errorRms": round(float(np.sqrt((self.error_now ** 2).mean())), 4) if airborne else None,
                "errorMax": round(float(self.error_now.max()), 4) if airborne else None,
                "minSeparation": round(self.min_separation, 3),
            },
        }

    def wind_grid(self) -> dict:
        t = float(self.data.time)
        field = self.wind.grid(t, GRID_X, GRID_Y, GRID_Z)
        quantised = np.clip(np.round(field / GRID_SCALE), -127, 127).astype(np.int8)
        return {
            "time": round(t, 4),
            "elapsed": round(self.elapsed, 4),
            "epoch": self.epoch,
            "x": [float(GRID_X[0]), float(GRID_X[-1]), len(GRID_X)],
            "y": [float(GRID_Y[0]), float(GRID_Y[-1]), len(GRID_Y)],
            "z": GRID_Z.round(3).tolist(),
            "scale": GRID_SCALE,
            "data": base64.b64encode(quantised.tobytes()).decode("ascii"),
        }

    def status(self) -> dict:
        with self.lock:
            segment = self.plan.state(float(self.data.time))
            return {
                "ready": self.error is None and self.steps > 0,
                "error": self.error,
                "mujocoVersion": mujoco.__version__,
                "drones": N_DRONES,
                "model": "Bitcraze Crazyflie 2 (MuJoCo Menagerie)",
                "physicsHz": PHYSICS_HZ,
                "controlHz": CONTROL_HZ,
                "integrator": "RK4",
                "showSeconds": self.plan.duration,
                "time": round(float(self.data.time), 3),
                "formation": segment.name,
                "windPattern": self.wind_state["name"],
                "realtimeFactor": round(self.realtime_factor(), 3),
                "lateResyncs": self.late_resyncs,
                "droneContacts": self.drone_contacts,
                "machine": platform.machine(),
            }


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the show headless and print per-formation stats.")
    parser.add_argument("--seconds", type=float, default=None)
    args = parser.parse_args()
    sim = Simulation()
    seconds = args.seconds or sim.plan.duration
    started = time.perf_counter()
    rows: dict[str, list[float]] = {}
    for step in range(int(seconds * PHYSICS_HZ)):
        sim.step()
        if step % 50 == 0:
            name = f"{sim.plan.state(sim.data.time).name} / {sim.wind_state['name']}"
            rows.setdefault(name, []).append(float(sim.error_now.max()))
    wall = time.perf_counter() - started
    for name, errors in rows.items():
        print(f"{name:32s} max error {max(errors) * 100:6.1f} cm")
    print(f"{seconds:.0f} s simulated in {wall:.1f} s ({seconds / wall:.1f}x real time); "
          f"drone contacts {sim.drone_contacts}; min separation {sim.min_separation:.2f} m")


if __name__ == "__main__":
    main()
