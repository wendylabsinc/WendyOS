"""The world, the robot, the memory and the show, stepped in real time on one thread."""

from __future__ import annotations

import argparse
import platform
import threading
import time
import traceback
from collections import deque

import mujoco
import numpy as np

from . import show
from .catalog import INBOUND, SHELVED, ZONES
from .memory import Answer, Memory
from .robot import G1
from .scene import render_bodies
from .skills import RobotFell, Skills
from .world import boxes, build_model

START = (0.0, -0.55, -np.pi / 2)


class Simulation:
    def __init__(self, *, realtime_speed: float = 1.0):
        self.model = build_model()
        self.data = mujoco.MjData(self.model)
        self.robot = G1(self.model, self.data)
        self.box_names = [name for name, _, _ in boxes()]
        self.box_items = {name: item for name, item, _ in boxes()}
        self.box_body = {name: self.model.body(name).id for name in self.box_names}
        self.box_qadr = {name: self.model.jnt_qposadr[self.model.body_jntadr[self.box_body[name]]] for name in self.box_names}
        self.box_vadr = {name: self.model.jnt_dofadr[self.model.body_jntadr[self.box_body[name]]] for name in self.box_names}
        self.skills = Skills(self.robot, {
            name: {"body": self.box_body[name], "weld": self.model.equality(f"grasp_{name}").id,
                   "connect": self.model.equality(f"grasp_right_{name}").id,
                   "half": np.array(item.size) / 2}
            for name, item, _ in boxes()})
        self.memory = Memory()
        self.render = np.array(render_bodies(self.model))
        self.lock = threading.Lock()
        self._stop = threading.Event()
        self._thread: threading.Thread | None = None
        self.speed = realtime_speed
        self.elapsed = 0.0
        self.loop_start = 0.0   # when the current loop of the show began
        self.epoch = 0
        self.falls = 0
        self.activity = ("Starting", "")
        self.events: deque[dict] = deque(maxlen=8)
        self.location: dict[str, tuple[str, int]] = {}
        self.target: dict | None = None   # the bay the robot is heading for, for the viewer
        self.rtf_window: deque[tuple[float, float]] = deque(maxlen=240)
        self.error: str | None = None
        mujoco.mj_forward(self.model, self.data)
        self._initial_box_qpos = {name: self.data.qpos[self.box_qadr[name]:self.box_qadr[name] + 7].copy()
                                  for name in self.box_names}
        self.robot.reset(*START)
        mujoco.mj_forward(self.model, self.data)
        self.show = show.run(self)

    # --- used by the show ------------------------------------------------------------------------
    def say(self, title: str, detail: str) -> None:
        self.activity = (title, detail)
        self.events.appendleft({"t": round(float(self.elapsed), 1), "title": title, "detail": detail})

    def moved(self, name: str, zone: str, bay: int) -> None:
        self.location[name] = (zone, bay)

    def box_for(self, label: str) -> str | None:
        return next((name for name, item in self.box_items.items() if item.label == label), None)

    def restock(self) -> None:
        """Put every box back where the shipment starts; detach any grasp."""
        d = self.data
        for name in self.box_names:
            d.qpos[self.box_qadr[name]:self.box_qadr[name] + 7] = self._initial_box_qpos[name]
            d.qvel[self.box_vadr[name]:self.box_vadr[name] + 6] = 0.0
        d.eq_active[:] = 0
        self.skills.held = None
        self.location = {f"box_{item.key}": (key, 0) for key, item in SHELVED.items()}
        self.location.update({f"box_{item.key}": ("cart", index) for index, item in enumerate(INBOUND)})
        mujoco.mj_forward(self.model, d)
        self.loop_start = self.elapsed
        self.epoch += 1

    # --- stepping -----------------------------------------------------------------------------
    def reset(self) -> None:
        """Start the show again from the top: robot at the start, shipment back on the cart."""
        self.robot.reset(*START)
        self.skills.reset()
        self.target = None
        self.epoch += 1
        self.show = show.run(self)

    def step(self) -> None:
        try:
            next(self.show)
        except RobotFell as error:
            self.falls += 1
            self.say("Fell", str(error))
            print(f"Fell during {self.activity[0]!r}: {error}; restarting the show", flush=True)
            self.reset()
        except Exception as error:  # keep the demo alive; the viewer and the log show what happened
            self.error = f"{type(error).__name__}: {error}"[:300]
            traceback.print_exc()
            self.say("Recovering", self.error)
            self.reset()
        self.elapsed += self.model.opt.timestep

    def run_headless(self, seconds: float) -> None:
        for _ in range(int(seconds / self.model.opt.timestep)):
            self.step()

    def start(self) -> None:
        self._thread = threading.Thread(target=self._loop, name="physics", daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2.0)
        self.memory.close()

    def _loop(self) -> None:
        wall0, sim0 = time.monotonic(), self.elapsed
        while not self._stop.is_set():
            wall = time.monotonic()
            due = sim0 + (wall - wall0) * self.speed
            if due - self.elapsed > 0.25:
                wall0, sim0 = wall, self.elapsed   # host stalled: resynchronise instead of racing
                continue
            if self.elapsed >= due:
                time.sleep(0.001)
                continue
            with self.lock:
                batch = 0
                while self.elapsed < due and batch < 40:
                    self.step()
                    batch += 1
                self.rtf_window.append((wall, self.elapsed))

    # --- views ---------------------------------------------------------------------------------
    def realtime_factor(self) -> float:
        if len(self.rtf_window) < 2:
            return 0.0
        (w0, s0), (w1, s1) = self.rtf_window[0], self.rtf_window[-1]
        return (s1 - s0) / max(w1 - w0, 1e-6)

    def snapshot(self) -> dict:
        """One frame for the viewer: body poses plus what the robot and its memory are doing.
        Call with the lock held."""
        d = self.data
        memory = self.memory
        last, log = memory.recent()
        return {
            "epoch": self.epoch,
            "elapsed": round(self.elapsed, 4),
            "time": round(self.elapsed - self.loop_start, 4),
            "positions": np.round(d.xpos[self.render], 4).reshape(-1).tolist(),
            "quaternions": np.round(d.xquat[self.render][:, [1, 2, 3, 0]], 5).reshape(-1).tolist(),
            "activity": {"title": self.activity[0], "detail": self.activity[1]},
            "events": list(self.events),
            "held": self.skills.held,
            "target": self.target,
            "walking": self.robot.walking(),
            "memory": {
                "backend": memory.backend, "detail": memory.detail, "calls": memory.calls, "error": memory.error,
                "last": None if last is None else _answer(last),
                "log": [_answer(a) for a in log],
            },
        }

    def status(self) -> dict:
        with self.lock:
            return {
                "ready": self.robot.policy_updates > 0,
                "time": round(self.elapsed, 2),
                "activity": self.activity[0],
                "memory": self.memory.backend,
                "memoryCalls": self.memory.calls,
                "memoryError": self.memory.error,
                "falls": self.falls,
                "lastError": self.error,
                "policyUpdates": self.robot.policy_updates,
                "physicsHz": round(1.0 / self.model.opt.timestep),
                "realtimeFactor": round(self.realtime_factor(), 3),
                "mujocoVersion": mujoco.__version__,
                "machine": platform.machine(),
            }


def _answer(answer: Answer) -> dict:
    return {
        "op": answer.operation, "purpose": answer.purpose, "text": answer.text, "note": answer.note,
        "roundTripMs": round(answer.round_trip_ms, 1),
        "serverMs": None if answer.server_ms is None else round(answer.server_ms, 1),
        "rows": answer.rows,
        "matches": [{"label": m.label, "kind": m.kind, "zone": m.zone, "shelf": m.shelf, "bay": m.bay,
                     "distance": round(m.distance, 4)}
                    for m in answer.matches],
    }


def main() -> None:
    parser = argparse.ArgumentParser(description="Run the warehouse show headless and print what happens.")
    parser.add_argument("--seconds", type=float, default=240.0)
    args = parser.parse_args()
    sim = Simulation()
    started = time.perf_counter()
    last = None
    for _ in range(int(args.seconds / sim.model.opt.timestep)):
        sim.step()
        if sim.activity != last:
            last = sim.activity
            print(f"{sim.elapsed:7.1f}s  {last[0]}: {last[1]}", flush=True)
    wall = time.perf_counter() - started
    print(f"{args.seconds:.0f} s simulated in {wall:.1f} s ({args.seconds / wall:.1f}x real time); "
          f"falls {sim.falls}; memory calls {sim.memory.calls} via {sim.memory.backend}")
    sim.close()


if __name__ == "__main__":
    main()
