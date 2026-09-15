"""ROS message admission checks that do not require rclpy."""

import importlib.util
import json
import math
import struct
from pathlib import Path
import sys
from types import SimpleNamespace

import pytest


spec = importlib.util.spec_from_file_location("patrol_ros_app", Path(__file__).with_name("ros_app.py"))
ros_app = importlib.util.module_from_spec(spec)
spec.loader.exec_module(ros_app)


def stamp(value):
    sec, nanosec = divmod(value, 1_000_000_000)
    return SimpleNamespace(sec=sec, nanosec=nanosec)


def odometry(frame="odom", child="base_link", quaternion=(0, 0, 0, 1), x=1):
    return SimpleNamespace(header=SimpleNamespace(frame_id=frame), child_frame_id=child,
        pose=SimpleNamespace(pose=SimpleNamespace(position=SimpleNamespace(x=x, y=2, z=0.3),
            orientation=SimpleNamespace(**dict(zip(("x", "y", "z", "w"), quaternion))))))


def scan(**overrides):
    fields = dict(header=SimpleNamespace(frame_id="base_footprint"), angle_min=-math.pi,
                  angle_max=math.pi - math.pi / 180, angle_increment=math.pi / 180,
                  range_min=0.1, range_max=12.0, ranges=[4.0] * 360)
    fields.update(overrides)
    return SimpleNamespace(**fields)


def test_capture_age_is_preserved_and_each_topic_advances_independently():
    gate = ros_app.ExposureGate()
    assert gate.capture_time("scan", stamp(9_800_000_000), wall_ns=10_000_000_000,
                             monotonic=50) == pytest.approx(49.8)
    assert gate.capture_time("odom", stamp(9_800_000_000), wall_ns=10_000_000_000,
                             monotonic=50) == pytest.approx(49.8)
    assert gate.capture_time("scan", stamp(9_800_000_000), wall_ns=10_010_000_000,
                             monotonic=50.01) is None
    assert gate.capture_time("scan", stamp(9_700_000_000), wall_ns=10_010_000_000,
                             monotonic=50.01) is None
    assert gate.capture_time("scan", stamp(9_900_000_000), wall_ns=10_010_000_000,
                             monotonic=50.01) == pytest.approx(49.9)


@pytest.mark.parametrize("source", [0, 9_650_000_000, 10_060_000_000])
def test_old_future_and_zero_stamps_do_not_refresh_sensor_gate(source):
    gate = ros_app.ExposureGate()
    assert gate.capture_time("scan", stamp(source), wall_ns=10_000_000_000, monotonic=50) is None
    assert not gate.last_stamps


@pytest.mark.parametrize("sec,nanosec", [(9, -1), (9, 1_000_000_000),
    (9, float("nan")), (float("inf"), 0), (9.0, 0), (True, 0)])
@pytest.mark.parametrize("ignore_capture_age", [False, True])
def test_malformed_stamp_fields_are_rejected(sec, nanosec, ignore_capture_age):
    gate = ros_app.ExposureGate(ignore_capture_age=ignore_capture_age)
    assert gate.capture_time("scan", SimpleNamespace(sec=sec, nanosec=nanosec),
                             wall_ns=10_000_000_000, monotonic=50) is None


@pytest.mark.parametrize("source", [1_000_000_000, 4_310_489_014_000_000])
def test_ignore_capture_age_uses_arrival_time_and_requires_advancing_stamps(source):
    gate = ros_app.ExposureGate(ignore_capture_age=True)
    for topic in ("scan", "odom"):
        assert gate.capture_time(topic, stamp(source), wall_ns=10_000_000_000, monotonic=50) == 50
        assert gate.ages[topic] == pytest.approx((10_000_000_000 - source) / 1e9)
        for replay in (source, source - 1):
            assert gate.capture_time(topic, stamp(replay), wall_ns=10_010_000_000, monotonic=50.01) is None
            assert gate.rejections[topic] == "capture_not_advancing"
        assert gate.capture_time(topic, stamp(source + 1), wall_ns=10_020_000_000, monotonic=50.02) == 50.02
    assert gate.capture_time("scan", stamp(0), wall_ns=10_000_000_000, monotonic=50) is None
    assert gate.rejections["scan"] == "nonpositive_capture_stamp"


def test_unit_quaternion_is_converted_to_heading():
    assert ros_app.pose_values(odometry(quaternion=(0, 0, math.sin(0.5), math.cos(0.5)))) == pytest.approx((1, 2, 1))


@pytest.mark.parametrize("message", [odometry(frame="map"), odometry(child="camera_link"),
    odometry(x=float("nan")), odometry(quaternion=(0, 0, 0, 0)),
    odometry(quaternion=(0, 0, float("inf"), 1)), odometry(quaternion=(0, 0, 0, 2))])
def test_invalid_odometry_frames_or_pose_are_rejected(message):
    assert ros_app.pose_values(message) is None


def test_valid_scan_metadata_is_accepted():
    assert ros_app.scan_metadata_valid(scan())


@pytest.mark.parametrize("overrides", [dict(header=SimpleNamespace(frame_id="camera_link")),
    dict(angle_increment=0), dict(angle_increment=float("nan")), dict(angle_max=0),
    dict(range_min=-1), dict(range_min=12), dict(range_max=float("inf"))])
def test_invalid_scan_frame_or_geometry_is_rejected(overrides):
    assert not ros_app.scan_metadata_valid(scan(**overrides))


@pytest.fixture
def node_factory(monkeypatch):
    class Publisher:
        def __init__(self):
            self.messages = []

        def publish(self, message):
            self.messages.append(message)

    class Node:
        def __init__(self, name):
            self.name = name

        def create_publisher(self, message_type, topic, qos):
            return Publisher()

        def create_subscription(self, *args):
            pass

        def create_service(self, *args):
            pass

        def create_timer(self, *args):
            pass

    class Twist:
        def __init__(self):
            self.header = SimpleNamespace(identity=SimpleNamespace(id=0, api_id=0),
                                          policy=SimpleNamespace(noreply=False))
            self.parameter = ""

    for name, module in {
        "unitree_api.msg": SimpleNamespace(Request=Twist),
        "nav_msgs.msg": SimpleNamespace(Odometry=object),
        "sensor_msgs.msg": SimpleNamespace(PointCloud2=object),
        "std_msgs.msg": SimpleNamespace(String=SimpleNamespace),
        "std_srvs.srv": SimpleNamespace(Trigger=object),
        "rclpy.node": SimpleNamespace(Node=Node),
        "rclpy.qos": SimpleNamespace(QoSProfile=SimpleNamespace,
                                     ReliabilityPolicy=SimpleNamespace(BEST_EFFORT=1)),
    }.items():
        monkeypatch.setitem(sys.modules, name, module)
    monkeypatch.setattr(ros_app.time, "monotonic", lambda: 50.0)
    monkeypatch.setattr(ros_app.time, "time_ns", lambda: 10_000_000_000)
    return ros_app.make_node


@pytest.fixture
def node(node_factory):
    return node_factory()


@pytest.fixture
def autostart_node(node_factory):
    return node_factory(autostart=True)


def deliver_observations(node, captured=9_900_000_000):
    pose = odometry(x=0)
    pose.pose.pose.position.y = 0
    pose.header.stamp = stamp(captured)
    laser = scan()
    laser.header.stamp = stamp(captured)
    node.odom(pose)
    node.scan(laser)


def last_command(node):
    command = node.drive.messages[-1]
    assert command.header.identity.api_id == 1008
    assert command.header.policy.noreply
    values = json.loads(command.parameter)
    assert values["y"] == 0
    return values["x"], values["z"]


@pytest.mark.parametrize("missing_topic", ["scan", "odom"])
@pytest.mark.parametrize("clock_offset_seconds", [-4_310_479.014, 4_310_479.014])
def test_unsynchronized_autostart_still_stops_when_either_stream_expires(
        node_factory, monkeypatch, missing_topic, clock_offset_seconds):
    node = node_factory(autostart=True, ignore_capture_age=True)
    source = 10_000_000_000_000_000
    wall = source - round(clock_offset_seconds * 1e9)
    monkeypatch.setattr(ros_app.time, "time_ns", lambda: wall)
    deliver_observations(node, source)
    node.tick()
    assert node.controller.active
    assert last_command(node) == (0.55, 0)
    assert json.loads(node.status_pub.messages[-1].data)["ignore_capture_age"] is True

    monkeypatch.setattr(ros_app.time, "monotonic", lambda: 50.36)
    monkeypatch.setattr(ros_app.time, "time_ns", lambda: wall + 360_000_000)
    if missing_topic == "scan":
        message = odometry(x=0)
        message.pose.pose.position.y = 0
        message.header.stamp = stamp(source + 360_000_000)
        node.odom(message)
    else:
        message = scan()
        message.header.stamp = stamp(source + 360_000_000)
        node.scan(message)
    node.tick()
    assert not node.controller.active
    assert node.controller.reason == "stale_" + missing_topic
    assert last_command(node) == (0, 0)


def test_ros_node_publishes_zero_on_startup_and_requires_start_service(node):
    assert len(node.drive.messages) == 1
    assert last_command(node) == (0, 0)
    deliver_observations(node)
    node.tick()
    assert last_command(node) == (0, 0)
    response = node.start(None, SimpleNamespace())
    assert response.success
    node.tick()
    assert last_command(node) == (0.55, 0)
    assert node.status_pub.messages[-1].data


def test_stop_service_publishes_zero_immediately_and_fresh_sensors_do_not_restart(node):
    deliver_observations(node)
    assert node.start(None, SimpleNamespace()).success
    node.tick()
    assert last_command(node)[0] > 0
    assert node.stop(None, SimpleNamespace()).success
    assert last_command(node) == (0, 0)
    deliver_observations(node, 9_950_000_000)
    node.tick()
    assert last_command(node) == (0, 0)


@pytest.mark.parametrize("fault", ["scan_frame", "odom_frame", "replayed_scan", "obstacle"])
def test_bad_observation_stops_ros_publisher_before_next_timer_tick(node, fault):
    deliver_observations(node)
    assert node.start(None, SimpleNamespace()).success
    node.tick()
    if fault == "odom_frame":
        pose = odometry(frame="map")
        pose.header.stamp = stamp(9_920_000_000)
        node.odom(pose)
    else:
        laser = scan()
        laser.header.stamp = stamp(9_900_000_000 if fault == "replayed_scan" else 9_920_000_000)
        if fault == "scan_frame":
            laser.header.frame_id = "camera_link"
        if fault == "obstacle":
            laser.ranges[180] = 0.5
        node.scan(laser)
    assert last_command(node) == (0, 0)
    assert not node.controller.active
    deliver_observations(node, 9_950_000_000)
    node.tick()
    assert last_command(node) == (0, 0)


def test_start_service_rejects_missing_observations_and_publishes_zero(node):
    response = node.start(None, SimpleNamespace())
    assert not response.success
    assert "stale_scan" in response.message
    assert last_command(node) == (0, 0)


def test_autostart_waits_for_both_observations_then_requests_motion_once(autostart_node):
    node = autostart_node
    assert len(node.drive.messages) == 1
    assert last_command(node) == (0, 0)
    node.tick()
    assert json.loads(node.status_pub.messages[-1].data)["autostart_pending"]
    laser = scan()
    laser.header.stamp = stamp(9_900_000_000)
    node.scan(laser)
    node.tick()
    assert not node.controller.active
    assert node.autostart_pending
    assert last_command(node) == (0, 0)
    pose = odometry(x=0)
    pose.pose.pose.position.y = 0
    pose.header.stamp = stamp(9_900_000_000)
    node.odom(pose)
    node.tick()
    assert node.controller.active
    assert not node.autostart_pending
    assert last_command(node) == (0.55, 0)
    targets = node.controller.targets
    pose.pose.pose.position.x = 0.2
    pose.header.stamp = stamp(9_950_000_000)
    node.odom(pose)
    node.tick()
    assert node.controller.targets == targets
    assert not json.loads(node.status_pub.messages[-1].data)["autostart_pending"]


@pytest.mark.parametrize("not_ready", ["stale", "obstacle", "unknown_scan"])
def test_autostart_waits_for_current_clear_observations(autostart_node, not_ready):
    node = autostart_node
    deliver_observations(node, 9_600_000_000 if not_ready == "stale" else 9_800_000_000)
    if not_ready != "stale":
        laser = scan()
        laser.header.stamp = stamp(9_900_000_000)
        laser.ranges[180] = 0.5 if not_ready == "obstacle" else float("inf")
        node.scan(laser)
    node.tick()
    assert not node.controller.active
    assert node.autostart_pending
    assert last_command(node) == (0, 0)
    deliver_observations(node, 9_950_000_000)
    node.tick()
    assert node.controller.active
    assert not node.autostart_pending


@pytest.mark.parametrize("service", ["start", "stop"])
def test_explicit_service_cancels_pending_autostart_even_if_start_fails(autostart_node, service):
    node = autostart_node
    response = getattr(node, service)(None, SimpleNamespace())
    assert response.success == (service == "stop")
    assert not node.autostart_pending
    deliver_observations(node)
    node.tick()
    assert not node.controller.active
    assert last_command(node) == (0, 0)


def test_autostart_bounds_failure_does_not_retry_from_later_pose(autostart_node):
    node = autostart_node
    deliver_observations(node, 9_800_000_000)
    pose = odometry(x=4.5)
    pose.header.stamp = stamp(9_900_000_000)
    node.odom(pose)
    node.tick()
    assert node.controller.reason == "route_outside_patrol_bounds"
    assert not node.autostart_pending
    deliver_observations(node, 9_950_000_000)
    node.tick()
    assert not node.controller.active
    assert last_command(node) == (0, 0)


@pytest.mark.parametrize("fault", ["stop", "stale", "invalid_pose", "obstacle"])
def test_started_automatic_route_does_not_restart_after_stop_or_fault(autostart_node, monkeypatch, fault):
    node = autostart_node
    deliver_observations(node, 9_800_000_000)
    node.tick()
    assert node.controller.active
    if fault == "stop":
        assert node.stop(None, SimpleNamespace()).success
    elif fault == "stale":
        monkeypatch.setattr(ros_app.time, "monotonic", lambda: 50.5)
        monkeypatch.setattr(ros_app.time, "time_ns", lambda: 10_500_000_000)
        node.tick()
    elif fault == "invalid_pose":
        pose = odometry(x=float("nan"))
        pose.header.stamp = stamp(9_900_000_000)
        node.odom(pose)
    else:
        laser = scan()
        laser.header.stamp = stamp(9_900_000_000)
        laser.ranges[180] = 0.5
        node.scan(laser)
    assert not node.controller.active
    assert not node.autostart_pending
    assert last_command(node) == (0, 0)
    deliver_observations(node, 10_450_000_000 if fault == "stale" else 9_950_000_000)
    node.tick()
    assert not node.controller.active
    assert last_command(node) == (0, 0)
    assert node.start(None, SimpleNamespace()).success


def test_completed_automatic_route_holds_zero_with_new_observations(autostart_node):
    node = autostart_node
    deliver_observations(node, 9_800_000_000)
    node.tick()
    for index, (x, y) in enumerate(node.controller.targets):
        pose = odometry(x=x)
        pose.pose.pose.position.y = y
        pose.header.stamp = stamp(9_850_000_000 + index * 10_000_000)
        node.odom(pose)
        node.tick()
    assert node.controller.state == "complete"
    assert not node.autostart_pending
    assert last_command(node) == (0, 0)
    deliver_observations(node, 9_950_000_000)
    node.tick()
    assert node.controller.state == "complete"
    assert last_command(node) == (0, 0)


def test_callback_scheduling_jitter_does_not_reverse_capture_order():
    gate=ros_app.ExposureGate()
    first=gate.capture_time('odom',stamp(9_900_000_000),wall_ns=10_000_000_000,monotonic=50)
    # The second callback is descheduled between reading wall and monotonic clocks.
    second=gate.capture_time('odom',stamp(9_920_000_000),wall_ns=10_020_000_000,monotonic=50.08)
    third=gate.capture_time('odom',stamp(9_940_000_000),wall_ns=10_040_000_000,monotonic=50.08)
    assert first < second < third
    assert (first,second,third)==pytest.approx((49.9,49.92,49.94))


def test_malformed_cloud_invalidates_admission_before_autostart(autostart_node):
    node=autostart_node
    deliver_observations(node)
    node.cloud(SimpleNamespace(header=SimpleNamespace(frame_id='odom')))
    node.tick()
    assert not node.controller.active
    assert node.controller.scan_at is None
    assert last_command(node)==(0,0)


def test_sparse_native_cloud_autostarts_and_measured_obstacle_stops(node_factory, monkeypatch):
    node = node_factory(autostart=True, ignore_capture_age=True, allow_scan_gaps=True)
    pose = odometry(x=0)
    pose.pose.pose.position.y = 0
    pose.header.stamp = stamp(1_000_000_000)
    node.odom(pose)
    cloud = SimpleNamespace(header=SimpleNamespace(stamp=pose.header.stamp, frame_id="base_link"),
        width=1, height=1, point_step=12, row_step=12, is_bigendian=False,
        data=struct.pack("<fff", 4.0, 0, 0.1),
        fields=[SimpleNamespace(name=name, offset=index * 4, count=1, datatype=7)
                for index, name in enumerate("xyz")])
    node.cloud(cloud)
    node.tick()
    assert node.controller.active
    assert last_command(node) == (0.55, 0)
    status = json.loads(node.status_pub.messages[-1].data)
    assert status["scan_coverage"] == {"observed": 1, "total": 72, "front_observed": 1, "front_total": 13}
    assert status["allow_scan_gaps"] is True
    monkeypatch.setattr(ros_app.time, "monotonic", lambda: 50.05)
    cloud.header.stamp = stamp(1_050_000_000)
    cloud.data = struct.pack("<fff", 0.4, 0, 0.1)
    node.cloud(cloud)
    assert not node.controller.active
    assert node.controller.reason == "obstacle"
    assert last_command(node) == (0, 0)


def test_unknown_scan_log_reports_coverage_and_sensor_error(autostart_node, capsys):
    node = autostart_node
    deliver_observations(node, 9_800_000_000)
    laser = scan()
    laser.header.stamp = stamp(9_900_000_000)
    laser.ranges[0] = math.inf
    node.scan(laser)
    node.tick()
    assert "Observed sectors: 359/360; front: 61/61" in capsys.readouterr().out
    status = json.loads(node.status_pub.messages[-1].data)
    assert status["sensor_errors"]["scan"] == "unknown_scan"
    assert status["scan_coverage"]["observed"] == 359
    assert last_command(node) == (0, 0)


@pytest.mark.parametrize("source,reason,age", [
    (9_000_000_000, "capture_too_old", 1.0),
    (11_000_000_000, "capture_in_future", -1.0),
])
def test_capture_rejection_reports_clock_direction_and_age(source, reason, age):
    gate = ros_app.ExposureGate()
    assert gate.capture_time("odom", stamp(source), wall_ns=10_000_000_000, monotonic=50) is None
    assert gate.rejections["odom"] == reason
    assert gate.ages["odom"] == age
    assert gate.capture_time("odom", stamp(9_900_000_000), wall_ns=10_000_000_000, monotonic=50)
    assert "odom" not in gate.rejections


def test_rejected_odometry_and_dependent_clouds_do_not_alternate_waiting_logs(autostart_node, capsys, monkeypatch):
    node = autostart_node
    pose = odometry()
    pose.header.stamp = stamp(1_000_000_000)
    for index in range(100):
        monkeypatch.setattr(ros_app.time, "monotonic", lambda: 50.0 + index)
        node.odom(pose)
        node.tick()
        node.cloud(SimpleNamespace())
        node.tick()
    lines = capsys.readouterr().out.splitlines()
    assert len(lines) == 1
    assert "odometry_capture_too_old" in lines[0]
    assert "Capture age: 9.000s" in lines[0]
    status = json.loads(node.status_pub.messages[-1].data)
    assert status["sensor_errors"] == {"odom": "odometry_capture_too_old", "scan": "waiting_for_odometry_orientation"}
    assert not node.controller.active
    assert last_command(node) == (0, 0)


def test_changed_waiting_errors_are_rate_limited_but_start_is_immediate(autostart_node, capsys, monkeypatch):
    node = autostart_node
    node.tick()
    pose = odometry()
    pose.header.stamp = stamp(1_000_000_000)
    node.odom(pose)
    node.tick()
    assert len(capsys.readouterr().out.splitlines()) == 1
    monkeypatch.setattr(ros_app.time, "monotonic", lambda: 55.0)
    monkeypatch.setattr(ros_app.time, "time_ns", lambda: 15_000_000_000)
    node.tick()
    assert "odometry_capture_too_old" in capsys.readouterr().out
    deliver_observations(node, 14_900_000_000)
    node.tick()
    assert node.controller.active
    assert "waypoint 1/4" in capsys.readouterr().out
