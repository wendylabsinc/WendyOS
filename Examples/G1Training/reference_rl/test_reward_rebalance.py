import numpy as np

from grip_reward import (
    CAN_TRACKING_REWARD_RATE,
    GRIP_REWARD_RATE,
    JOINT_TRACKING_REWARD_RATE,
    LIFT_ABOVE_8CM_REWARD_RATE,
    REWARD_CONTRACT,
    THREE_FINGER_REWARD_RATE,
    lift_above_threshold_score,
)
from contact_location import CONTACT_INSTRUMENTATION_CONTRACT, TOP_CONTACT_PENALTY_RATE


def test_reward_rebalance_rates_and_no_force_ceiling():
    assert JOINT_TRACKING_REWARD_RATE == 0.25
    assert CAN_TRACKING_REWARD_RATE == 0.5
    assert GRIP_REWARD_RATE == 4.0
    assert THREE_FINGER_REWARD_RATE == 4.0
    assert LIFT_ABOVE_8CM_REWARD_RATE == 4.0
    assert REWARD_CONTRACT["force_ceiling"] is None
    assert TOP_CONTACT_PENALTY_RATE == 2.0
    assert CONTACT_INSTRUMENTATION_CONTRACT["force_scaled"] is False
    assert CONTACT_INSTRUMENTATION_CONTRACT["force_ceiling_n"] is None


def test_lift_reward_is_present_on_every_step_strictly_above_8cm():
    lift = np.array([0.0799, 0.08, 0.080001, 0.20])
    np.testing.assert_array_equal(lift_above_threshold_score(lift), [0.0, 0.0, 1.0, 1.0])
