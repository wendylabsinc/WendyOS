"""Optional rectified-RGB person detector with conservative spatial tracks.

Run ``python -m robot_navigation.detector`` inside the ROS image. OpenCV's
built-in HOG detector requires no downloaded model weights. It is an example
front end: seated/occluded people, unusual viewpoints and lighting may be missed;
posters and mirrors may be detected. Its sigmoid SVM score is an uncalibrated
ranking score, not a person probability. IoU tracks are spatial continuity only,
not biometric identity. Even unique box matches cannot prove identity.

The navigation process must independently require aligned depth, capture-time
TF, target freshness, local planning and motion readiness. Replace this node
with a validated detector/tracker publishing vision_msgs/Detection2DArray when
the installation needs those stronger perception capabilities.

The ROS/OpenCV imports are lazy so tracking and geometry can be tested without
a ROS installation. RGB_TOPIC must carry a rectified image in the same optical
frame and dimensions as the aligned depth and CameraInfo used by grounding.
"""

from __future__ import annotations

import math
import os
from typing import Sequence
import uuid


BBox = tuple[float, float, float, float]


def _valid_box(box: BBox) -> bool:
    return (len(box) == 4 and all(math.isfinite(v) for v in box)
            and box[0] >= 0 and box[1] >= 0 and box[2] > box[0] and box[3] > box[1])


def intersection_over_union(a: BBox, b: BBox) -> float:
    if not _valid_box(a) or not _valid_box(b):
        raise ValueError("boxes must be finite xyxy pixel bounds")
    intersection = max(0.0, min(a[2], b[2])-max(a[0], b[0])) * max(0.0, min(a[3], b[3])-max(a[1], b[1]))
    union = (a[2]-a[0])*(a[3]-a[1]) + (b[2]-b[0])*(b[3]-b[1]) - intersection
    return intersection / union


class IoUTracker:
    """Preserve a track only for an unambiguous one-to-one overlap.

    There is no nearest-neighbour or largest-overlap tie breaking. Candidate
    ambiguity, overlapping current people, absence, or a capture-time gap
    produce new IDs, forcing the target registry to invalidate old selections.
    """

    def __init__(self, min_iou: float = 0.35, candidate_iou: float = 0.05,
                 ambiguity_iou: float = 0.1, max_gap: float = 0.75,
                 max_detections: int = 64):
        if (not all(math.isfinite(v) for v in (min_iou, candidate_iou, ambiguity_iou, max_gap))
                or not 0 < candidate_iou <= min_iou <= 1
                or not 0 < ambiguity_iou <= 1 or max_gap <= 0
                or not isinstance(max_detections, int) or not 1 <= max_detections <= 256):
            raise ValueError("invalid tracking configuration")
        self.min_iou, self.candidate_iou = min_iou, candidate_iou
        self.ambiguity_iou, self.max_gap = ambiguity_iou, max_gap
        self.max_detections = max_detections
        self._tracks: list[tuple[str, BBox]] = []
        self._stamp: float | None = None

    def reset(self) -> None:
        self._tracks = []
        self._stamp = None

    def update(self, boxes: Sequence[BBox], stamp: float) -> list[str]:
        if not math.isfinite(stamp) or stamp < 0 or len(boxes) > self.max_detections:
            self.reset()
            raise ValueError("invalid capture time or excessive detection count")
        if any(not _valid_box(box) for box in boxes):
            self.reset()
            raise ValueError("invalid detection box")
        if self._stamp is not None and (stamp <= self._stamp or stamp-self._stamp > self.max_gap):
            self.reset()
        ambiguous = {i for i, box in enumerate(boxes) for j, other in enumerate(boxes)
                     if i != j and intersection_over_union(box, other) >= self.ambiguity_iou}
        overlaps = [[intersection_over_union(box, previous) for _, previous in self._tracks] for box in boxes]
        candidates = [[j for j, overlap in enumerate(row) if overlap >= self.candidate_iou] for row in overlaps]
        owners = [[i for i, options in enumerate(candidates) if j in options] for j in range(len(self._tracks))]
        current = []
        for i, box in enumerate(boxes):
            options = candidates[i]
            if (i not in ambiguous and len(options) == 1 and len(owners[options[0]]) == 1
                    and overlaps[i][options[0]] >= self.min_iou):
                track_id = self._tracks[options[0]][0]
            else:
                track_id = "hog-" + uuid.uuid4().hex
            current.append((track_id, tuple(box)))
        self._tracks, self._stamp = current, stamp
        return [track_id for track_id, _ in current]


def resize_dimensions(width: int, height: int, max_width: int = 640,
                      max_height: int = 480) -> tuple[int, int]:
    if (width <= 0 or height <= 0 or width*height > 16_777_216
            or not 64 <= max_width <= 1280 or not 128 <= max_height <= 960):
        raise ValueError("invalid or oversized input image")
    scale = min(1.0, max_width/width, max_height/height)
    return max(1, int(width*scale)), max(1, int(height*scale))


def restore_box(box: tuple[float, float, float, float], original: tuple[int, int],
                resized: tuple[int, int]) -> BBox:
    """Restore an OpenCV xywh rectangle to original-image xyxy coordinates."""
    x, y, width, height = box
    ow, oh = original
    rw, rh = resized
    if min(ow, oh, rw, rh, width, height) <= 0 or not all(math.isfinite(v) for v in box):
        raise ValueError("invalid image or detection dimensions")
    return (max(0.0, x*ow/rw), max(0.0, y*oh/rh),
            min(float(ow), (x+width)*ow/rw), min(float(oh), (y+height)*oh/rh))


def _env_number(name: str, default: float, low: float, high: float) -> float:
    value = float(os.environ.get(name, default))
    if not math.isfinite(value) or not low <= value <= high:
        raise ValueError(f"{name} must be in {low}..{high}")
    return value


def main() -> None:
    # These packages are supplied by the optional ROS/OpenCV container image.
    import cv2
    import numpy as np
    import rclpy
    from rclpy.node import Node
    from rclpy.qos import DurabilityPolicy, HistoryPolicy, QoSProfile, ReliabilityPolicy
    from sensor_msgs.msg import Image
    from vision_msgs.msg import Detection2D, Detection2DArray, ObjectHypothesisWithPose

    class PersonDetector(Node):
        def __init__(self):
            super().__init__("robot_navigation_person_detector")
            self.max_width = int(_env_number("HOG_MAX_WIDTH", 640, 64, 1280))
            self.max_height = int(_env_number("HOG_MAX_HEIGHT", 480, 128, 960))
            self.max_age = _env_number("HOG_MAX_SOURCE_AGE", 0.5, 0.05, 1.0)
            self.hit_threshold = _env_number("HOG_HIT_THRESHOLD", 1.0, 0, 10)
            self.hog = cv2.HOGDescriptor()
            self.hog.setSVMDetector(cv2.HOGDescriptor_getDefaultPeopleDetector())
            self.tracker = IoUTracker()
            self.latest = None
            self.last_stamp = -math.inf
            qos = QoSProfile(history=HistoryPolicy.KEEP_LAST, depth=1,
                             reliability=ReliabilityPolicy.BEST_EFFORT,
                             durability=DurabilityPolicy.VOLATILE)
            self.publisher = self.create_publisher(Detection2DArray,
                os.environ.get("DETECTIONS_TOPIC", "/robot_navigation/people"), qos)
            self.subscription = self.create_subscription(Image,
                os.environ.get("RGB_TOPIC", "/camera/color/image_rect"), self.accept, qos)
            self.timer = self.create_timer(1/_env_number("HOG_MAX_FPS", 5, 1, 10), self.process)
            self.get_logger().warning("HOG is an example detector: seated people/occlusions may be missed; mirrors/posters and identity are unverified. Depth and navigation checks remain required.")

        def accept(self, image):
            self.latest = image  # One pending image; never queue old inference work.

        def process(self):
            message, self.latest = self.latest, None
            if message is None:
                return
            output = Detection2DArray()
            output.header = message.header  # Preserve source capture time and optical frame.
            stamp = message.header.stamp.sec + message.header.stamp.nanosec*1e-9
            now = self.get_clock().now().nanoseconds*1e-9
            if not math.isfinite(stamp) or stamp < 0 or now-stamp > self.max_age or stamp-now > 0.05:
                self.tracker.reset()
                self.publisher.publish(output)
                return
            if stamp <= self.last_stamp:
                return
            self.last_stamp = stamp
            try:
                if not message.header.frame_id:
                    raise ValueError("source optical frame is required")
                width, height = message.width, message.height
                rw, rh = resize_dimensions(width, height, self.max_width, self.max_height)
                channels = {"rgb8": 3, "bgr8": 3, "mono8": 1}.get(message.encoding)
                if channels is None or message.step < width*channels or len(message.data) != message.step*height:
                    raise ValueError("only packed/padded rgb8, bgr8, or mono8 images are supported")
                pixels = np.frombuffer(message.data, dtype=np.uint8).reshape(height, message.step)
                image = pixels[:, :width*channels].reshape(height, width, channels)
                if channels == 1:
                    image = image[:, :, 0]
                elif message.encoding == "rgb8":
                    image = cv2.cvtColor(image, cv2.COLOR_RGB2BGR)
                image = cv2.resize(image, (rw, rh)) if (rw, rh) != (width, height) else image.copy()
                if rw < 64 or rh < 128:
                    raise ValueError("image is smaller than the HOG detector window")
                rectangles, weights = self.hog.detectMultiScale(image, hitThreshold=self.hit_threshold,
                    winStride=(8, 8), padding=(8, 8), scale=1.1, finalThreshold=2.0)
                boxes, scores = [], []
                for rectangle, weight in zip(rectangles, np.asarray(weights).reshape(-1)):
                    weight = float(weight)
                    if math.isfinite(weight) and weight >= self.hit_threshold:
                        boxes.append(restore_box(tuple(float(v) for v in rectangle), (width, height), (rw, rh)))
                        scores.append(1/(1+math.exp(-min(40.0, max(-40.0, weight)))))
                # Slow inference cannot turn old frames into current targets.
                if self.get_clock().now().nanoseconds*1e-9-stamp > self.max_age:
                    raise ValueError("source image expired during inference")
                ids = self.tracker.update(boxes, stamp)
                for box, score, track_id in zip(boxes, scores, ids):
                    detection = Detection2D()
                    detection.header = message.header
                    detection.id = track_id
                    x0, y0, x1, y1 = box
                    center = detection.bbox.center
                    position = center.position if hasattr(center, "position") else center
                    position.x, position.y = (x0+x1)/2, (y0+y1)/2
                    detection.bbox.size_x, detection.bbox.size_y = x1-x0, y1-y0
                    hypothesis = ObjectHypothesisWithPose()
                    hypothesis.hypothesis.class_id = "person"
                    hypothesis.hypothesis.score = float(score)
                    detection.results = [hypothesis]
                    output.detections.append(detection)
            except (ValueError, TypeError, cv2.error) as error:
                self.tracker.reset()
                output.detections = []
                self.get_logger().warning(f"Person frame rejected: {error}")
            self.publisher.publish(output)

    rclpy.init()
    node = None
    try:
        node = PersonDetector()
        rclpy.spin(node)
    finally:
        if node is not None:
            node.destroy_node()
        rclpy.shutdown()


if __name__ == "__main__":
    main()
