"""ROS adapter for calibrated target grounding and matching camera observations.

Only capture-time transforms are used. Image/calibration frames and dimensions
must agree; no default calibration, latest-TF fallback, monocular position, or
implicit depth registration is performed. Rectified/aligned topics are a local
deployment contract, not something that can be established from their names.
"""

from __future__ import annotations

from array import array
from collections import deque
import json
import math
import sys
import threading

from .grounding import DepthImage, Detection, GroundingError, Intrinsics, RigidTransform, RobotPose


def _bounded_records(records: list[dict], count: int, max_bytes: int) -> list[dict]:
    result = []
    for record in records[:count]:
        candidate = result+[record]
        if len(json.dumps(candidate, allow_nan=False).encode("utf-8")) > max_bytes:
            break
        result = candidate
    return result


def _visible_targets(targets: list[dict]) -> list[dict]:
    # Leave room for metadata, JSON escaping and a base64-encoded 40 KiB JPEG
    # within Wendy's 100,000-byte proxied tool result cap.
    return _bounded_records(targets, 16, 16000)


def stamp_seconds(header) -> float:
    sec, nano = header.stamp.sec, header.stamp.nanosec
    if not isinstance(sec, int) or not isinstance(nano, int) or sec < 0 or not 0 <= nano < 1_000_000_000:
        raise GroundingError("invalid_time", "invalid ROS source timestamp")
    return sec + nano*1e-9


def _image_dimensions(message, bytes_per_pixel: int) -> None:
    width, height, step = message.width, message.height, message.step
    if (not all(isinstance(v, int) for v in (width, height, step))
            or not 1 <= width <= 8192 or not 1 <= height <= 8192
            or width*height > 16_777_216 or step < width*bytes_per_pixel
            or step*height > 128*1024*1024 or len(message.data) != step*height):
        raise GroundingError("dimension_mismatch", "malformed or oversized image dimensions/stride")


def decode_depth(message) -> DepthImage:
    """Decode padded/endian ROS 16UC1 millimetres or 32FC1 metres."""
    if message.encoding not in ("16UC1", "32FC1"):
        raise GroundingError("unsupported_depth", "depth encoding must be 16UC1 millimetres or 32FC1 metres")
    code, itemsize = ("H", 2) if message.encoding == "16UC1" else ("f", 4)
    _image_dimensions(message, itemsize)
    raw = array(code)
    data = memoryview(message.data)
    for row in range(message.height):
        start = row*message.step
        raw.frombytes(data[start:start+message.width*itemsize])
    if bool(message.is_bigendian) != (sys.byteorder == "big"):
        raw.byteswap()
    values = array("f", (value*0.001 for value in raw)) if code == "H" else raw
    return DepthImage(message.width, message.height, values, message.header.frame_id,
                      stamp_seconds(message.header), aligned=True)


def camera_intrinsics(message) -> Intrinsics:
    """Use P for rectified pixels; reject unsupported cropping or rectification."""
    p, r = message.p, message.r
    if (len(p) != 12 or len(r) != 9 or not all(math.isfinite(v) for v in (*p, *r))
            or p[0] <= 0 or p[5] <= 0):
        raise GroundingError("invalid_intrinsics", "CameraInfo requires finite calibrated projection and rectification matrices")
    # A nonidentity rectification rotation changes the optical ray frame.
    # Supporting it requires an explicit rectified-frame transform, not silently
    # using the raw optical TF. Stereo projection offsets are also not ignored.
    expected_r = (1,0,0,0,1,0,0,0,1)
    if (any(abs(a-b) > 1e-5 for a, b in zip(r, expected_r))
            or any(abs(p[i]) > 1e-5 for i in (1,3,4,7,8,9,11)) or abs(p[10]-1) > 1e-5):
        raise GroundingError("unsupported_calibration", "provide an aligned rectified optical frame with identity R and zero stereo projection offset")
    roi = message.roi
    if (message.binning_x not in (0,1) or message.binning_y not in (0,1)
            or roi.x_offset != 0 or roi.y_offset != 0
            or roi.width not in (0,message.width) or roi.height not in (0,message.height)):
        raise GroundingError("unsupported_calibration", "cropped/binned calibration must first be expressed for the delivered pixels")
    return Intrinsics(message.width, message.height, p[0], p[5], p[2], p[6], message.header.frame_id, rectified=True)


def detections_from_message(message, max_sync_delta: float, max_detections: int = 256) -> list[Detection]:
    if len(message.detections) > max_detections:
        raise GroundingError("too_many_detections", "detection frame exceeds configured capacity")
    source_stamp = stamp_seconds(message.header)
    items = []
    for item in message.detections:
        if (item.header.frame_id != message.header.frame_id
                or abs(stamp_seconds(item.header)-source_stamp) > max_sync_delta):
            raise GroundingError("frame_mismatch", "individual detection headers must match the source image header")
        scores = [(hyp.hypothesis.class_id, float(hyp.hypothesis.score)) for hyp in item.results]
        if not scores or any(not math.isfinite(score) or not 0 <= score <= 1 for _, score in scores):
            raise GroundingError("invalid_detection", "detector class scores must be finite values in 0..1")
        scores.sort(key=lambda entry: entry[1], reverse=True)
        label, score = scores[0]
        if label != "person":
            continue
        if any(other_label != label and other_score == score for other_label, other_score in scores[1:]):
            raise GroundingError("ambiguous_detection", "top class hypotheses disagree")
        center = item.bbox.center
        position = center.position if hasattr(center, "position") else center
        x, y, width, height, theta = position.x, position.y, item.bbox.size_x, item.bbox.size_y, center.theta
        if not all(math.isfinite(v) for v in (x,y,width,height,theta)) or abs(theta) > 1e-5:
            raise GroundingError("invalid_bbox", "grounding requires a finite axis-aligned bounding box")
        items.append(Detection(item.id, label, score, (x-width/2, y-height/2, x+width/2, y+height/2)))
    return items


class Perception:
    def __init__(self, node, settings: dict, registry):
        from rclpy.duration import Duration
        from rclpy.qos import DurabilityPolicy, HistoryPolicy, QoSProfile, ReliabilityPolicy
        from rclpy.time import Time
        from sensor_msgs.msg import CameraInfo, Image
        from tf2_ros import Buffer, TransformException, TransformListener
        from vision_msgs.msg import Detection2DArray

        self.node, self.registry = node, registry
        self.nav_frame = settings["runtime"]["navigation_frame"]
        if registry.config.nav_frame != self.nav_frame:
            raise ValueError("target registry and runtime navigation frames must match")
        self._lock = threading.RLock()
        self._cache = {kind: deque(maxlen=16) for kind in ("rgb", "depth", "camera_info")}
        self._pending = deque(maxlen=4)
        self._last_stamp = -math.inf
        self._frame = None
        self._jpeg = None
        self._image_metadata = None
        self._image_error = ""
        self._reason = "waiting for synchronized RGB, aligned depth, calibration and detections"
        self._bridge = None
        self._Time, self._TransformException = Time, TransformException
        self._tf = Buffer(cache_time=Duration(seconds=3.0))
        self._listener = TransformListener(self._tf, node)
        qos = QoSProfile(history=HistoryPolicy.KEEP_LAST, depth=1,
                         reliability=ReliabilityPolicy.BEST_EFFORT,
                         durability=DurabilityPolicy.VOLATILE)
        kinds = (("rgb", Image), ("depth", Image), ("camera_info", CameraInfo), ("detections", Detection2DArray))
        self._subscriptions = [node.create_subscription(message_type, settings["topics"][kind],
                              lambda message, kind=kind: self._receive(kind, message), qos)
                              for kind, message_type in kinds]
        self._timer = node.create_timer(0.1, self._tick)

    def _now(self) -> float:
        return self.node.get_clock().now().nanoseconds*1e-9

    def _invalidate(self, reason: str) -> None:
        self.registry.invalidate_all(reason)
        self._reason = reason
        self._frame = None
        self._jpeg = None
        self._image_metadata = None

    def _receive(self, kind: str, message) -> None:
        with self._lock:
            try:
                stamp = stamp_seconds(message.header)
                if not message.header.frame_id or len(message.header.frame_id) > 256:
                    raise GroundingError("frame_mismatch", "source optical frame must contain 1..256 characters")
                if kind == "rgb":
                    channels = {"rgb8": 3, "bgr8": 3, "mono8": 1}.get(message.encoding)
                    if channels is None:
                        raise GroundingError("unsupported_rgb", "RGB encoding must be rgb8, bgr8 or mono8")
                    _image_dimensions(message, channels)
                elif kind == "depth":
                    size = {"16UC1": 2, "32FC1": 4}.get(message.encoding)
                    if size is None:
                        raise GroundingError("unsupported_depth", "depth encoding must be 16UC1 or 32FC1")
                    _image_dimensions(message, size)
                elif kind == "camera_info":
                    camera_intrinsics(message)
                if kind == "detections":
                    # Explicit disappearance invalidates selected targets even
                    # while the synchronizer is waiting for another source.
                    if stamp > self._last_stamp:
                        if not message.detections:
                            self._invalidate("no people in the current detection frame")
                        self._pending.append(message)
                else:
                    self._cache[kind].append(message)
                self._process_pending()
            except (GroundingError, ValueError, TypeError, AttributeError, OverflowError) as error:
                if kind in self._cache:
                    self._cache[kind].clear()
                self._invalidate(str(error))

    def _match(self, kind: str, stamp: float):
        items = self._cache[kind]
        if not items:
            return None
        closest = min(items, key=lambda message: abs(stamp_seconds(message.header)-stamp))
        tolerance = 1e-6 if kind == "rgb" else self.registry.config.max_sync_delta
        return closest if abs(stamp_seconds(closest.header)-stamp) <= tolerance else None

    def _process_pending(self) -> None:
        now = self._now()
        cfg = self.registry.config
        for detection_message in sorted(self._pending, key=lambda message: stamp_seconds(message.header), reverse=True):
            stamp = stamp_seconds(detection_message.header)
            if stamp <= self._last_stamp:
                continue
            if now-stamp >= min(cfg.target_ttl, cfg.max_observation_age) or stamp-now > cfg.max_future_skew:
                self._invalidate("detection capture time is stale or unsynchronized")
                self._pending.remove(detection_message)
                continue
            rgb, depth, info = (self._match(kind, stamp) for kind in ("rgb", "depth", "camera_info"))
            if any(message is None for message in (rgb, depth, info)):
                self._reason = "waiting for capture-time RGB, aligned depth and CameraInfo"
                continue
            frame_id = detection_message.header.frame_id
            if any(message.header.frame_id != frame_id for message in (rgb, depth, info)):
                raise GroundingError("frame_mismatch", "RGB, aligned depth, CameraInfo and detections must share one optical frame")
            if any((message.width,message.height) != (rgb.width,rgb.height) for message in (depth,info)):
                raise GroundingError("dimension_mismatch", "RGB, depth and CameraInfo image dimensions differ")
            capture_time = self._Time.from_msg(detection_message.header.stamp)
            try:
                tf = self._tf.lookup_transform(self.nav_frame, frame_id, capture_time)
            except self._TransformException:
                self._reason = "waiting for transform at detection capture time"
                continue
            translation, rotation = tf.transform.translation, tf.transform.rotation
            transform = RigidTransform.from_quaternion(frame_id, self.nav_frame, stamp,
                (translation.x,translation.y,translation.z), (rotation.x,rotation.y,rotation.z,rotation.w))
            detections = detections_from_message(detection_message, cfg.max_sync_delta, cfg.max_targets)
            targets = self.registry.update(detections, decode_depth(depth), camera_intrinsics(info), transform,
                                           stamp=stamp, frame_id=frame_id, now=self._now())
            self._last_stamp = stamp
            self._frame = {"source_stamp": stamp, "source_frame": frame_id,
                           "rgb_stamp": stamp_seconds(rgb.header), "depth_stamp": stamp_seconds(depth.header),
                           "calibration_stamp": stamp_seconds(info.header), "expires_at": stamp+cfg.target_ttl}
            self._pending = deque((message for message in self._pending if stamp_seconds(message.header) > stamp), maxlen=4)
            self._reason = "" if targets else "no valid grounded person targets"
            self._jpeg, self._image_metadata = self._make_jpeg(rgb, targets)
            break

    def _make_jpeg(self, rgb, targets) -> tuple[bytes | None, dict | None]:
        try:
            import cv2
            from cv_bridge import CvBridge
            if self._bridge is None:
                self._bridge = CvBridge()
            image = self._bridge.imgmsg_to_cv2(rgb, desired_encoding="bgr8")
            targets = _visible_targets(targets)
            labels = {str(i+1): target["target_id"] for i, target in enumerate(targets)}
            for width in (640, 448, 320):
                factor = min(1.0, width/rgb.width, 480/rgb.height)
                rw, rh = max(1,round(rgb.width*factor)), max(1,round(rgb.height*factor))
                annotated = cv2.resize(image, (rw,rh))
                for i, target in enumerate(targets):
                    x0,y0,x1,y1 = (round(value*factor) for value in target["bbox"])
                    cv2.rectangle(annotated, (x0,y0), (x1,y1), (0,230,100), 2)
                    cv2.putText(annotated, str(i+1), (x0,max(14,y0-4)), cv2.FONT_HERSHEY_SIMPLEX, 0.5, (0,230,100), 1)
                for quality in (70,50,35,20):
                    ok, encoded = cv2.imencode(".jpg", annotated, [cv2.IMWRITE_JPEG_QUALITY, quality])
                    if ok and len(encoded) <= 40*1024:
                        self._image_error = ""
                        return encoded.tobytes(), {"mime_type": "image/jpeg", "width": rw, "height": rh,
                            "source_stamp": stamp_seconds(rgb.header), "source_frame": rgb.header.frame_id,
                            "labels": labels, "bytes": len(encoded)}
            self._image_error = "annotated image could not fit the 40 KiB budget"
        except Exception as error:
            # A JPEG codec failure does not turn validated sensor readings into
            # invented image data; return the explicit missing-image reason.
            self._image_error = f"camera JPEG unavailable: {type(error).__name__}"
        return None, None

    def _tick(self) -> None:
        with self._lock:
            try:
                self._process_pending()
                self.registry.observed_targets(self._now())
            except (GroundingError, ValueError, TypeError, AttributeError, OverflowError) as error:
                self._invalidate(str(error))

    def robot_targets(self) -> dict:
        with self._lock:
            now = self._now()
            targets = self.registry.observed_targets(now)
            visible = _visible_targets(targets)
            rejections = _bounded_records(list(self.registry.last_rejections), 8, 4000)
            fresh = self._frame is not None and 0 <= now-self._frame["source_stamp"] < min(self.registry.config.target_ttl, self.registry.config.max_observation_age)
            return {"status": "observed" if targets else "unknown", "targets": visible,
                    "total_targets": len(targets), "targets_truncated": len(visible) != len(targets),
                    "source_stamp": self._frame["source_stamp"] if self._frame else None,
                    "source_freshness": "fresh" if fresh else "unknown",
                    "reason": self._reason if fresh else "no current synchronized camera observation",
                    "rejections": rejections, "rejections_truncated": len(rejections) != len(self.registry.last_rejections),
                    "mirror_exclusion": "unverified"}

    def observation(self) -> tuple[dict, bytes | None]:
        with self._lock:
            result = self.robot_targets()
            current = result["source_freshness"] == "fresh"
            result["capture"] = dict(self._frame) if self._frame and current else None
            result["image"] = dict(self._image_metadata) if self._image_metadata and current else None
            result["image_error"] = self._image_error if current else "no current synchronized camera image"
            return result, self._jpeg if current else None

    def choose_goal(self, target_id: str, robot_pose: RobotPose, standoff: float | None = None) -> dict:
        return self.registry.choose_goal(target_id, robot_pose, self._now(), standoff)
