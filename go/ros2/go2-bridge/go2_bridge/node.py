"""Go2 Air ROS 2 bridge: the agent's paired WebRTC connection as the simulator's
ROS 2 contract (unitree_go/unitree_api types, same topic names and frames).

Robot data arrives through WendyUnitreeRobotService.Stream on the agent socket;
commands go back through WendyUnitreeRobotService.Command, which also stops a
move after 500 ms unless it is refreshed.
"""

import array
import collections
import logging
import os
import queue
import threading
import time

import grpc
import numpy as np
import rclpy
from foxglove_msgs.msg import CompressedVideo
from rclpy._rclpy_pybind11 import RCLError
from rclpy.executors import ExternalShutdownException
from geometry_msgs.msg import TransformStamped, Twist
from nav_msgs.msg import Odometry
from rclpy.node import Node
from rclpy.qos import QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import Image, Imu, JointState, Joy, PointCloud2, PointField
from std_msgs.msg import String, UInt8MultiArray
from tf2_ros import StaticTransformBroadcaster, TransformBroadcaster
from unitree_api.msg import Request, Response
from unitree_go.msg import LowState, SportModeState

from . import convert
from . import unitree_robot_service_pb2 as robot_pb
from . import unitree_robot_service_pb2_grpc as robot_grpc
from . import unitree_service_pb2 as pairing_pb
from . import unitree_service_pb2_grpc as pairing_grpc

log = logging.getLogger("go2-bridge")

ROBOT_TOPICS = [
    "rt/lf/lowstate",
    "rt/lf/sportmodestate",
    "rt/sportmodestate",
    "rt/utlidar/voxel_map_compressed",
    "rt/utlidar/robot_pose",
    "rt/wirelesscontroller",
    "rt/multiplestate",
    "rt/uslam/frontend/cloud_world_ds",
]
# More robot topics to request, comma-separated; they appear as /go2/raw/<topic>.
EXTRA_TOPICS = [t.strip() for t in os.environ.get("GO2_EXTRA_TOPICS", "").split(",") if t.strip()]
RATE_LOG_SECONDS = 10
MAX_FORWARD, MAX_LATERAL, MAX_YAW = 0.8, 0.5, 1.0  # the agent's limits, same as the simulator's
# Response codes used by the Go2 simulator (go2_sim/native_commands.py).
UNSUPPORTED, DENIED = 3203, 3205

# Mount positions match the simulator (go/simulator/go2/go2_sim/sensors.py).
MOUNTS = {"imu_link": (0.0, 0.0, 0.0), "utlidar_imu": (0.0, 0.0, 0.0),
          "lidar_link": (0.28, 0.0, 0.08), "utlidar_lidar": (0.28, 0.0, 0.08),
          "camera_link": (0.40, 0.0, 0.04)}
CAMERA_FRAME = "camera_optical_frame"

COMMANDS = {
    "move": robot_pb.UNITREE_COMMAND_MOVE,
    "stop": robot_pb.UNITREE_COMMAND_STOP,
    "stand_up": robot_pb.UNITREE_COMMAND_STAND_UP,
    "lie_down": robot_pb.UNITREE_COMMAND_LIE_DOWN,
    "hello": robot_pb.UNITREE_COMMAND_HELLO,
}
# The robot acknowledges a posture change only after moving for a while (the
# agent waits up to 3 s), so these get a longer deadline and run off the ROS
# executor, where they would otherwise hold up /cmd_vel and stops.
POSTURE_COMMANDS = {"stand_up", "lie_down", "hello"}
COMMAND_TIMEOUT = 1
POSTURE_COMMAND_TIMEOUT = 4


def fill(msg, data):
    """Copy matching fields from robot JSON into a ROS message, recursively.
    Missing or mistyped fields keep their defaults."""
    if not isinstance(data, dict):
        return
    for name, kind in msg.get_fields_and_field_types().items():
        if name not in data:
            continue
        value, current = data[name], getattr(msg, name)
        try:
            if "/" in kind and "[" in kind:  # fixed array of messages
                for item, element in zip(current, value):
                    fill(item, element)
            elif "/" in kind:
                fill(current, value)
            elif "[" in kind or kind.startswith("sequence"):
                cast = float if "float" in kind or "double" in kind else int
                values = [cast(v) for v in value]
                if "[" in kind and not kind.startswith("sequence"):
                    size = len(current)
                    values = (values + [cast(v) for v in current[len(values):]])[:size]
                setattr(msg, name, values)
            elif kind in ("float", "double"):
                setattr(msg, name, float(value))
            elif kind == "boolean":
                setattr(msg, name, bool(value))
            elif kind == "string":
                setattr(msg, name, str(value))
            else:
                setattr(msg, name, int(value))
        except (TypeError, ValueError, AssertionError):
            continue


class Bridge(Node):
    def __init__(self, channel, serial):
        super().__init__("go2_bridge")
        self.serial = serial
        self.robot = robot_grpc.WendyUnitreeRobotServiceStub(channel)
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE)
        pub = self.create_publisher
        self.pubs = {
            "lowstate": pub(LowState, "/lowstate", qos),
            "lf_lowstate": pub(LowState, "/lf/lowstate", qos),
            "sportmodestate": pub(SportModeState, "/sportmodestate", qos),
            "lf_sportmodestate": pub(SportModeState, "/lf/sportmodestate", qos),
            "imu": pub(Imu, "/imu/data", qos),
            "lidar_imu": pub(Imu, "/utlidar/imu", qos),
            "joints": pub(JointState, "/joint_states", qos),
            "odom": pub(Odometry, "/odom", qos),
            "robot_odom": pub(Odometry, "/utlidar/robot_odom", qos),
            "cloud": pub(PointCloud2, "/utlidar/cloud", qos),
            "cloud_base": pub(PointCloud2, "/utlidar/cloud_base", qos),
            "voxel_map": pub(PointCloud2, "/go2/voxel_map", qos),
            "joy": pub(Joy, "/joy", qos),
            "image": pub(Image, "/camera/color/image_raw", qos),
            "h264": pub(CompressedVideo, "/camera/color/h264", qos),
            "response": pub(Response, "/api/sport/response", qos),
        }
        self.raw_pubs = {}
        self.tf = TransformBroadcaster(self)
        self.static_tf = StaticTransformBroadcaster(self)
        self.publish_mounts()
        self.decoder = None
        # Messages per robot topic and voxel map timing, logged every RATE_LOG_SECONDS.
        self.stats_lock = threading.Lock()
        self.counts = collections.Counter()
        self.voxel_times = []  # arrival, monotonic seconds
        self.cloud_seconds = []  # decode and publish time per voxel map turned into clouds
        self.cloud_summary = None
        self.video_stats = collections.Counter()
        self.create_timer(RATE_LOG_SECONDS, self.log_rates)
        self.video_queue = queue.Queue()
        self.decoding = False
        threading.Thread(target=self.run_decoder, daemon=True).start()
        # A stopped/restarting agent can consume the full command deadline. Keep
        # that wait off the single-threaded ROS executor, and retain only the
        # newest velocity while the worker is busy. The agent's 500 ms lease
        # still stops an in-flight move if commands stop arriving.
        self.cmd_vel_queue = queue.Queue(maxsize=1)
        threading.Thread(target=self.run_cmd_vel, daemon=True).start()
        self.pose = None  # (stamp, position, quaternion_xyzw) from the latest odometry
        self.create_subscription(Twist, "/cmd_vel", self.on_cmd_vel, qos)
        self.create_subscription(Request, "/api/sport/request", self.on_sport_request, 10)

    # --- robot -> ROS ---------------------------------------------------

    def run_stream(self):
        """Stream from the agent forever; reconnect when the robot or agent drops."""
        while rclpy.ok():
            try:
                request = robot_pb.UnitreeStreamRequest(serial=self.serial, topics=ROBOT_TOPICS + EXTRA_TOPICS,
                                                      video=True)
                for message in self.robot.Stream(request):
                    if message.HasField("topic"):
                        self.on_topic(message.topic)
                    elif message.HasField("video"):
                        self.on_video(message.video)
            except grpc.RpcError as e:
                log.warning("robot stream ended: %s", e.details() if hasattr(e, "details") else e)
            except RCLError:
                if not rclpy.ok():
                    return  # shutting down
                raise
            time.sleep(2)

    def stamp(self, received_unix_nanos):
        t = self.get_clock().now().to_msg()
        if received_unix_nanos:
            t.sec, t.nanosec = divmod(received_unix_nanos, 1_000_000_000)
        return t

    def on_topic(self, m):
        try:
            topic, data, payload = convert.parse_envelope(m.data, m.binary)
        except (ValueError, KeyError, IndexError):
            return
        with self.stats_lock:
            self.counts[m.topic] += 1
            if topic == "rt/utlidar/voxel_map_compressed":
                self.voxel_times.append(time.monotonic())
        self.publish_raw(m.topic, m.data, m.binary)
        stamp = self.stamp(m.received_unix_nanos)
        if topic == "rt/lf/lowstate":
            self.on_lowstate(data, stamp)
        elif topic in ("rt/lf/sportmodestate", "rt/sportmodestate"):
            self.on_sportmodestate(data, stamp, low_frequency=topic == "rt/lf/sportmodestate")
        elif topic == "rt/utlidar/voxel_map_compressed" and payload is not None:
            self.on_voxels(data, payload, stamp)
        elif topic == "rt/wirelesscontroller":
            msg = Joy()
            msg.header.stamp = stamp
            msg.axes, msg.buttons = convert.joy(data or {})
            self.pubs["joy"].publish(msg)

    def log_rates(self):
        with self.stats_lock:
            counts, self.counts = self.counts, collections.Counter()
            voxels, self.voxel_times = self.voxel_times, []
            clouds, self.cloud_seconds = self.cloud_seconds, []
            video, self.video_stats = self.video_stats, collections.Counter()
        rates = ", ".join(f"{t} {n / RATE_LOG_SECONDS:.1f}" for t, n in sorted(counts.items()))
        log.info("robot messages per second: %s", rates or "none")
        if len(voxels) > 1:
            log.info("voxel maps: %d arrived, arrival gaps %s s; %d turned into clouds, decode and publish %s s",
                     len(voxels), spread([b - a for a, b in zip(voxels, voxels[1:])]), len(clouds), spread(clouds))
        if self.cloud_summary:
            log.info("latest cloud: %s", self.cloud_summary)
        if video["decoded"]:
            log.info("video: %d frames decoded, %d images published, %.1f s busy, %d waiting",
                     video["decoded"], video["published"], video["seconds"], self.video_queue.qsize())

    def publish_raw(self, topic, data, binary):
        pub = self.raw_pubs.get(topic)
        if pub is None:
            kind = UInt8MultiArray if binary else String
            pub = self.raw_pubs[topic] = self.create_publisher(kind, convert.raw_topic_name(topic), 10)
        if pub.get_subscription_count() == 0:
            return
        if binary:
            msg = UInt8MultiArray()
            msg.data = byte_array(data)
        else:
            msg = String()
            msg.data = data.decode("utf-8", "replace")
        pub.publish(msg)

    def on_lowstate(self, data, stamp):
        low = LowState()
        fill(low, data)
        self.pubs["lowstate"].publish(low)
        self.pubs["lf_lowstate"].publish(low)
        joints = convert.joint_state(data or {})
        if joints:
            msg = JointState()
            msg.header.stamp = stamp
            msg.name, msg.position, msg.velocity, msg.effort = joints
            self.pubs["joints"].publish(msg)

    def on_sportmodestate(self, data, stamp, low_frequency):
        sport = SportModeState()
        fill(sport, data)
        self.pubs["lf_sportmodestate" if low_frequency else "sportmodestate"].publish(sport)
        data = data or {}
        quaternion = (data.get("imu_state") or {}).get("quaternion")
        position = data.get("position")
        if not quaternion or not position:
            return
        # Over Wi-Fi only the sport state carries the full IMU; lowstate has just rpy.
        self.publish_imu(data["imu_state"], stamp)
        odom = Odometry()
        odom.header.stamp, odom.header.frame_id, odom.child_frame_id = stamp, "odom", "base_link"
        p = odom.pose.pose.position
        p.x, p.y, p.z = (float(v) for v in position[:3])
        q = odom.pose.pose.orientation
        q.x, q.y, q.z, q.w = convert.quaternion_xyzw(quaternion)
        # Assumed world-frame velocity (verify on hardware); ROS twist is in base_link.
        velocity = data.get("velocity") or [0.0, 0.0, 0.0]
        vx, vy = convert.world_to_body(float(velocity[0]), float(velocity[1]), convert.yaw_of(quaternion))
        odom.twist.twist.linear.x, odom.twist.twist.linear.y = vx, vy
        odom.twist.twist.angular.z = float(data.get("yaw_speed", 0.0))
        self.pubs["odom"].publish(odom)
        self.pubs["robot_odom"].publish(odom)
        self.pose = (stamp, (p.x, p.y, p.z), (q.x, q.y, q.z, q.w))
        tf = TransformStamped()
        tf.header = odom.header
        tf.child_frame_id = "base_link"
        tf.transform.translation.x, tf.transform.translation.y, tf.transform.translation.z = p.x, p.y, p.z
        tf.transform.rotation = q
        self.tf.sendTransform(tf)

    def publish_imu(self, imu_state, stamp):
        imu = Imu()
        imu.header.stamp, imu.header.frame_id = stamp, "imu_link"
        q = imu.orientation
        q.x, q.y, q.z, q.w = convert.quaternion_xyzw(imu_state["quaternion"])
        g, a = imu_state.get("gyroscope", [0, 0, 0]), imu_state.get("accelerometer", [0, 0, 0])
        imu.angular_velocity.x, imu.angular_velocity.y, imu.angular_velocity.z = map(float, g)
        imu.linear_acceleration.x, imu.linear_acceleration.y, imu.linear_acceleration.z = map(float, a)
        self.pubs["imu"].publish(imu)
        imu.header.frame_id = "utlidar_imu"
        self.pubs["lidar_imu"].publish(imu)

    def on_voxels(self, meta, payload, stamp):
        """The robot sends an accumulated voxel map in odom. Publish it as is
        (/go2/voxel_map) and, using the latest pose, in base_link
        (/utlidar/cloud_base) and in the LiDAR's frame (/utlidar/cloud), like
        the simulator. The body-frame clouds carry the pose's timestamp so
        apps can pair them with odometry."""
        names = ("voxel_map", "cloud_base", "cloud")
        if not any(self.pubs[n].get_subscription_count() for n in names):
            return
        started = time.monotonic()
        try:
            points = convert.decode_voxels(payload, meta)
        except Exception as e:  # malformed frame
            log.warning("voxel decode failed: %s", e)
            return
        self.pubs["voxel_map"].publish(cloud_message(points, stamp, str(meta.get("frame_id") or "odom")))
        if self.pose is None:
            return
        pose_stamp, position, quaternion = self.pose
        body = convert.points_to_body(points, position, quaternion)
        self.cloud_summary = cloud_summary(body, position)
        self.pubs["cloud_base"].publish(cloud_message(body, pose_stamp, "base_link"))
        self.pubs["cloud"].publish(cloud_message(body - np.asarray(MOUNTS["utlidar_lidar"], dtype=np.float32),
                                                 pose_stamp, "utlidar_lidar"))
        with self.stats_lock:
            self.cloud_seconds.append(time.monotonic() - started)

    def on_video(self, frame):
        stamp = self.stamp(frame.received_unix_nanos)
        if self.pubs["h264"].get_subscription_count():
            msg = CompressedVideo()
            msg.timestamp, msg.frame_id, msg.format = stamp, CAMERA_FRAME, "h264"
            msg.data = byte_array(frame.annexb)
            self.pubs["h264"].publish(msg)
        # Decode only while someone watches, on its own thread so robot state and
        # LiDAR never wait for video. None resets the decoder.
        if self.pubs["image"].get_subscription_count():
            self.video_queue.put((bytes(frame.annexb), stamp))
            self.decoding = True
        elif self.decoding:
            self.video_queue.put(None)
            self.decoding = False

    def run_decoder(self):
        """Decode every frame (H.264 needs them all) but convert and publish only
        the newest one, so a slow consumer costs frames, not latency."""
        while True:
            item = self.video_queue.get()
            if item is None:
                self.decoder = None
                continue
            started = time.monotonic()
            frame = self.decode(item[0])
            if frame is None or not self.video_queue.empty():
                with self.stats_lock:
                    self.video_stats["decoded"] += 1
                    self.video_stats["seconds"] += time.monotonic() - started
                continue
            image = frame.to_ndarray(format="rgb24")
            msg = Image()
            msg.header.stamp, msg.header.frame_id = item[1], CAMERA_FRAME
            msg.height, msg.width = image.shape[:2]
            msg.encoding, msg.step = "rgb8", image.shape[1] * 3
            msg.data = byte_array(image.tobytes())
            try:
                self.pubs["image"].publish(msg)
            except RCLError:
                if not rclpy.ok():
                    return
                raise
            with self.stats_lock:
                self.video_stats["decoded"] += 1
                self.video_stats["published"] += 1
                self.video_stats["seconds"] += time.monotonic() - started

    def decode(self, annexb):
        import av  # imported lazily: only needed once video arrives
        if self.decoder is None:
            # A new viewer starts mid-stream; FFmpeg complains until the next keyframe.
            av.logging.set_level(av.logging.FATAL)
            self.decoder = av.CodecContext.create("h264", "r")
        try:
            frames = self.decoder.decode(av.Packet(bytes(annexb)))
        except av.AVError:
            return None
        return frames[-1] if frames else None

    def publish_mounts(self):
        transforms = []
        now = self.get_clock().now().to_msg()
        for name, (x, y, z) in MOUNTS.items():
            tf = TransformStamped()
            tf.header.stamp, tf.header.frame_id, tf.child_frame_id = now, "base_link", name
            tf.transform.translation.x, tf.transform.translation.y, tf.transform.translation.z = x, y, z
            tf.transform.rotation.w = 1.0
            transforms.append(tf)
        optical = TransformStamped()
        optical.header.stamp, optical.header.frame_id, optical.child_frame_id = now, "camera_link", CAMERA_FRAME
        r = optical.transform.rotation
        r.x, r.y, r.z, r.w = 0.5, -0.5, 0.5, -0.5
        transforms.append(optical)
        self.static_tf.sendTransform(transforms)

    # --- ROS -> robot ---------------------------------------------------

    def command(self, name, forward=0.0, lateral=0.0, yaw=0.0):
        try:
            self.robot.Command(robot_pb.UnitreeCommandRequest(
                serial=self.serial, command=COMMANDS[name],
                forward=convert.clamp(forward, MAX_FORWARD), lateral=convert.clamp(lateral, MAX_LATERAL),
                yaw=convert.clamp(yaw, MAX_YAW)),
                timeout=POSTURE_COMMAND_TIMEOUT if name in POSTURE_COMMANDS else COMMAND_TIMEOUT)
            return True
        except grpc.RpcError as e:
            log.warning("command %s failed: %s", name, e.details())
            return False

    def run_cmd_vel(self):
        while rclpy.ok():
            try:
                twist = self.cmd_vel_queue.get(timeout=0.5)
            except queue.Empty:
                continue
            if twist.linear.x == 0 and twist.linear.y == 0 and twist.angular.z == 0:
                self.command("stop")
            else:
                self.command("move", twist.linear.x, twist.linear.y, twist.angular.z)

    def on_cmd_vel(self, twist):
        # Do not build a stale command backlog during an agent outage. Replacing
        # the queued value also ensures a newly received stop supersedes a move.
        while True:
            try:
                self.cmd_vel_queue.put_nowait(twist)
                return
            except queue.Full:
                try:
                    self.cmd_vel_queue.get_nowait()
                except queue.Empty:
                    # The worker consumed it between Full and get_nowait.
                    pass

    def on_sport_request(self, request):
        identity = request.header.identity
        mapped = convert.sport_request(identity.api_id, request.parameter)
        response = Response()
        response.header.identity.id, response.header.identity.api_id = identity.id, identity.api_id
        if mapped is None:
            log.warning("sport API %d is not supported by the Go2 bridge", identity.api_id)
            response.header.status.code = UNSUPPORTED
            self.pubs["response"].publish(response)
            return

        def run():
            response.header.status.code = 0 if self.command(*mapped) else DENIED
            try:
                self.pubs["response"].publish(response)
            except RCLError:
                if rclpy.ok():
                    raise  # otherwise the bridge is shutting down
        if mapped[0] in POSTURE_COMMANDS:
            threading.Thread(target=run, daemon=True).start()
        else:
            run()


def byte_array(data):
    """Humble's generated setters check every element of a bytes value in Python
    (about 0.1 s for one camera image); an array.array('B') skips that."""
    return array.array("B", data)


def cloud_message(points, stamp, frame_id):
    msg = PointCloud2()
    msg.header.stamp, msg.header.frame_id = stamp, frame_id
    msg.height, msg.width = 1, len(points)
    msg.fields = [PointField(name=n, offset=4 * i, datatype=PointField.FLOAT32, count=1)
                  for i, n in enumerate("xyz")]
    msg.is_bigendian, msg.point_step, msg.is_dense = False, 12, True
    msg.row_step = 12 * len(points)
    msg.data = byte_array(np.ascontiguousarray(points, dtype=np.float32).tobytes())
    return msg


def cloud_summary(body, position):
    """Where the body-frame points are, to check the map lines up with the robot."""
    if len(body) == 0:
        return "no points"
    radius = np.hypot(body[:, 0], body[:, 1])
    band = (body[:, 2] >= -0.2) & (body[:, 2] <= 0.4) & (radius <= 12)
    nearest = f"{radius[band].min():.2f} m" if band.any() else "-"
    return (f"{len(body)} points, body z {body[:, 2].min():.2f}..{body[:, 2].max():.2f} m, "
            f"{int(band.sum())} between -0.2 and 0.4 m (nearest {nearest}), robot at "
            + ", ".join(f"{v:.2f}" for v in position))


def spread(values):
    if not values:
        return "-"
    return f"min {min(values):.3f} mean {sum(values) / len(values):.3f} max {max(values):.3f}"


def find_serial(channel):
    serial = os.environ.get("GO2_SERIAL")
    if serial:
        return serial
    pairing = pairing_grpc.WendyUnitreeServiceStub(channel)
    while True:
        try:
            robots = pairing.ListUnitreePairings(pairing_pb.ListUnitreePairingsRequest(), timeout=5).robots
            if len(robots) == 1:
                return robots[0].serial
            log.warning("found %d paired Go2 robots; set GO2_SERIAL", len(robots))
        except grpc.RpcError as e:
            log.warning("agent unavailable: %s", e.details())
        time.sleep(5)


def main():
    logging.basicConfig(level=logging.INFO, format="%(name)s: %(message)s")
    channel = grpc.insecure_channel("unix:" + os.environ["WENDY_AGENT_SOCKET"])
    serial = find_serial(channel)
    log.info("bridging Go2 %s", serial)
    rclpy.init()
    node = Bridge(channel, serial)
    threading.Thread(target=node.run_stream, daemon=True).start()
    try:
        rclpy.spin(node)
    except (KeyboardInterrupt, ExternalShutdownException):
        pass  # normal shutdown; the finally block stops the robot
    except RCLError:
        pass  # a callback raced shutdown; cleanup still runs below
    finally:
        node.command("stop")
        rclpy.try_shutdown()


if __name__ == "__main__":
    main()
