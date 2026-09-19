from types import SimpleNamespace as NS
import pytest
from admission import ExposureGate, HallwayApp


def stamp(ns):
    return NS(sec=ns//1_000_000_000, nanosec=ns%1_000_000_000)


@pytest.mark.parametrize("ignore_age", [False, True])
def test_stamps_must_advance_in_both_clock_modes(ignore_age):
    gate = ExposureGate(ignore_capture_age=ignore_age)
    source = 9_900_000_000 if not ignore_age else 1_000_000_000
    captured = gate.capture_time("odom", stamp(source), wall_ns=10_000_000_000, monotonic=50)
    assert captured == pytest.approx(50 if ignore_age else 49.9)
    for replay in (source, source-1, 0):
        assert gate.capture_time("odom", stamp(replay), wall_ns=10_100_000_000, monotonic=50.1) is None


def test_strict_mode_rejects_old_and_future_captures():
    gate = ExposureGate()
    for source in (1_000_000_000, 20_000_000_000):
        assert gate.capture_time("scan", stamp(source), wall_ns=10_000_000_000, monotonic=50) is None


def test_cloud_without_odometry_invalidates_scan():
    app = HallwayApp()
    assert not app.cloud(NS())
    assert app.controller.scan_at is None
    assert app.sensor_errors["scan"] == "waiting_for_odometry"


def odometry(ns):
    return NS(header=NS(frame_id="odom", stamp=stamp(ns)), child_frame_id="base_link",
              pose=NS(pose=NS(position=NS(x=0., y=0., z=.3),
                              orientation=NS(x=0., y=0., z=0., w=1.))))


def cloud(ns):
    import math
    import struct
    data = b"".join(struct.pack("<fff", 2*math.cos(i*math.pi/36),
                                2*math.sin(i*math.pi/36), 0) for i in range(72))
    return NS(header=NS(frame_id="base_link", stamp=stamp(ns)), width=72, height=1,
              point_step=12, row_step=len(data), data=data, is_bigendian=False,
              fields=[NS(name=n, offset=i*4, datatype=7, count=1) for i,n in enumerate("xyz")])


def test_delayed_cloud_pairs_with_capture_without_rewinding_pose(monkeypatch):
    import admission
    app = HallwayApp(ignore_capture_age=True)
    earlier, latest = odometry(1_000_000_000), odometry(1_240_000_000)
    assert app.odom(earlier, now=50)
    assert app.odom(latest, now=50.24)
    original = admission.cloud_scan
    selected = []
    def project(message, pose):
        selected.append(pose)
        return original(message, pose)
    monkeypatch.setattr(admission, "cloud_scan", project)
    assert app.cloud(cloud(1_000_000_000), now=50.25)
    assert selected == [earlier]
    assert app.latest_odom is latest
    assert app.controller.pose_at == 50.24


def test_missing_matching_capture_still_rejects_cloud():
    app = HallwayApp(ignore_capture_age=True)
    assert app.odom(odometry(1_240_000_000), now=50.24)
    assert not app.cloud(cloud(1_000_000_000), now=50.25)
    assert "100 ms" in app.sensor_errors["scan"]


def test_odometry_history_is_bounded_and_cleared_on_replay():
    app = HallwayApp(ignore_capture_age=True)
    for i in range(100):
        assert app.odom(odometry(1_000_000_000+i*20_000_000), now=50+i*.02)
    assert len(app.odom_history) <= 26
    assert not app.odom(odometry(1_000_000_000), now=52)
    assert not app.odom_history
    assert not app.cloud(cloud(2_980_000_000), now=52)
    assert app.sensor_errors["scan"] == "waiting_for_odometry"
