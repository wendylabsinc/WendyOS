"""Single-owner MuJoCo world with leased commands and a physical G1 controller.

Only reset sets the floating base pose. Normal movement is exclusively through
actuator torques and mj_step. Callers serialize access to this object.
"""

from pathlib import Path
import math
import secrets
import time

import mujoco
import numpy as np

from .policy import WalkingPolicy, JOINT_COUNT


DEFAULT_ASSETS = Path(__file__).resolve().parents[1] / "assets"
TIMESTEP = 0.002
COMMAND_TIMEOUT = 0.2
LOW_LEVEL_TIMEOUT = 0.04
VELOCITY_MIN = np.array([-0.5, -0.3, -0.2])
VELOCITY_LIMITS = np.array([1.0, 0.3, 0.2])
JOINT_NAMES = [
    "left_hip_pitch_joint", "left_hip_roll_joint", "left_hip_yaw_joint",
    "left_knee_joint", "left_ankle_pitch_joint", "left_ankle_roll_joint",
    "right_hip_pitch_joint", "right_hip_roll_joint", "right_hip_yaw_joint",
    "right_knee_joint", "right_ankle_pitch_joint", "right_ankle_roll_joint",
    "waist_yaw_joint", "waist_roll_joint", "waist_pitch_joint",
    "left_shoulder_pitch_joint", "left_shoulder_roll_joint", "left_shoulder_yaw_joint",
    "left_elbow_joint", "left_wrist_roll_joint", "left_wrist_pitch_joint", "left_wrist_yaw_joint",
    "right_shoulder_pitch_joint", "right_shoulder_roll_joint", "right_shoulder_yaw_joint",
    "right_elbow_joint", "right_wrist_roll_joint", "right_wrist_pitch_joint", "right_wrist_yaw_joint",
]


class Simulation:
    def __init__(self, asset_dir=None, *, monotonic=time.monotonic, model=None):
        self.assets = Path(asset_dir) if asset_dir is not None else DEFAULT_ASSETS
        self.policy = WalkingPolicy(self.assets)
        self.config = {"model_joint_names": JOINT_NAMES.copy()}
        self.model = model if model is not None else mujoco.MjModel.from_xml_path(str(self.assets / "robot/scene_29dof.xml"))
        if (self.model.nq, self.model.nv, self.model.nu) != (36, 35, JOINT_COUNT):
            raise ValueError("G1 world requires one floating 29-DoF robot")
        self.model.opt.timestep = TIMESTEP
        self.data = mujoco.MjData(self.model)
        self._clock = monotonic
        self.joint_names = self.config["model_joint_names"]
        joint_ids = [self.model.joint(name).id for name in self.joint_names]
        self.joint_ranges = self.model.jnt_range[joint_ids].copy()
        self.qaddr = self.model.jnt_qposadr[joint_ids]
        self.vaddr = self.model.jnt_dofadr[joint_ids]
        actuator_joints = list(self.model.actuator_trnid[:, 0])
        self.actuators = np.array([actuator_joints.index(joint) for joint in joint_ids])
        self.torque_ranges = self.model.actuator_ctrlrange[self.actuators].copy()
        if not self.model.actuator_ctrllimited[self.actuators].all():
            raise ValueError("G1 actuators must have physical torque limits")
        if not np.array_equal(self.model.actuator_gear[self.actuators],
                              np.tile([1., 0., 0., 0., 0., 0.], (JOINT_COUNT, 1))):
            raise ValueError("G1 torque motors require unit transmission gear")
        self.body_id = self.model.body("pelvis").id
        self.imu_id = self.model.site("imu").id
        self.gyro = self.data.sensor("imu_gyro").data
        root_joint = self.model.body_jntadr[self.body_id]
        if (self.model.jnt_type[root_joint] != mujoco.mjtJoint.mjJNT_FREE
                or self.model.jnt_qposadr[root_joint] != 0 or self.model.jnt_dofadr[root_joint] != 0):
            raise ValueError("G1 pelvis must have an unconstrained floating joint")
        if self.model.neq:
            raise ValueError("G1 walking world must not contain support equality constraints")
        self.decimation = round(self.policy.period / TIMESTEP)
        if self.decimation < 1 or not math.isclose(self.decimation * TIMESTEP, self.policy.period):
            raise ValueError("policy period must be an integer number of physics steps")
        self.epoch = 0
        self.reset()

    def reset(self):
        self.epoch += 1
        self.owner = None
        self._last_received = None
        self._last_clock = self._clock()
        self.command = np.zeros(3)
        self.applied_command = np.zeros(3)
        self.target = self.policy.default.copy()
        self.control_mode = "sport"
        self._low_level = None
        self._low_level_received = None
        self._transition = None
        self._force_policy_update = True
        self._steps = 0
        self.policy.reset()
        mujoco.mj_resetData(self.model, self.data)
        self.data.qpos[:7] = [0, 0, 0.793, 1, 0, 0, 0]
        self.data.qpos[self.qaddr] = self.policy.default
        self.data.qvel[:] = 0
        mujoco.mj_forward(self.model, self.data)
        self.mode = "standing"

    def arm(self, mode="sport"):
        if mode not in {"sport", "lowlevel"}:
            raise ValueError("control mode must be sport or lowlevel")
        if self.mode in {"paused", "fallen", "damping", "fault", "zero_torque"}:
            raise ValueError(f"cannot arm a {self.mode} robot")
        if self.owner is not None:
            raise PermissionError("another command source already owns the robot")
        self.owner = secrets.token_urlsafe(24)
        self.control_mode = mode
        if mode == "lowlevel":
            self.command[:] = 0
            self.applied_command[:] = 0
            self._last_received = None
            self._low_level = None
            self._low_level_received = self._clock()
            self.policy.reset()
            self.mode = "lowlevel"
        return self.owner

    def _require_owner(self, token):
        if self.owner is None or not isinstance(token, str) or not secrets.compare_digest(token, self.owner):
            raise PermissionError("command ownership expired; acquire a new control grant")

    def command_velocity(self, vx, vy, wz, token=None):
        self._require_owner(token)
        self._require_sport()
        if self.mode not in {"standing", "moving"}:
            raise ValueError(f"cannot move a {self.mode} robot")
        if any(isinstance(v, (bool, np.bool_)) or not isinstance(v, (int, float, np.integer, np.floating)) for v in (vx, vy, wz)):
            raise ValueError("velocity must contain three finite numbers")
        command = np.asarray([vx, vy, wz], dtype=np.float64)
        if not np.isfinite(command).all() or (np.any(command < VELOCITY_MIN) or np.any(command > VELOCITY_LIMITS)):
            raise ValueError(f"velocity requires {VELOCITY_MIN.tolist()} <= [vx, vy, wz] <= {VELOCITY_LIMITS.tolist()}")
        self.command[:] = command
        self._last_received = self._clock()

    def stop(self, token=None):
        self._require_owner(token)
        self._require_sport()
        self.command[:] = 0
        self._last_received = None

    def release(self, token=None):
        self._require_owner(token)
        if self.control_mode == "lowlevel":
            self._safe_mode("damping")
            return
        self.stop(token)
        self.owner = None

    def _require_sport(self):
        if self.control_mode != "sport":
            raise ValueError("sport commands require exclusive sport control")

    def stand_down(self, token=None):
        self._require_owner(token)
        self._require_sport()
        raise ValueError("G1 velocity policy does not implement a sitting posture")

    def stand_up(self, token=None):
        self._require_owner(token)
        self._require_sport()
        if self.mode in {"standing", "moving"}:
            self.stop(token)
            return
        raise ValueError("G1 velocity policy does not implement recovery; reset is required")

    def zero_torque(self, token=None):
        self._require_owner(token)
        self._safe_mode("zero_torque")

    @staticmethod
    def _joint_vector(value, name):
        if not isinstance(value, (list, tuple, np.ndarray)) or np.shape(value) != (JOINT_COUNT,):
            raise ValueError(f"{name} must contain 29 finite numbers")
        if any(isinstance(v, (bool, np.bool_)) or not isinstance(v, (int, float, np.integer, np.floating)) for v in value):
            raise ValueError(f"{name} must contain 29 finite numbers")
        try:
            result = np.asarray(value, dtype=np.float64).copy()
        except (TypeError, ValueError, OverflowError) as error:
            raise ValueError(f"{name} must contain 29 finite numbers") from error
        if not np.isfinite(result).all():
            raise ValueError(f"{name} must contain 29 finite numbers")
        return result

    def command_low_level(self, q, dq, kp, kd, tau, active, token=None):
        """Accept normalized motor-order commands; the adapter owns SDK/CRC parsing.

        Inactive slots still need finite, in-range normalized positions. SDK
        position/velocity stop sentinels must be resolved before this boundary.
        """
        self._require_owner(token)
        if self.control_mode != "lowlevel" or self.mode != "lowlevel":
            raise ValueError("low-level commands require exclusive lowlevel control")
        now = self._clock()
        if self._low_level_expired(now):
            self._safe_mode("damping")
            raise PermissionError("low-level command watchdog expired; control grant revoked")
        values = {name: self._joint_vector(value, name) for name, value in
                  (("q", q), ("dq", dq), ("kp", kp), ("kd", kd), ("tau", tau))}
        if not isinstance(active, (list, tuple, np.ndarray)) or np.shape(active) != (JOINT_COUNT,) or any(not isinstance(v, (bool, np.bool_)) for v in active):
            raise ValueError("active must contain 29 booleans")
        if np.any(values["q"] < self.joint_ranges[:, 0]) or np.any(values["q"] > self.joint_ranges[:, 1]):
            raise ValueError("joint position exceeds model joint limits")
        if np.any(values["kp"] < 0) or np.any(values["kp"] > 300) or np.any(values["kd"] < 0) or np.any(values["kd"] > 20):
            raise ValueError("joint gains require 0 <= kp <= 300 and 0 <= kd <= 20")
        if np.any(values["tau"] < self.torque_ranges[:, 0]) or np.any(values["tau"] > self.torque_ranges[:, 1]):
            raise ValueError("feedforward torque exceeds model force limits")
        # Bound the PD velocity product as well as the final torque. This is a
        # simulator command limit, not a claim about hardware motor speed.
        if np.any(np.abs(values["dq"]) > 40):
            raise ValueError("joint velocity target exceeds 40 rad/s")
        values["active"] = np.asarray(active, dtype=bool).copy()
        self._low_level = values
        self._low_level_received = now

    def _low_level_expired(self, now):
        return (self._low_level_received is None or now < self._last_clock or
                now < self._low_level_received or now - self._low_level_received >= LOW_LEVEL_TIMEOUT)

    def pause(self):
        self.owner = None
        self.command[:] = 0
        self.applied_command[:] = 0
        self._last_received = None
        if self.mode != "paused":
            self._before_pause = "damping" if self.control_mode == "lowlevel" else self.mode
            if self.control_mode == "lowlevel":
                self.control_mode = None
                self._low_level = None
                self._low_level_received = None
            self.mode = "paused"

    def resume(self):
        if self.mode == "paused":
            self.mode = self._before_pause

    def damp(self, token=None):
        self._require_owner(token)
        self._safe_mode("damping")

    def _safe_mode(self, mode):
        self.owner = None
        self.command[:] = 0
        self.applied_command[:] = 0
        self._last_received = None
        self._low_level = None
        self._low_level_received = None
        self._transition = None
        self.control_mode = None
        self.mode = mode

    def _walking_torque(self, q, dq):
        if self._force_policy_update or self._steps % self.decimation == 0:
            rotation = self.data.site_xmat[self.imu_id].reshape(3, 3)
            gravity = rotation.T @ np.array([0.0, 0.0, -1.0])
            self.target = self.policy.targets(self.gyro, gravity,
                                              self.applied_command, q, dq)
            self._force_policy_update = False
        return self.policy.kp * (self.target - q) - self.policy.kd * dq

    def step(self):
        if self.mode == "paused":
            return
        now = self._clock()
        if self.mode == "lowlevel" and self._low_level_expired(now):
            self._safe_mode("damping")
        if now < self._last_clock or self._last_received is None or now - self._last_received >= COMMAND_TIMEOUT:
            self.command[:] = 0
        self._last_clock = now
        if not np.isfinite(self.data.qpos).all() or not np.isfinite(self.data.qvel).all():
            self._safe_mode("fault")
            raise RuntimeError("non-finite MuJoCo state")
        upright = self.data.xmat[self.body_id].reshape(3, 3)[2, 2]
        low_gait = self.mode in {"standing", "moving"} and self.data.qpos[2] < 0.45
        if self.mode not in {"fallen", "damping", "fault", "zero_torque"} and self.data.time > 0.5 and (low_gait or upright < 0.35):
            self._safe_mode("fallen")
        delta = np.array([1.0, 1.0, 2.0]) * TIMESTEP
        self.applied_command += np.clip(self.command - self.applied_command, -delta, delta)
        q, dq = self.data.qpos[self.qaddr], self.data.qvel[self.vaddr]
        if self.mode == "zero_torque":
            torque = np.zeros(JOINT_COUNT)
        elif self.mode in {"fallen", "damping", "fault"}:
            torque = -2.0 * dq
        elif self.mode == "lowlevel":
            if self._low_level is None:
                torque = -2.0 * dq
            else:
                cmd = self._low_level
                torque = cmd["kp"] * (cmd["q"] - q) + cmd["kd"] * (cmd["dq"] - dq) + cmd["tau"]
                torque = np.where(cmd["active"], torque, 0.0)
        else:
            torque = self._walking_torque(q, dq)
            self.mode = "moving" if np.linalg.norm(self.applied_command) > 0.01 else "standing"
        torque = np.clip(torque, self.torque_ranges[:, 0], self.torque_ranges[:, 1])
        self.data.ctrl[self.actuators] = torque
        mujoco.mj_step(self.model, self.data)
        self._steps += 1

    def snapshot(self):
        return {
            "time": float(self.data.time), "epoch": self.epoch, "mode": self.mode,
            "control_mode": self.control_mode,
            "posture_phase": self._transition["phase"] if self._transition else None,
            "ncontact": int(self.data.ncon),
            "armed": self.owner is not None,
            "position": self.data.qpos[:3].tolist(),
            "quaternion_wxyz": self.data.qpos[3:7].tolist(),
            "linear_velocity_world": self.data.qvel[:3].tolist(),
            "angular_velocity_body": self.data.qvel[3:6].tolist(),
            "command": self.command.tolist(), "applied_command": self.applied_command.tolist(),
            "joints": {"name": self.joint_names, "q": self.data.qpos[self.qaddr].tolist(),
                       "dq": self.data.qvel[self.vaddr].tolist(),
                       "torque": self.data.qfrc_actuator[self.vaddr].tolist()},
        }
