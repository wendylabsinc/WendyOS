"""Read-only dashboard for native Go2 ROS sensors and optional standard extensions."""

import argparse
from collections import deque
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
from pathlib import Path
import struct
import threading
import time
from urllib.parse import urlsplit
import zlib

from go2_io import CLOUD_TOPIC, ODOM_TOPIC, cloud_scan, sensor_wall_ns


TOPICS = {
    "odom": (ODOM_TOPIC, "odom"),
    "imu": ("/utlidar/imu", "utlidar_imu"),
    "scan": (CLOUD_TOPIC, "base_link"),
    "joints": ("/joint_states", None),
    "camera": ("/camera/color/image_raw", "camera_optical_frame"),
}
STALE_AFTER = 0.6


def vector(value):
    result = [value.x, value.y, value.z]
    if not all(math.isfinite(v) for v in result):
        raise ValueError("Non-finite vector")
    return result


def heading(q):
    values = [q.x, q.y, q.z, q.w]
    if not all(math.isfinite(v) for v in values):
        raise ValueError("Non-finite orientation")
    norm = math.sqrt(sum(v * v for v in values))
    if abs(norm - 1) > 0.01:
        raise ValueError("Invalid orientation")
    x, y, z, w = [v / norm for v in values]
    return math.atan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z))


def scan_values(message):
    metadata = (message.angle_min, message.angle_increment, message.range_min, message.range_max)
    if (not all(math.isfinite(v) for v in metadata) or message.angle_increment <= 0
            or message.range_min < 0 or message.range_max <= message.range_min
            or not 1 <= len(message.ranges) <= 4096):
        raise ValueError("Invalid scan metadata")
    ranges = [float(v) if math.isfinite(v) and message.range_min <= v <= message.range_max
              else None for v in message.ranges]
    valid = [v for v in ranges if v is not None]
    # Keep the browser payload bounded for other compatible scan producers.
    step = max(1, math.ceil(len(ranges) / 720))
    return {"ranges": ranges[::step], "angle_min": message.angle_min,
            "angle_increment": message.angle_increment * step, "range_max": message.range_max,
            "coverage": len(valid) / len(ranges), "nearest": min(valid) if valid else None}


def image_values(message):
    width, height, step = message.width, message.height, message.step
    if (message.encoding != "rgb8" or not 1 <= width <= 1920 or not 1 <= height <= 1080
            or not width * 3 <= step <= width * 3 + 4096
            or len(message.data) != step * height):
        raise ValueError("Expected a bounded rgb8 image with valid row stride")
    return {"width": width, "height": height, "step": step}, bytes(message.data)


def png_image(metadata, pixels):
    """Encode ROS rgb8 rows directly, respecting padding, without image libraries."""
    width, height, step = (metadata[key] for key in ("width", "height", "step"))

    def chunk(kind, data):
        return struct.pack("!I", len(data)) + kind + data + struct.pack("!I", zlib.crc32(kind + data))

    rows = b"".join(b"\0" + pixels[y * step:y * step + width * 3] for y in range(height))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack("!2I5B", width, height, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress(rows, level=1)) + chunk(b"IEND", b""))


class SensorStore:
    def __init__(self, *, ignore_capture_age=False):
        self.ignore_capture_age = ignore_capture_age
        self.lock = threading.RLock()
        self.samples = {}
        self.errors = {}
        self.receipts = {key: deque(maxlen=2000) for key in TOPICS}
        self.trail = deque(maxlen=600)
        self.trail_total = 0
        self.last_trail_time = None
        self.camera = None
        self.camera_cache = None

    def observe(self, key, message, *, wall_ns=None, now=None):
        wall_ns = sensor_wall_ns(time.time_ns() if wall_ns is None else wall_ns)
        now = time.monotonic() if now is None else now
        try:
            stamp = message.header.stamp
            if (type(stamp.sec) is not int or type(stamp.nanosec) is not int
                    or not math.isfinite(wall_ns) or not math.isfinite(now)):
                raise ValueError("Invalid capture timestamp or application clock")
            source = stamp.sec * 1_000_000_000 + stamp.nanosec
            age = (wall_ns - source) / 1e9
            if (not 0 <= stamp.nanosec < 1_000_000_000 or source <= 0
                    or (not self.ignore_capture_age and not -0.05 <= age < STALE_AFTER)):
                raise ValueError("Old or invalid capture timestamp")
            expected_frame = TOPICS[key][1]
            if expected_frame and message.header.frame_id != expected_frame:
                raise ValueError("Unexpected sensor frame")
            if key == "odom":
                if message.child_frame_id != "base_link":
                    raise ValueError("Expected odom to base_link")
                data = {"position": vector(message.pose.pose.position),
                        "yaw": heading(message.pose.pose.orientation),
                        "velocity": vector(message.twist.twist.linear)}
            elif key == "imu":
                data = {"acceleration": vector(message.linear_acceleration),
                        "angular_velocity": vector(message.angular_velocity),
                        "yaw": heading(message.orientation)}
            elif key == "scan":
                data = scan_values(message)
            elif key == "joints":
                if (not 1 <= len(message.name) <= 64 or len(message.name) != len(message.position)
                        or not all(math.isfinite(v) for v in message.position)
                        or len(set(message.name)) != len(message.name)):
                    raise ValueError("Invalid joint positions")
                data = {"names": list(message.name), "positions": list(message.position)}
            else:
                data, pixels = image_values(message)
            with self.lock:
                previous = self.samples.get(key)
                if previous and source <= previous["stamp_ns"]:
                    raise ValueError("Repeated or reordered capture")
                self.samples[key] = {"stamp_ns": source, "captured": now if self.ignore_capture_age else now - max(0, age),
                                     "capture_age_seconds": age, "data": data}
                self.receipts[key].append(now)
                self.errors.pop(key, None)
                if key == "camera":
                    self.camera = (source, data, pixels)
                if key == "odom" and (self.last_trail_time is None or now - self.last_trail_time >= 0.2):
                    point = data["position"][:2]
                    if (self.trail and (math.dist(point, self.trail[-1]) > 1.0
                                       or now - self.last_trail_time > 2)):
                        self.trail.clear()
                    self.trail.append(point)
                    self.trail_total += 1
                    self.last_trail_time = now
            return True
        except (ValueError, TypeError, AttributeError, OverflowError) as error:
            with self.lock:
                self.errors[key] = str(error)
            return False

    def snapshot(self, now=None):
        now = time.monotonic() if now is None else now
        with self.lock:
            topics = {}
            for key, (topic, _) in TOPICS.items():
                sample = self.samples.get(key)
                age = now - sample["captured"] if sample else None
                receipts = self.receipts[key]
                while receipts and receipts[0] <= now - 2:
                    receipts.popleft()
                topics[key] = {"topic": topic, "state": "waiting" if age is None else
                               "live" if 0 <= age < STALE_AFTER else "stale",
                               "age_ms": max(0, age) * 1000 if age is not None else None,
                               "rate_hz": len(receipts) / 2, "error": self.errors.get(key),
                               "data": sample["data"] if sample else None,
                               "stamp_ns": str(sample["stamp_ns"]) if sample else None,
                               "capture_age_seconds": sample["capture_age_seconds"] if sample else None}
            return {"ignore_capture_age": self.ignore_capture_age, "topics": topics, "trail": list(self.trail), "trail_total": self.trail_total}

    def camera_png(self, now=None):
        now = time.monotonic() if now is None else now
        with self.lock:
            sample = self.samples.get("camera")
            if not sample or not 0 <= now - sample["captured"] < STALE_AFTER:
                return None
            source, metadata, pixels = self.camera
            if self.camera_cache and self.camera_cache[0] == source:
                return self.camera_cache[1]
        encoded = png_image(metadata, pixels)
        with self.lock:
            self.camera_cache = (source, encoded)
        return encoded


def make_server(store, host, port):
    page = Path(__file__).with_name("index.html").read_bytes()

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            path = urlsplit(self.path).path
            if path == "/":
                self.reply(200, "text/html; charset=utf-8", page)
            elif path == "/api/status":
                self.reply(200, "application/json", json.dumps(store.snapshot(), allow_nan=False).encode())
            elif path == "/camera.png":
                frame = store.camera_png()
                self.reply(200 if frame else 503, "image/png" if frame else "text/plain",
                           frame or b"Waiting for a fresh ROS camera exposure")
            else:
                self.reply(404, "text/plain", b"Not found")

        def reply(self, code, kind, body):
            self.send_response(code)
            self.send_header("Content-Type", kind)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                # The peer already disconnected; cleanup can continue.
                pass

        def log_message(self, *_):
            pass

    return ThreadingHTTPServer((host, port), Handler)


def make_node(store):
    from nav_msgs.msg import Odometry
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy
    from sensor_msgs.msg import Image, Imu, JointState, PointCloud2

    node = Node("wendy_go2_sensor_dashboard")
    qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
    def observe_cloud(message):
        try:
            store.observe("scan", cloud_scan(message))
        except (ValueError, TypeError, AttributeError) as error:
            with store.lock:
                store.errors["scan"] = str(error)
    for key, kind in (("odom", Odometry), ("imu", Imu), ("scan", PointCloud2),
                      ("joints", JointState), ("camera", Image)):
        node.create_subscription(kind, TOPICS[key][0], (observe_cloud if key == "scan" else lambda message, key=key: store.observe(key, message)), qos)
    return node


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8904)
    parser.add_argument("--ignore-capture-age", action="store_true",
                        help="use local arrival time for sensor timeouts with unsynchronized clocks")
    args, ros_args = parser.parse_known_args()
    import rclpy
    from rclpy.executors import ExternalShutdownException

    store = SensorStore(ignore_capture_age=args.ignore_capture_age)
    if args.ignore_capture_age:
        print("Capture age checks disabled. Sensor timeouts use local arrival time.", flush=True)
    server = make_server(store, args.host, args.port)
    rclpy.init(args=ros_args)
    node = make_node(store)
    worker = threading.Thread(target=server.serve_forever, daemon=True)
    worker.start()
    print(f"Go2 sensor dashboard listening on port {args.port}", flush=True)
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        # Expected shutdown signals; the finally block closes the server and node.
        pass
    except Exception:
        if rclpy.ok():
            raise
    finally:
        server.shutdown()
        server.server_close()
        worker.join()
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
