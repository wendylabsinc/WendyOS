"""Exercise route completion and failure behavior without ROS or physics."""

import json
import math

import pytest

from patrol import OBSERVATION_TIMEOUT, PatrolController


def observe(controller, now, pose=(0.0, 0.0, 0.0), ranges=None):
    assert controller.observe_pose(*pose, now)
    return controller.observe_scan([4.0] * 360 if ranges is None else ranges,
                                   -math.pi, math.pi / 180, 0.1, 12.0, now)


def test_start_requires_both_fresh_observations_and_does_not_move_immediately():
    controller = PatrolController()
    assert controller.tick(1) == (0, 0)
    assert not controller.start(1)
    assert controller.reason == "stale_scan"
    controller.observe_scan([4.0] * 360, -math.pi, math.pi / 180, 0.1, 12.0, 1)
    assert not controller.start(1)
    assert controller.reason == "stale_odom"
    assert controller.observe_pose(0, 0, 0, 1)
    assert controller.start(1)
    assert controller.command == (0, 0)
    assert controller.tick(1) == (controller.SPEED, 0)


def test_route_is_relative_to_start_position_and_heading():
    controller = PatrolController(laps=2)
    observe(controller, 1, (1.0, 2.0, math.pi / 2))
    assert controller.start(1)
    for actual, expected in zip(controller.targets[:4], ((1, 3), (0, 3), (0, 2), (1, 2))):
        assert actual == pytest.approx(expected)
    assert controller.targets[4:] == controller.targets[:4]
    observe(controller, 1.1, (1.0, 2.1, math.pi / 2))
    targets = controller.targets
    assert controller.start(1.1)
    assert controller.targets == targets  # Repeated Start is idempotent while running.


def test_default_square_completes_with_bounded_commands():
    controller = PatrolController()
    x, y, heading, now = 0.0, 0.0, 0.0, 1.0
    observe(controller, now)
    assert controller.start(now)
    visited = set()
    for _ in range(3000):
        now += 0.05
        observe(controller, now, (x, y, heading))
        linear, angular = controller.tick(now)
        visited.add(controller.index)
        assert 0 <= linear <= 0.35
        assert abs(angular) <= 0.4
        if not controller.active:
            break
        x += linear * math.cos(heading) * 0.05
        y += linear * math.sin(heading) * 0.05
        heading += angular * 0.05
    assert controller.state == "complete"
    assert controller.reason == "route_complete"
    assert {0, 1, 2, 3, 4}.issubset(visited)
    assert math.hypot(x, y) <= controller.ARRIVAL_DISTANCE
    assert controller.command == (0, 0)
    observe(controller, now + 0.1, (x, y, heading))
    assert controller.tick(now + 0.1) == (0, 0)


@pytest.mark.parametrize("missing", ["scan", "odom"])
def test_stale_observation_disarms_and_recovery_requires_explicit_start(missing):
    controller = PatrolController()
    observe(controller, 1)
    assert controller.start(1)
    assert controller.tick(1)[0] > 0
    now = 1 + OBSERVATION_TIMEOUT + 0.01
    if missing == "scan":
        controller.observe_pose(0, 0, 0, now)
    else:
        controller.observe_scan([4.0] * 360, -math.pi, math.pi / 180, 0.1, 12, now)
    assert controller.tick(now) == (0, 0)
    assert controller.reason == "stale_" + missing
    observe(controller, now + 0.05)
    assert controller.tick(now + 0.05) == (0, 0)
    assert controller.start(now + 0.05)


@pytest.mark.parametrize("index,distance", [(180, 0.85), (90, 0.55), (0, 0.4)])
def test_front_or_body_obstacle_stops_immediately_and_latches(index, distance):
    controller = PatrolController()
    observe(controller, 1)
    assert controller.start(1)
    controller.tick(1)
    ranges = [4.0] * 360
    ranges[index] = distance
    assert observe(controller, 1.05, ranges=ranges)
    assert not controller.active
    assert controller.reason == "obstacle"
    assert controller.command == (0, 0)
    observe(controller, 1.1)
    assert controller.tick(1.1) == (0, 0)


@pytest.mark.parametrize("bad", [float("nan"), float("inf"), -1.0, 13.0, "clear", True])
def test_unknown_rear_ray_disarms_instead_of_inventing_clear_space(bad):
    controller = PatrolController()
    observe(controller, 1)
    assert controller.start(1)
    ranges = [4.0] * 360
    ranges[0] = bad
    assert not observe(controller, 1.05, ranges=ranges)
    assert controller.reason == "unknown_scan"
    assert controller.tick(1.05) == (0, 0)


def test_gap_mode_accepts_sparse_returns_and_reports_actual_coverage():
    controller = PatrolController(allow_scan_gaps=True)
    ranges = [math.inf] * 360
    ranges[180] = 4.0
    assert observe(controller, 1, ranges=ranges)
    assert controller.start(1)
    assert controller.tick(1) == (controller.SPEED, 0)
    status = controller.status(1)
    assert status["scan_coverage"] == {"observed": 1, "total": 360, "front_observed": 1, "front_total": 61}
    assert status["front_clearance"] == status["body_clearance"] == 4.0
    json.dumps(status, allow_nan=False)
    assert controller.tick(1 + OBSERVATION_TIMEOUT + 0.01) == (0, 0)


@pytest.mark.parametrize("index,distance", [(180, 0.85), (90, 0.55), (0, 0.4)])
def test_gap_mode_still_stops_for_measured_front_side_and_rear_obstacles(index, distance):
    controller = PatrolController(allow_scan_gaps=True)
    ranges = [math.inf] * 360
    ranges[180] = 4.0
    assert observe(controller, 1, ranges=ranges)
    assert controller.start(1)
    ranges[index] = distance
    assert observe(controller, 1.05, ranges=ranges)
    assert controller.reason == "obstacle"
    assert not controller.active
    assert controller.command == (0, 0)


@pytest.mark.parametrize("bad", [math.nan, -math.inf, -1.0, 13.0, "clear", True])
def test_gap_mode_does_not_ignore_invalid_measurements(bad):
    controller = PatrolController(allow_scan_gaps=True)
    observe(controller, 1)
    assert controller.start(1)
    ranges = [math.inf] * 360
    ranges[180] = 4.0
    ranges[0] = bad
    assert not observe(controller, 1.05, ranges=ranges)
    assert controller.tick(1.05) == (0, 0)


@pytest.mark.parametrize("rear", [math.inf, 4.0])
def test_gap_mode_rejects_empty_scans_and_scans_without_front_returns(rear):
    controller = PatrolController(allow_scan_gaps=True)
    ranges = [math.inf] * 360
    ranges[0] = rear
    assert not observe(controller, 1, ranges=ranges)
    assert not controller.start(1)
    assert controller.tick(1) == (0, 0)


def test_incomplete_scan_and_invalid_pose_do_not_preserve_old_admission():
    controller = PatrolController()
    observe(controller, 1)
    assert controller.start(1)
    assert not controller.observe_scan([4.0] * 358, -math.pi, math.pi / 180, 0.1, 12, 1.1)
    assert not controller.start(1.1)
    observe(controller, 1.2)
    assert controller.start(1.2)
    assert not controller.observe_pose(float("nan"), 0, 0, 1.3)
    assert controller.tick(1.3) == (0, 0)
    assert not controller.start(1.3)


def test_room_bounds_reject_transformed_route_and_live_pose():
    controller = PatrolController()
    observe(controller, 1, (4.5, 0, 0))
    assert not controller.start(1)
    assert controller.reason == "route_outside_patrol_bounds"
    observe(controller, 1.1)
    assert controller.start(1.1)
    assert not controller.observe_pose(5.01, 0, 0, 1.2)
    assert not controller.active
    assert not controller.start(1.2)


def test_waypoint_timeout_stops_robot_that_does_not_make_progress():
    controller = PatrolController()
    observe(controller, 1)
    assert controller.start(1)
    observe(controller, 1 + controller.WAYPOINT_TIMEOUT)
    assert controller.tick(1 + controller.WAYPOINT_TIMEOUT) == (0, 0)
    assert controller.reason == "waypoint_timeout"


@pytest.mark.parametrize("bad_now", [float("nan"), float("inf"), 0.5])
def test_invalid_clock_disarms_and_status_remains_serializable(bad_now):
    controller = PatrolController()
    observe(controller, 1)
    assert controller.start(1)
    assert controller.tick(bad_now) == (0, 0)
    assert controller.reason == "invalid_clock"
    json.dumps(controller.status(bad_now), allow_nan=False)


@pytest.mark.parametrize("route,laps", [([], 1), ([[float("nan"), 0]], 1),
    ([[True, 0]], 1), ([[4, 0]], 1), ([[3, 3]], 1), ([[0, 0]], 1),
    ([[1, 0]], 0), ([[1, 0]], 6), ([[1, 0]], True), ([[1, 0]], 1.5),
    ([[1, 0, 0]], 1), ([[3, 0], [-3, 0]] * 8, 1), ("[[1, 0]]", 1)])
def test_invalid_or_unbounded_route_configuration_is_rejected(route, laps):
    with pytest.raises(ValueError):
        PatrolController(route, laps)
