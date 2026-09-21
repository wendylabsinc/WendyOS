"""Failure-behavior tests independent of ROS or the simulator implementation."""

import math

from controller import GoalFollower


def ready(now=10.0):
    controller = GoalFollower()
    controller.observe_odom(0, 0, 0, now)
    controller.receipts["imu"] = now
    controller.observe_scan([5.0] * 360, -math.pi, 2 * math.pi / 360, 0.1, 12, now)
    controller.mounts_ready = True
    controller.set_goal(1, 0, 1, "odom")
    return controller


def test_stale_scan_cancels_goal_and_cannot_restart_from_fresh_data():
    controller = ready()
    assert controller.tick(10.0)[0] > 0
    controller.receipts["odom"] = controller.receipts["imu"] = 10.3
    assert controller.tick(10.301) == (0, 0)
    assert controller.state == "stale_scan" and controller.goal is None
    controller.observe_scan([5.0] * 360, -math.pi, 2 * math.pi / 360, 0.1, 12, 10.31)
    assert controller.tick(10.31) == (0, 0)
    assert controller.state == "stale_scan"
    controller.set_goal(1, 0, 2, "odom")
    assert controller.tick(10.32)[0] > 0


def test_front_obstacle_holds_goal_until_clearance_exceeds_hysteresis():
    controller = ready()
    ranges = [5.0] * 360
    ranges[180] = 0.6
    controller.observe_scan(ranges, -math.pi, 2 * math.pi / 360, 0.1, 12, 10)
    assert controller.tick(10) == (0, 0)
    assert controller.state == "obstacle" and controller.goal is not None
    ranges[180] = 0.7
    controller.observe_scan(ranges, -math.pi, 2 * math.pi / 360, 0.1, 12, 10.1)
    assert controller.tick(10.1) == (0, 0)
    ranges[180] = 0.8
    controller.observe_scan(ranges, -math.pi, 2 * math.pi / 360, 0.1, 12, 10.2)
    assert controller.tick(10.2)[0] > 0


def test_missing_returns_are_unknown_space_and_stop_motion():
    controller = ready()
    controller.observe_scan([float("inf")] * 360, -math.pi, 2 * math.pi / 360, 0.1, 12, 10)
    assert controller.tick(10) == (0, 0)
    assert controller.state == "unknown_scan"


def test_missing_tf_and_invalid_goal_never_emit_motion():
    controller = ready()
    controller.mounts_ready = False
    assert controller.tick(10) == (0, 0)
    assert controller.state == "waiting_for_tf"
    controller.mounts_ready = True
    for x, y, frame in [(math.nan, 0, "odom"), (1, 0, "map"), (6, 0, "odom")]:
        controller.set_goal(x, y, 2, frame)
        assert controller.tick(10) == (0, 0)
        assert controller.state == "invalid_goal"


def test_arrival_cancels_goal_and_emits_zero():
    controller = ready()
    controller.observe_odom(0.8, 0.05, 0, 10)
    assert controller.tick(10) == (0, 0)
    assert controller.state == "arrived" and controller.goal is None


def test_unfresh_odom_or_imu_cancels_goal():
    for topic in ("odom", "imu"):
        controller = ready()
        controller.receipts[topic] = 9.0
        assert controller.tick(10) == (0, 0)
        assert controller.state == "stale_" + topic and controller.goal is None
