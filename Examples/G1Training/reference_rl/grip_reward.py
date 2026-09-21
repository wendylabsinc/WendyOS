"""Reward opposed can contact without using force as a penalty or cutoff."""

import numpy as np
import torch


JOINT_TRACKING_REWARD_RATE = 0.25
CAN_TRACKING_REWARD_RATE = 0.5
GRIP_REWARD_RATE = 4.0
THREE_FINGER_REWARD_RATE = 4.0
LIFT_ABOVE_8CM_REWARD_RATE = 4.0
LIFT_REWARD_THRESHOLD_M = 0.08
SECURE_SIDE_FORCE_N = 2.0
REWARD_CONTRACT = {
    "version": "grasp-upweighted-continuous-8cm-lift-no-force-ceiling-v4",
    "joint_tracking_rate": JOINT_TRACKING_REWARD_RATE,
    "can_tracking_rate": CAN_TRACKING_REWARD_RATE,
    "grip_rate": GRIP_REWARD_RATE,
    "three_finger_grip_rate": THREE_FINGER_REWARD_RATE,
    "lift_above_8cm_rate": LIFT_ABOVE_8CM_REWARD_RATE,
    "lift_threshold_m": LIFT_REWARD_THRESHOLD_M,
    "secure_side_force_n": SECURE_SIDE_FORCE_N,
    "opposed_formula": "min(thumb_N, index_plus_middle_N)/2 clipped to [0,1] during the reference grip phase",
    "three_finger_formula": "min(thumb_N, index_N, middle_N)/2 clipped to [0,1] during the reference grip phase",
    "lift_formula": "one while current can lift is strictly above 8 cm, on every control step",
    "scaling": "mean over 25 physics substeps, multiplied by control dt=0.025",
    "force_source": "simulator contact normals; reward only, no actor input",
    "force_ceiling": None,
}


def lift_above_threshold_score(lift_m):
    """Continuous-in-time eligibility: one for every step currently above 8 cm."""
    values = np.asarray(lift_m, dtype=np.float64)
    return (values > LIFT_REWARD_THRESHOLD_M).astype(np.float64)


def opposed_grip_score(forces, enabled):
    """B x 3: thumb, index+middle, and total right-hand/can normal force."""
    score = (torch.minimum(forces[:, 0], forces[:, 1]) / SECURE_SIDE_FORCE_N).clamp(0, 1)
    return torch.where(enabled, score, 0.0)


def three_finger_grip_score(digit_forces, enabled):
    """B x 3: independent thumb, index, and middle can-contact force."""
    if digit_forces.ndim != 2 or digit_forces.shape[1] != 3:
        raise ValueError("Expected B x 3 thumb/index/middle forces")
    score = (digit_forces.min(dim=1).values / SECURE_SIDE_FORCE_N).clamp(0, 1)
    return torch.where(enabled, score, 0.0)


def reference_grip_schedule(hand_targets, can_positions, origin, destination):
    """Stop the bonus at recorded release, regardless of policy cooperation."""
    lifted = np.flatnonzero(can_positions[:, 2] - origin[2] > 0.03)
    if not len(lifted):
        raise ValueError("Reference never lifts; cannot identify grip/release")
    grasp = hand_targets[lifted[0]]
    direction = hand_targets[-1] - grasp
    length = np.linalg.norm(direction)
    if length <= 0.02:
        raise ValueError("Reference lacks a distinct open-hand release target")
    opening = (hand_targets - grasp) @ direction / length
    corridor = (
        (np.linalg.norm(can_positions[:, :2] - destination[:2], axis=1) < 0.045)
        & (np.abs(can_positions[:, 2] - destination[2]) < 0.020)
    )
    release = np.flatnonzero(
        corridor & (opening >= 0.02) & (np.arange(len(hand_targets)) > lifted[0])
    )
    if not len(release):
        raise ValueError("Reference release not found in placement corridor")
    return np.arange(len(hand_targets)) < release[0]
