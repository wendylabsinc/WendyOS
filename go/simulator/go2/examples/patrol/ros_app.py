"""Run a finite Go2 waypoint patrol using ROS observations and velocity requests."""

import argparse
import json
import math
import time

from go2_io import CLOUD_TOPIC, ODOM_TOPIC, SPORT_TOPIC, cloud_scan, sport_request, sensor_wall_ns

from patrol import DEFAULT_WAYPOINTS, OBSERVATION_TIMEOUT, PatrolController, finite


class ExposureGate:
    """Admit advancing wall-clock capture stamps without resetting their age."""

    def __init__(self):
        self.last_stamps = {}
        self.clock_anchor = None

    def capture_time(self, topic, stamp, *, wall_ns=None, monotonic=None):
        wall_ns = sensor_wall_ns(time.time_ns() if wall_ns is None else wall_ns)
        monotonic = time.monotonic() if monotonic is None else monotonic
        if (type(stamp.sec) is not int or type(stamp.nanosec) is not int
                or not 0 <= stamp.nanosec < 1_000_000_000
                or not finite(wall_ns) or not finite(monotonic)):
            return None
        source = stamp.sec * 1_000_000_000 + stamp.nanosec
        age = (wall_ns - source) / 1e9
        if (source <= 0 or not -0.05 <= age < OBSERVATION_TIMEOUT
                or source <= self.last_stamps.get(topic, -1)):
            return None
        self.last_stamps[topic] = source
        if self.clock_anchor is None:
            self.clock_anchor = (wall_ns, monotonic)
        anchor_wall, anchor_monotonic = self.clock_anchor
        return min(monotonic, anchor_monotonic + (source - anchor_wall) / 1e9)


def pose_values(message):
    if message.header.frame_id != "odom" or message.child_frame_id != "base_link":
        return None
    pose = message.pose.pose
    q = pose.orientation
    values = (pose.position.x, pose.position.y, pose.position.z, q.x, q.y, q.z, q.w)
    if not all(finite(value) for value in values):
        return None
    norm = math.hypot(q.x, q.y, q.z, q.w)
    if abs(norm - 1.0) > 0.01:
        return None
    qx, qy, qz, qw = (value / norm for value in (q.x, q.y, q.z, q.w))
    heading = math.atan2(2 * (qw * qz + qx * qy), 1 - 2 * (qy * qy + qz * qz))
    return pose.position.x, pose.position.y, heading


def scan_metadata_valid(message):
    fields = (message.angle_min, message.angle_max, message.angle_increment,
              message.range_min, message.range_max)
    return (message.header.frame_id == "base_footprint" and all(finite(value) for value in fields)
            and 0 < message.angle_increment <= math.radians(5.01)
            and 0 <= message.range_min < message.range_max
            and abs(message.angle_max - message.angle_min
                    - (len(message.ranges) - 1) * message.angle_increment) < 1e-4)


def make_node(waypoints=DEFAULT_WAYPOINTS, laps=1, *, autostart=False):
    from unitree_api.msg import Request
    from nav_msgs.msg import Odometry
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy
    from sensor_msgs.msg import PointCloud2
    from std_msgs.msg import String
    from std_srvs.srv import Trigger

    class PatrolApp(Node):
        def __init__(self):
            super().__init__("wendy_go2_patrol")
            self.controller = PatrolController(waypoints, laps)
            self.autostart_pending = autostart
            self.last_report = None
            self.exposures = ExposureGate()
            self.latest_odom = None
            self.drive = self.create_publisher(Request, SPORT_TOPIC, QoSProfile(depth=1))
            self.status_pub = self.create_publisher(String, "/patrol/status", QoSProfile(depth=1))
            sensors = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
            self.create_subscription(PointCloud2, CLOUD_TOPIC, self.cloud, sensors)
            self.create_subscription(Odometry, ODOM_TOPIC, self.odom, sensors)
            self.create_service(Trigger, "/patrol/start", self.start)
            self.create_service(Trigger, "/patrol/stop", self.stop)
            self.create_timer(0.05, self.tick)
            self.publish_command(0.0, 0.0)

        def publish_command(self, linear, angular):
            self.drive.publish(sport_request(Request, linear, 0.0, angular))

        def stop_controller(self, reason):
            self.controller.stop(reason)
            self.publish_command(0.0, 0.0)

        def cloud(self, message):
            try:
                if self.latest_odom is None:
                    raise ValueError("waiting_for_odometry_orientation")
                self.scan(cloud_scan(message, self.latest_odom))
            except (ValueError, TypeError, AttributeError) as error:
                self.controller.reject_scan("invalid_point_cloud: " + str(error))
                self.publish_command(0.0, 0.0)

        def scan(self, message):
            if not scan_metadata_valid(message):
                self.controller.reject_scan("invalid_scan_frame_or_metadata")
                self.publish_command(0.0, 0.0)
                return
            captured = self.exposures.capture_time("scan", message.header.stamp)
            if captured is None:
                self.controller.reject_scan("invalid_scan_exposure")
                self.publish_command(0.0, 0.0)
                return
            self.controller.observe_scan(message.ranges, message.angle_min, message.angle_increment,
                                         message.range_min, message.range_max, captured)
            if not self.controller.active:
                self.publish_command(0.0, 0.0)

        def odom(self, message):
            pose = pose_values(message)
            captured = self.exposures.capture_time("odom", message.header.stamp) if pose else None
            if pose is None or captured is None:
                self.latest_odom = None
                self.controller.pose = self.controller.pose_at = None
                self.stop_controller("invalid_odometry_exposure_or_pose")
                return
            if not self.controller.observe_pose(*pose, captured):
                self.latest_odom = None
                self.publish_command(0.0, 0.0)
            else:
                self.latest_odom = message

        def start(self, request, response):
            self.autostart_pending = False
            now = time.monotonic()
            response.success = self.controller.start(now)
            if not response.success:
                self.publish_command(0.0, 0.0)
            response.message = json.dumps(self.controller.status(now), allow_nan=False)
            return response

        def stop(self, request, response):
            self.autostart_pending = False
            self.stop_controller("stopped_by_service")
            response.success = True
            response.message = "Patrol stopped; call /patrol/start to start a new route from the current pose."
            return response

        def tick(self):
            now = time.monotonic()
            if self.autostart_pending and self.controller.observation_error(now) is None:
                self.autostart_pending = False
                self.controller.start(now)
            self.publish_command(*self.controller.tick(now))
            status = self.controller.status(now)
            status["autostart_pending"] = self.autostart_pending
            blocker = self.controller.observation_error(now)
            status["readiness_error"] = blocker
            self.status_pub.publish(String(data=json.dumps(status, allow_nan=False)))
            report = (self.controller.active, self.controller.reason, self.controller.index,
                      blocker if self.autostart_pending else None)
            if report != self.last_report:
                self.last_report = report
                if self.autostart_pending:
                    print(f"Patrol waiting: {blocker}; last sensor result: {self.controller.reason}. "
                          f"Expecting {CLOUD_TOPIC} and {ODOM_TOPIC}.", flush=True)
                elif self.controller.active:
                    print(f"Patrol is requesting waypoint {self.controller.index + 1}/{len(self.controller.targets)}.",
                          flush=True)
                else:
                    print(f"Patrol stopped: {self.controller.reason}. "
                          "It will not restart automatically. "
                          "Start a new route with: wendy --device <device> device ros2 call "
                          "/patrol/start std_srvs/srv/Trigger '{}'", flush=True)

    return PatrolApp()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--waypoints", type=json.loads, default=DEFAULT_WAYPOINTS,
                        help='JSON [forward, left] offsets in metres, e.g. "[[1,0],[1,1],[0,1],[0,0]]"')
    parser.add_argument("--laps", type=int, default=1, help="finite number of laps, from 1 to 5")
    parser.add_argument("--autostart", action="store_true",
                        help="start one route after fresh observations arrive")
    args, ros_args = parser.parse_known_args()
    try:
        PatrolController(args.waypoints, args.laps)
    except ValueError as error:
        parser.error(str(error))

    import rclpy
    from rclpy.executors import ExternalShutdownException

    rclpy.init(args=ros_args)
    node = make_node(args.waypoints, args.laps, autostart=args.autostart)
    print("Go2 patrol started. " + ("The route starts automatically after fresh observations arrive. "
          if args.autostart else "Call /patrol/start to begin a route. ") +
          f"Using {CLOUD_TOPIC}, {ODOM_TOPIC} and {SPORT_TOPIC}.", flush=True)
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        pass
    except Exception:
        if rclpy.ok():
            raise
    finally:
        if rclpy.ok():
            node.stop_controller("application_shutdown")
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
