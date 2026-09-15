"""ROS 2 adapter for the Go2 simulator's reactive roaming example.

Sensor observations and velocity requests use ROS only. The sandbox's explicit
command grant determines whether these requests can move the virtual robot.
"""

import argparse
import json
import math
import time

from controller import RoamController


MAX_SOURCE_AGE = 0.35


def stale_observation(observed, now):
    for topic in ("scan", "odom"):
        if topic not in observed or not 0 <= now - observed[topic] < MAX_SOURCE_AGE:
            return "stale_" + topic
    return None


def pose_values(message):
    """Accept only finite odometry expressed in the expected robot frames."""
    if message.header.frame_id != "odom" or message.child_frame_id != "base_link":
        return None
    pose = message.pose.pose
    x, y = pose.position.x, pose.position.y
    q = pose.orientation
    values = (x, y, pose.position.z, q.x, q.y, q.z, q.w)
    if not all(math.isfinite(value) for value in values):
        return None
    norm = math.sqrt(q.x * q.x + q.y * q.y + q.z * q.z + q.w * q.w)
    if abs(norm - 1) > 0.01:
        return None
    qx, qy, qz, qw = (value / norm for value in (q.x, q.y, q.z, q.w))
    yaw = math.atan2(2 * (qw * qz + qx * qy), 1 - 2 * (qy * qy + qz * qz))
    return x, y, yaw


class ExposureGate:
    """Map fresh, strictly advancing ROS capture stamps to monotonic time."""

    def __init__(self):
        self.last_stamps = {}

    def capture_time(self, topic, stamp, *, wall_ns=None, monotonic=None):
        wall_ns = time.time_ns() if wall_ns is None else wall_ns
        monotonic = time.monotonic() if monotonic is None else monotonic
        source = stamp.sec * 1_000_000_000 + stamp.nanosec
        age = (wall_ns - source) / 1e9
        if (not 0 <= stamp.nanosec < 1_000_000_000 or source <= 0
                or not -0.05 <= age < MAX_SOURCE_AGE
                or source <= self.last_stamps.get(topic, -1)):
            return None
        self.last_stamps[topic] = source
        # Preserve exposure age; delayed packets must not buy another full
        # freshness interval merely by arriving at the controller now.
        return monotonic - max(0.0, age)


def make_node(*, autostart=False):
    from geometry_msgs.msg import Twist
    from nav_msgs.msg import Odometry
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy
    from sensor_msgs.msg import LaserScan
    from std_msgs.msg import String
    from std_srvs.srv import Trigger

    class RoamingApp(Node):
        def __init__(self):
            super().__init__("wendy_go2_roam")
            self.controller = RoamController()
            self.exposures = ExposureGate()
            self.observed = {}
            self.autostart_pending = autostart
            self.observation_error = None
            self.drive = self.create_publisher(Twist, "/cmd_vel", QoSProfile(depth=1))
            self.status_pub = self.create_publisher(String, "/roam/status", QoSProfile(depth=1))
            sensors = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
            self.create_subscription(LaserScan, "/scan", self.scan, sensors)
            self.create_subscription(Odometry, "/odom", self.odom, sensors)
            self.create_service(Trigger, "/roam/start", self.start)
            self.create_service(Trigger, "/roam/stop", self.stop)
            self.create_timer(0.05, self.tick)
            self.publish_command(0.0, 0.0)

        def scan(self, message):
            metadata = (message.angle_min, message.angle_increment, message.range_min, message.range_max)
            if (message.header.frame_id != "lidar_link"
                    or not all(math.isfinite(value) for value in metadata)
                    or message.angle_increment <= 0 or message.range_min < 0
                    or message.range_max <= message.range_min):
                self.observation_error = "Invalid scan frame or metadata"
                return
            captured = self.exposures.capture_time("scan", message.header.stamp)
            if captured is None:
                self.observation_error = "Stale or reordered scan exposure"
                return
            accepted = self.controller.observe_scan(message.ranges, message.angle_min,
                message.angle_increment, message.range_min, message.range_max, captured)
            if accepted:
                self.observed["scan"] = captured
                self.observation_error = None
            else:
                self.observed.pop("scan", None)
                self.observation_error = "Scan coverage is incomplete"

        def odom(self, message):
            pose = pose_values(message)
            if pose is None:
                self.observation_error = "Invalid odometry frame or pose"
                return
            captured = self.exposures.capture_time("odom", message.header.stamp)
            if captured is None:
                self.observation_error = "Stale or reordered odometry exposure"
                return
            if self.controller.observe_pose(*pose, captured):
                self.observed["odom"] = captured

        def start(self, request, response):
            self.autostart_pending = False
            now = time.monotonic()
            stale = stale_observation(self.observed, now)
            if stale:
                self.controller.stop(stale)
                response.success = False
            else:
                response.success = self.controller.start(now)
            response.message = json.dumps(self.controller.status(now), allow_nan=False)
            return response

        def stop(self, request, response):
            self.stop_controller("Stopped by /roam/stop")
            response.success = True
            response.message = "Roaming stopped; a new /roam/start request is required."
            return response

        def stop_controller(self, reason):
            self.autostart_pending = False
            self.controller.stop(reason)
            self.publish_command(0.0, 0.0)

        def publish_command(self, linear, angular):
            message = Twist()
            message.linear.x, message.angular.z = linear, angular
            self.drive.publish(message)

        def tick(self):
            now = time.monotonic()
            stale = stale_observation(self.observed, now)
            if self.autostart_pending and stale is None:
                self.autostart_pending = False
                self.controller.start(now)
            if self.controller.active and stale:
                self.controller.stop(stale)
            self.publish_command(*self.controller.tick(now))
            status = self.controller.status(now)
            status.update(autostart_pending=self.autostart_pending,
                          observation_error=self.observation_error)
            self.status_pub.publish(String(data=json.dumps(status, allow_nan=False)))

    return RoamingApp()


def main():
    import rclpy
    from rclpy.executors import ExternalShutdownException

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--autostart", action="store_true",
                        help="start once after fresh scan and odometry arrive")
    args, ros_args = parser.parse_known_args()
    rclpy.init(args=ros_args)
    node = make_node(autostart=args.autostart)
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        pass
    except Exception:
        if rclpy.ok():
            raise
    finally:
        if rclpy.ok():
            node.stop_controller("Application shutdown")
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
