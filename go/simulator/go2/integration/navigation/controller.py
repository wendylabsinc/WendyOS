"""Small, bounded ROS goal follower used to validate the virtual robot.

This is a reactive integration client, not a map planner or a Nav2 replacement.
It takes goals and all motion feedback through ROS. The controller has no HTTP
client and never imports the simulator.
"""

import argparse
import json
import math
import time


def yaw(q):
    return math.atan2(2 * (q.w * q.z + q.x * q.y), 1 - 2 * (q.y * q.y + q.z * q.z))


def stamp_ns(stamp):
    return stamp.sec * 1_000_000_000 + stamp.nanosec


def clamp(value, lower, upper):
    return min(upper, max(lower, value))


class GoalFollower:
    tolerance = 0.25
    obstacle_clearance = 0.65
    scan_timeout = 0.3
    observation_timeout = 0.3

    def __init__(self):
        self.goal = None
        self.goal_stamp = None
        self.odom = None
        self.receipts = {}
        self.mounts_ready = False
        self.clearance = None
        self.coverage = 0.0
        self.state = "idle"
        self.command = (0.0, 0.0)
        self.distance = None
        self.obstacle_latched = False

    def set_goal(self, x, y, stamp, frame):
        self.goal_stamp = stamp
        self.command = (0.0, 0.0)
        if frame != "odom" or not all(math.isfinite(v) and abs(v) <= 5.3 for v in (x, y)):
            self.goal = None
            self.state = "invalid_goal"
            return
        self.goal = (x, y)
        self.state = "goal_received"
        self.obstacle_latched = False

    def observe_odom(self, x, y, heading, now):
        if all(math.isfinite(v) for v in (x, y, heading)):
            self.odom = (x, y, heading)
            self.receipts["odom"] = now

    def observe_scan(self, ranges, angle_min, increment, minimum, maximum, now):
        cone = [r for index, r in enumerate(ranges)
                if abs(angle_min + index * increment) <= math.radians(35)]
        valid = [r for r in cone if math.isfinite(r) and minimum <= r <= maximum]
        # Missing returns are unknown space, not synthetic maximum-range hits.
        self.coverage = len(valid) / len(cone) if cone else 0.0
        self.clearance = min(valid) if valid else None
        self.receipts["scan"] = now

    def tick(self, now):
        self.command = (0.0, 0.0)
        if self.goal is None:
            return self.command
        for topic in ("scan", "odom", "imu"):
            age = now - self.receipts.get(topic, float("-inf"))
            timeout = self.scan_timeout if topic == "scan" else self.observation_timeout
            if age < 0 or age >= timeout:
                # Stale perception cancels this goal. Fresh scans alone must
                # not restart an unattended old goal.
                self.goal = None
                self.state = "stale_" + topic
                return self.command
        if not self.mounts_ready:
            self.state = "waiting_for_tf"
            return self.command
        if self.coverage < 0.6 or self.clearance is None:
            self.state = "unknown_scan"
            return self.command
        x, y, heading = self.odom
        dx, dy = self.goal[0] - x, self.goal[1] - y
        self.distance = math.hypot(dx, dy)
        if self.distance <= self.tolerance:
            self.goal = None
            self.state = "arrived"
            return self.command
        threshold = self.obstacle_clearance + (0.1 if self.obstacle_latched else 0.0)
        if self.clearance <= threshold:
            self.obstacle_latched = True
            self.state = "obstacle"
            return self.command
        self.obstacle_latched = False
        error = math.atan2(math.sin(math.atan2(dy, dx) - heading),
                           math.cos(math.atan2(dy, dx) - heading))
        angular = clamp(1.4 * error, -0.4, 0.4)
        # The quadruped policy has a small-command deadband. Use a bounded
        # walking speed until entering the explicit 25 cm arrival tolerance.
        linear = clamp(0.6 * self.distance, 0.18, 0.3) if abs(error) < 0.45 else 0.0
        self.command = (linear, angular)
        self.state = "moving"
        return self.command

    def status(self, now):
        return {"state": self.state, "goal_stamp": self.goal_stamp,
                "goal": self.goal, "distance": self.distance,
                "command": list(self.command), "clearance": self.clearance,
                "scan_coverage": self.coverage, "tf_ready": self.mounts_ready,
                "ages_ms": {key: max(0.0, now - value) * 1000
                            for key, value in self.receipts.items()}}


def make_node(scan_topic):
    from geometry_msgs.msg import PoseStamped, Twist
    from nav_msgs.msg import Odometry
    from rclpy.node import Node
    from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy
    from sensor_msgs.msg import Imu, LaserScan
    from std_msgs.msg import String
    from tf2_msgs.msg import TFMessage

    class NavigationClient(Node):
        def __init__(self):
            super().__init__("wendy_go2_navigation_client")
            self.follower = GoalFollower()
            self.transforms = set()
            self.last_stamps = {}
            self.drive = self.create_publisher(Twist, "/cmd_vel", QoSProfile(depth=1))
            self.status_pub = self.create_publisher(String, "/navigation/status", QoSProfile(depth=10))
            sensors = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
            self.create_subscription(PoseStamped, "/goal_pose", self.goal, 1)
            self.create_subscription(Odometry, "/odom", self.odom, sensors)
            self.create_subscription(Imu, "/imu/data", self.imu, sensors)
            self.create_subscription(LaserScan, scan_topic, self.scan, sensors)
            self.create_subscription(TFMessage, "/tf", self.tf, 10)
            self.create_subscription(TFMessage, "/tf_static", self.tf, QoSProfile(
                depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL))
            self.create_timer(0.05, self.tick)

        def fresh(self, key, header):
            source = stamp_ns(header.stamp)
            age = (time.time_ns() - source) / 1e9
            if not -0.05 <= age < 0.3 or source <= self.last_stamps.get(key, -1):
                return False
            self.last_stamps[key] = source
            return True

        def goal(self, message):
            if not self.fresh("goal", message.header):
                return
            self.follower.set_goal(message.pose.position.x, message.pose.position.y,
                                   stamp_ns(message.header.stamp), message.header.frame_id)

        def odom(self, message):
            if (message.header.frame_id != "odom" or message.child_frame_id != "base_link"
                    or not self.fresh("odom", message.header)):
                return
            # Estimated pose must declare uncertainty, including planar yaw.
            if not all(math.isfinite(message.pose.covariance[i]) and message.pose.covariance[i] > 0
                       for i in (0, 7, 35)):
                return
            pose = message.pose.pose
            self.follower.observe_odom(pose.position.x, pose.position.y,
                                       yaw(pose.orientation), time.monotonic())

        def imu(self, message):
            if message.header.frame_id == "imu_link" and self.fresh("imu", message.header):
                self.follower.receipts["imu"] = time.monotonic()

        def scan(self, message):
            if message.header.frame_id != "lidar_link" or not self.fresh("scan", message.header):
                return
            self.follower.observe_scan(message.ranges, message.angle_min, message.angle_increment,
                                       message.range_min, message.range_max, time.monotonic())

        def tf(self, message):
            for transform in message.transforms:
                self.transforms.add((transform.header.frame_id, transform.child_frame_id))
            self.follower.mounts_ready = {
                ("odom", "base_link"), ("base_link", "imu_link"), ("base_link", "lidar_link")
            }.issubset(self.transforms)

        def tick(self):
            now = time.monotonic()
            linear, angular = self.follower.tick(now)
            command = Twist()
            command.linear.x, command.angular.z = linear, angular
            self.drive.publish(command)
            self.status_pub.publish(String(data=json.dumps(self.follower.status(now), allow_nan=False)))

    return NavigationClient()


def main():
    import rclpy
    from rclpy.executors import ExternalShutdownException
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scan-topic", default="/scan")
    args = parser.parse_args()
    rclpy.init()
    node = make_node(args.scan_topic)
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        # Expected during normal shutdown; cleanup happens in finally below.
        pass
    except Exception:
        # Humble can surface an invalid-context RCLError during SIGTERM
        # shutdown instead of ExternalShutdownException.
        if rclpy.ok():
            raise
    finally:
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
