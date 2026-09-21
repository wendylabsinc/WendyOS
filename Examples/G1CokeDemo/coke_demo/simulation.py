"""Interactive native MuJoCo replay using the supplied expert controller."""
from __future__ import annotations

import hashlib
import json
import queue
import time
from pathlib import Path

import mujoco
import numpy as np


class Simulation:
    def __init__(self, expert: Path):
        # The repository lock is the trust root, including verification.json
        # and the helper/controller inputs used after model construction.
        lock = json.loads((Path(__file__).resolve().parents[1] / "assets.lock.json").read_text())
        for relative, spec in lock["files"].items():
            if not relative.startswith("expert/"):
                continue
            name = relative.removeprefix("expert/")
            with (expert / name).open("rb") as stream:
                hasher = hashlib.sha256()
                for chunk in iter(lambda: stream.read(1024 * 1024), b""):
                    hasher.update(chunk)
                digest = hasher.hexdigest()
            if digest != spec["sha256"]:
                raise ValueError(f"Expert checksum mismatch: {name}")
        self.model = mujoco.MjModel.from_binary_path(str(expert / "model.mjb"))
        if abs(self.model.opt.timestep - .001) > 1e-12:
            raise ValueError("Expected the supplied 1 kHz model")
        self.data = mujoco.MjData(self.model)
        self.result = json.loads((expert / "result.json").read_text())
        self.qualification = json.loads((expert / "qualification-60s.json").read_text())
        with np.load(expert / "rollout.npz", allow_pickle=False) as archive:
            self.initial_state = archive["initial_state"].copy()
            trace = archive["trace"].copy()
            names = archive["joint_names"].tolist()
        # Import the exact source interpolation after MuJoCo has selected its
        # native platform. The CLI sets the platform before this module loads.
        from expert.replay_expert import smooth_retime, UPPER, HAND

        self.raw_references = trace[:, 87:].copy()
        self.references = smooth_retime(self.raw_references, 2400)
        self.names = names
        joints = self.model.actuator_trnid[:, 0]
        self.order = np.array([names.index(self.model.joint(int(j)).name) for j in joints])
        self.qa = self.model.jnt_qposadr[[self.model.joint(name).id for name in names]]
        self.aq = self.model.jnt_qposadr[joints]
        self.av = self.model.jnt_dofadr[joints]
        self.owned = np.isin(self.order, UPPER) | np.isin(self.order, HAND)
        self.locked = np.isin(self.order, [13, 14])
        controller = self.result["controller"]
        kp, kd = np.full(43, 60.0), np.full(43, 1.5)
        kp[[19, 20, 21, 33, 34, 35]] = 40
        kp[HAND] = controller.get("hand_kp_by_joint", [controller["hand_kp"]] * 7)
        kd[HAND] = controller["hand_kd"]
        self.kp, self.kd = kp[self.order], kd[self.order]
        self.bounds = self.model.jnt_actfrcrange[joints]
        self.can = self.model.body("bottle_body").id
        Simulation.reset(self)

    def reset(self):
        mujoco.mj_resetData(self.model, self.data)
        mujoco.mj_setState(self.model, self.data, self.initial_state, mujoco.mjtState.mjSTATE_INTEGRATION)
        mujoco.mj_forward(self.model, self.data)
        self.start = self.data.qpos[self.qa].copy()
        self.initial_can = self.data.xpos[self.can].copy()
        self.frame = 0
        self.max_lift = 0.0

    def step(self):
        if self.frame >= len(self.references):
            return False
        target = self.references[self.frame]
        for _ in range(25):
            base = 100 * (self.start[self.order] - self.data.qpos[self.aq]) - 4 * self.data.qvel[self.av] + self.data.qfrc_bias[self.av]
            pd = self.kp * (target[self.order] - self.data.qpos[self.aq]) - self.kd * self.data.qvel[self.av]
            torque = np.where(self.owned, pd, base)
            torque[self.locked] = 0
            self.data.ctrl[:] = np.clip(torque, self.bounds[:, 0], self.bounds[:, 1])
            mujoco.mj_step(self.model, self.data)
            self.max_lift = max(self.max_lift, float(self.data.xpos[self.can, 2] - self.initial_can[2]))
        if not np.isfinite(self.data.qpos).all() or not np.isfinite(self.data.qvel).all():
            raise RuntimeError("Simulation produced non-finite state")
        self.frame += 1
        return True

    def metrics(self):
        return {
            "source": "attempt-000001", "controller": "recorded expert PD targets",
            "steps": self.frame, "duration_seconds": self.frame / 40,
            "maximum_lift_m": self.max_lift,
            "final_placement_distance_m": float(np.linalg.norm(self.data.xpos[self.can, :2] - np.asarray(self.result["marker"][:2]))),
            "physical_commands_sent": 0,
        }


def run(expert: Path, *, headless=False, steps=2400):
    sim = Simulation(expert)
    if headless:
        for _ in range(steps):
            sim.step()
        return sim.metrics()
    import mujoco.viewer

    events = queue.SimpleQueue()
    paused = False
    print("Coke demo | Expert simulation | Space: pause | R: restart | Esc: close", flush=True)
    with mujoco.viewer.launch_passive(sim.model, sim.data, key_callback=events.put) as viewer:
        viewer.cam.lookat[:] = [0, 0, .95]
        viewer.cam.distance = 2.8
        viewer.cam.azimuth = -55
        viewer.cam.elevation = -28
        while viewer.is_running():
            tick = time.perf_counter()
            with viewer.lock():
                while not events.empty():
                    key = events.get_nowait()
                    if key == 32:
                        paused = not paused
                    elif key in (82, 114):
                        sim.reset()
                        paused = False
                if not paused:
                    if not sim.step():
                        paused = True
                        print(json.dumps(sim.metrics(), indent=2), flush=True)
                if sim.frame % 40 == 0 and not paused:
                    print(f"\rExpert {sim.frame / 40:4.0f}/60 s | Lift {sim.max_lift * 100:.1f} cm", end="", flush=True)
            viewer.sync()
            time.sleep(max(0, .025 - (time.perf_counter() - tick)))
    return sim.metrics()
