"""Additional real DDS load and an independent pinned-SDK CRC oracle.

No simulator module or SDK transport is imported. The optional soak image carries
only the generated Unitree ROS package and the SDK's original CRC source.
"""

import ast
from collections import deque
import hashlib
import math
from pathlib import Path
import struct


SDK_CRC_SHA256 = "b95a530423f72c5acc96811f75677699cb3a185b6327353e7c44b355a425a20e"


def sdk_crc_oracle(path=Path("/app/pinned_sdk_crc.py")):
    source = path.read_bytes()
    if hashlib.sha256(source).hexdigest() != SDK_CRC_SHA256:
        raise ValueError("the independent SDK CRC source does not match the pinned Unitree commit")
    tree = ast.parse(source)
    sdk = next(item for item in tree.body if isinstance(item, ast.ClassDef) and item.name == "CRC")
    methods = [item for item in sdk.body if isinstance(item, ast.FunctionDef)
               and item.name in {"__PackLowState", "__Trans", "_crc_py"}]
    for method in methods:
        method.returns = None
        for argument in method.args.args:
            argument.annotation = None
    constructor = next(item for item in sdk.body if isinstance(item, ast.FunctionDef)
                       and item.name == "__init__")
    format_expression = next(item.value for item in constructor.body if isinstance(item, ast.Assign)
                             and any(isinstance(target, ast.Attribute) and target.attr == "__packFmtLowState"
                                     for target in item.targets))
    pure = ast.ClassDef(name="CRC", bases=[], keywords=[], body=methods, decorator_list=[])
    namespace = {"struct": struct}
    exec(compile(ast.fix_missing_locations(ast.Module(body=[pure], type_ignores=[])), str(path), "exec"), namespace)
    oracle = namespace["CRC"]()
    oracle._CRC__packFmtLowState = eval(compile(ast.Expression(format_expression), str(path), "eval"), {})
    if struct.calcsize(oracle._CRC__packFmtLowState) != 1180:
        raise ValueError("unexpected pinned SDK LowState record layout")
    return lambda message: oracle._crc_py(oracle._CRC__PackLowState(message))


def check(condition, message):
    if not condition:
        raise ValueError(message)


class FullSensorChecks:
    """Called by the probe's single executor, while its state lock is held."""

    def __init__(self, oracle=None):
        self.crc = oracle or sdk_crc_oracle()
        self.errors = []
        self.low_count = self.crc_checked = 0
        self.last_tick = None
        self.image_hashes = deque(maxlen=120)
        self.images = {}
        self.camera_info = {}
        self.scans = {}
        self.clouds = {}

    @staticmethod
    def remember(values, stamp, message):
        values[stamp] = message
        if len(values) > 30:
            values.pop(next(iter(values)))

    def observe(self, name, message, stamp):
        try:
            if name == "lowstate":
                self.low_count += 1
                self.last_tick = int(message.tick)
                # Receive every 500 Hz message, independently verify every
                # tenth (~50 Hz) with the SDK's original bitwise algorithm.
                if (self.low_count - 1) % 10 == 0:
                    check(list(message.head) == [0xFE, 0xEF] and message.level_flag == 0xFF,
                          "invalid native LowState header")
                    check(len(message.motor_state) == 20, "native LowState must contain twenty slots")
                    check(message.crc == self.crc(message), "native LowState failed the pinned SDK CRC")
                    check(all(math.isfinite(value) for motor in message.motor_state[:12]
                              for value in (motor.q, motor.dq, motor.ddq, motor.tau_est)),
                          "native joint observation is nonfinite")
                    self.crc_checked += 1
            elif name == "image":
                check(message.header.frame_id == "camera_optical_frame", "camera image frame mismatch")
                check(message.width == 640 and message.height == 360 and message.encoding == "rgb8"
                      and message.step == 640 * 3 and len(message.data) == 640 * 360 * 3,
                      "camera image layout or payload is incomplete")
                digest = hashlib.sha256(message.data).hexdigest()
                self.image_hashes.append(digest)
                self.remember(self.images, stamp, (message.width, message.height))
            elif name == "camera_info":
                check(message.header.frame_id == "camera_optical_frame", "camera calibration frame mismatch")
                check(message.width == 640 and message.height == 360 and
                      all(math.isfinite(value) for value in [*message.k, *message.p, *message.d])
                      and message.k[0] > 0 and message.k[4] > 0 and message.k[8] == 1,
                      "invalid camera calibration")
                self.remember(self.camera_info, stamp, (message.width, message.height))
            elif name == "cloud":
                check(message.header.frame_id == "utlidar_lidar", "point cloud frame mismatch")
                check(not message.is_bigendian and message.height == 1 and message.point_step == 12
                      and message.row_step == message.width * 12
                      and len(message.data) == message.row_step
                      and [(field.name, field.offset, field.datatype, field.count) for field in message.fields]
                      == [("x", 0, 7, 1), ("y", 4, 7, 1), ("z", 8, 7, 1)],
                      "point cloud layout or payload is invalid")
                points = list(struct.iter_unpack("<fff", message.data))
                check(points and all(math.isfinite(value) for point in points for value in point),
                      "point cloud is empty or contains nonfinite returns")
                self.remember(self.clouds, stamp, points)
            elif name == "scan":
                self.remember(self.scans, stamp, message)
        except (AssertionError, ValueError, TypeError, OverflowError, struct.error) as error:
            if len(self.errors) < 20:
                self.errors.append(f"{name}: {error}")

    def coherent_pairs(self):
        image_stamps = set(self.images) & set(self.camera_info)
        cloud_stamps = set(self.scans) & set(self.clouds)
        if not image_stamps or not cloud_stamps:
            return None
        image_stamp, cloud_stamp = max(image_stamps), max(cloud_stamps)
        check(self.images[image_stamp] == self.camera_info[image_stamp], "image/calibration dimensions disagree")
        scan, points = self.scans[cloud_stamp], self.clouds[cloud_stamp]
        horizontal = [point for point in points if abs(point[2]) < 1e-7]
        finite = [(index, distance) for index, distance in enumerate(scan.ranges) if math.isfinite(distance)]
        check(len(horizontal) == len(finite), "cloud horizontal ring and scan have different return counts")
        for (index, distance), point in zip(finite, horizontal):
            angle = scan.angle_min + scan.angle_increment * index
            check(abs(math.hypot(point[0], point[1]) - distance) < 1e-4 and
                  abs(math.atan2(math.sin(math.atan2(point[1], point[0]) - angle),
                                 math.cos(math.atan2(point[1], point[0]) - angle))) < 1e-5,
                  "cloud horizontal ray disagrees with the same-stamp scan")
        return {"image_stamp_ns": image_stamp, "cloud_stamp_ns": cloud_stamp,
                "cloud_points": len(points), "horizontal_returns": len(horizontal)}

    def status(self):
        return {"errors": self.errors.copy(), "crc_checked": self.crc_checked,
                "lowstate_received": self.low_count, "latest_tick_ms": self.last_tick,
                "distinct_recent_image_hashes": len(set(self.image_hashes)),
                "crc_sampling": "every tenth received LowState, pinned SDK packer and bitwise CRC"}
