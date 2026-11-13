"""Pure conversions from Go2 WebRTC messages to ROS-shaped values.

No ROS imports here so the logic is testable anywhere. Message formats follow
unitree_webrtc_connect@e0abc5780761539eff89a382a79319e5cf6ad1f4.
"""

import json
import math
import struct

import lz4.block
import numpy as np

# Unitree SDK motor order (LowState.motor_state[0:12]) and the Go2 URDF names.
JOINT_NAMES = [
    f"{leg}_{part}_joint" for leg in ("FR", "FL", "RR", "RL") for part in ("hip", "thigh", "calf")
]

# Unitree sport API ids accepted from /api/sport/request, mapped to agent commands.
SPORT_MOVE, SPORT_STOP_MOVE, SPORT_STAND_DOWN, SPORT_RECOVERY_STAND, SPORT_HELLO = 1008, 1003, 1005, 1006, 1016


def parse_envelope(data: bytes, binary: bool):
    """Split a robot message into (topic, data dict, payload bytes or None)."""
    if not binary:
        envelope = json.loads(data)
        return envelope.get("topic", ""), envelope.get("data"), None
    # LiDAR framing: u16 2, u16 0, u32 JSON length, 4 reserved bytes, JSON, payload.
    # Other binary: u16 JSON length, 2 reserved bytes, JSON, payload.
    first, second = struct.unpack_from("<HH", data, 0)
    if first == 2 and second == 0:
        (length,) = struct.unpack_from("<I", data, 4)
        start = 12
    else:
        length, start = first, 4
    envelope = json.loads(data[start:start + length])
    return envelope.get("topic", ""), envelope.get("data"), bytes(data[start + length:])


# The next two functions are adapted from unitree_webrtc_connect
# lidar/lidar_decoder_native.py (BSD-2-Clause, see LICENSE-unitree_webrtc_connect).
def decode_voxels(payload: bytes, meta: dict) -> np.ndarray:
    """Occupied voxel centres as an (N, 3) float32 array in the map's frame."""
    raw = lz4.block.decompress(payload, uncompressed_size=int(meta["src_size"]))
    buf = np.frombuffer(raw, dtype=np.uint8)
    nonzero = np.nonzero(buf)[0]
    if len(nonzero) == 0:
        return np.empty((0, 3), dtype=np.float32)
    bits = np.unpackbits(buf[nonzero]).reshape(-1, 8)
    z = nonzero // 0x800
    y = (nonzero % 0x800) // 0x10
    x_base = (nonzero % 0x10) * 8
    x = np.repeat(x_base, 8) + np.tile(np.arange(8), len(nonzero))
    mask = bits.ravel() == 1
    grid = np.column_stack((x[mask], np.repeat(y, 8)[mask], np.repeat(z, 8)[mask]))
    resolution = float(meta.get("resolution", 0.05))
    return (grid * resolution + np.asarray(meta["origin"], dtype=np.float64)).astype(np.float32)


def quaternion_xyzw(wxyz):
    """Unitree quaternions are [w, x, y, z]; ROS uses x, y, z, w."""
    w, x, y, z = (float(v) for v in wxyz)
    return x, y, z, w


def yaw_of(wxyz):
    w, x, y, z = (float(v) for v in wxyz)
    return math.atan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z))


def points_to_body(points, position, quaternion_xyzw):
    """Express odom-frame points in base_link, given the robot pose in odom."""
    x, y, z, w = quaternion_xyzw
    rotation = np.array([
        [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
        [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
        [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)]])
    # Row vectors: (p - t) @ R equals R^T (p - t).
    return ((points.astype(np.float64) - np.asarray(position, dtype=np.float64)) @ rotation).astype(np.float32)


def world_to_body(vx, vy, yaw):
    c, s = math.cos(yaw), math.sin(yaw)
    return c * vx + s * vy, -s * vx + c * vy


def joint_state(lowstate: dict):
    """(names, positions, velocities, efforts) for the 12 leg motors."""
    motors = (lowstate.get("motor_state") or [])[:12]
    if len(motors) < 12:
        return None
    return (
        JOINT_NAMES,
        [float(m.get("q", 0.0)) for m in motors],
        [float(m.get("dq", 0.0)) for m in motors],
        [float(m.get("tau_est", 0.0)) for m in motors],
    )


def joy(controller: dict):
    """sensor_msgs/Joy axes [lx, ly, rx, ry] and the 16 key bits as buttons."""
    axes = [float(controller.get(k, 0.0)) for k in ("lx", "ly", "rx", "ry")]
    keys = int(controller.get("keys", 0))
    return axes, [(keys >> bit) & 1 for bit in range(16)]


def sport_request(api_id: int, parameter: str):
    """Map a unitree_api Request to (command, forward, lateral, yaw), or None if not allowed."""
    if api_id == SPORT_MOVE:
        p = json.loads(parameter or "{}")
        return "move", float(p.get("x", 0.0)), float(p.get("y", 0.0)), float(p.get("z", 0.0))
    return {
        SPORT_STOP_MOVE: ("stop", 0.0, 0.0, 0.0),
        SPORT_RECOVERY_STAND: ("stand_up", 0.0, 0.0, 0.0),
        SPORT_STAND_DOWN: ("lie_down", 0.0, 0.0, 0.0),
        SPORT_HELLO: ("hello", 0.0, 0.0, 0.0),
    }.get(api_id)


def clamp(value, limit):
    if not math.isfinite(value):
        return 0.0
    return max(-limit, min(limit, value))


def raw_topic_name(robot_topic: str) -> str:
    """rt/lf/lowstate -> /go2/raw/lf/lowstate"""
    return "/go2/raw/" + robot_topic.removeprefix("rt/")
