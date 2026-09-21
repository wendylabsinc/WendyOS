"""Subscribe to actual simulator ROS messages and serve a local observation UI."""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import math
from pathlib import Path
import threading
from urllib.request import urlopen

import rclpy
from rclpy.node import Node
from rclpy.qos import QoSProfile, DurabilityPolicy, ReliabilityPolicy, qos_profile_sensor_data
from sensor_msgs.msg import CameraInfo, Image, Imu, JointState, LaserScan
from nav_msgs.msg import Odometry
from std_msgs.msg import UInt64

from .state import Observations, TOPICS


def vector(value):
    return [value.x, value.y, value.z]


def quaternion(value):
    return [value.x, value.y, value.z, value.w]


class Receiver(Node):
    def __init__(self, observations):
        super().__init__("g1_training_observer")
        self.observations = observations
        for key, msg_type in [("joints", JointState), ("imu", Imu), ("odom", Odometry),
                              ("camera", Image), ("camera_info", CameraInfo), ("scan", LaserScan),
                              ("epoch", UInt64)]:
            qos = (QoSProfile(depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL,
                              reliability=ReliabilityPolicy.RELIABLE)
                   if key == "epoch" else qos_profile_sensor_data)
            self.create_subscription(msg_type, TOPICS[key],
                                     lambda msg, key=key: self.receive(key, msg), qos)

    def receive(self, key, msg):
        try:
            stamp = None
            frame = None
            if hasattr(msg, "header"):
                stamp = msg.header.stamp.sec + msg.header.stamp.nanosec / 1e9
            if key == "joints":
                if not msg.name or len(msg.position) != len(msg.name):
                    raise ValueError("Joint names and positions must have matching lengths")
                value = {"names": list(msg.name), "positions": list(msg.position),
                         "velocities": list(msg.velocity)}
            elif key == "imu":
                value = {"orientation_xyzw": quaternion(msg.orientation),
                         "angular_velocity": vector(msg.angular_velocity),
                         "linear_acceleration": vector(msg.linear_acceleration)}
            elif key == "odom":
                value = {"position": vector(msg.pose.pose.position),
                         "orientation_xyzw": quaternion(msg.pose.pose.orientation),
                         "linear_velocity": vector(msg.twist.twist.linear)}
            elif key == "camera":
                if (msg.encoding not in ("rgb8", "bgr8")
                        or not (0 < msg.width <= 4096 and 0 < msg.height <= 4096)
                        or msg.step < msg.width * 3 or len(msg.data) != msg.step * msg.height):
                    raise ValueError("Invalid RGB image encoding, dimensions or payload")
                frame = (msg.width, msg.height, msg.encoding, msg.step, bytes(msg.data))
                value = {"width": msg.width, "height": msg.height, "encoding": msg.encoding,
                         "frame_id": msg.header.frame_id}
            elif key == "camera_info":
                value = {"width": msg.width, "height": msg.height, "k": list(msg.k),
                         "frame_id": msg.header.frame_id}
            elif key == "scan":
                valid = [r for r in msg.ranges if math.isfinite(r)]
                value = {"rays": len(msg.ranges), "finite_rays": len(valid),
                         "nearest_m": min(valid) if valid else None}
            else:
                value = int(msg.data)
            # Ensure invalid sensor numbers cannot break the API or count as ready.
            json.dumps(value, allow_nan=False)
            self.observations.record(key, value, stamp, frame)
        except (ValueError, TypeError, OverflowError) as error:
            self.observations.failure(key, error)


def poll_simulator(observations, stop):
    fields = ("simulation", "robot_kind", "vm_name", "healthy", "ready", "mode", "epoch",
              "source_digest", "policy_bundle", "world", "sensor_settings")
    while not stop.is_set():
        try:
            with urlopen("http://127.0.0.1:8890/api/status", timeout=1) as response:
                status = json.load(response)
            observations.set_simulator({key: status.get(key) for key in fields})
        except (OSError, ValueError) as error:
            observations.set_simulator({"error": str(error), "healthy": False})
        stop.wait(0.5)


def handler_for(observations):
    page = Path(__file__).with_name("index.html").read_bytes()

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            path = self.path.split("?", 1)[0]
            code = 200
            if path in ("/", "/index.html"):
                body, content_type = page, "text/html; charset=utf-8"
            elif path in ("/api/status", "/healthz"):
                status = observations.snapshot()
                code = 503 if path == "/healthz" and not status["ready"] else 200
                body, content_type = json.dumps(status, allow_nan=False).encode(), "application/json"
            elif path == "/camera.jpg":
                body, content_type = observations.jpeg(), "image/jpeg"
                if body is None:
                    code, body, content_type = 503, b"Waiting for fresh ROS camera data", "text/plain"
            else:
                code, body, content_type = 404, b"Not found", "text/plain"
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            try:
                self.wfile.write(body)
            except (BrokenPipeError, ConnectionResetError):
                # The peer already disconnected; cleanup can continue.
                pass

        def log_message(self, *args):
            pass

    return Handler


def main():
    rclpy.init()
    observations = Observations()
    node = Receiver(observations)
    stop = threading.Event()
    server = ThreadingHTTPServer(("0.0.0.0", 8891), handler_for(observations))
    threads = [threading.Thread(target=server.serve_forever, daemon=True),
               threading.Thread(target=poll_simulator, args=(observations, stop), daemon=True)]
    for thread in threads:
        thread.start()
    node.get_logger().info("G1 observation app listening on :8891; receiving ROS 2 domain 0")
    try:
        rclpy.spin(node)
    except KeyboardInterrupt:
        # The finally block stops the server on operator interruption.
        pass
    finally:
        stop.set()
        server.shutdown()
        server.server_close()
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
