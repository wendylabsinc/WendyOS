"""MuJoCo front camera exposures shared by ROS and the browser sensor preview."""

from dataclasses import dataclass
import math
import time

import numpy as np

CAMERA_WIDTH = 640
CAMERA_HEIGHT = 360
CAMERA_FOVY = 60.0
CAMERA_FRAME = "camera_optical_frame"


def preview_jpeg(runtime):
    """Return the retained sensor exposure or describe why none is available."""
    with runtime.lock:
        runtime.camera_demand = time.monotonic()
        if not runtime.render:
            raise RuntimeError("Camera rendering is disabled for this runtime (GO2_RENDER=0).")
        if runtime.errors.get("render"):
            raise RuntimeError("Camera renderer failed: " + runtime.errors["render"])
        if not runtime.sensor_settings["camera_enabled"]:
            raise RuntimeError("Camera sensor is disabled. Enable Camera active to resume it.")
        if runtime.sim.mode == "paused":
            raise RuntimeError("Camera sensor is paused with the simulation. Resume to receive images.")
        if runtime.sim.mode == "fault":
            raise RuntimeError("Camera sensor stopped after a simulation fault. Reset the world to recover.")
        if runtime.camera_jpeg is None:
            raise RuntimeError("Waiting for the first MuJoCo camera exposure.")
        return runtime.camera_jpeg


def intrinsics(width=CAMERA_WIDTH, height=CAMERA_HEIGHT, fovy=CAMERA_FOVY):
    """Pinhole intrinsics for MuJoCo's vertical field of view, pixel centers."""
    focal = height / (2 * math.tan(math.radians(fovy) / 2))
    return focal, focal, (width - 1) / 2, (height - 1) / 2


@dataclass(frozen=True)
class CameraFrame:
    epoch: int
    generation: int
    time: float
    wall_timestamp_ns: int
    rgb: np.ndarray
    frame_id: str = CAMERA_FRAME

    def __post_init__(self):
        if self.rgb.dtype != np.uint8 or self.rgb.shape != (CAMERA_HEIGHT, CAMERA_WIDTH, 3):
            raise ValueError("camera frame must be 640x360 RGB uint8")
        # Renderers may recycle their readback buffer. Retain an immutable copy.
        owned = self.rgb.copy()
        owned.flags.writeable = False
        object.__setattr__(self, "rgb", owned)
