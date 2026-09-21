import math
import pytest
from controller import HallwayController


def corridor(front=5, width=1.4):
    values = []
    for i in range(72):
        a = -math.pi + i * math.pi/36
        c, s = math.cos(a), math.sin(a)
        walls = [width/2/abs(s)] if abs(s) > 1e-9 else []
        walls.append(front/c if c > 1e-9 else -1/c if c < -1e-9 else 12)
        values.append(min(12, *walls))
    return values


def observe(c, now=10, *, front=5, width=1.4, x=0):
    assert c.observe_pose(x, 0, 0, now)
    assert c.observe_scan(corridor(front, width), -math.pi, math.pi/36, now)


def test_idle_start_stop_and_limits():
    c = HallwayController(max_seconds=5)
    assert c.tick(10) == (0, 0)
    assert not c.start(10)
    observe(c)
    assert c.start(10)
    assert c.tick(10) == pytest.approx((.55, 0))
    observe(c, 15)
    assert c.tick(15) == (0, 0)
    assert c.reason == "exploration_limit"
    observe(c, 15.1)
    assert c.tick(15.1) == (0, 0)


@pytest.mark.parametrize("sensor", ["scan", "odom"])
def test_each_sensor_expiry_latches_stop(sensor):
    c = HallwayController()
    observe(c)
    assert c.start(10)
    if sensor == "scan":
        c.observe_pose(0, 0, 0, 10.36)
    else:
        c.observe_scan(corridor(), -math.pi, math.pi/36, 10.36)
    assert c.tick(10.36) == (0, 0)
    assert c.reason == "stale_" + sensor
    observe(c, 10.4)
    assert c.tick(10.4) == (0, 0)


def test_blocked_corridor_and_side_obstacle_stop():
    c = HallwayController()
    observe(c)
    assert c.start(10)
    observe(c, 10.1, front=.8)
    assert c.tick(10.1) == (0, 0)
    assert c.reason == "blocked_or_dead_end"
    observe(c, 10.2)
    assert c.start(10.2)
    observe(c, 10.3, width=.7)
    assert not c.active
    assert c.reason == "side_obstacle"


def test_front_wall_is_not_misclassified_as_near_body_side_obstacle():
    c = HallwayController()
    observe(c, front=.65)
    assert c.scan["front"] == pytest.approx(.65)
    assert c.scan["left"] > .6
    assert c.scan["right"] > .6


def test_branch_turn_stops_before_rotating():
    c = HallwayController()
    observe(c)
    assert c.start(10)
    for now in (10.1, 10.4, 10.8):
        c.observe_pose(0, 0, 0, now)
        values = corridor(front=.8)
        values[52:57] = [4.0] * 5
        c.observe_scan(values, -math.pi, math.pi/36, now)
        command = c.tick(now)
        if now < 10.7:
            assert command == (0, 0)
    assert command == (0, c.TURN_SPEED)


@pytest.mark.parametrize("bad", [math.nan, -math.inf, -1, 0, 13, True, "clear"])
def test_invalid_scan_disarms(bad):
    c = HallwayController()
    observe(c)
    assert c.start(10)
    scan = corridor()
    scan[36] = bad
    assert not c.observe_scan(scan, -math.pi, math.pi/36, 10.1)
    assert c.tick(10.1) == (0, 0)


def test_unknown_forward_sector_cannot_authorize_motion():
    c = HallwayController()
    observe(c)
    scan = corridor()
    scan[33:40] = [math.inf] * 7
    c.observe_scan(scan, -math.pi, math.pi/36, 10.1)
    assert not c.start(10.1)
    assert c.reason == "unknown_corridor"


def test_odometry_jump_is_not_mistaken_for_progress():
    c = HallwayController()
    observe(c)
    assert c.start(10)
    assert not c.observe_pose(2, 0, 0, 10.02)
    assert c.reason == "odometry_jump"
    assert c.tick(10.02) == (0, 0)


@pytest.mark.parametrize("limits", [{"max_seconds": math.nan}, {"max_distance": True},
                                    {"max_distance": 100}, {"prefer": "back"}])
def test_invalid_limits_rejected(limits):
    with pytest.raises(ValueError):
        HallwayController(**limits)
