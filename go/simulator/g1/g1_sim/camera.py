"""Calibrated virtual front camera frames, independent of the observer view."""

from dataclasses import dataclass
import math

import numpy as np

CAMERA_WIDTH = 640
CAMERA_HEIGHT = 360
CAMERA_FOVY = 60.0
CAMERA_FRAME = "camera_optical_frame"


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
