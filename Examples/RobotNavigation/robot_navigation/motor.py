"""Stamped command delivery and a watchdog independent of ROS simulation time.

Importing this module cannot import ROS/Unitree or open hardware. The ROS entry
point constructs a backend only after both local commissioning flags are true.
A stop command is an attempt, never evidence that the robot physically stopped.
"""
from dataclasses import dataclass
import importlib
import json
import math
import re
import threading
import time


@dataclass(frozen=True)
class MotorConfig:
    mode: str = "disabled"
    motion_enabled: bool = False
    watchdog_commissioned: bool = False
    command_timeout: float = 0.2
    max_linear: float = 0.25
    max_angular: float = 0.4
    base_frame: str = "base_link"
    output_topic: str = "/cmd_vel"
    network_interface: str = ""

    def __post_init__(self):
        if self.mode not in {"disabled", "twist", "unitree"}:
            raise ValueError("motor mode must be disabled, twist or unitree")
        if type(self.motion_enabled) is not bool or type(self.watchdog_commissioned) is not bool:
            raise ValueError("motor commissioning flags must be booleans")
        for name in ("command_timeout", "max_linear", "max_angular"):
            if not _finite(getattr(self, name)) or getattr(self, name) <= 0:
                raise ValueError(f"{name} must be positive and finite")
        if self.command_timeout > 1:
            raise ValueError("motor command_timeout must be at most one second")
        if not isinstance(self.base_frame, str) or not self.base_frame:
            raise ValueError("base_frame is required")
        if not isinstance(self.output_topic, str) or not re.fullmatch(r"/[A-Za-z_][A-Za-z0-9_/]*", self.output_topic):
            raise ValueError("motor output_topic must be an absolute ROS name")
        if self.output_topic == "/robot_navigation/safe_cmd_vel":
            raise ValueError("motor output cannot feed its own guarded input")
        if self.mode == "unitree" and (not isinstance(self.network_interface, str) or not re.fullmatch(r"[A-Za-z0-9_.:-]{1,15}", self.network_interface)):
            raise ValueError("unitree requires an explicit valid network_interface")

    @property
    def enabled(self):
        return self.mode != "disabled" and self.motion_enabled and self.watchdog_commissioned

    @classmethod
    def from_settings(cls, settings):
        motor, runtime = settings["motor"], settings["runtime"]
        return cls(mode=motor["mode"], command_timeout=motor["command_timeout"],
                   output_topic=motor["output_topic"], network_interface=motor["network_interface"],
                   motion_enabled=runtime["motion_enabled"], watchdog_commissioned=runtime["watchdog_commissioned"],
                   max_linear=runtime["max_linear_speed"], max_angular=runtime["max_angular_speed"],
                   base_frame=runtime["base_frame"])


def _finite(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


class MotorGate:
    """Thread-safe core with injected backend and monotonic clock.

    Backend move/stop must return promptly; Unitree's adapter sets its RPC timeout.
    A separate commissioned controller watchdog must still cover this entire
    process dying or its native middleware blocking. Faults require a local restart.
    """
    def __init__(self, config=MotorConfig(), backend=None, *, monotonic=time.monotonic,
                 unavailable_reason="backend_unavailable"):
        self.config, self.backend, self._clock = config, backend, monotonic
        self._lock = threading.RLock()
        self._last_received = self._last_stamp = None
        self._last_monotonic = None
        self._stopped = False
        self._closed = False
        self._fault = None
        self._reason = "awaiting_command"
        if not config.enabled:
            self._reason = "motor_disabled" if config.mode == "disabled" else "motor_not_commissioned"
        elif backend is None:
            self._reason = unavailable_reason[:200]

    def _now(self):
        now = self._clock()
        if not _finite(now) or (self._last_monotonic is not None and now < self._last_monotonic):
            self._fault = self._reason = "monotonic_clock_invalid"
            return None
        self._last_monotonic = now
        return now

    def _can_output(self):
        return self.config.enabled and self.backend is not None

    def _stop_output(self):
        if not self._can_output() or self._stopped:
            return
        try:
            self.backend.stop()
            self._stopped = True
        except Exception as exc:
            self._fault = "backend_stop_failed"
            self._reason = f"backend_stop_failed: {exc}"[:200]

    def reject(self, reason):
        with self._lock:
            if not self._can_output() or self._closed:
                return False
            self._last_received = None
            self._reason = reason
            self._stop_output()
            return False

    def receive(self, linear, angular, stamp_seconds, ros_now_seconds, frame_id):
        with self._lock:
            if not self._can_output() or self._closed or self._fault:
                return False
            now = self._now()
            if now is None:
                self._stop_output()
                return False
            if frame_id != self.config.base_frame:
                return self.reject("command_frame_invalid")
            if not all(_finite(v) for v in (linear, angular, stamp_seconds, ros_now_seconds)):
                return self.reject("command_nonfinite")
            if abs(linear) > self.config.max_linear or abs(angular) > self.config.max_angular:
                return self.reject("command_exceeds_limits")
            age = ros_now_seconds - stamp_seconds
            if stamp_seconds <= 0 or not 0 <= age <= self.config.command_timeout:
                return self.reject("command_stamp_stale_or_future")
            if self._last_stamp is not None and stamp_seconds <= self._last_stamp:
                return self.reject("command_stamp_replayed")
            self._last_stamp, self._last_received = stamp_seconds, now
            if linear == 0 and angular == 0:
                self._reason = "guarded_stop"
                self._stop_output()
                return self._fault is None
            self._stopped = False  # Even a failed move may have reached the device.
            try:
                self.backend.move(float(linear), float(angular))
            except Exception as exc:
                self._fault = "backend_move_failed"
                self._reason = f"backend_move_failed: {exc}"[:200]
                self._last_received = None
                self._stop_output()
                return False
            after = self._now()
            if after is None or after - now > self.config.command_timeout:
                self._fault = self._reason = "backend_command_expired"
                self._last_received = None
                self._stop_output()
                return False
            self._reason = "forwarding_guarded_command"
            return True

    def tick(self):
        with self._lock:
            if not self._can_output() or self._closed:
                return
            now = self._now()
            if self._fault or now is None:
                self._stop_output()
            elif self._last_received is None or now - self._last_received >= self.config.command_timeout:
                self._last_received = None
                self._reason = "command_watchdog_expired"
                self._stop_output()

    def stop(self, reason="shutdown"):
        with self._lock:
            self._closed = True
            self._last_received = None
            if self._can_output():
                self._reason = reason
                self._stop_output()

    def status(self):
        with self._lock:
            available = self._can_output() and self._fault is None and not self._closed
            return {"available": available, "ready": available, "mode": self.config.mode,
                    "reason": self._reason, "stop_command_sent": self._stopped,
                    "stopped_confirmed": False, "command_timeout": self.config.command_timeout}


class UnitreeBackend:
    """Optional SDK transport. Construction is only allowed by the armed factory."""
    def __init__(self, network_interface, timeout, *, import_module=importlib.import_module):
        channel = import_module("unitree_sdk2py.core.channel")
        sport = import_module("unitree_sdk2py.go2.sport.sport_client")
        # Go2's sport control domain is 0. Interface comes from the local config.
        channel.ChannelFactoryInitialize(0, network_interface)
        self.client = sport.SportClient(enableLease=False)
        self.client.SetTimeout(timeout)
        self.client.Init()

    @staticmethod
    def _check(code):
        if type(code) is not int or code != 0:
            raise RuntimeError(f"Unitree SDK returned {code!r}")

    def move(self, linear, angular):
        self._check(self.client.Move(linear, 0.0, angular))

    def stop(self):
        self._check(self.client.StopMove())


def build_backend(config, *, twist_factory=None, unitree_factory=None):
    """Gates factory invocation itself: disabled mode cannot initialize hardware."""
    if not config.enabled:
        return None, "motor_not_commissioned"
    try:
        if config.mode == "twist":
            if twist_factory is None:
                return None, "twist_backend_unavailable"
            return twist_factory(), ""
        factory = unitree_factory or UnitreeBackend
        return factory(config.network_interface, min(0.1, config.command_timeout / 2)), ""
    except Exception as exc:
        return None, f"{config.mode}_backend_unavailable: {exc}"[:200]


def main():
    # Lazy imports keep simulation/unit tests independent of ROS and robot SDKs.
    import rclpy
    from geometry_msgs.msg import Twist, TwistStamped
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy, DurabilityPolicy
    from std_msgs.msg import String
    from .settings import load_settings

    config = MotorConfig.from_settings(load_settings())
    rclpy.init()

    class MotorNode(Node):
        def __init__(self):
            super().__init__("robot_navigation_motor")
            self.status_pub = self.create_publisher(String, "/robot_navigation/motor_status", 1)

            def twist_factory():
                publisher = self.create_publisher(Twist, config.output_topic, 1)
                class TwistBackend:
                    def move(self, linear, angular):
                        msg = Twist()
                        msg.linear.x, msg.angular.z = linear, angular
                        publisher.publish(msg)
                    def stop(self):
                        publisher.publish(Twist())
                return TwistBackend()

            backend, reason = build_backend(config, twist_factory=twist_factory)
            self.gate = MotorGate(config, backend, unavailable_reason=reason)
            qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT,
                             durability=DurabilityPolicy.VOLATILE)
            self.create_subscription(TwistStamped, "/robot_navigation/safe_cmd_vel", self.command, qos)
            self.create_timer(0.1, self.publish_status)
            self.done = threading.Event()
            self.watchdog = threading.Thread(target=self.watch, name="motor-watchdog", daemon=True)
            self.watchdog.start()

        def command(self, msg):
            # Lateral, vertical and roll/pitch commands are outside this adapter.
            extra = (msg.twist.linear.y, msg.twist.linear.z, msg.twist.angular.x, msg.twist.angular.y)
            if any(not _finite(v) or v != 0 for v in extra):
                self.gate.reject("unsupported_velocity_axis")
                return
            stamp = msg.header.stamp.sec + msg.header.stamp.nanosec / 1e9
            now = self.get_clock().now().nanoseconds / 1e9
            self.gate.receive(msg.twist.linear.x, msg.twist.angular.z, stamp, now, msg.header.frame_id)

        def watch(self):
            while not self.done.wait(min(0.02, config.command_timeout / 4)):
                self.gate.tick()

        def publish_status(self):
            data = self.gate.status()
            data["source_stamp"] = self.get_clock().now().nanoseconds / 1e9
            msg = String()
            msg.data = json.dumps(data, allow_nan=False)
            self.status_pub.publish(msg)

        def close(self):
            self.done.set()
            self.watchdog.join(timeout=config.command_timeout + 0.5)
            self.gate.stop()

    node = MotorNode()
    try:
        rclpy.spin(node)
    except KeyboardInterrupt:
        pass
    finally:
        node.close()
        node.destroy_node()
        rclpy.try_shutdown()


if __name__ == "__main__":
    main()
