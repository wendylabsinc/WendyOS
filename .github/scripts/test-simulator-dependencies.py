"""Exercise the simulator's native wheels and camera encoding on ROS Humble Python."""

import io
import sys

import mujoco
import numpy as np
import onnxruntime
from PIL import Image
import yaml

assert sys.version_info[:2] == (3, 10), sys.version
model = mujoco.MjModel.from_xml_string(
    '<mujoco><worldbody><body><freejoint/><geom type="sphere" size="0.1"/></body></worldbody></mujoco>'
)
data = mujoco.MjData(model)
mujoco.mj_step(model, data)
assert data.time > 0
assert "CPUExecutionProvider" in onnxruntime.get_available_providers()
pixels = np.full((24, 32, 3), [30, 80, 150], dtype=np.uint8)
encoded = io.BytesIO()
Image.fromarray(pixels).save(encoded, format="JPEG")
encoded.seek(0)
with Image.open(encoded) as decoded:
    assert decoded.size == (32, 24)
    assert np.abs(np.asarray(decoded).astype(int) - pixels).max() <= 3
assert yaml.safe_load("robot: go2\n") == {"robot": "go2"}
print("Python 3.10 simulator dependencies and JPEG encoding passed")
