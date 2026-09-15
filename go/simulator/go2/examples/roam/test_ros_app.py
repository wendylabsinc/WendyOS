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


@pytest.mark.parametrize("compatibility", [False, True])
def test_cloud_before_odometry_keeps_autostart_pending(monkeypatch, compatibility):
    import json
    import struct
    import sys
    import ros_app

    class Node:
        def __init__(self, name):
            pass
        def create_publisher(self, *args):
            return SimpleNamespace(publish=lambda message: None)
        def create_subscription(self, *args):
            pass
        def create_service(self, *args):
            pass
        def create_timer(self, *args):
            pass

    def request():
        return SimpleNamespace(header=SimpleNamespace(identity=SimpleNamespace(),
                                                      policy=SimpleNamespace()), parameter='')

    for name, module in {
        'rclpy.node': SimpleNamespace(Node=Node),
        'rclpy.qos': SimpleNamespace(QoSProfile=SimpleNamespace,
                                    ReliabilityPolicy=SimpleNamespace(BEST_EFFORT=1)),
        'unitree_api.msg': SimpleNamespace(Request=request),
        'nav_msgs.msg': SimpleNamespace(Odometry=object),
        'sensor_msgs.msg': SimpleNamespace(PointCloud2=object),
        'std_msgs.msg': SimpleNamespace(String=SimpleNamespace),
        'std_srvs.srv': SimpleNamespace(Trigger=object),
    }.items():
        monkeypatch.setitem(sys.modules, name, module)
    monkeypatch.setattr(ros_app.time, 'monotonic', lambda: 50.0)
    monkeypatch.setattr(ros_app.time, 'time_ns', lambda: 10_000_000_000)
    node = ros_app.make_node(autostart=True, ignore_capture_age=compatibility,
                             allow_scan_gaps=compatibility)
    cloud = SimpleNamespace(header=SimpleNamespace(frame_id='base_link', stamp=stamp(9_900_000_000)),
        width=72, height=1, point_step=12, row_step=864, is_bigendian=False,
        fields=[SimpleNamespace(name=n, offset=i*4, datatype=7, count=1) for i,n in enumerate('xyz')],
        data=b''.join(struct.pack('<fff', 3*math.cos(i*math.pi/36), 3*math.sin(i*math.pi/36), 0)
                      for i in range(72)))
    if compatibility:
        cloud.header.stamp = stamp(1_000_000_000)
        cloud.width = 1
        cloud.row_step = 12
        cloud.data = struct.pack('<fff', 3, 0, 0)
    node.cloud(cloud)
    node.tick()
    assert node.autostart_pending and not node.controller.active
    pose = odometry(x=0)
    pose.header.stamp = cloud.header.stamp
    node.odom(pose)
    node.cloud(cloud)
    node.tick()
    assert node.controller.active and not node.autostart_pending
    monkeypatch.setattr(ros_app.time, 'monotonic', lambda: 50.36)
    node.tick()
    assert not node.controller.active
    assert node.controller.reason == 'stale_scan'


@pytest.mark.parametrize("source", [1_000_000_000, 4_310_489_014_000_000])
def test_clock_bypass_accepts_offset_but_rejects_replays_and_still_expires(source):
    gate = ExposureGate(ignore_capture_age=True)
    observed = {}
    for topic in ("scan", "odom"):
        observed[topic] = gate.capture_time(topic, stamp(source), wall_ns=10_000_000_000, monotonic=50)
        assert observed[topic] == 50
        assert gate.capture_time(topic, stamp(source), wall_ns=10_000_000_000, monotonic=50.2) is None
        assert gate.capture_time(topic, stamp(source - 1), wall_ns=10_000_000_000, monotonic=50.2) is None
    assert stale_observation(observed, 50.36) == "stale_scan"
    observed["scan"] = 50.36
    assert stale_observation(observed, 50.36) == "stale_odom"


@pytest.mark.parametrize("stamp_value", [stamp(0), SimpleNamespace(sec=1.0, nanosec=0),
                                        SimpleNamespace(sec=1, nanosec=-1)])
def test_clock_bypass_rejects_invalid_stamps(stamp_value):
    gate = ExposureGate(ignore_capture_age=True)
    assert gate.capture_time("scan", stamp_value, wall_ns=10_000_000_000, monotonic=50) is None
