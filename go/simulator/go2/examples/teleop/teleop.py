"""Browser hold-to-drive controls that publish velocity requests over ROS 2."""

import argparse
import json
import math
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import secrets
import threading
import time


DEADMAN_SECONDS = 0.25
PUBLISH_SECONDS = 0.05
KEYS = frozenset("wsadqe")
ZERO = (0.0, 0.0, 0.0)


class ControlError(ValueError):
    def __init__(self, message, status=400):
        super().__init__(message)
        self.status = status


def fields(payload, expected):
    if not isinstance(payload, dict) or payload.keys() != expected:
        raise ControlError("Unexpected or missing request fields")


def identity(payload):
    session, sequence = payload["session"], payload["sequence"]
    if not isinstance(session, str) or not 1 <= len(session) <= 128:
        raise ControlError("Invalid control session")
    if type(sequence) is not int or not 0 <= sequence <= 2**53 - 1:
        raise ControlError("Sequence must be a nonnegative safe integer")
    return session, sequence


def speed(value, maximum):
    if type(value) not in (int, float) or (type(value) is float and not math.isfinite(value)):
        raise ControlError("Speeds must be finite numbers")
    return max(0.0, min(maximum, value))


def velocity(payload):
    fields(payload, {"session", "sequence", "keys", "speed", "turn_speed"})
    identity(payload)
    pressed = payload["keys"]
    if (not isinstance(pressed, list) or len(pressed) > len(KEYS)
            or any(not isinstance(key, str) or key not in KEYS for key in pressed)
            or len(set(pressed)) != len(pressed)):
        raise ControlError("Keys must be unique members of w, s, a, d, q, e")
    linear, angular = speed(payload["speed"], 0.6), speed(payload["turn_speed"], 1.0)
    x = linear * (("w" in pressed) - ("s" in pressed))
    y = min(linear, 0.4) * (("a" in pressed) - ("d" in pressed))
    length = math.hypot(x, y)
    if length > linear:
        x, y = x * linear / length, y * linear / length
    return x, y, angular * (("q" in pressed) - ("e" in pressed))


class TeleopControl:
    """One explicit browser session; expiry is terminal until enabled again.

    Publishing shares the state lock so a timer cannot send an old nonzero
    command after a concurrent release has published its stop command.
    """

    def __init__(self, publish, clock=time.monotonic):
        self.publish, self.clock = publish, clock
        self.lock = threading.Lock()
        self.session = None
        self.sequence = -1
        self.deadline = 0.0
        self.command = ZERO
        self.closed = False
        self.reason = "Controls disabled"
        self.publish(*ZERO)

    def _stop(self, reason):
        self.session = None
        self.sequence = -1
        self.command = ZERO
        self.reason = reason
        self.publish(*ZERO)

    def _expire(self, now):
        if self.session is not None and now >= self.deadline:
            self._stop("Heartbeat expired; enable controls again")

    def _authorize(self, session, sequence, now):
        self._expire(now)
        if self.session is None or session != self.session:
            raise ControlError("Control session expired or belongs to another tab", 409)
        if sequence <= self.sequence:
            raise ControlError("Out-of-order control request", 409)

    def enable(self, payload):
        fields(payload, set())
        with self.lock:
            now = self.clock()
            self._expire(now)
            if self.closed:
                raise ControlError("Application is shutting down", 409)
            if self.session is not None:
                raise ControlError("Another tab has controls; disable them there first", 409)
            self.session = secrets.token_urlsafe(24)
            self.sequence = -1
            self.deadline = now + DEADMAN_SECONDS
            self.command = ZERO
            self.reason = "Enabled; hold a direction to drive"
            self.publish(*ZERO)
            return {"session": self.session, "deadman_seconds": DEADMAN_SECONDS}

    def drive(self, payload):
        command = velocity(payload)
        session, sequence = identity(payload)
        with self.lock:
            now = self.clock()
            self._authorize(session, sequence, now)
            self.sequence = sequence
            self.command = command
            self.deadline = now + DEADMAN_SECONDS
            self.reason = "Requesting motion" if command != ZERO else "Enabled; holding position"
            if command == ZERO:
                self.publish(*ZERO)
            return {"velocity": list(command)}

    def release(self, payload):
        fields(payload, {"session", "sequence"})
        session, sequence = identity(payload)
        with self.lock:
            self._authorize(session, sequence, self.clock())
            self._stop("Controls disabled")
        return {"released": True}

    def stop(self, reason="Application shutdown"):
        with self.lock:
            self.closed = True
            self._stop(reason)

    def tick(self):
        with self.lock:
            self._expire(self.clock())
            self.publish(*self.command)

    def status(self):
        with self.lock:
            now = self.clock()
            self._expire(now)
            return {"enabled": self.session is not None, "reason": self.reason,
                    "velocity": list(self.command), "deadman_seconds": DEADMAN_SECONDS,
                    "heartbeat_remaining": max(0.0, self.deadline - now) if self.session else 0.0}


def parse_json(body):
    def object_pairs(pairs):
        obj = {}
        for key, value in pairs:
            if key in obj:
                raise ValueError("Duplicate JSON field")
            obj[key] = value
        return obj

    def invalid_constant(value):
        raise ValueError("Nonfinite JSON number")

    try:
        return json.loads(body, object_pairs_hook=object_pairs, parse_constant=invalid_constant)
    except (ValueError, UnicodeDecodeError, RecursionError) as error:
        raise ControlError("Expected valid JSON with unique fields and finite numbers") from error


def handler_for(control):
    page = Path(__file__).with_name("index.html").read_bytes()

    class Handler(BaseHTTPRequestHandler):
        def setup(self):
            super().setup()
            self.connection.settimeout(2)

        def send(self, status, body, kind="application/json"):
            self.send_response(status)
            self.send_header("Content-Type", kind)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "same-origin")
            self.send_header("Content-Security-Policy",
                             "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'")
            self.end_headers()
            self.wfile.write(body)

        def json(self, status, value):
            self.send(status, json.dumps(value, allow_nan=False).encode())

        def do_GET(self):
            if self.path == "/":
                self.send(200, page, "text/html; charset=utf-8")
            elif self.path == "/api/status":
                self.json(200, control.status())
            else:
                self.json(404, {"error": "Not found"})

        def do_POST(self):
            routes = {"/api/enable": control.enable, "/api/drive": control.drive,
                      "/api/release": control.release}
            try:
                if self.path not in routes:
                    raise ControlError("Not found", 404)
                origin = self.headers.get("Origin")
                if origin is not None and origin != "http://" + self.headers.get("Host", ""):
                    raise ControlError("Cross-origin control requests are disabled", 403)
                if self.headers.get_content_type() != "application/json":
                    raise ControlError("Content-Type must be application/json", 415)
                length = self.headers.get("Content-Length", "")
                if (not length.isascii() or not length.isdigit() or len(length) > 4
                        or not 0 < int(length) <= 4096):
                    raise ControlError("Expected a JSON body of at most 4096 bytes", 413)
                payload = parse_json(self.rfile.read(int(length)))
                self.json(200, routes[self.path](payload))
            except ControlError as error:
                self.json(error.status, {"error": str(error)})

        def log_message(self, format, *args):
            pass

    return Handler


def make_node():
    from geometry_msgs.msg import Twist
    from rclpy.node import Node
    from rclpy.qos import QoSProfile

    class TeleopNode(Node):
        def __init__(self):
            super().__init__("wendy_go2_teleop")
            self.drive = self.create_publisher(Twist, "/cmd_vel", QoSProfile(depth=1))
            self.control = TeleopControl(self.publish_command)
            self.create_timer(PUBLISH_SECONDS, self.control.tick)

        def publish_command(self, x, y, yaw):
            message = Twist()
            message.linear.x, message.linear.y, message.angular.z = x, y, yaw
            self.drive.publish(message)

    return TeleopNode()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--host", default="0.0.0.0", help="HTTP bind address (default: all guest interfaces)")
    parser.add_argument("--port", default=8903, type=int)
    args, ros_args = parser.parse_known_args()
    if not 1 <= args.port <= 65535:
        parser.error("--port must be between 1 and 65535")

    import rclpy
    from rclpy.executors import ExternalShutdownException

    rclpy.init(args=ros_args)
    node = make_node()
    server = None
    serving = None
    try:
        server = ThreadingHTTPServer((args.host, args.port), handler_for(node.control))
        server.daemon_threads = True
        serving = threading.Thread(target=server.serve_forever, daemon=True)
        serving.start()
        node.get_logger().info(
            f"Teleop HTTP listening on {args.host}:{args.port}. "
            "Managed Go2 gives new apps control automatically. "
            "In manual mode, grant Teleop control in the sandbox. "
            "Enable the control panel and hold a direction to drive.")
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        pass
    finally:
        if rclpy.ok():
            node.control.stop()
        if server is not None:
            server.shutdown()
            server.server_close()
        if serving is not None:
            serving.join()
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
