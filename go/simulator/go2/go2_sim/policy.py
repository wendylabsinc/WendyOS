"""CPU inference for the pinned Go2 policy and its matching observation contract."""

from collections import deque
from pathlib import Path
import time

import numpy as np
import onnxruntime as ort
import yaml


class WalkingPolicy:
    TERMS = ("base_ang_vel", "projected_gravity", "velocity_commands",
             "joint_pos_rel", "joint_vel_rel", "last_action")

    def __init__(self, assets: Path):
        with (assets / "policy/deploy.yaml").open() as stream:
            self.config = yaml.safe_load(stream)
        if tuple(self.config["observations"]) != self.TERMS:
            raise ValueError("the policy observation order differs from the pinned contract")
        self.default = np.asarray(self.config["default_joint_pos"], dtype=np.float64)
        self.kp = np.asarray(self.config["stiffness"], dtype=np.float64)
        self.kd = np.asarray(self.config["damping"], dtype=np.float64)
        self.period = float(self.config["step_dt"])
        self.action_config = self.config["actions"]["JointPositionAction"]
        options = ort.SessionOptions()
        options.intra_op_num_threads = 1
        options.inter_op_num_threads = 1
        options.execution_mode = ort.ExecutionMode.ORT_SEQUENTIAL
        self.session = ort.InferenceSession(str(assets / "policy/policy.onnx"),
                                            options, providers=["CPUExecutionProvider"])
        self.input = self.session.get_inputs()[0]
        self.inference_ms = deque(maxlen=30000)
        self.updates = 0
        self.reset()

    def reset(self):
        self.history = {}
        self.action = np.zeros(12, dtype=np.float32)

    def targets(self, angular_velocity, gravity, velocity_command, q, dq):
        values = (angular_velocity, gravity, velocity_command, q - self.default,
                  dq, self.action)
        pieces = []
        for name, value in zip(self.TERMS, values, strict=True):
            config = self.config["observations"][name]
            value = (np.clip(value, *config["clip"]) * config["scale"]).astype(np.float32)
            length = int(config["history_length"])
            if name not in self.history:
                self.history[name] = deque((value.copy() for _ in range(length)), maxlen=length)
            else:
                self.history[name].append(value)
            # The exported network expects term-major, oldest-to-newest history.
            pieces.extend(self.history[name])
        observation = np.concatenate(pieces).reshape(1, -1)
        started = time.perf_counter()
        result = self.session.run(["actions"], {self.input.name: observation})
        self.inference_ms.append((time.perf_counter() - started) * 1000)
        self.updates += 1
        action = np.asarray(result[0], dtype=np.float32).reshape(-1)
        if action.shape != (12,) or not np.isfinite(action).all():
            raise RuntimeError("walking policy returned invalid joint actions")
        bounds = np.asarray(self.action_config["clip"])
        self.action = np.clip(action, bounds[:, 0], bounds[:, 1])
        return self.action * self.action_config["scale"] + self.action_config["offset"]
