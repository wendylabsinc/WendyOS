"""ROS adapter for the separate velocity guard process; imports are lazy."""

import json
import math


def parse_permit(text):
    if not isinstance(text, str) or len(text) > 1024:
        raise ValueError("invalid permit size")
    value = json.loads(text)
    if not isinstance(value, dict) or set(value) != {"enabled", "max_speed"} or type(value["enabled"]) is not bool:
        raise ValueError("permit must contain enabled and max_speed")
    speed = value["max_speed"]
    if isinstance(speed, bool) or not isinstance(speed, (int, float)) or not math.isfinite(speed) or speed <= 0:
        raise ValueError("invalid permit speed")
    return value["enabled"], speed


def parse_stop(text):
    if not isinstance(text, str) or len(text) > 1024:
        raise ValueError("invalid stop size")
    value = json.loads(text)
    if not isinstance(value, dict) or set(value) != {"latch"} or type(value["latch"]) is not bool:
        raise ValueError("stop must contain a boolean latch")
    return value["latch"]


def planar_origin(translation, quaternion):
    """Reject tilted scan planes; commissioned 2D clearance uses a level scan."""
    if len(translation) != 3 or len(quaternion) != 4:
        raise ValueError("invalid transform shape")
    if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) for v in (*translation, *quaternion)):
        raise ValueError("nonfinite transform")
    x, y, z, w = quaternion
    if abs(x*x + y*y + z*z + w*w - 1) > 0.001:
        raise ValueError("transform quaternion must be normalized")
    roll = math.atan2(2*(w*x + y*z), 1-2*(x*x+y*y))
    pitch = math.asin(max(-1.0, min(1.0, 2*(w*y-z*x))))
    if abs(roll) > 0.05 or abs(pitch) > 0.05:
        raise ValueError("scan plane is not level in base frame")
    yaw = math.atan2(2*(w*z+x*y), 1-2*(y*y+z*z))
    return translation[0], translation[1], yaw


def main():
    import rclpy
    from rclpy.duration import Duration
    from rclpy.node import Node
    from rclpy.qos import qos_profile_sensor_data
    from rclpy.time import Time
    from geometry_msgs.msg import Twist, TwistStamped
    from nav_msgs.msg import Odometry
    from sensor_msgs.msg import LaserScan
    from std_msgs.msg import String
    from tf2_ros import Buffer, TransformListener
    from .guard import GuardConfig, VelocityGuard
    from .settings import load_settings

    settings = load_settings()
    runtime = settings["runtime"]
    config = GuardConfig(**settings["guard"], base_frame=runtime["base_frame"],
                         max_linear=runtime["max_linear_speed"], max_angular=runtime["max_angular_speed"],
                         commissioned=runtime["motion_enabled"] and runtime["watchdog_commissioned"])

    class GuardNode(Node):
        def __init__(self):
            super().__init__("robot_navigation_guard")
            self.guard = VelocityGuard(config, source_clock=self.source_now)
            self.tf_buffer = Buffer(cache_time=Duration(seconds=5))
            self.tf_listener = TransformListener(self.tf_buffer, self)
            self.output = self.create_publisher(TwistStamped, "/robot_navigation/safe_cmd_vel", 1)
            self.status_output = self.create_publisher(String, "/robot_navigation/guard_status", 1)
            self.create_subscription(LaserScan, settings["topics"]["scan"], self.on_scan, qos_profile_sensor_data)
            self.create_subscription(Odometry, settings["topics"]["odometry"], self.on_odometry, qos_profile_sensor_data)
            self.create_subscription(Twist, "/robot_navigation/unsafe_cmd_vel", self.on_command, 1)
            self.create_subscription(String, "/robot_navigation/permit", self.on_permit, 1)
            self.create_subscription(String, "/robot_navigation/stop", self.on_stop, 1)
            # A ROS-clock pause also pauses this timer; the motor process has an
            # independent wall-clock timeout and rejects stale stamped commands.
            self.create_timer(0.05, self.publish)

        def source_now(self):
            return self.get_clock().now().nanoseconds / 1_000_000_000

        def on_scan(self, message):
            stamp = message.header.stamp.sec + message.header.stamp.nanosec / 1_000_000_000
            frame, origin = config.base_frame, (0.0, 0.0, 0.0)
            try:
                if message.header.frame_id != config.base_frame:
                    transform = self.tf_buffer.lookup_transform(config.base_frame, message.header.frame_id,
                                                                Time.from_msg(message.header.stamp), timeout=Duration(seconds=0))
                    t, q = transform.transform.translation, transform.transform.rotation
                    origin = planar_origin((t.x, t.y, t.z), (q.x, q.y, q.z, q.w))
            except Exception:
                frame = "invalid_transform"
            self.guard.update_scan(stamp=stamp, frame_id=frame, angle_min=message.angle_min,
                                   angle_increment=message.angle_increment, ranges=message.ranges,
                                   range_min=message.range_min, range_max=message.range_max, origin=origin)

        def on_odometry(self, message):
            stamp = message.header.stamp.sec + message.header.stamp.nanosec / 1_000_000_000
            frame = message.child_frame_id if message.header.frame_id == runtime["odometry_frame"] else "invalid_odometry_frame"
            v, w = message.twist.twist.linear, message.twist.twist.angular
            self.guard.update_odometry(stamp=stamp, frame_id=frame, linear_speed=math.hypot(v.x, v.y), angular_speed=w.z)

        def on_command(self, message):
            if any(value != 0 for value in (message.linear.z, message.angular.x, message.angular.y)):
                self.guard.update_command(math.nan, 0, 0)
            else:
                self.guard.update_command(message.linear.x, message.linear.y, message.angular.z)

        def on_permit(self, message):
            try:
                enabled, speed = parse_permit(message.data)
                self.guard.update_permit(enabled, speed)
            except (ValueError, TypeError):
                self.guard.update_permit(False)

        def on_stop(self, message):
            try:
                latch = parse_stop(message.data)
            except (ValueError, TypeError):
                latch = True
            if latch:
                self.guard.stop()
            else:
                self.guard.update_permit(False)
            self.publish()

        def publish(self):
            state = self.guard.evaluate()
            command = TwistStamped()
            command.header.stamp = self.get_clock().now().to_msg()
            command.header.frame_id = config.base_frame
            command.twist.linear.x = state["linear_x"]
            command.twist.angular.z = state["angular_z"]
            self.output.publish(command)
            state.update(scan_stamp=self.guard.scan[0] if self.guard.scan else None,
                         frame_id=config.base_frame, sensors_ready=self.guard.sensor_problem() is None,
                         coverage_ok=self.guard.scan is not None)
            message = String()
            message.data = json.dumps(state, allow_nan=False)
            self.status_output.publish(message)

    rclpy.init()
    node = GuardNode()
    try:
        rclpy.spin(node)
    finally:
        node.guard.update_permit(False)
        try:
            node.publish()
        finally:
            node.destroy_node()
            rclpy.shutdown()


if __name__ == "__main__":
    main()
