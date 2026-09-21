"""Bounded, thread-safe telemetry independent of ROS imports."""

from collections import deque
from copy import deepcopy
from io import BytesIO
import threading
import time

from PIL import Image


TOPICS = {
    "joints": "/joint_states",
    "imu": "/imu/data",
    "odom": "/odom",
    "camera": "/camera/color/image_raw",
    "camera_info": "/camera/color/camera_info",
    "scan": "/scan",
    "epoch": "/simulation/epoch",
}
CORE = ("joints", "imu", "odom", "camera", "camera_info")
MAX_AGE = 2.0


def camera_jpeg(frame):
    width, height, encoding, step, data = frame
    if encoding not in ("rgb8", "bgr8") or not (0 < width <= 4096 and 0 < height <= 4096):
        raise ValueError("Expected rgb8/bgr8 image up to 4096 x 4096")
    if step < width * 3 or len(data) != step * height:
        raise ValueError("Invalid camera stride or payload length")
    image = Image.frombytes("RGB", (width, height), data, "raw",
                            "RGB" if encoding == "rgb8" else "BGR", step, 1)
    output = BytesIO()
    image.save(output, format="JPEG", quality=85)
    return output.getvalue()


class Observations:
    def __init__(self, clock=time.monotonic, wall_clock=time.time):
        self.clock, self.wall_clock = clock, wall_clock
        self.lock = threading.Lock()
        self.started = clock()
        self.streams = {key: {"count": 0, "arrivals": deque(maxlen=1000),
                              "received": None, "stamp": None, "value": None,
                              "error": None} for key in TOPICS}
        self.frame = None
        self.simulator = {}
        self.simulator_received = None

    def record(self, key, value, stamp=None, frame=None):
        with self.lock:
            stream = self.streams[key]
            now = self.clock()
            stream["count"] += 1
            stream["arrivals"].append(now)
            stream.update(received=now, stamp=stamp, value=value, error=None)
            if frame is not None:
                self.frame = frame

    def failure(self, key, error):
        with self.lock:
            self.streams[key]["error"] = str(error)

    def set_simulator(self, status):
        with self.lock:
            self.simulator = status
            self.simulator_received = self.clock()

    def snapshot(self):
        with self.lock:
            now, wall = self.clock(), self.wall_clock()
            topics = {}
            for key, stream in self.streams.items():
                arrivals = [t for t in stream["arrivals"] if now - t <= 3]
                hz = ((len(arrivals) - 1) / (arrivals[-1] - arrivals[0])
                      if len(arrivals) > 1 and arrivals[-1] > arrivals[0] else 0)
                age = None if stream["received"] is None else now - stream["received"]
                source_age = None if stream["stamp"] is None else wall - stream["stamp"]
                fresh = (age is not None and age < MAX_AGE and not stream["error"]
                         and (key == "epoch" or
                              (source_age is not None and -0.5 <= source_age < MAX_AGE)))
                topics[key] = {"topic": TOPICS[key], "count": stream["count"],
                               "hz": round(hz, 2), "age_seconds": age,
                               "source_age_seconds": source_age, "fresh": fresh,
                               "value": deepcopy(stream["value"]), "error": stream["error"]}
            sim = deepcopy(self.simulator)
            sim_fresh = (self.simulator_received is not None
                         and now - self.simulator_received < MAX_AGE)
            simulator_ok = (sim_fresh and sim.get("simulation") is True
                            and sim.get("robot_kind") == "g1" and sim.get("healthy") is True)
            ready = simulator_ok and all(topics[key]["fresh"] for key in CORE)
            joints = topics["joints"]["value"] or {}
            return {"app": "sh.wendy.examples.g1-training", "version": "0.2.0",
                    "ready": ready, "uptime_seconds": now - self.started,
                    "simulator": sim, "simulator_fresh": sim_fresh,
                    "ros_domain_id": 0, "topics": topics,
                    "policy": {"running": False, "compatible": False,
                               "observed_joint_count": len(joints.get("names", [])),
                               "required_joint_count": 43,
                               "reason": "The grasp policy requires the matching 43-joint Dex3 model, "
                                         "RGB-D, can masks, reference bank and checkpoint. "
                                         "This app receives simulator observations; policy inference "
                                         "and training are not running in this container."}}

    def jpeg(self):
        with self.lock:
            stream = self.streams["camera"]
            if (self.frame is None or stream["error"] or stream["received"] is None
                    or self.clock() - stream["received"] >= MAX_AGE
                    or stream["stamp"] is None
                    or not -0.5 <= self.wall_clock() - stream["stamp"] < MAX_AGE):
                return None
            frame = self.frame
        return camera_jpeg(frame)
