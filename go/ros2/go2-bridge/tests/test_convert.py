import json
import math
import struct

import lz4.block
import numpy as np
import pytest

from go2_bridge import convert


def lidar_frame(meta, payload):
    header = json.dumps({"type": "msg", "topic": "rt/utlidar/voxel_map_compressed", "data": meta}).encode()
    return struct.pack("<HHI4x", 2, 0, len(header)) + header + payload


def test_parse_text_and_lidar_envelopes():
    topic, data, payload = convert.parse_envelope(b'{"type":"msg","topic":"rt/lf/lowstate","data":{"a":1}}', False)
    assert (topic, data, payload) == ("rt/lf/lowstate", {"a": 1}, None)
    topic, data, payload = convert.parse_envelope(lidar_frame({"resolution": 0.05}, b"\x01\x02"), True)
    assert topic == "rt/utlidar/voxel_map_compressed" and data == {"resolution": 0.05} and payload == b"\x01\x02"


def test_decode_voxels_places_set_bits_at_voxel_centres():
    grid = bytearray(0x800 * 2)
    grid[0] = 0b10000000              # x=0, y=0, z=0
    grid[0x800 + 0x10 * 3 + 2] = 0b01000000  # x=2*8+1=17, y=3, z=1
    payload = lz4.block.compress(bytes(grid), store_size=False)
    meta = {"src_size": len(grid), "origin": [1.0, 2.0, -0.5], "resolution": 0.1}
    points = convert.decode_voxels(payload, meta)
    expected = np.array([[1.0, 2.0, -0.5], [1.0 + 1.7, 2.0 + 0.3, -0.5 + 0.1]], dtype=np.float32)
    assert points.dtype == np.float32
    np.testing.assert_allclose(points, expected, atol=1e-6)


def test_decode_voxels_empty():
    payload = lz4.block.compress(bytes(64), store_size=False)
    assert convert.decode_voxels(payload, {"src_size": 64, "origin": [0, 0, 0]}).shape == (0, 3)


def test_quaternion_and_yaw():
    half = math.sqrt(0.5)
    assert convert.quaternion_xyzw([half, 0, 0, half]) == (0.0, 0.0, half, half)
    assert convert.yaw_of([half, 0, 0, half]) == pytest.approx(math.pi / 2)
    vx, vy = convert.world_to_body(0.0, 1.0, math.pi / 2)
    assert (vx, vy) == pytest.approx((1.0, 0.0))


def test_joint_state_uses_sdk_motor_order():
    low = {"motor_state": [{"q": i, "dq": -i, "tau_est": 2 * i} for i in range(20)]}
    names, q, dq, tau = convert.joint_state(low)
    assert names[0] == "FR_hip_joint" and names[11] == "RL_calf_joint"
    assert q == [float(i) for i in range(12)] and dq[3] == -3.0 and tau[11] == 22.0
    assert convert.joint_state({"motor_state": [{}] * 4}) is None


def test_joy_axes_and_key_bits():
    axes, buttons = convert.joy({"lx": 0.5, "ly": -1, "rx": 0, "ry": 0.25, "keys": 0b101})
    assert axes == [0.5, -1.0, 0.0, 0.25]
    assert buttons[:4] == [1, 0, 1, 0] and len(buttons) == 16


def test_sport_request_allowlist():
    assert convert.sport_request(1008, '{"x":0.2,"y":0.1,"z":-0.3}') == ("move", 0.2, 0.1, -0.3)
    assert convert.sport_request(1003, "") == ("stop", 0.0, 0.0, 0.0)
    assert convert.sport_request(1006, "")[0] == "stand_up"
    assert convert.sport_request(1005, "")[0] == "lie_down"
    assert convert.sport_request(1016, "")[0] == "hello"
    assert convert.sport_request(1009, "") is None  # Sit is not allowed
    assert convert.sport_request(1001, "") is None  # Damp is not allowed


def test_clamp_and_raw_topic_name():
    assert convert.clamp(5, 0.3) == 0.3 and convert.clamp(-5, 0.6) == -0.6
    assert convert.clamp(float("nan"), 0.3) == 0.0
    assert convert.raw_topic_name("rt/lf/lowstate") == "/go2/raw/lf/lowstate"


def test_points_to_body_undoes_robot_pose():
    half = math.sqrt(0.5)
    # Robot at (1, 2, 0.3) facing +y (yaw 90 degrees); a point 1 m ahead of it.
    body = convert.points_to_body(np.array([[1.0, 3.0, 0.3]]), (1.0, 2.0, 0.3), (0.0, 0.0, half, half))
    np.testing.assert_allclose(body, [[1.0, 0.0, 0.0]], atol=1e-6)
