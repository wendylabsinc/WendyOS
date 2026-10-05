"""The Unitree G1: NVIDIA GR00T Decoupled WBC for legs and waist, arm IK, and grasping.

The two ONNX policies (Balance while standing, Walk while moving) come from
NVlabs/GR00T-WholeBodyControl. They run at 50 Hz and set position targets for
the 12 leg joints and 3 waist joints; PD torques follow at every physics step.
Observation layout, gains and scaling mirror the upstream sim2mujoco runner.
The arms are not part of the policy: a damped least-squares IK moves both palms
to their targets, with gravity compensation, inside the arm motors' torque limits.
"""

from __future__ import annotations

import collections

import mujoco
import numpy as np
import onnxruntime as ort
import yaml

from .world import ROBOT_DIR

CONFIG = yaml.safe_load((ROBOT_DIR / "g1_gear_wbc.yaml").read_text())
KPS = np.asarray(CONFIG["kps"], dtype=np.float64)
KDS = np.asarray(CONFIG["kds"], dtype=np.float64)
DEFAULT = np.asarray(CONFIG["default_angles"], dtype=np.float64)
CMD_SCALE = np.asarray(CONFIG["cmd_scale"], dtype=np.float64)
POLICY_JOINTS = 15          # legs + waist
BODY_JOINTS = 29
OBS = 86
HISTORY = int(CONFIG["obs_history_len"])
POLICY_PERIOD = 0.02
ARM_KP, ARM_KD, ARM_TORQUE = 120.0, 2.0, 25.0
# Each arm has one joint more than a palm pose needs. That spare freedom holds the shoulders rolled
# out by this much (rad), so the elbows stay clear of the torso while the palms do their job.
ELBOWS_OUT = 0.35
STAND_HEIGHT = 0.74
ARM_JOINTS = [f"{side}_{joint}_joint" for side in ("left", "right") for joint in
              ("shoulder_pitch", "shoulder_roll", "shoulder_yaw", "elbow", "wrist_roll", "wrist_pitch", "wrist_yaw")]
# Empty hands: arms hang by the sides, elbows a little bent (at zero the G1's forearms point forward).
RELAXED = np.array([0.12, 0.16, 0.0, 1.3, 0.0, 0.0, 0.0,
                    0.12, -0.16, 0.0, 1.3, 0.0, 0.0, 0.0])
# While walking empty-handed each arm swings with the opposite leg: shoulder pitch follows the other
# hip's pitch (both axes point the same way, so forward leg means forward arm).
ARM_SWING = 0.55
LEFT_HIP_PITCH, RIGHT_HIP_PITCH = 0, 6   # indices into the policy joints


def _session(name: str) -> ort.InferenceSession:
    options = ort.SessionOptions()
    options.intra_op_num_threads = 1
    options.inter_op_num_threads = 1
    return ort.InferenceSession(str(ROBOT_DIR / "policy" / name), options, providers=["CPUExecutionProvider"])


def _gravity_in_body(quat: np.ndarray) -> np.ndarray:
    matrix = np.zeros(9)
    mujoco.mju_quat2Mat(matrix, quat)
    return matrix.reshape(3, 3).T @ np.array([0.0, 0.0, -1.0])


def yaw_of(quat: np.ndarray) -> float:
    w, x, y, z = quat
    return float(np.arctan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z)))


class G1:
    def __init__(self, model: mujoco.MjModel, data: mujoco.MjData):
        self.m, self.d = model, data
        self.balance = _session("GR00T-WholeBodyControl-Balance.onnx")
        self.walk = _session("GR00T-WholeBodyControl-Walk.onnx")
        self.decimation = round(POLICY_PERIOD / model.opt.timestep)
        self.arm_dof = np.array([model.jnt_dofadr[model.joint(n).id] for n in ARM_JOINTS])
        self.arm_qadr = np.array([model.jnt_qposadr[model.joint(n).id] for n in ARM_JOINTS])
        self.arm_range = np.array([model.jnt_range[model.joint(n).id] for n in ARM_JOINTS])
        self.palm = [model.site("left_palm").id, model.site("right_palm").id]
        self.pelvis = model.body("pelvis").id
        self.scratch = mujoco.MjData(model)
        self.policy_updates = 0
        self.reset(0.0, 0.0, 0.0)

    def reset(self, x: float, y: float, yaw: float) -> None:
        d = self.d
        d.qpos[0:3] = [x, y, 0.793]
        d.qpos[3:7] = [np.cos(yaw / 2), 0.0, 0.0, np.sin(yaw / 2)]
        d.qpos[7:7 + POLICY_JOINTS] = DEFAULT
        d.qpos[7 + POLICY_JOINTS:7 + BODY_JOINTS] = 0.0
        d.qvel[:6 + BODY_JOINTS] = 0.0
        self.action = np.zeros(POLICY_JOINTS, dtype=np.float32)
        self.target = DEFAULT.copy()
        self.history = collections.deque([np.zeros(OBS, np.float32)] * HISTORY, maxlen=HISTORY)
        self.cmd = np.zeros(3)
        self.cmd_goal = np.zeros(3)
        self.height = STAND_HEIGHT
        self.height_goal = STAND_HEIGHT
        self.rpy = np.zeros(3)
        self.rpy_goal = np.zeros(3)
        self.arm_q = RELAXED.copy()
        d.qpos[self.arm_qadr] = RELAXED
        self.hip_mean = DEFAULT[[LEFT_HIP_PITCH, RIGHT_HIP_PITCH]].copy()
        self.steps = 0

    # --- policy -----------------------------------------------------------------------
    def _observe(self) -> np.ndarray:
        d = self.d
        obs = np.zeros(OBS, np.float32)
        obs[0:3] = self.cmd * CMD_SCALE
        obs[3] = self.height
        obs[4:7] = self.rpy
        obs[7:10] = d.qvel[3:6] * CONFIG["ang_vel_scale"]
        obs[10:13] = _gravity_in_body(d.qpos[3:7])
        q = d.qpos[7:7 + BODY_JOINTS].copy()
        q[:POLICY_JOINTS] -= DEFAULT
        obs[13:42] = q * CONFIG["dof_pos_scale"]
        obs[42:71] = d.qvel[6:6 + BODY_JOINTS] * CONFIG["dof_vel_scale"]
        obs[71:86] = self.action
        return obs

    def walking(self) -> bool:
        return float(np.linalg.norm(self.cmd)) > 0.05

    def step(self) -> None:
        d, dt = self.d, self.m.opt.timestep
        self.cmd += np.clip(self.cmd_goal - self.cmd, -1.0 * dt, 1.0 * dt)
        self.height += np.clip(self.height_goal - self.height, -0.25 * dt, 0.25 * dt)
        self.rpy += np.clip(self.rpy_goal - self.rpy, -0.5 * dt, 0.5 * dt)
        legs = (self.target - d.qpos[7:7 + POLICY_JOINTS]) * KPS - d.qvel[6:6 + POLICY_JOINTS] * KDS
        arms = ((self.arm_q - d.qpos[self.arm_qadr]) * ARM_KP - d.qvel[self.arm_dof] * ARM_KD
                + d.qfrc_bias[self.arm_dof])
        d.ctrl[:POLICY_JOINTS] = legs
        d.ctrl[POLICY_JOINTS:BODY_JOINTS] = np.clip(arms, -ARM_TORQUE, ARM_TORQUE)
        mujoco.mj_step(self.m, d)
        self.steps += 1
        if self.steps % self.decimation == 0:
            self.history.append(self._observe())
            obs = np.concatenate(self.history)[None].astype(np.float32)
            policy = self.walk if self.walking() else self.balance
            self.action = policy.run(None, {policy.get_inputs()[0].name: obs})[0].reshape(-1).astype(np.float32)
            self.target = self.action * CONFIG["action_scale"] + DEFAULT
            self.policy_updates += 1

    def fallen(self) -> bool:
        return self.d.qpos[2] < 0.35 or self.d.xmat[self.pelvis].reshape(3, 3)[2, 2] < 0.5

    # --- frames -------------------------------------------------------------------------
    def heading(self) -> float:
        return yaw_of(self.d.qpos[3:7])

    def base_frame(self) -> tuple[np.ndarray, np.ndarray]:
        yaw = self.heading()
        c, s = np.cos(yaw), np.sin(yaw)
        return np.array([self.d.qpos[0], self.d.qpos[1], 0.0]), np.array([[c, -s, 0.0], [s, c, 0.0], [0.0, 0.0, 1.0]])

    # --- empty hands -------------------------------------------------------------------------
    def relax_arms(self, blend: float = 1.0, start: np.ndarray | None = None) -> None:
        """Arms hanging by the sides, swinging with the opposite leg while walking. `blend` eases
        from `start` (the arms' joints when the hands let go) into that pose."""
        hips = self.d.qpos[7 + np.array([LEFT_HIP_PITCH, RIGHT_HIP_PITCH])]
        self.hip_mean += 0.02 * (hips - self.hip_mean)            # ~1 s average: the stance, not the stride
        swing = ARM_SWING * (hips - self.hip_mean)
        goal = RELAXED.copy()
        goal[0] += swing[1]       # left shoulder follows the right hip
        goal[7] += swing[0]       # right shoulder follows the left hip
        if start is not None and blend < 1.0:
            goal = start + (goal - start) * blend
        self.arm_q = np.clip(goal, self.arm_range[:, 0] + 0.02, self.arm_range[:, 1] - 0.02)

    # --- arm IK ---------------------------------------------------------------------------
    def solve_arms(self, targets, iterations: int = 12, damping: float = 0.05) -> None:
        """targets: [(position, rotation) for left palm, right palm] in the world frame."""
        s = self.scratch
        s.qpos[:] = self.d.qpos
        # start from where the arms really are, so the solution stays on their current branch
        # (the elbows stay put) even if they lag behind, e.g. while carrying a box together
        q = self.d.qpos[self.arm_qadr].copy()
        jp = np.zeros((3, self.m.nv))
        jr = np.zeros((3, self.m.nv))
        for _ in range(iterations):
            mujoco.mj_kinematics(self.m, s)
            mujoco.mj_comPos(self.m, s)
            step = np.zeros(14)
            for hand, (goal_p, goal_r) in enumerate(targets):
                site = self.palm[hand]
                p = s.site_xpos[site]
                r = s.site_xmat[site].reshape(3, 3)
                err_r = goal_r @ r.T
                rotvec = 0.5 * np.array([err_r[2, 1] - err_r[1, 2], err_r[0, 2] - err_r[2, 0], err_r[1, 0] - err_r[0, 1]])
                err = np.concatenate([goal_p - p, 0.4 * rotvec])
                mujoco.mj_jacSite(self.m, s, jp, jr, site)
                cols = self.arm_dof[hand * 7:(hand + 1) * 7]
                jac = np.vstack([jp[:, cols], 0.4 * jr[:, cols]])
                pinv = jac.T @ np.linalg.inv(jac @ jac.T + damping ** 2 * np.eye(6))
                dq = pinv @ err
                # in the null space of the palm task: roll the shoulder out toward ELBOWS_OUT
                roll = hand * 7 + 1
                posture = np.zeros(7)
                posture[1] = 0.3 * ((ELBOWS_OUT if hand == 0 else -ELBOWS_OUT) - q[roll])
                dq += (np.eye(7) - pinv @ jac) @ posture
                step[hand * 7:(hand + 1) * 7] = np.clip(dq, -0.2, 0.2)
            q = np.clip(q + step, self.arm_range[:, 0] + 0.02, self.arm_range[:, 1] - 0.02)
            s.qpos[self.arm_qadr] = q
        self.arm_q = q

    # --- grasp constraints --------------------------------------------------------------------
    def _local(self, body: int, point: np.ndarray) -> np.ndarray:
        return self.d.xmat[body].reshape(3, 3).T @ (point - self.d.xpos[body])

    def attach(self, weld: int, connect: int) -> None:
        """Hold a box in both hands where they are now: weld it to the left hand and pin the
        right palm to it."""
        m, d = self.m, self.d
        hand, box = m.eq_obj1id[weld], m.eq_obj2id[weld]
        inverse = np.zeros(4)
        mujoco.mju_negQuat(inverse, d.xquat[hand])
        relquat = np.zeros(4)
        mujoco.mju_mulQuat(relquat, inverse, d.xquat[box])
        m.eq_data[weld, 0:3] = 0.0
        m.eq_data[weld, 3:6] = self._local(hand, d.xpos[box])
        m.eq_data[weld, 6:10] = relquat
        m.eq_data[weld, 10] = 1.0
        palm = d.site_xpos[self.palm[1]].copy()
        m.eq_data[connect, 0:3] = self._local(m.eq_obj1id[connect], palm)
        m.eq_data[connect, 3:6] = self._local(m.eq_obj2id[connect], palm)
        d.eq_active[weld] = 1
        d.eq_active[connect] = 1

    def detach(self, weld: int, connect: int) -> None:
        self.d.eq_active[weld] = 0
        self.d.eq_active[connect] = 0
