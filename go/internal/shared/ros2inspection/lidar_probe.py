"""Trusted, bounded ROS 2 LiDAR subscriber; stdout is JSON lines, never raw data.

Kept independent of ROS at import time so binary decoding and geometry can be
tested on the development host. The agent embeds and invokes this fixed source
with one JSON options argument; options never become Python or shell source.
"""

import datetime
import json
import math
import random
import re
import struct
import sys
import time


CLOUD = "sensor_msgs/msg/PointCloud2"
SCAN = "sensor_msgs/msg/LaserScan"
MAX_DATA_BYTES = 64 * 1024 * 1024
MAX_RESULT_BYTES = 64 * 1024
AXES = ("+x", "+x+y", "+y", "-x+y", "-x", "-x-y", "-y", "+x-y")


class ProbeError(Exception):
    def __init__(self, code, message):
        super().__init__(message)
        self.code = code


def options_from_json(value):
    if not isinstance(value, dict):
        raise ProbeError("invalid_options", "Options must be a JSON object")
    opts = dict(value)
    topic = opts.get("topic", "")
    if not isinstance(topic, str) or len(topic) > 255 or not re.fullmatch(
            r"/[A-Za-z_][A-Za-z0-9_]*(/[A-Za-z_][A-Za-z0-9_]*)*", topic):
        raise ProbeError("invalid_options", "topic must be an absolute ROS topic name")
    opts.setdefault("message_type", CLOUD)
    if opts["message_type"] not in (CLOUD, SCAN):
        raise ProbeError("invalid_options", "message_type must be PointCloud2 or LaserScan")
    for key, default, lower, upper in (
            ("duration_seconds", 10, 1, 60), ("count", 1, 1, 5),
            ("max_points", 200000, 100, 1000000), ("sample_points", 32, 0, 128)):
        item = opts.setdefault(key, default)
        if isinstance(item, bool) or not isinstance(item, (int, float)) or not math.isfinite(item) or int(item) != item or not lower <= item <= upper:
            raise ProbeError("invalid_options", "%s must be an integer in %d..%d" % (key, lower, upper))
        opts[key] = int(item)
    for key, default, lower, upper in (
            ("min_z", -1, -100, 100), ("max_z", 2, -100, 100),
            ("min_range", 0.05, 0, 1000), ("max_range", 20, 0, 1000)):
        item = opts.setdefault(key, default)
        if isinstance(item, bool) or not isinstance(item, (int, float)) or not math.isfinite(item) or not lower <= item <= upper:
            raise ProbeError("invalid_options", "%s must be finite and in %s..%s" % (key, lower, upper))
    if opts["min_z"] >= opts["max_z"] or opts["min_range"] >= opts["max_range"]:
        raise ProbeError("invalid_options", "Filter minimum must be less than maximum")
    frame = opts.setdefault("target_frame", "")
    if not isinstance(frame, str) or len(frame) > 255 or (frame and not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_/-]*", frame)):
        raise ProbeError("invalid_options", "target_frame must be a frame identifier of at most 255 characters")
    if not isinstance(opts.setdefault("use_sim_time", False), bool):
        raise ProbeError("invalid_options", "use_sim_time must be a boolean")
    return opts


def cloud_points(msg, max_points):
    """Validate first, then visit every point, including padded organized rows."""
    width, height, step, row_step = msg.width, msg.height, msg.point_step, msg.row_step
    if any(isinstance(v, bool) or not isinstance(v, int) or v < 0 for v in (width, height, step, row_step)):
        raise ProbeError("invalid_cloud", "Invalid PointCloud2 dimensions or strides")
    total = width * height
    if total > max_points or len(msg.data) > MAX_DATA_BYTES:
        raise ProbeError("point_limit", "PointCloud2 exceeds the configured point limit or 64 MiB data limit; publish a smaller cloud")
    if step == 0 or row_step < width * step or len(msg.data) != row_step * height:
        raise ProbeError("invalid_cloud", "PointCloud2 data length, point_step, or row_step is inconsistent")
    fields = {}
    for field in msg.fields:
        if field.name in ("x", "y", "z"):
            if field.name in fields:
                raise ProbeError("invalid_cloud", "PointCloud2 contains duplicate coordinate fields")
            fields[field.name] = field
    readers = []
    for name in ("x", "y", "z"):
        field = fields.get(name)
        if field is None or field.count != 1 or field.datatype not in (7, 8):
            raise ProbeError("invalid_cloud", "PointCloud2 needs scalar FLOAT32 or FLOAT64 x, y, and z fields")
        reader = struct.Struct((">" if msg.is_bigendian else "<") + ("f" if field.datatype == 7 else "d"))
        if field.offset < 0 or field.offset + reader.size > step:
            raise ProbeError("invalid_cloud", "PointCloud2 coordinate field exceeds point_step")
        readers.append((reader, field.offset))
    data = memoryview(msg.data)

    def points():
        for row in range(height):
            for column in range(width):
                offset = row * row_step + column * step
                yield tuple(reader.unpack_from(data, offset + field_offset)[0] for reader, field_offset in readers)
    return total, points()


def scan_points(msg, max_points):
    total = len(msg.ranges)
    if total > max_points:
        raise ProbeError("point_limit", "LaserScan exceeds the configured point limit")
    values = (msg.angle_min, msg.angle_max, msg.angle_increment, msg.range_min, msg.range_max, msg.time_increment)
    if not all(math.isfinite(v) for v in values) or msg.range_min < 0 or msg.range_max <= msg.range_min or msg.time_increment < 0:
        raise ProbeError("invalid_scan", "LaserScan angle, timing, or range metadata is invalid")
    if total > 1 and msg.angle_increment == 0:
        raise ProbeError("invalid_scan", "LaserScan angle_increment must be nonzero")
    if total:
        last_angle = msg.angle_min + (total - 1) * msg.angle_increment
        if not math.isfinite(last_angle) or abs(last_angle - msg.angle_max) > max(1e-4, abs(msg.angle_increment) * 0.1):
            raise ProbeError("invalid_scan", "LaserScan angle_max does not match its ranges and angle_increment")

    def points():
        for index, distance in enumerate(msg.ranges):
            if not math.isfinite(distance):
                yield (math.nan, math.nan, math.nan)
            elif not msg.range_min <= distance <= msg.range_max:
                yield None  # A finite reading outside the sensor's own range.
            else:
                angle = msg.angle_min + index * msg.angle_increment
                yield (distance * math.cos(angle), distance * math.sin(angle), 0.0)
    return total, points()


def transform_function(transform):
    """Convert a TF rigid transform to a small, allocation-free point function."""
    t, q = transform.translation, transform.rotation
    values = (t.x, t.y, t.z, q.x, q.y, q.z, q.w)
    if not all(math.isfinite(value) for value in values):
        raise ProbeError("invalid_transform", "TF contains nonfinite coordinates")
    norm = math.sqrt(q.x*q.x + q.y*q.y + q.z*q.z + q.w*q.w)
    if not math.isfinite(norm) or abs(norm - 1) > 0.01:
        raise ProbeError("invalid_transform", "TF quaternion is not normalized")
    x, y, z, w = (q.x/norm, q.y/norm, q.z/norm, q.w/norm)
    matrix = (1-2*(y*y+z*z), 2*(x*y-z*w), 2*(x*z+y*w),
              2*(x*y+z*w), 1-2*(x*x+z*z), 2*(y*z-x*w),
              2*(x*z-y*w), 2*(y*z+x*w), 1-2*(x*x+y*y))

    def apply(point):
        px, py, pz = point
        return (matrix[0]*px + matrix[1]*py + matrix[2]*pz + t.x,
                matrix[3]*px + matrix[4]*py + matrix[5]*pz + t.y,
                matrix[6]*px + matrix[7]*py + matrix[8]*pz + t.z)
    return apply


def summarize(msg, opts, observed_ns, clock_ns, transform=None, deadline=None):
    if opts["message_type"] == CLOUD:
        total, points = cloud_points(msg, opts["max_points"])
    else:
        total, points = scan_points(msg, opts["max_points"])
    stamp = msg.header.stamp
    if not 0 <= stamp.nanosec < 1000000000:
        raise ProbeError("invalid_stamp", "Source timestamp nanoseconds are out of range")
    source_frame = msg.header.frame_id
    if not isinstance(source_frame, str) or len(source_frame.encode("utf-8")) > 255:
        raise ProbeError("invalid_frame", "Source frame is invalid or exceeds 255 bytes")
    target_frame = opts["target_frame"] or source_frame
    if target_frame != source_frame and transform is None:
        raise ProbeError("transform_unavailable", "Requested target frame requires a transform at the source timestamp")
    apply = transform_function(transform) if transform is not None else None
    sectors = [{"axis": axis, "center_azimuth_degrees": index * 45,
                "point_count": 0, "min_distance_m": None, "nearest_xyz_m": None}
               for index, axis in enumerate(AXES)]
    finite = included = 0
    rejected = {"nonfinite": 0, "height": 0, "range": 0, "sensor_range": 0}
    bounds_min, bounds_max = [math.inf] * 3, [-math.inf] * 3
    samples, rng = [], random.Random(0)
    for index, point in enumerate(points):
        if index % 4096 == 0 and deadline is not None and time.monotonic() >= deadline:
            raise ProbeError("time_limit", "Deadline reached while decoding; no partial geometry was returned")
        if point is None:
            finite += 1
            rejected["sensor_range"] += 1
            continue
        if not all(math.isfinite(value) for value in point):
            rejected["nonfinite"] += 1
            continue
        finite += 1
        if apply:
            point = apply(point)
            if not all(math.isfinite(value) for value in point):
                raise ProbeError("invalid_transform", "TF produced nonfinite coordinates")
        x, y, z = point
        if not opts["min_z"] <= z <= opts["max_z"]:
            rejected["height"] += 1
            continue
        distance = math.hypot(x, y)
        if not opts["min_range"] <= distance <= opts["max_range"]:
            rejected["range"] += 1
            continue
        included += 1
        sector = sectors[int(math.floor((math.atan2(y, x) + math.pi/8) / (math.pi/4))) % 8]
        sector["point_count"] += 1
        if sector["min_distance_m"] is None or distance < sector["min_distance_m"]:
            sector["min_distance_m"], sector["nearest_xyz_m"] = distance, list(point)
        for axis, value in enumerate(point):
            bounds_min[axis], bounds_max[axis] = min(bounds_min[axis], value), max(bounds_max[axis], value)
        # Deterministic reservoir limits output only; all points affect minima.
        if len(samples) < opts["sample_points"]:
            samples.append(list(point))
        elif opts["sample_points"]:
            selected = rng.randrange(included)
            if selected < opts["sample_points"]:
                samples[selected] = list(point)
    source_ns = stamp.sec * 1000000000 + stamp.nanosec
    result = {
        "schema_version": 1, "topic": opts["topic"], "message_type": opts["message_type"],
        "status": "observed", "source_frame": source_frame, "frame_id": target_frame,
        "source_stamp": {"sec": stamp.sec, "nanosec": stamp.nanosec},
        "observed_at": datetime.datetime.fromtimestamp(observed_ns / 1e9, datetime.timezone.utc).isoformat().replace("+00:00", "Z"),
        "source_freshness": "unknown",
        "source_age_seconds": (clock_ns - source_ns) / 1e9 if source_ns != 0 and clock_ns not in (None, 0) else None,
        "clock_basis": "ros_sim_time" if opts["use_sim_time"] else "device_wall",
        "clock_synchronization": "unverified", "transform_applied": apply is not None,
        "distance_metric": "horizontal_xy", "direction_reference": "frame_axes",
        "sector_width_degrees": 45, "azimuth_reference": "+x=0,+y=90; counterclockwise about +z",
        "filters": {key: opts[key] for key in ("min_z", "max_z", "min_range", "max_range")},
        "total_points": total, "finite_points": finite, "included_points": included,
        "rejected_points": rejected, "sectors": sectors, "sample_points": samples,
        "bounds": {"min_xyz_m": bounds_min, "max_xyz_m": bounds_max} if included else None,
        "coverage": "observed_returns_only", "motion_compensated": False,
        "subscription_qos": {"reliability": "best_effort", "durability": "volatile", "history": "keep_last", "depth": 1},
    }
    if opts["message_type"] == SCAN:
        result["scan_timing"] = {"stamp_reference": "first_ray", "time_increment_seconds": msg.time_increment}
    return result


def error_result(opts, code, message):
    return {"schema_version": 1, "topic": opts.get("topic", ""),
            "message_type": opts.get("message_type", CLOUD), "status": "unknown",
            "source_freshness": "unknown", "error": {"code": code, "message": str(message)[:1000]}}


def emit(result):
    line = json.dumps(result, separators=(",", ":"), allow_nan=False)
    if len(line.encode("utf-8")) >= MAX_RESULT_BYTES:
        raise ProbeError("result_limit", "LiDAR summary exceeds the 64 KiB output limit")
    print(line, flush=True)


def run(opts):
    import rclpy
    from rclpy.parameter import Parameter
    from rclpy.qos import QoSProfile, ReliabilityPolicy, DurabilityPolicy, HistoryPolicy
    from rclpy.time import Time
    from sensor_msgs.msg import PointCloud2, LaserScan

    deadline = time.monotonic() + opts["duration_seconds"]
    rclpy.init(args=[])
    node = None
    try:
        node = rclpy.create_node("wendy_lidar_inspector", enable_rosout=False,
                                 start_parameter_services=False, use_global_arguments=False,
                                 parameter_overrides=[Parameter("use_sim_time", value=opts["use_sim_time"])])
        buffer = listener = None
        if opts["target_frame"]:
            from tf2_ros import Buffer, TransformListener
            # Omitting node avoids exposing Buffer's optional frame-graph service.
            buffer = Buffer()
            listener = TransformListener(buffer, node, spin_thread=False)
        pending = []

        def receive(msg):
            observed_ns = time.time_ns()
            clock_ns = node.get_clock().now().nanoseconds if opts["use_sim_time"] else observed_ns
            # Keep only one latest message even while spinning to receive TF.
            pending[:] = [(msg, observed_ns, clock_ns)]

        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT,
                         durability=DurabilityPolicy.VOLATILE, history=HistoryPolicy.KEEP_LAST)
        subscription = node.create_subscription(PointCloud2 if opts["message_type"] == CLOUD else LaserScan,
                                                opts["topic"], receive, qos)
        emitted = 0
        while time.monotonic() < deadline and emitted < opts["count"]:
            if not pending:
                rclpy.spin_once(node, timeout_sec=min(0.1, max(0.0, deadline-time.monotonic())))
                continue
            msg, observed_ns, clock_ns = pending.pop()
            transform = None
            if opts["target_frame"] and opts["target_frame"] != msg.header.frame_id:
                if not msg.header.frame_id or (msg.header.stamp.sec == 0 and msg.header.stamp.nanosec == 0):
                    raise ProbeError("transform_unavailable", "TF needs a source frame and nonzero source timestamp; latest TF is not substituted")
                stamp = Time.from_msg(msg.header.stamp)
                message_wait_started = time.monotonic()
                while time.monotonic() < deadline:
                    if buffer.can_transform(opts["target_frame"], msg.header.frame_id, stamp):
                        transform = buffer.lookup_transform(opts["target_frame"], msg.header.frame_id, stamp).transform
                        break
                    rclpy.spin_once(node, timeout_sec=min(0.05, max(0.0, deadline-time.monotonic())))
                    # The very first cloud may predate the listener's TF cache.
                    # Give interpolation a chance, then try a newer received
                    # cloud instead of waiting forever for discarded history.
                    if pending and time.monotonic() - message_wait_started >= 0.25:
                        msg, observed_ns, clock_ns = pending.pop()
                        if not msg.header.frame_id or (msg.header.stamp.sec == 0 and msg.header.stamp.nanosec == 0):
                            raise ProbeError("transform_unavailable", "TF needs a source frame and nonzero source timestamp; latest TF is not substituted")
                        stamp = Time.from_msg(msg.header.stamp)
                        message_wait_started = time.monotonic()
                if transform is None:
                    raise ProbeError("transform_unavailable", "No TF from %s to %s at the source timestamp within the observation deadline" % (msg.header.frame_id, opts["target_frame"]))
            result = summarize(msg, opts, observed_ns, clock_ns, transform, deadline)
            if time.monotonic() >= deadline:
                raise ProbeError("time_limit", "Observation deadline reached; incomplete geometry was omitted")
            emit(result)
            emitted += 1
        if emitted == 0:
            emit(error_result(opts, "no_messages", "No sensor messages arrived before the deadline with best-effort, volatile QoS; readings remain unknown"))
        elif emitted < opts["count"]:
            emit(error_result(opts, "time_limit", "Observation deadline reached before the requested sample count"))
    finally:
        if node is not None:
            node.destroy_node()
        rclpy.shutdown()


def main():
    opts = {}
    try:
        if len(sys.argv) != 2:
            raise ProbeError("invalid_options", "Expected one JSON options argument")
        opts = options_from_json(json.loads(sys.argv[1]))
        run(opts)
    except ProbeError as error:
        emit(error_result(opts, error.code, error))
    except (KeyboardInterrupt, SystemExit):
        pass
    except Exception as error:
        emit(error_result(opts, "probe_error", error))


if __name__ == "__main__":
    main()
