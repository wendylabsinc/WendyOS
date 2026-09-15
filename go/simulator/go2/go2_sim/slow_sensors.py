"""Lidar and robot-camera publication, independent of the fast state worker."""

from array import array
from collections import deque
import math
import time

from builtin_interfaces.msg import Time
from rclpy.node import Node
from rclpy.qos import QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import CameraInfo, Image, LaserScan, PointCloud2, PointField

from .camera import CAMERA_FRAME, CAMERA_HEIGHT, CAMERA_WIDTH, intrinsics
from .lidar import Lidar
from .sensors import LIDAR_POSITION, PhysicsSampler, SCAN_ANGLES, SCAN_COUNT, SCAN_MAX, SCAN_MIN


def timestamp(wall_ns):
    sec, nanosec = divmod(wall_ns, 1_000_000_000)
    return Time(sec=sec, nanosec=nanosec)


class SlowObservations(Node):
    def __init__(self, runtime, samples):
        super().__init__("wendy_go2_sensors")
        self.runtime, self.samples = runtime, samples
        self.image_publish_ms = deque(maxlen=300)
        self.sampler = PhysicsSampler(runtime.sim)
        self.lidar_identity = (runtime.sim.epoch, runtime.sensor_generation)
        self.lidar = Lidar(seed=runtime.seed, dropout=runtime.sensor_settings["lidar_dropout"])
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE)
        self.scan_pub = self.create_publisher(LaserScan, "/scan", qos)
        self.cloud_pub = self.create_publisher(PointCloud2, "/utlidar/cloud", qos)
        self.body_cloud_pub = self.create_publisher(PointCloud2, "/utlidar/cloud_base", qos)
        self.image_pub = self.create_publisher(Image, "/camera/color/image_raw", qos)
        self.info_pub = self.create_publisher(CameraInfo, "/camera/color/camera_info", qos)
        self.scan = LaserScan()
        self.scan.header.frame_id = "lidar_link"
        self.scan.angle_min, self.scan.angle_max = float(SCAN_ANGLES[0]), float(SCAN_ANGLES[-1])
        self.scan.angle_increment = 2 * math.pi / SCAN_COUNT
        self.scan.scan_time, self.scan.time_increment = 0.1, 0.0
        self.scan.range_min, self.scan.range_max = SCAN_MIN, SCAN_MAX
        self.cloud = PointCloud2()
        self.cloud.header.frame_id = "utlidar_lidar"
        self.cloud.height, self.cloud.point_step = 1, 12
        self.cloud.is_bigendian, self.cloud.is_dense = False, True
        self.cloud.fields = [PointField(name=name, offset=index*4, datatype=PointField.FLOAT32, count=1)
                             for index, name in enumerate(("x", "y", "z"))]
        self.body_cloud = PointCloud2()
        self.body_cloud.header.frame_id = "base_link"
        self.body_cloud.height, self.body_cloud.point_step = 1, 12
        self.body_cloud.is_bigendian, self.body_cloud.is_dense = False, True
        self.body_cloud.fields = self.cloud.fields
        self.image = Image()
        self.image.header.frame_id = CAMERA_FRAME
        self.image.height, self.image.width = CAMERA_HEIGHT, CAMERA_WIDTH
        self.image.encoding, self.image.is_bigendian = "rgb8", False
        self.image.step = CAMERA_WIDTH * 3
        self.info = CameraInfo()
        self.info.header.frame_id = CAMERA_FRAME
        self.info.height, self.info.width = CAMERA_HEIGHT, CAMERA_WIDTH
        self.info.distortion_model, self.info.d = "plumb_bob", [0.0] * 5
        fx, fy, cx, cy = intrinsics()
        self.info.k = [fx, 0., cx, 0., fy, cy, 0., 0., 1.]
        self.info.r = [1., 0., 0., 0., 1., 0., 0., 0., 1.]
        self.info.p = [fx, 0., cx, 0., 0., fy, cy, 0., 0., 0., 1., 0.]
        self.last_lidar = self.last_camera = None
        self.next_lidar = 0.0
        self.samples.update({"cloud": 0, "camera": 0})
        # Poll immutable camera exposures at 60 Hz; each is emitted once. Lidar
        # keeps its own 10 Hz simulation-time grid and never blocks fast state.
        self.timer = self.create_timer(1 / 60, self.sample)

    def current(self, epoch, generation):
        return (self.runtime.sim.epoch == epoch and
                getattr(self.runtime, "observation_generation", 0) == generation and
                self.runtime.sim.mode not in {"paused", "fault"})

    def sample(self):
        self.publish_camera()
        with self.runtime.observation_lock:
            with self.runtime.lock:
                identity = (self.runtime.sim.epoch, float(self.runtime.sim.data.time))
                generation = getattr(self.runtime, "observation_generation", 0)
                settings = self.runtime.sensor_settings
                lidar_identity = (identity[0], self.runtime.sensor_generation)
                if lidar_identity != self.lidar_identity:
                    # A reset or new scenario restarts the declared seeded
                    # dropout stream. Scan and cloud share that single mask.
                    self.lidar = Lidar(seed=self.runtime.seed, dropout=settings["lidar_dropout"])
                    self.lidar_identity = lidar_identity
                    self.last_lidar = None
                    self.next_lidar = 0.0
                if (self.runtime.sim.mode in {"paused", "fault"} or
                        not settings["lidar_enabled"] or
                        identity == self.last_lidar or
                        (self.last_lidar is not None and identity[0] == self.last_lidar[0] and
                         identity[1] + 1e-9 < self.next_lidar)):
                    return
            if not self.sampler.capture(self.runtime):
                return
        # Rays run without either lifecycle or physics lock. Validate the
        # captured generation again before making the result observable.
        result = self.lidar.sample(self.sampler)
        browser_lidar = getattr(self.runtime, "browser_lidar", None)
        browser_record = browser_lidar.prepare(result, self.sampler, generation) if browser_lidar else None
        scan, cloud = self.scan, self.cloud
        scan.header.stamp = cloud.header.stamp = timestamp(result["wall_timestamp_ns"])
        scan.ranges = result["ranges"].tolist()
        cloud.width = len(result["xyz"])
        cloud.row_step = cloud.width * cloud.point_step
        # Generated ROS setters accept typed arrays without walking every byte
        # in Python. A bytes assignment would validate millions of ints/s and
        # hold the GIL long enough to delay physics and fast native state.
        cloud.data = array("B", result["xyz"].astype("<f4", copy=False).tobytes())
        body_cloud = self.body_cloud
        body_cloud.header.stamp = cloud.header.stamp
        body_cloud.width, body_cloud.row_step = cloud.width, cloud.row_step
        # The virtual lidar has identity rotation relative to the body.
        body_cloud.data = array("B", (result["xyz"] + LIDAR_POSITION).astype("<f4").tobytes())
        with self.runtime.observation_lock:
            with self.runtime.lock:
                if (not self.current(result["epoch"], generation)
                        or not self.runtime.sensor_settings["lidar_enabled"]):
                    return
            self.scan_pub.publish(scan)
            self.cloud_pub.publish(cloud)
            self.body_cloud_pub.publish(body_cloud)
            if browser_lidar:
                browser_lidar.publish(browser_record)
            self.samples["scan"] += 1
            self.samples["cloud"] += 1
            self.last_lidar = (result["epoch"], result["time"])
            self.next_lidar = (math.floor((result["time"] + 1e-9) / 0.1) + 1) * 0.1

    def publish_camera(self):
        with self.runtime.observation_lock, self.runtime.lock:
            frame = getattr(self.runtime, "camera_frame", None)
            if frame is None or not self.runtime.sensor_settings["camera_enabled"]:
                return
            identity = (frame.epoch, frame.generation, frame.time)
            if identity == self.last_camera or not self.current(frame.epoch, frame.generation):
                return
        self.image.header.stamp = self.info.header.stamp = timestamp(frame.wall_timestamp_ns)
        self.image.data = array("B", frame.rgb.tobytes())
        with self.runtime.observation_lock:
            with self.runtime.lock:
                if (not self.current(frame.epoch, frame.generation)
                        or not self.runtime.sensor_settings["camera_enabled"]):
                    return
            publish_started = time.perf_counter()
            self.image_pub.publish(self.image)
            self.image_publish_ms.append((time.perf_counter() - publish_started) * 1000)
            self.info_pub.publish(self.info)
            self.samples["camera"] += 1
            self.last_camera = identity
