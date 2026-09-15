"""Sensor admission, wire image and HTTP checks; no ROS installation needed."""

from copy import deepcopy
import json
import math
import struct
import threading
from types import SimpleNamespace as NS
from urllib.error import HTTPError
from urllib.request import urlopen
import zlib

import pytest

from dashboard import SensorStore, make_server, png_image, image_values, scan_values


def header(frame, source=10_000_000_000):
    sec, nanosec = divmod(source, 1_000_000_000)
    return NS(frame_id=frame, stamp=NS(sec=sec, nanosec=nanosec))


def camera(source=10_000_000_000):
    # Two rows, one RGB pixel and one byte of row padding each.
    return NS(header=header("camera_optical_frame", source), width=1, height=2,
              step=4, encoding="rgb8", data=b"\xff\x00\x00\x99\x00\xff\x00\x99")


def odom(x=0, source=10_000_000_000):
    zero = NS(x=0, y=0, z=0)
    return NS(header=header("odom", source), child_frame_id="base_link",
              pose=NS(pose=NS(position=NS(x=x, y=0, z=0.3), orientation=NS(x=0, y=0, z=0, w=1))),
              twist=NS(twist=NS(linear=zero)))


def scan():
    return NS(header=header("lidar_link"), ranges=[1, math.inf, math.nan, -1, 13, 2],
              angle_min=-math.pi, angle_increment=math.pi / 3, range_min=0.1, range_max=12)


def test_png_preserves_rgb_pixels_and_ignores_row_padding():
    metadata, pixels = image_values(camera())
    encoded = png_image(metadata, pixels)
    assert encoded.startswith(b"\x89PNG\r\n\x1a\n")
    cursor, chunks = 8, {}
    while cursor < len(encoded):
        length = struct.unpack_from("!I", encoded, cursor)[0]
        kind = encoded[cursor + 4:cursor + 8]
        payload = encoded[cursor + 8:cursor + 8 + length]
        checksum = struct.unpack_from("!I", encoded, cursor + 8 + length)[0]
        assert checksum == zlib.crc32(kind + payload)
        chunks[kind] = payload
        cursor += 12 + length
    assert struct.unpack("!2I5B", chunks[b"IHDR"]) == (1, 2, 8, 2, 0, 0, 0)
    assert zlib.decompress(chunks[b"IDAT"]) == b"\x00\xff\x00\x00\x00\x00\xff\x00"


@pytest.mark.parametrize("change", [dict(encoding="bgr8"), dict(step=2), dict(width=1921),
                                          dict(height=0), dict(data=b""), dict(step=999999)])
def test_malformed_images_are_rejected(change):
    message = camera()
    message.__dict__.update(change)
    with pytest.raises(ValueError):
        image_values(message)


def test_delayed_capture_expires_at_source_age_and_recovery_is_read_only():
    store = SensorStore()
    assert store.observe("camera", camera(), wall_ns=10_400_000_000, now=20)
    assert store.snapshot(20)["topics"]["camera"]["age_ms"] == pytest.approx(400)
    assert store.camera_png(20.1) is not None
    assert store.camera_png(20.21) is None
    assert store.snapshot(20.21)["topics"]["camera"]["state"] == "stale"
    assert store.observe("camera", camera(10_700_000_000), wall_ns=10_700_000_000, now=20.3)
    assert store.camera_png(20.3) is not None


@pytest.mark.parametrize("source", [0, 9_399_000_000, 10_051_000_000])
def test_old_zero_and_future_timestamps_do_not_count_as_delivery(source):
    store = SensorStore()
    assert not store.observe("camera", camera(source), wall_ns=10_000_000_000, now=20)
    topic = store.snapshot(20)["topics"]["camera"]
    assert topic["state"] == "waiting"
    assert topic["rate_hz"] == 0


def test_repeated_and_reordered_samples_do_not_refresh_rate_or_freshness():
    store = SensorStore()
    assert store.observe("camera", camera(), wall_ns=10_000_000_000, now=20)
    for source in (10_000_000_000, 9_999_000_000):
        assert not store.observe("camera", camera(source), wall_ns=10_100_000_000, now=20.1)
    assert store.snapshot(20.1)["topics"]["camera"]["rate_hz"] == 0.5
    assert store.snapshot(22.1)["topics"]["camera"]["rate_hz"] == 0


def test_unknown_lidar_is_null_in_strict_json_and_has_no_invented_clearance():
    values = scan_values(scan())
    assert values["ranges"] == [1, None, None, None, None, 2]
    assert values["coverage"] == pytest.approx(1 / 3)
    assert values["nearest"] == 1
    message = scan()
    message.ranges = [math.inf] * 360
    values = scan_values(message)
    assert values["coverage"] == 0
    assert values["nearest"] is None
    json.dumps(values, allow_nan=False)


def test_pose_trail_is_bounded_and_resets_at_discontinuity():
    store = SensorStore()
    for i in range(605):
        source = 10_000_000_000 + i * 250_000_000
        assert store.observe("odom", odom(i / 100, source), wall_ns=source, now=20 + i / 4)
    snapshot = store.snapshot(171)
    assert len(snapshot["trail"]) == 600
    assert snapshot["trail_total"] == 605
    assert store.observe("odom", odom(-4, 162_000_000_000), wall_ns=162_000_000_000, now=172)
    assert store.snapshot(172)["trail"] == [[-4, 0]]


@pytest.mark.parametrize("change", ["parent", "child", "nan", "quaternion"])
def test_invalid_odometry_cannot_enter_trail(change):
    store, message = SensorStore(), odom()
    if change == "parent": message.header.frame_id = "map"
    if change == "child": message.child_frame_id = "camera_link"
    if change == "nan": message.pose.pose.position.x = math.nan
    if change == "quaternion": message.pose.pose.orientation.w = 0
    assert not store.observe("odom", message, wall_ns=10_000_000_000, now=20)
    assert store.snapshot(20)["trail"] == []


def test_joint_and_imu_data_remain_finite_and_valid():
    store = SensorStore()
    joint = NS(header=header(""), name=["hip", "knee"], position=[0.1, 0.5])
    assert store.observe("joints", joint, wall_ns=10_000_000_000, now=20)
    invalid = deepcopy(joint)
    invalid.position = [0.1]
    assert not store.observe("joints", invalid, wall_ns=10_000_000_000, now=20)
    imu = NS(header=header("imu_link"), linear_acceleration=NS(x=0, y=0, z=9.81),
             angular_velocity=NS(x=0, y=0, z=0.1), orientation=NS(x=0, y=0, z=0, w=1))
    assert store.observe("imu", imu, wall_ns=10_000_000_000, now=20)
    json.dumps(store.snapshot(20), allow_nan=False)


def test_http_serves_page_and_snapshot_and_withholds_missing_camera():
    server = make_server(SensorStore(), "127.0.0.1", 0)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    base = f"http://127.0.0.1:{server.server_port}"
    try:
        with urlopen(base) as response:
            assert b"Sensor desk" in response.read()
        with urlopen(base + "/api/status") as response:
            assert response.headers["Cache-Control"] == "no-store"
            result = json.load(response)
            assert set(result["topics"]) == {"odom", "imu", "scan", "joints", "camera"}
            assert all(topic["state"] == "waiting" for topic in result["topics"].values())
        for path, code in (("/camera.png", 503), ("/missing", 404)):
            with pytest.raises(HTTPError) as error:
                urlopen(base + path)
            assert error.value.code == code
    finally:
        server.shutdown()
        server.server_close()
        worker.join()
