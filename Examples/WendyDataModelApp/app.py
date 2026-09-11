#!/usr/bin/env python3
"""Reference model app for the Wendy model harness.

Demonstrates the three harness contracts end to end:

  1. Frames in     — camera frames FED BY THE HARNESS on a v4l2loopback node
                     granted by the camera entitlement. The app never opens
                     the physical device: it reads the same producer the
                     episode capture adapter consumes, so the two never fight
                     over the camera, and every frame the model sees is
                     recorded into the active episode under the identity that
                     arrives with it in the buffer timestamp.
  2. Predictions out — one "prediction" record per processed frame, naming
                     the sample identifiers it was computed from, plus a
                     "person_detected" event, over the app-private data
                     socket granted by the episode-write entitlement.
  3. Actuation out — a Robot Operating System 2 (ROS 2) Twist command on
                     /cmd_vel when ROS 2 is available and enabled; the
                     same decision is logged when it is not.

The detector is YOLOv8n exported to Open Neural Network Exchange (ONNX)
at image build time and run with onnxruntime on the Central Processing
Unit (CPU) by default, so the app runs on any dev box. See README.md for
the Jetson Graphics Processing Unit (GPU) wheel path.
"""

from __future__ import annotations

import logging
import os
import sys
import time

import cv2
import numpy as np
import onnxruntime as ort

import wendydata
import wendyframes

log = logging.getLogger("model-app")

MODEL_NAME = "yolov8n"
# Pinned in the Dockerfile export stage; keep the two in sync.
MODEL_VERSION = os.environ.get("WENDY_MODEL_VERSION", "8.3.63")
MODEL_PATH = os.environ.get("WENDY_MODEL_PATH", os.path.join(os.path.dirname(__file__), "yolov8n.onnx"))
# The harness source to consume, as reported by `wendy data sources`
# (for example "v4l2:/dev/video0" or "ipcamera:200"). Empty means "pick the
# first subscribable camera the harness offers", which is what a
# single-camera demo device wants.
# The agent-fed node. wendy sets WENDY_CAMERA_NODE when it wires the two-plane
# path; the default is the first loopback node it creates.
CAMERA_NODE = os.environ.get("WENDY_CAMERA_NODE", "/dev/video10")
# An agent restart takes the node away while the app is perfectly healthy, so
# that is treated as transient. The budget refills on every frame delivered;
# 0 exits on the first failure.
CAMERA_RECONNECT_ATTEMPTS = int(os.environ.get("WENDY_CAMERA_RECONNECT_ATTEMPTS", "5"))
CAMERA_RECONNECT_DELAY_SECONDS = float(os.environ.get("WENDY_CAMERA_RECONNECT_DELAY_SECONDS", "2"))
CAMERA_SOURCE = os.environ.get("WENDY_CAMERA_SOURCE", "")
CONFIDENCE_THRESHOLD = float(os.environ.get("WENDY_CONFIDENCE_THRESHOLD", "0.25"))
IOU_THRESHOLD = float(os.environ.get("WENDY_IOU_THRESHOLD", "0.45"))
# Rate-limit friendliness: the agent caps an app at 200 records per second
# across its connections; this is the ceiling this app holds itself to
# (default 5 per second). It is a CEILING, not a target: on a CPU-only
# Jetson Orin Nano one YOLOv8n inference takes roughly 450 ms, so the loop
# runs at about 2 predictions per second and this gate never binds. What
# actually keeps the app current is discarding the backlog rather than
# draining it (see freshest_frames in wendyframes.py); lower this only to
# spend less CPU on inference deliberately.
#
# Frames the app receives but does not score are still delivered, so the
# episode's model-input ledger records every one of them, and each
# prediction reports how many it passed over.
PREDICTIONS_PER_SECOND = float(os.environ.get("WENDY_PREDICTIONS_PER_SECOND", "5"))
# The sensor stream can end while the app is still healthy: the agent
# Actuation is optional: a plain Jetson demo has no ROS 2 stack. Set
# WENDY_MODEL_APP_ROS2=1 (and run an image with rclpy) to publish Twists.
ROS2_REQUESTED = os.environ.get("WENDY_MODEL_APP_ROS2", "0") == "1"
ACTUATION_TOPIC = os.environ.get("WENDY_ACTUATION_TOPIC", "/cmd_vel")
INPUT_SIZE = 640
PERSON_CLASS_ID = 0
TOP_DETECTIONS = 5

COCO_CLASSES = [
    "person", "bicycle", "car", "motorcycle", "airplane", "bus", "train", "truck", "boat",
    "traffic light", "fire hydrant", "stop sign", "parking meter", "bench", "bird", "cat",
    "dog", "horse", "sheep", "cow", "elephant", "bear", "zebra", "giraffe", "backpack",
    "umbrella", "handbag", "tie", "suitcase", "frisbee", "skis", "snowboard", "sports ball",
    "kite", "baseball bat", "baseball glove", "skateboard", "surfboard", "tennis racket",
    "bottle", "wine glass", "cup", "fork", "knife", "spoon", "bowl", "banana", "apple",
    "sandwich", "orange", "broccoli", "carrot", "hot dog", "pizza", "donut", "cake", "chair",
    "couch", "potted plant", "bed", "dining table", "toilet", "tv", "laptop", "mouse",
    "remote", "keyboard", "cell phone", "microwave", "oven", "toaster", "sink",
    "refrigerator", "book", "clock", "vase", "scissors", "teddy bear", "hair drier",
    "toothbrush",
]


class Actuator:
    """Publishes Twist commands over ROS 2 when available, or logs the
    decision when not. Both modes exercise the same control logic."""

    def __init__(self):
        self.node = None
        self.publisher = None
        self.twist_type = None
        if not ROS2_REQUESTED:
            log.info("actuation: ROS 2 disabled (set WENDY_MODEL_APP_ROS2=1 to publish %s)", ACTUATION_TOPIC)
            return
        try:
            import rclpy
            from geometry_msgs.msg import Twist
        except ImportError as exc:
            log.warning("actuation: ROS 2 requested but rclpy is unavailable (%s); logging decisions instead", exc)
            return
        rclpy.init()
        self.node = rclpy.create_node("wendy_data_model_app")
        self.publisher = self.node.create_publisher(Twist, ACTUATION_TOPIC, 10)
        self.twist_type = Twist
        log.info("actuation: publishing geometry_msgs/Twist on %s", ACTUATION_TOPIC)

    def act(self, angular_z: float, linear_x: float) -> None:
        if self.publisher is None:
            log.info("actuation decision (not published): linear.x=%.3f angular.z=%.3f", linear_x, angular_z)
            return
        msg = self.twist_type()
        msg.linear.x = float(linear_x)
        msg.angular.z = float(angular_z)
        self.publisher.publish(msg)

    def close(self) -> None:
        if self.node is not None:
            import rclpy

            self.node.destroy_node()
            rclpy.shutdown()


def steer_towards(detections, frame_width: int) -> tuple[float, float]:
    """A demo proportional controller: turn towards the largest detected
    person and creep forward when one is roughly centered. Returns
    (angular_z, linear_x); zeros when no person is in view."""
    people = [d for d in detections if d["class_id"] == PERSON_CLASS_ID]
    if not people or frame_width <= 0:
        return 0.0, 0.0
    largest = max(people, key=lambda d: d["box"][2] * d["box"][3])
    x, _, w, _ = largest["box"]
    center_offset = ((x + w / 2.0) / frame_width) - 0.5  # -0.5 .. 0.5
    angular_z = -1.2 * center_offset
    linear_x = 0.1 if abs(center_offset) < 0.15 else 0.0
    return angular_z, linear_x


def person_appearance(detections, person_present: bool) -> tuple[bool, float | None]:
    """Decide whether this frame is the START of a person's appearance.

    Returns (person_present_now, confidence_to_report). `confidence_to_report`
    is None unless this frame is a rising edge, so the caller emits one
    `person_detected` event per appearance rather than one per frame.

    Edge-triggering is what makes the event usable as a campaign trigger. A
    level-triggered event would fire on every frame a person stays in view,
    and since the campaign starts an episode per trigger, someone walking past
    would produce a stream of episodes instead of the one clip that actually
    covers them. Kept as a pure function so that "one episode per appearance"
    is a tested property rather than an inline claim, and it can be checked
    without a camera, a model, or an agent.
    """
    people = [d for d in detections if d["class_id"] == PERSON_CLASS_ID]
    if not people:
        return False, None
    if person_present:
        # Still there. The episode already covering them is the right one.
        return True, None
    return True, max(d["confidence"] for d in people)


def letterbox(frame: np.ndarray) -> tuple[np.ndarray, float, int, int]:
    """Resize with preserved aspect ratio onto a 640x640 canvas."""
    h, w = frame.shape[:2]
    scale = min(INPUT_SIZE / w, INPUT_SIZE / h)
    nw, nh = int(round(w * scale)), int(round(h * scale))
    resized = cv2.resize(frame, (nw, nh))
    canvas = np.full((INPUT_SIZE, INPUT_SIZE, 3), 114, dtype=np.uint8)
    dx, dy = (INPUT_SIZE - nw) // 2, (INPUT_SIZE - nh) // 2
    canvas[dy : dy + nh, dx : dx + nw] = resized
    return canvas, scale, dx, dy


def detect(session: ort.InferenceSession, input_name: str, frame: np.ndarray) -> list[dict]:
    """Run YOLOv8n and return [{class_id, class_name, confidence, box}]
    with boxes as [x, y, w, h] in original frame pixels."""
    canvas, scale, dx, dy = letterbox(frame)
    blob = canvas[:, :, ::-1].astype(np.float32) / 255.0  # BGR -> RGB
    blob = np.transpose(blob, (2, 0, 1))[np.newaxis, ...]
    (output,) = session.run(None, {input_name: blob})
    # Output shape (1, 84, 8400): 4 box coordinates + 80 class scores.
    predictions = np.squeeze(output, axis=0).T  # (8400, 84)
    scores = predictions[:, 4:]
    class_ids = np.argmax(scores, axis=1)
    confidences = scores[np.arange(len(class_ids)), class_ids]
    keep = confidences >= CONFIDENCE_THRESHOLD
    boxes_cxcywh = predictions[keep, :4]
    class_ids = class_ids[keep]
    confidences = confidences[keep]
    boxes = []
    for cx, cy, bw, bh in boxes_cxcywh:
        x = (cx - bw / 2.0 - dx) / scale
        y = (cy - bh / 2.0 - dy) / scale
        boxes.append([x, y, bw / scale, bh / scale])
    if not boxes:
        return []
    indices = cv2.dnn.NMSBoxes(boxes, confidences.tolist(), CONFIDENCE_THRESHOLD, IOU_THRESHOLD)
    detections = []
    for i in np.array(indices).flatten():
        detections.append(
            {
                "class_id": int(class_ids[i]),
                "class_name": COCO_CLASSES[int(class_ids[i])],
                "confidence": round(float(confidences[i]), 4),
                "box": [round(float(v), 1) for v in boxes[i]],
            }
        )
    detections.sort(key=lambda d: d["confidence"], reverse=True)
    return detections


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(name)s %(levelname)s %(message)s")
    log.info("model %s v%s from %s", MODEL_NAME, MODEL_VERSION, MODEL_PATH)
    session = ort.InferenceSession(MODEL_PATH, providers=ort.get_available_providers())
    input_name = session.get_inputs()[0].name
    log.info("onnxruntime providers: %s", session.get_providers())

    client = wendydata.DataSocketClient()
    log.info("data socket: %s", client.path)
    actuator = Actuator()

    log.info("camera node: %s", CAMERA_NODE)

    interval = 1.0 / max(PREDICTIONS_PER_SECOND, 0.1)
    next_score_at = 0.0
    person_present = False
    skipped = 0
    try:
        # freshest_frames decodes on its own thread and hands over only the most
        # recent frame, so inference always runs on current input rather than
        # draining a backlog. Without it the loop falls behind the producer by
        # however much slower inference is than capture, and the sample
        # identifiers each prediction references drift out of the range the
        # episode recorded, which is what makes the join unresolvable.
        for sensor_frame, discarded in wendyframes.resilient_frames(
            CAMERA_NODE, CAMERA_SOURCE, CAMERA_RECONNECT_ATTEMPTS, CAMERA_RECONNECT_DELAY_SECONDS
        ):
            # Decoded while the previous inference was still running, then
            # dropped on purpose. Added to the same counter the rate gate feeds
            # so one field accounts for every frame that arrived and was not
            # scored, whatever the reason.
            skipped += discarded
            if sensor_frame.dropped_before:
                # The harness produced frames this app never received. Say so
                # rather than leaving a silent gap in the sample identifiers.
                log.warning(
                    "the harness dropped %d frame(s) before %s@%d: the model is not keeping up",
                    sensor_frame.dropped_before,
                    sensor_frame.source_id,
                    sensor_frame.boottime_nanos,
                )
            now = time.monotonic()
            if now < next_score_at:
                # Holds predictions under PREDICTIONS_PER_SECOND. Rarely binds,
                # because inference is slower than the ceiling; when it does,
                # the frame is counted as skipped like any other. It was still
                # delivered, so the episode's ledger holds it either way.
                skipped += 1
                continue
            next_score_at = now + interval
            frame = sensor_frame.image

            detections = detect(session, input_name, frame)
            uncertainty = wendydata.uncertainty_score(
                (d["confidence"] for d in detections), CONFIDENCE_THRESHOLD
            )

            record = wendydata.build_prediction(
                MODEL_NAME,
                MODEL_VERSION,
                uncertainty,
                detections[:TOP_DETECTIONS],
                {
                    "frame_width": frame.shape[1],
                    "frame_height": frame.shape[0],
                    "sensor_boottime_nanos": sensor_frame.boottime_nanos,
                    "frames_skipped_since_last_prediction": skipped,
                },
                inputs=sensor_frame.input_refs(),
            )
            skipped = 0
            client.send(record)

            # Edge-trigger the demo event so a person standing in front of
            # the camera fires the campaign once, not once per frame.
            person_present, confidence = person_appearance(detections, person_present)
            if confidence is not None:
                client.send(wendydata.build_event("person_detected", {"confidence": confidence}))
                log.info("person detected (confidence %.2f)", confidence)

            angular_z, linear_x = steer_towards(detections, frame.shape[1])
            actuator.act(angular_z, linear_x)
        # Reached only once the stream stayed gone for the whole reconnect
        # budget; frames_with_reconnect has already said why.
        log.info("no more frames from %s; shutting down", source_id)
    except KeyboardInterrupt:
        pass
    finally:
        sensors.close()
        client.close()
        actuator.close()


if __name__ == "__main__":
    main()
