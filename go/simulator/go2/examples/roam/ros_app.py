"""Native ROS 2 adapter for reactive roaming on Go2 robots and the simulator.

Sensor observations and velocity requests use ROS only. The sandbox's explicit
command grant determines whether these requests can move the virtual robot.
"""

import argparse
import json
import math
import time

from go2_io import CLOUD_TOPIC, ODOM_TOPIC, SPORT_TOPIC, cloud_scan, sport_request, sensor_wall_ns

from controller import RoamController, finite


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
    """Admit advancing captures, optionally using arrival time for their age."""

    def __init__(self, *, ignore_capture_age=False):
        self.ignore_capture_age = ignore_capture_age
        self.last_stamps = {}
        self.clock_anchor = None
        self.rejections = {}
        self.ages = {}

    def capture_time(self, topic, stamp, *, wall_ns=None, monotonic=None):
        wall_ns = sensor_wall_ns(time.time_ns() if wall_ns is None else wall_ns)
        monotonic = time.monotonic() if monotonic is None else monotonic
        self.ages.pop(topic, None)
        if (type(stamp.sec) is not int or type(stamp.nanosec) is not int
                or not 0 <= stamp.nanosec < 1_000_000_000
                or not finite(wall_ns) or not finite(monotonic)):
            self.rejections[topic] = "malformed_capture_stamp"
            return None
        source = stamp.sec * 1_000_000_000 + stamp.nanosec
        age = (wall_ns - source) / 1e9
        self.ages[topic] = age
        reason = ("nonpositive_capture_stamp" if source <= 0 else
                  "capture_in_future" if not self.ignore_capture_age and age < -0.05 else
                  "capture_too_old" if not self.ignore_capture_age and age >= MAX_SOURCE_AGE else
                  "capture_not_advancing" if source <= self.last_stamps.get(topic, -1) else None)
        if reason:
            self.rejections[topic] = reason
            return None
        self.rejections.pop(topic, None)
        self.last_stamps[topic] = source
        if self.ignore_capture_age:
            return monotonic
        if self.clock_anchor is None:
            self.clock_anchor = (wall_ns, monotonic)
        anchor_wall, anchor_monotonic = self.clock_anchor
        return min(monotonic, anchor_monotonic + (source - anchor_wall) / 1e9)


def make_node(*, autostart=False, ignore_capture_age=False, allow_scan_gaps=False):
    from unitree_api.msg import Request
    from nav_msgs.msg import Odometry
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy
    from sensor_msgs.msg import PointCloud2
    from std_msgs.msg import String
    from std_srvs.srv import Trigger

    class RoamingApp(Node):
        def __init__(self):
            super().__init__("wendy_go2_roam")
            self.controller = RoamController(allow_scan_gaps=allow_scan_gaps)
            self.exposures = ExposureGate(ignore_capture_age=ignore_capture_age)
            self.latest_odom = None
            self.observed = {}
            self.autostart_pending = autostart
            self.observation_error = None
            self.drive = self.create_publisher(Request, SPORT_TOPIC, QoSProfile(depth=1))
            self.status_pub = self.create_publisher(String, "/roam/status", QoSProfile(depth=1))
            sensors = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
            self.create_subscription(PointCloud2, CLOUD_TOPIC, self.cloud, sensors)
            self.create_subscription(Odometry, ODOM_TOPIC, self.odom, sensors)
            self.create_service(Trigger, "/roam/start", self.start)
            self.create_service(Trigger, "/roam/stop", self.stop)
            self.create_timer(0.05, self.tick)
            self.publish_command(0.0, 0.0)

        def cloud(self, message):
            try:
                if self.latest_odom is None:
                    raise ValueError("waiting_for_odometry_orientation")
                self.scan(cloud_scan(message, self.latest_odom))
            except (ValueError, TypeError, AttributeError) as error:
                self.observed.pop("scan", None)
                self.observation_error = str(error)
                self.controller.stop("invalid_point_cloud: " + str(error))
                self.publish_command(0.0, 0.0)

        def scan(self, message):
            metadata = (message.angle_min, message.angle_increment, message.range_min, message.range_max)
            if (message.header.frame_id != "base_footprint"
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
            if not self.controller.active:
                self.publish_command(0.0, 0.0)

        def odom(self, message):
            pose = pose_values(message)
            if pose is None:
                self.latest_odom = None
                self.observation_error = "Invalid odometry frame or pose"
                return
            captured = self.exposures.capture_time("odom", message.header.stamp)
            if captured is None:
                self.latest_odom = None
                self.observation_error = "Stale or reordered odometry exposure"
                return
            if self.controller.observe_pose(*pose, captured):
                self.latest_odom = message
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
            self.drive.publish(sport_request(Request, linear, 0.0, angular))

        def tick(self):
            now = time.monotonic()
            stale = stale_observation(self.observed, now)
            if (self.autostart_pending and stale is None and self.controller._scan
                    and self.controller._scan["front"]["usable"]):
                self.autostart_pending = False
                self.controller.start(now)
            if self.controller.active and stale:
                self.controller.stop(stale)
            self.publish_command(*self.controller.tick(now))
            status = self.controller.status(now)
            status.update(autostart_pending=self.autostart_pending,
                          observation_error=self.observation_error,
                          ignore_capture_age=self.exposures.ignore_capture_age,
                          capture_age_seconds=dict(self.exposures.ages))
            self.status_pub.publish(String(data=json.dumps(status, allow_nan=False)))

    return RoamingApp()


def main():
    import rclpy
    from rclpy.executors import ExternalShutdownException

    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--autostart", action="store_true",
                        help="start once after accepted scan and odometry arrive")
    parser.add_argument("--ignore-capture-age", action="store_true",
                        help="use local arrival time for sensor timeouts with unsynchronized clocks")
    parser.add_argument("--allow-scan-gaps", action="store_true",
                        help="allow missing returns; require measured clearance in each sector used for motion")
    args, ros_args = parser.parse_known_args()
    rclpy.init(args=ros_args)
    node = make_node(autostart=args.autostart, ignore_capture_age=args.ignore_capture_age,
                     allow_scan_gaps=args.allow_scan_gaps)
    if args.ignore_capture_age:
        print("Capture age checks disabled. Sensor timeouts use local arrival time.", flush=True)
    if args.allow_scan_gaps:
        print("Scan gaps allowed. Obstacle checks use measured returns; unobserved obstacles may be missed.",
              flush=True)
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        # Expected shutdown signals; finally releases the node and server.
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
