"""CPU inference for the pinned Unitree G1 29-DoF velocity policy.

The public state/target/gain arrays use hardware motor order. Permutation into
the trained policy's joint order is confined to this adapter. Observation and
action conventions follow unitree_rl_lab at 4960b847 (Apache-2.0).
"""

from collections import deque
from pathlib import Path
import time

import numpy as np
import onnxruntime as ort
import yaml


JOINT_COUNT = 29
POLICY_TO_MOTOR = np.array([
    0, 6, 12, 1, 7, 13, 2, 8, 14, 3, 9, 15, 22, 4, 10, 16, 23,
    5, 11, 17, 24, 18, 25, 19, 26, 20, 27, 21, 28,
], dtype=np.intp)


class WalkingPolicy:
    TERMS = ("base_ang_vel", "projected_gravity", "velocity_commands",
             "joint_pos_rel", "joint_vel_rel", "last_action")

    def __init__(self, assets: Path):
        with (assets / "policy/deploy.yaml").open() as stream:
            self.config = yaml.safe_load(stream)
        if (tuple(self.config["observations"]) != self.TERMS
                or not np.array_equal(self.config["joint_ids_map"], POLICY_TO_MOTOR)
                or self.config.get("use_gym_history", False)):
            raise ValueError("G1 observation order differs from the pinned contract")
        self.period = float(self.config["step_dt"])
        if self.period != 0.02:
            raise ValueError("G1 policy requires a 20 ms control period")
        self.policy_default = np.asarray(self.config["default_joint_pos"], dtype=np.float64)
        self.default = np.empty(JOINT_COUNT, dtype=np.float64)
        self.default[POLICY_TO_MOTOR] = self.policy_default
        # The deployed C++ assigns gains directly by motor index, not policy index.
        self.kp = np.asarray(self.config["stiffness"], dtype=np.float64)
        self.kd = np.asarray(self.config["damping"], dtype=np.float64)
        self.action_config = self.config["actions"]["JointPositionAction"]
        for values in (self.policy_default, self.kp, self.kd):
            if values.shape != (JOINT_COUNT,) or not np.isfinite(values).all():
                raise ValueError("G1 deployment requires 29 finite joint parameters")
        if (self.action_config["clip"] is not None
                or not np.array_equal(self.action_config["offset"], self.policy_default)
                or not np.array_equal(self.action_config["scale"], np.full(JOINT_COUNT, 0.25))):
            raise ValueError("G1 action configuration differs from the pinned contract")
        for index, name in enumerate(self.TERMS):
            config = self.config["observations"][name]
            size = 3 if index < 3 else JOINT_COUNT
            expected_scale = 0.2 if index == 0 else 0.05 if index == 4 else 1.0
            if (config["clip"] is not None or config["history_length"] != 5
                    or not np.array_equal(config["scale"], np.full(size, expected_scale))):
                raise ValueError(f"G1 observation {name} differs from the pinned contract")
        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
        self.session = ort.InferenceSession(str(assets / "policy/policy.onnx"),
                                            options, providers=["CPUExecutionProvider"])
        inputs, outputs = self.session.get_inputs(), self.session.get_outputs()
        if (len(inputs) != 1 or inputs[0].name != "obs" or inputs[0].shape != [1, 480]
                or inputs[0].type != "tensor(float)" or len(outputs) != 1
                or outputs[0].name != "actions" or outputs[0].shape != [1, JOINT_COUNT]
                or outputs[0].type != "tensor(float)"):
            raise ValueError("G1 ONNX input/output contract differs from the pinned model")
        self.input = inputs[0]
        self.inference_ms = deque(maxlen=30000)
        self.updates = 0
        self.reset()

    def reset(self):
        self.history = {}
        self.action = np.zeros(JOINT_COUNT, dtype=np.float32)

    def observation(self, angular_velocity, gravity, velocity_command, q, dq):
        """Advance term-major history from one genuine current physical sample."""
        values = (angular_velocity, gravity, velocity_command,
                  np.asarray(q)[POLICY_TO_MOTOR] - self.policy_default,
                  np.asarray(dq)[POLICY_TO_MOTOR], self.action)
        pieces = []
        for index, (name, value) in enumerate(zip(self.TERMS, values, strict=True)):
            value = np.asarray(value, dtype=np.float32)
            if value.shape != ((3,) if index < 3 else (JOINT_COUNT,)) or not np.isfinite(value).all():
                raise ValueError(f"invalid G1 observation term: {name}")
            value = value * np.asarray(self.config["observations"][name]["scale"], dtype=np.float32)
            if name not in self.history:
                self.history[name] = deque((value.copy() for _ in range(5)), maxlen=5)
            else:
                self.history[name].append(value.copy())
            pieces.extend(self.history[name])
        return np.concatenate(pieces).reshape(1, 480)

    def targets(self, angular_velocity, gravity, velocity_command, q, dq):
        observation = self.observation(angular_velocity, gravity, velocity_command, q, dq)
        started = time.perf_counter()
        result = self.session.run(["actions"], {"obs": observation})
        self.inference_ms.append((time.perf_counter() - started) * 1000)
        self.updates += 1
        action = np.asarray(result[0], dtype=np.float32).reshape(-1)
        if action.shape != (JOINT_COUNT,) or not np.isfinite(action).all():
            raise RuntimeError("G1 walking policy returned invalid joint actions")
        self.action = action.copy()
        target = np.empty(JOINT_COUNT, dtype=np.float64)
        target[POLICY_TO_MOTOR] = self.policy_default + 0.25 * action
        return target
