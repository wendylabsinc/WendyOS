"""Shared native Go2 ROS contract. Copied into standalone examples by sync_examples.py."""
import json
import math
import os
import time
from types import SimpleNamespace

ODOM_TOPIC = "/utlidar/robot_odom"
CLOUD_TOPIC = "/utlidar/cloud_base"
SPORT_TOPIC = "/api/sport/request"
SCAN_COUNT = 72
SCAN_INCREMENT = 2 * math.pi / SCAN_COUNT


def cloud_scan(message, odometry=None):
    """Project measured body-frame obstacle returns; unobserved sectors stay unknown.

    Use the body-height band [-0.2, 0.4] metres, excluding the floor below it.
    Bin minima include every return in that band, regardless of ring/intensity.
    This is a conservative obstacle slice, not a reconstruction of free space.
    """
    if message.header.frame_id != "base_link":
        raise ValueError("Expected /utlidar/cloud_base in base_link")
    if (type(message.width) is not int or type(message.height) is not int
            or not 0 < message.width * message.height <= 200000
            or not 12 <= message.point_step <= 256
            or not message.width * message.point_step <= message.row_step
            or len(message.data) != message.row_step * message.height
            or len(message.data) > 16000000):
        raise ValueError("Invalid PointCloud2 dimensions or row stride")
    fields = {}
    for field in message.fields:
        if field.name in ("x", "y", "z"):
            if (field.name in fields or field.count != 1 or field.datatype not in (7, 8)
                    or field.offset < 0
                    or field.offset + (4 if field.datatype == 7 else 8) > message.point_step):
                raise ValueError("Invalid PointCloud2 XYZ layout")
            fields[field.name] = (field.offset, "f" if field.datatype == 7 else "d")
    if set(fields) != {"x", "y", "z"}:
        raise ValueError("PointCloud2 requires XYZ fields")
    # Vectorized decoding keeps native clouds from starving command/odom callbacks.
    # Explicit strides support organized clouds with padded rows and extra fields.
    import numpy as np
    endian = ">" if message.is_bigendian else "<"
    dtype = np.dtype({"names": list("xyz"),
                      "formats": [endian + fields[n][1] for n in "xyz"],
                      "offsets": [fields[n][0] for n in "xyz"],
                      "itemsize": message.point_step})
    points = np.ndarray((message.height, message.width), dtype=dtype,
                        buffer=bytes(message.data), strides=(message.row_step, message.point_step))
    x, y, z = (points[n].astype(np.float64).reshape(-1) for n in "xyz")
    frame = "base_link"
    if odometry is not None:
        if odometry.header.frame_id != "odom" or odometry.child_frame_id != "base_link":
            raise ValueError("Expected odom to base_link orientation")
        def stamp_ns(stamp):
            return stamp.sec * 1_000_000_000 + stamp.nanosec
        if abs(stamp_ns(message.header.stamp) - stamp_ns(odometry.header.stamp)) > 100_000_000:
            raise ValueError("Cloud and odometry captures differ by more than 100 ms")
        q = odometry.pose.pose.orientation
        values = (q.x, q.y, q.z, q.w)
        if not all(math.isfinite(value) for value in values):
            raise ValueError("Invalid cloud orientation")
        norm = math.hypot(*values)
        if abs(norm - 1) > 0.01:
            raise ValueError("Invalid cloud orientation")
        qx, qy, qz, qw = (value / norm for value in values)
        rotation = np.array([
            [1-2*(qy*qy+qz*qz), 2*(qx*qy-qz*qw), 2*(qx*qz+qy*qw)],
            [2*(qx*qy+qz*qw), 1-2*(qx*qx+qz*qz), 2*(qy*qz-qx*qw)],
            [2*(qx*qz-qy*qw), 2*(qy*qz+qx*qw), 1-2*(qx*qx+qy*qy)]])
        yaw = math.atan2(rotation[1, 0], rotation[0, 0])
        c, sine = math.cos(yaw), math.sin(yaw)
        level = np.array([[c, sine, 0], [-sine, c, 0], [0, 0, 1]]) @ rotation
        # Keep heading-relative XY but align Z with odometry's vertical axis.
        x, y, z = level @ np.vstack((x, y, z))
        frame = "base_footprint"
    radii = np.hypot(x, y)
    valid = (np.isfinite(x) & np.isfinite(y) & np.isfinite(z)
             & (z >= -0.2) & (z <= 0.4) & (radii > 0) & (radii <= 12.0))
    indices = np.floor((np.arctan2(y[valid], x[valid]) + math.pi) / SCAN_INCREMENT + 0.5).astype(int) % SCAN_COUNT
    ranges = np.full(SCAN_COUNT, np.inf)
    np.minimum.at(ranges, indices, radii[valid])
    ranges = ranges.tolist()
    return SimpleNamespace(header=SimpleNamespace(stamp=message.header.stamp, frame_id=frame),
        ranges=ranges, angle_min=-math.pi, angle_max=math.pi-SCAN_INCREMENT,
        angle_increment=SCAN_INCREMENT, range_min=0.0, range_max=12.0)


def sport_request(request_type, x, y, yaw):
    values = (x, y, yaw)
    if any(type(v) not in (int, float) or not math.isfinite(v) for v in values):
        raise ValueError("Sport velocity must be finite")
    if any(abs(v) > limit for v, limit in zip(values, (0.8, 0.5, 1.0))):
        raise ValueError("Sport velocity exceeds example limits")
    message = request_type()
    message.header.identity.id = time.time_ns()
    message.header.identity.api_id = 1008
    message.header.policy.noreply = True
    message.parameter = json.dumps(dict(zip(("x", "y", "z"), map(float, values))), allow_nan=False)
    return message


def sensor_wall_ns(wall_ns):
    """Apply an explicitly measured device clock offset, never learn age from receipt.

    Offset = sensor clock minus application clock. Defaults to synchronized clocks.
    """
    offset = float(os.environ.get("GO2_SENSOR_CLOCK_OFFSET_SECONDS", "0"))
    if not math.isfinite(offset):
        raise ValueError("GO2_SENSOR_CLOCK_OFFSET_SECONDS must be finite")
    return wall_ns + round(offset * 1e9)
