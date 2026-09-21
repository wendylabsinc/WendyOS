"""Can-contact location classification and top-contact penalty contract."""

from __future__ import annotations

import numpy as np


CONTACT_LOCATION_NAMES = ("top", "side", "bottom")
TOP_CONTACT_MARGIN_M = 0.008
TOP_CONTACT_MIN_NORMAL_FORCE_N = 0.1
TOP_CONTACT_PENALTY_RATE = 2.0

CONTACT_INSTRUMENTATION_CONTRACT = {
    "version": "can-contact-location-and-carry-causes-v1",
    "locations": list(CONTACT_LOCATION_NAMES),
    "location_counting": "one boolean hit per physics substep and world for each location",
    "top_region": f"can-local z >= cylinder half-height - {TOP_CONTACT_MARGIN_M} m",
    "minimum_normal_force_n": TOP_CONTACT_MIN_NORMAL_FORCE_N,
    "top_contact_penalty_rate": TOP_CONTACT_PENALTY_RATE,
    "top_contact_penalty_eligibility": "right-hand/can top contact during reference grip phase before release",
    "top_contact_penalty_scaling": "negative rate times control dt times top-contact physics-substep fraction",
    "force_scaled": False,
    "force_ceiling_n": None,
    "force_ceiling": None,
    "carry_cause_names": ["hand_contact", "lost_opposed", "both"],
    "carry_cause_counting": "independent boolean cause hits per physics substep while carry audit is active and landing is not accepted",
}


def classify_can_contact(local_position, half_height_m, margin_m=TOP_CONTACT_MARGIN_M):
    """Return 0=top, 1=side, or 2=bottom from a can-local contact point."""
    point = np.asarray(local_position, dtype=np.float64)
    if point.shape != (3,) or not np.isfinite(point).all():
        raise ValueError("Expected one finite can-local xyz contact point")
    half_height_m = float(half_height_m)
    margin_m = float(margin_m)
    if not np.isfinite(half_height_m) or half_height_m <= 0:
        raise ValueError("Can half-height must be finite and positive")
    if not np.isfinite(margin_m) or not 0 <= margin_m < half_height_m:
        raise ValueError("Contact margin must be finite and inside the can half-height")
    if point[2] >= half_height_m - margin_m:
        return 0
    if point[2] <= -half_height_m + margin_m:
        return 2
    return 1


def top_contact_penalty(top_contact_substeps, physics_substeps, control_dt):
    """Binary-in-time penalty; contact force magnitude never scales the penalty."""
    hits = np.asarray(top_contact_substeps, dtype=np.float64)
    if not np.isfinite(hits).all() or np.any(hits < 0) or np.any(hits > physics_substeps):
        raise ValueError("Top-contact substeps must be within the control interval")
    if int(physics_substeps) <= 0 or float(control_dt) <= 0:
        raise ValueError("Physics substeps and control dt must be positive")
    return -float(control_dt) * TOP_CONTACT_PENALTY_RATE * hits / int(physics_substeps)
