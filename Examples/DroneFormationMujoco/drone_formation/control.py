"""Per-drone flight controller, vectorised across the swarm.

Position: PID on the reference trajectory with acceleration feedforward. The
integral term is what holds a drone in place against a steady wind; it never
sees the wind directly. Attitude: a geometric controller on SO(3). The mixer
turns thrust and body torques into four rotor thrusts.
"""

from __future__ import annotations

import numpy as np

from .model import (GRAVITY, INERTIA, MASS, ROTOR_MAX_THRUST, ROTOR_SPIN, ROTOR_TORQUE_COEFF,
                    ROTOR_XY)


KP = np.array([6.5, 6.5, 9.0])
KD = np.array([4.2, 4.2, 5.5])
KI = np.array([2.6, 2.6, 3.0])
INTEGRAL_LIMIT = 0.6  # m s
MAX_TILT = np.deg2rad(38.0)
KR = INERTIA * np.array([22.0, 22.0, 8.0]) ** 2
KW = INERTIA * 2.0 * np.array([0.8, 0.8, 0.9]) * np.array([22.0, 22.0, 8.0])
IDLE_THRUST = 0.004  # N per rotor: props turn, the drone stays put

# [thrust, tau_x, tau_y, tau_z] = MIX @ rotor thrusts
MIX = np.vstack([
    np.ones(4),
    ROTOR_XY[:, 1],
    -ROTOR_XY[:, 0],
    ROTOR_SPIN * ROTOR_TORQUE_COEFF,
])
UNMIX = np.linalg.inv(MIX)


def quat_to_matrix(q: np.ndarray) -> np.ndarray:
    """(N, 4) wxyz quaternions to (N, 3, 3) rotation matrices."""
    w, x, y, z = q[:, 0], q[:, 1], q[:, 2], q[:, 3]
    return np.stack([
        np.stack([1 - 2 * (y * y + z * z), 2 * (x * y - w * z), 2 * (x * z + w * y)], axis=1),
        np.stack([2 * (x * y + w * z), 1 - 2 * (x * x + z * z), 2 * (y * z - w * x)], axis=1),
        np.stack([2 * (x * z - w * y), 2 * (y * z + w * x), 1 - 2 * (x * x + y * y)], axis=1),
    ], axis=1)


class SwarmController:
    def __init__(self, count: int) -> None:
        self.count = count
        self.integral = np.zeros((count, 3))
        self.thrust = np.zeros(count)
        self.rotors = np.zeros((count, 4))

    def reset(self) -> None:
        self.integral[:] = 0.0
        self.thrust[:] = 0.0
        self.rotors[:] = 0.0

    def update(self, dt: float, position: np.ndarray, velocity: np.ndarray, rotation: np.ndarray,
               rates: np.ndarray, target: tuple[np.ndarray, np.ndarray, np.ndarray],
               motors: str, integrate: bool) -> np.ndarray:
        """Rotor thrust commands, shaped (N, 4)."""
        if motors == "off":
            self.integral[:] = 0.0
            self.rotors[:] = 0.0
            self.thrust[:] = 0.0
            return self.rotors
        if motors == "idle":
            self.integral[:] = 0.0
            self.rotors[:] = IDLE_THRUST
            self.thrust[:] = 4 * IDLE_THRUST
            return self.rotors

        p_ref, v_ref, a_ref = target
        error = position - p_ref
        if integrate:
            self.integral = np.clip(self.integral + error * dt, -INTEGRAL_LIMIT, INTEGRAL_LIMIT)
        else:
            self.integral *= 0.98
        accel = a_ref - KP * error - KD * (velocity - v_ref) - KI * self.integral
        # Limit tilt by capping the horizontal demand against the vertical one.
        vertical = np.clip(accel[:, 2] + GRAVITY, 0.35 * GRAVITY, 1.9 * GRAVITY)
        horizontal = accel[:, :2]
        limit = vertical * np.tan(MAX_TILT)
        norm = np.linalg.norm(horizontal, axis=1)
        scale = np.minimum(1.0, limit / np.maximum(norm, 1e-9))
        force = MASS * np.column_stack([horizontal * scale[:, None], vertical])

        b3 = rotation[:, :, 2]
        thrust = np.einsum("ni,ni->n", force, b3)
        thrust = np.clip(thrust, 0.0, 4 * ROTOR_MAX_THRUST * 0.95)

        # Desired attitude: thrust along the demanded force, nose toward +x.
        b3_d = force / np.linalg.norm(force, axis=1, keepdims=True)
        b1_c = np.array([1.0, 0.0, 0.0])
        b2_d = np.cross(b3_d, b1_c)
        b2_d /= np.linalg.norm(b2_d, axis=1, keepdims=True)
        b1_d = np.cross(b2_d, b3_d)
        r_d = np.stack([b1_d, b2_d, b3_d], axis=2)

        m = np.einsum("nji,njk->nik", r_d, rotation) - np.einsum("nji,njk->nik", rotation, r_d)
        e_r = 0.5 * np.stack([m[:, 2, 1], m[:, 0, 2], m[:, 1, 0]], axis=1)
        j_w = INERTIA * rates
        torque = -KR * e_r - KW * rates + np.cross(rates, j_w)

        wrench = np.column_stack([thrust, torque])
        rotors = wrench @ UNMIX.T
        saturated = (rotors < 0.0) | (rotors > ROTOR_MAX_THRUST)
        if saturated.any():
            # Give up yaw first, then clip: attitude matters more than heading.
            rows = saturated.any(axis=1)
            wrench[rows, 3] = 0.0
            rotors[rows] = wrench[rows] @ UNMIX.T
        rotors = np.clip(rotors, 0.0, ROTOR_MAX_THRUST)
        self.rotors = rotors
        self.thrust = rotors.sum(axis=1)
        return rotors
