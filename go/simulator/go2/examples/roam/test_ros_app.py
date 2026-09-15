"""ROS observation admission checks that also run without a ROS installation."""

import math
from types import SimpleNamespace

import pytest

from ros_app import ExposureGate, pose_values, stale_observation


def stamp(nanoseconds):
    seconds, fraction = divmod(nanoseconds, 1_000_000_000)
    return SimpleNamespace(sec=seconds, nanosec=fraction)


def odometry(*, frame="odom", child="base_link", x=1.0, quaternion=(0, 0, 0, 1)):
    return SimpleNamespace(header=SimpleNamespace(frame_id=frame), child_frame_id=child,
        pose=SimpleNamespace(pose=SimpleNamespace(
            position=SimpleNamespace(x=x, y=2.0, z=0.3),
            orientation=SimpleNamespace(**dict(zip(("x", "y", "z", "w"), quaternion))))))


def test_delayed_capture_keeps_its_source_age_and_rejects_reordered_packets():
    gate = ExposureGate()
    assert gate.capture_time("scan", stamp(9_800_000_000), wall_ns=10_000_000_000,
                             monotonic=50) == pytest.approx(49.8)
    assert gate.capture_time("scan", stamp(9_800_000_000), wall_ns=10_010_000_000,
                             monotonic=50.01) is None
    assert gate.capture_time("scan", stamp(9_750_000_000), wall_ns=10_010_000_000,
                             monotonic=50.01) is None
    assert gate.capture_time("odom", stamp(9_800_000_000), wall_ns=10_000_000_000,
                             monotonic=50) == pytest.approx(49.8)


@pytest.mark.parametrize("source", [0, 9_650_000_000, 10_060_000_000])
def test_old_zero_and_future_exposures_do_not_refresh_observations(source):
    gate = ExposureGate()
    assert gate.capture_time("scan", stamp(source), wall_ns=10_000_000_000,
                             monotonic=50) is None
    assert gate.last_stamps == {}


def test_invalid_timestamp_fraction_is_not_admitted():
    gate = ExposureGate()
    assert gate.capture_time("scan", SimpleNamespace(sec=9, nanosec=1_000_000_000),
                             wall_ns=10_000_000_000, monotonic=50) is None


def test_ros_adapter_requires_current_odometry_as_well_as_scan():
    assert stale_observation({"scan": 49.9, "odom": 49.9}, 50) is None
    assert stale_observation({"scan": 49.9, "odom": 49.6}, 50) == "stale_odom"
    assert stale_observation({"scan": 49.9}, 50) == "stale_odom"
    assert stale_observation({"scan": 49.6, "odom": 49.9}, 50) == "stale_scan"


def test_finite_odometry_quaternion_is_converted_to_yaw():
    assert pose_values(odometry(quaternion=(0, 0, math.sin(0.5), math.cos(0.5)))) == pytest.approx((1, 2, 1))


@pytest.mark.parametrize("message", [
    odometry(frame="map"), odometry(child="camera_link"), odometry(x=float("nan")),
    odometry(quaternion=(0, 0, 0, 0)), odometry(quaternion=(0, 0, float("inf"), 1)),
])
def test_wrong_frames_and_invalid_robot_poses_are_rejected(message):
    assert pose_values(message) is None
