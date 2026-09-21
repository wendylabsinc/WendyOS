"""Explore office hallways through native Go2 ROS observations and Move requests."""
import argparse
import json
import time

from admission import HallwayApp
from go2_io import CLOUD_TOPIC, ODOM_TOPIC, SPORT_TOPIC, sport_request


def make_node(*, autostart=False, **options):
    from nav_msgs.msg import Odometry
    from sensor_msgs.msg import PointCloud2
    from std_msgs.msg import String
    from std_srvs.srv import Trigger
    from unitree_api.msg import Request
    from rclpy.node import Node
    from rclpy.qos import QoSProfile, ReliabilityPolicy

    class NodeImpl(Node):
        def __init__(self):
            super().__init__("wendy_go2_hallway")
            self.app = HallwayApp(**options)
            self.autostart_pending = autostart
            self.report = None
            self.publisher = self.create_publisher(Request, SPORT_TOPIC, QoSProfile(depth=1))
            self.status_pub = self.create_publisher(String, "/hallway/status", QoSProfile(depth=1))
            qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
            self.create_subscription(Odometry, ODOM_TOPIC, self.odom, qos)
            self.create_subscription(PointCloud2, CLOUD_TOPIC, self.cloud, qos)
            self.create_service(Trigger, "/hallway/start", self.start)
            self.create_service(Trigger, "/hallway/stop", self.stop)
            self.create_timer(.05, self.tick)
            self.publish(0, 0)

        def publish(self, x, yaw):
            self.publisher.publish(sport_request(Request, x, 0.0, yaw))

        def odom(self, message):
            self.app.odom(message)
            if not self.app.controller.active:
                self.publish(0, 0)

        def cloud(self, message):
            self.app.cloud(message)
            if not self.app.controller.active:
                self.publish(0, 0)

        def start(self, request, response):
            self.autostart_pending = False
            response.success = self.app.controller.start(time.monotonic())
            self.publish(0, 0)
            response.message = json.dumps(self.app.status(time.monotonic()), allow_nan=False)
            return response

        def stop(self, request, response):
            self.autostart_pending = False
            self.app.controller.stop("operator_stop")
            self.publish(0, 0)
            response.success, response.message = True, "Stopped. Call /hallway/start for a new exploration."
            return response

        def tick(self):
            now = time.monotonic()
            if self.autostart_pending and self.app.controller.observation_error(now) is None:
                self.autostart_pending = False
                self.app.controller.start(now)
            self.publish(*self.app.controller.tick(now))
            status = self.app.status(now)
            status["autostart_pending"] = self.autostart_pending
            self.status_pub.publish(String(data=json.dumps(status, allow_nan=False)))
            report = (status["state"], status["reason"], tuple(sorted(status["sensor_errors"].items())))
            if report != self.report:
                self.report = report
                print(json.dumps(status, allow_nan=False), flush=True)

    return NodeImpl()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--autostart", action="store_true", help="begin once observations are ready")
    parser.add_argument("--ignore-capture-age", action="store_true", help="use local arrival time for unsynchronized clocks")
    parser.add_argument("--max-distance", type=float, default=20)
    parser.add_argument("--max-seconds", type=float, default=180)
    parser.add_argument("--prefer", choices=("left", "right"), default="left")
    args, ros_args = parser.parse_known_args()
    options = vars(args)
    # Validate before creating a command publisher.
    HallwayApp(**{k: v for k, v in options.items() if k != "autostart"})
    import rclpy
    from rclpy.executors import ExternalShutdownException
    rclpy.init(args=ros_args)
    node = make_node(**options)
    print("Hallway explorer ready. " + ("Waiting to start automatically." if args.autostart else
          "Call /hallway/start when ready. Deployment alone does not start exploration."), flush=True)
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        # Expected shutdown signals; finally performs cleanup.
        pass
    finally:
        if rclpy.ok():
            node.app.controller.stop("application_shutdown")
            node.publish(0, 0)
        node.destroy_node()
        if rclpy.ok():
            rclpy.shutdown()


if __name__ == "__main__":
    main()
