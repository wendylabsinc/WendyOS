"""One-shot shaping reward for seating the can against the inside palm before lift."""

PALM_CONTACT_REQUIRED_S = 1.0
PALM_CONTACT_BONUS = 1.0
PALM_CONTACT_MIN_FORCE_N = 0.1
PALM_CONTACT_MIN_SUBSTEP_FRACTION = 0.8
PALM_PRELIFT_MAX_M = 0.03

# Calibrated from contact points in the right_wrist_yaw_link frame during the
# clean, zero-residual attempt-000077 reference replay. The model has no
# separately named palm geom, so this selects only its inside-facing region.
PALM_LOCAL_X_RANGE_M = (0.075, 0.110)
PALM_LOCAL_Y_RANGE_M = (0.008, 0.025)
PALM_LOCAL_ABS_Z_MAX_M = 0.050

PALM_REWARD_CONTRACT = {
    "version": "inside-palm-prelift-1s-no-force-ceiling-v2",
    "required_continuous_seconds": PALM_CONTACT_REQUIRED_S,
    "one_time_bonus": PALM_CONTACT_BONUS,
    "minimum_normal_force_n": PALM_CONTACT_MIN_FORCE_N,
    "force_ceiling": None,
    "minimum_physics_substep_fraction": PALM_CONTACT_MIN_SUBSTEP_FRACTION,
    "prelift_maximum_m": PALM_PRELIFT_MAX_M,
    "reward_input": "privileged simulator contact only; never a policy observation",
    "anti_farming": "reward only improvements in the episode-best continuous streak; total is capped at one bonus",
}


def is_inside_palm_region(local_point) -> bool:
    """Return whether a palm-body contact point is on the calibrated inside face."""
    x, y, z = (float(value) for value in local_point)
    return (
        PALM_LOCAL_X_RANGE_M[0] <= x <= PALM_LOCAL_X_RANGE_M[1]
        and PALM_LOCAL_Y_RANGE_M[0] <= y <= PALM_LOCAL_Y_RANGE_M[1]
        and abs(z) <= PALM_LOCAL_ABS_Z_MAX_M
    )


def eligible_palm_contact(total_normal_force_n: float, lift_m: float) -> bool:
    """Require real contact before a 3 cm lift, with no high-force cutoff."""
    return (
        PALM_CONTACT_MIN_FORCE_N <= float(total_normal_force_n)
        and float(lift_m) < PALM_PRELIFT_MAX_M
    )


def update_palm_progress(streak_s: float, best_progress: float, complete: bool,
                         eligible: bool, dt: float):
    """Advance a continuous streak and return a bounded, non-farmable reward."""
    if complete:
        return 0.0, 1.0, True, 0.0
    streak_s = float(streak_s) + float(dt) if eligible else 0.0
    progress = min(streak_s / PALM_CONTACT_REQUIRED_S, 1.0)
    next_best = max(float(best_progress), progress)
    reward = PALM_CONTACT_BONUS * (next_best - float(best_progress))
    return streak_s, next_best, next_best >= 1.0, reward
