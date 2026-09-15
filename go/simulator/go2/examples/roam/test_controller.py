"""Safety and movement decisions without ROS or a simulator dependency."""

import json
import math

import pytest

from controller import RoamController


def readings(*, front=4.0, left=4.0, right=4.0):
    result = [5.0] * 360
    for index in range(360):
        degrees = index - 180
        if -25 <= degrees <= 25:
            result[index] = front
        elif 30 <= degrees <= 110:
            result[index] = left
        elif -110 <= degrees <= -30:
            result[index] = right
    return result


def observe(controller, now, values=None, **sectors):
    return controller.observe_scan(values if values is not None else readings(**sectors),
                                   -math.pi, math.pi / 180, 0.1, 12.0, now)


def moving(now=0.0):
    controller = RoamController()
    observe(controller, now)
    assert controller.start(now)
    return controller


def test_explicit_start_and_stop_are_required_and_commands_are_bounded():
    controller = RoamController()
    assert controller.tick(0) == (0, 0)
    assert not controller.start(0)
    observe(controller, 0)
    assert controller.tick(0) == (0, 0)
    assert controller.start(0)
    assert controller.tick(0) == (0.55, 0)
    controller.stop("operator_stop")
    observe(controller, 0.1)
    assert controller.tick(0.1) == (0, 0)
    assert controller.status(0.1)["reason"] == "operator_stop"


def test_stale_scan_latches_stopped_and_new_data_cannot_rearm():
    controller = moving()
    assert controller.tick(0.351) == (0, 0)
    assert controller.status(0.351)["reason"] == "stale_scan"
    observe(controller, 0.4)
    assert controller.tick(0.4) == (0, 0)
    assert controller.start(0.4)
    assert controller.tick(0.4)[0] > 0


def test_old_or_duplicate_scans_and_poses_cannot_overwrite_newer_observations():
    controller = moving(10)
    assert not observe(controller, 9, front=0.2)
    assert not observe(controller, 10, front=0.2)
    assert controller.tick(10) == (0.55, 0)
    assert controller.observe_pose(1, 2, 0.2, 10)
    assert not controller.observe_pose(100, 100, 2, 9)
    assert not controller.observe_pose(100, 100, 2, 10)
    assert controller._pose == (1, 2, pytest.approx(0.2))


@pytest.mark.parametrize("unknown", [math.nan, math.inf, -math.inf, 0, 0.05, 13.0, True, "3"])
def test_one_unknown_forward_return_never_permits_forward_motion(unknown):
    controller = moving()
    scan = readings()
    scan[180] = unknown
    observe(controller, 0.1, scan)
    assert controller.tick(0.1) == (0, 0)
    assert controller.status(0.1)["reason"] == "unknown_forward_path"
    observe(controller, 0.2)
    assert controller.tick(0.2) == (0, 0)


def test_distant_unknown_rear_returns_do_not_strand_a_clear_forward_corridor():
    controller = RoamController()
    scan = readings()
    scan[:30] = [math.inf] * 30
    scan[-30:] = [math.inf] * 30
    observe(controller, 0, scan)
    assert controller.start(0)
    assert controller.tick(0) == (0.55, 0)


def test_gap_mode_drives_with_front_returns_but_requires_observed_turn_sectors():
    controller = RoamController(allow_scan_gaps=True)
    scan = [math.inf] * 360
    scan[180] = 4.0
    observe(controller, 0, scan)
    assert controller.start(0)
    assert controller.tick(0) == (0.55, 0)
    assert controller.status(0)["scan_coverage"]["front"] == {"observed": 1, "total": 51}
    scan[180] = 0.6
    observe(controller, 0.1, scan)
    assert controller.tick(0.1) == (0, 0)
    assert controller.reason == "no_clear_turn"


def test_gap_mode_turns_using_measured_clearance_and_stops_for_close_return():
    controller = RoamController(allow_scan_gaps=True)
    scan = [math.inf] * 360
    scan[180], scan[240] = 0.6, 2.0
    observe(controller, 0, scan)
    assert controller.start(0)
    assert controller.tick(0) == (0, 0.4)
    scan[240] = 0.2
    observe(controller, 0.1, scan)
    assert controller.tick(0.1) == (0, 0)
    assert controller.reason == "turn_path_blocked"


@pytest.mark.parametrize("bad", [math.nan, -math.inf, -1, 13, True, "clear"])
def test_gap_mode_rejects_invalid_readings(bad):
    controller = RoamController(allow_scan_gaps=True)
    scan = [math.inf] * 360
    scan[180], scan[0] = 4.0, bad
    assert not observe(controller, 0, scan)
    assert not controller.start(0)


def test_gap_mode_rejects_empty_front_sector():
    controller = RoamController(allow_scan_gaps=True)
    scan = [math.inf] * 360
    scan[0] = 4.0
    observe(controller, 0, scan)
    assert not controller.start(0)
    assert controller.reason == "unknown_forward_path"


@pytest.mark.parametrize("values,increment", [([4] * 180, math.pi / 180),
                                             ([4] * 360, 0), ([4] * 360, math.nan),
                                             ([4] * 36, math.pi / 18), (None, math.pi / 180)])
def test_missing_or_sparse_angular_coverage_is_rejected(values, increment):
    controller = moving()
    assert not controller.observe_scan(values, -math.pi, increment, 0.1, 12.0, 0.1)
    assert controller.tick(0.1) == (0, 0)


def test_obstacle_stops_forward_and_turns_toward_clearer_side_without_oscillation():
    controller = moving()
    observe(controller, 0.1, front=0.7, left=4, right=1.0)
    assert controller.tick(0.1) == (0, 0.4)
    assert controller.status(0.1)["turn_direction"] == "left"
    for now in (0.2, 0.3, 0.4):
        observe(controller, now, front=0.9, left=1, right=6)
        assert controller.tick(now) == (0, 0.4)
    assert controller.status(0.4)["state"] == "turning"


def test_unknown_turn_sector_is_blocked_and_no_clear_side_stops():
    controller = moving()
    scan = readings(front=0.6, left=5, right=2)
    scan[260] = math.inf
    observe(controller, 0.1, scan)
    assert controller.tick(0.1) == (0, -0.4)
    controller = moving()
    observe(controller, 0.1, front=0.2, left=0.2, right=0.2)
    assert controller.tick(0.1) == (0, 0)
    assert controller.status(0.1)["reason"] == "no_clear_turn"


def test_heading_feedback_handles_wrap_and_requires_clearance_hysteresis():
    controller = moving()
    yaw = math.pi - 0.1
    controller.observe_pose(0, 0, yaw, 0)
    observe(controller, 0.1, front=0.6)
    assert controller.tick(0.1) == (0, 0.4)
    observe(controller, 4.1, front=1.1)
    controller.observe_pose(0, 0, yaw + math.pi / 2 + 0.01, 4.1)
    assert controller.tick(4.1) == (0, 0.4), "clearing entry threshold alone must not resume forward"
    observe(controller, 4.2, front=1.3)
    controller.observe_pose(0, 0, yaw + math.pi / 2 + 0.02, 4.2)
    assert controller.tick(4.2) == (0.55, 0)


def test_time_bounded_turn_works_without_odometry_but_stalled_heading_cannot_fake_progress():
    controller = moving()
    observe(controller, 0.1, front=0.6)
    controller.tick(0.1)
    observe(controller, 4.1, front=2)
    assert controller.tick(4.1) == (0.55, 0)
    controller = moving()
    controller.observe_pose(0, 0, 0, 0)
    observe(controller, 0.1, front=0.6)
    controller.tick(0.1)
    for now in (4.1, 6.61):
        observe(controller, now, front=2)
        controller.observe_pose(0, 0, 0, now)
        command = controller.tick(now)
        assert command[0] == 0
    assert command == (0, 0)
    assert controller.status(6.61)["reason"] == "turn_timeout"


def test_committed_turn_ignores_new_distant_rear_unknowns_but_stops_for_close_returns():
    controller = moving()
    observe(controller, 0.1, front=0.6)
    controller.tick(0.1)
    scan = readings(front=0.9)
    scan[280] = math.inf
    observe(controller, 0.2, scan)
    assert controller.tick(0.2) == (0, 0.4)
    scan[270] = 0.2
    observe(controller, 0.3, scan)
    assert controller.tick(0.3) == (0, 0)
    assert controller.status(0.3)["reason"] == "turn_path_blocked"


def test_stuck_forward_motion_enters_bounded_recovery_but_actual_progress_keeps_cruising():
    controller = moving()
    controller.observe_pose(0, 0, 0, 0)
    controller.tick(0)
    observe(controller, 3.1)
    controller.observe_pose(0.02, 0, 0, 3.1)
    assert controller.tick(3.1) == (0, 0.4)
    assert controller.status(3.1)["reason"] == "stuck_recovery"
    controller = moving()
    controller.observe_pose(0, 0, 0, 0)
    controller.tick(0)
    observe(controller, 3.1)
    controller.observe_pose(0.6, 0, 0, 3.1)
    assert controller.tick(3.1) == (0.55, 0)


@pytest.mark.parametrize("bad_time", [math.nan, math.inf, -1, True])
def test_bad_clock_stops_and_status_remains_json_safe(bad_time):
    controller = moving()
    assert controller.tick(bad_time) == (0, 0)
    assert controller.status(0)["reason"] == "invalid_clock"
    json.dumps(controller.status(bad_time), allow_nan=False)


def test_gap_mode_waits_at_zero_then_resumes_on_fresh_front_returns():
    controller = RoamController(allow_scan_gaps=True)
    observe(controller, 0)
    assert controller.start(0)
    assert controller.tick(0) == (0.55, 0)
    observe(controller, 0.1, front=math.inf)
    assert controller.tick(0.1) == (0, 0)
    assert controller.active
    assert controller.status(0.1)["state"] == "waiting_scan"
    assert controller.status(0.1)["reason"] == "waiting_for_front_returns"
    assert controller.tick(0.15) == (0, 0)
    observe(controller, 0.2)
    assert controller.tick(0.2) == (0.55, 0)
    assert controller.status(0.2)["state"] == "cruising"


@pytest.mark.parametrize("recovered_before_tick", [False, True])
def test_gap_mode_prolonged_loss_latches_even_if_returns_arrive_before_next_tick(recovered_before_tick):
    controller = RoamController(allow_scan_gaps=True)
    observe(controller, 0)
    assert controller.start(0)
    observe(controller, 0.1, front=math.inf)
    assert controller.tick(0.1) == (0, 0)
    observe(controller, 0.3, front=math.inf)
    assert controller.tick(0.3) == (0, 0)
    observe(controller, 0.46, front=4 if recovered_before_tick else math.inf)
    assert controller.tick(0.46) == (0, 0)
    assert not controller.active
    assert controller.reason == "unknown_forward_path"
    observe(controller, 0.5)
    assert controller.tick(0.5) == (0, 0)
    assert controller.start(0.5)


@pytest.mark.parametrize("cause", ["operator_stop", "stale_scan", "invalid_scan"])
def test_gap_wait_never_rearms_after_a_latched_stop(cause):
    controller = RoamController(allow_scan_gaps=True)
    observe(controller, 0)
    assert controller.start(0)
    observe(controller, 0.1, front=math.inf)
    assert controller.tick(0.1) == (0, 0)
    if cause == "operator_stop":
        controller.stop(cause)
    elif cause == "stale_scan":
        controller.tick(0.46)
    else:
        observe(controller, 0.2, front=math.nan)
    assert controller.reason == cause
    observe(controller, 0.5)
    assert controller.tick(0.5) == (0, 0)
    assert not controller.active


def test_gap_recovery_rechecks_obstacles_before_forward_motion():
    controller = RoamController(allow_scan_gaps=True)
    observe(controller, 0)
    assert controller.start(0)
    observe(controller, 0.1, front=math.inf)
    assert controller.tick(0.1) == (0, 0)
    observe(controller, 0.2, front=0.6)
    assert controller.tick(0.2) == (0, 0.4)
    assert controller.reason == "obstacle"


def test_gap_during_turn_does_not_count_waiting_as_rotation():
    controller = RoamController(allow_scan_gaps=True)
    observe(controller, 0, front=0.6)
    assert controller.start(0)
    assert controller.tick(0) == (0, 0.4)
    for step in range(1, 39):
        now = step / 10
        observe(controller, now, front=math.inf if step == 1 else 4)
        assert controller.tick(now) == ((0, 0) if step == 1 else (0, 0.4))
    observe(controller, 4.0)
    assert controller.tick(4.0) == (0, 0.4)
    observe(controller, 4.1)
    assert controller.tick(4.1) == (0.55, 0)
