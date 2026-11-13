"""Pinhole RGB and axial depth of the collision world, independent of the viewer.

This is an ideal sensor, not the HP60C optics, USB device or firmware. Depth
is little-endian uint16 millimetres; zero means outside the 0.2–4 m range.
"""

import io
import math

import numpy as np
from PIL import Image

WIDTH, HEIGHT = 320, 240
HFOV = math.radians(69)
FX = FY = WIDTH / (2 * math.tan(HFOV / 2))
CX, CY = (WIDTH - 1) / 2, (HEIGHT - 1) / 2
CAMERA_OFFSET = (.285, 0., .235)
NEAR, FAR = .2, 4.


def calibration():
    return {"width": WIDTH, "height": HEIGHT, "encoding": "16UC1",
            "depth_unit": "mm", "near_m": NEAR, "far_m": FAR,
            "k": [FX, 0., CX, 0., FY, CY, 0., 0., 1.],
            "frame_id": "camera_optical_frame", "offset": CAMERA_OFFSET,
            "model": "ideal pinhole, approximate mounting and field of view"}


def render(state, obstacles, half_size=5.):
    v, u = np.indices((HEIGHT, WIDTH), dtype=np.float32)
    # Camera optical +z forward, +x right, +y down. The model has +x forward.
    right, down = (u - CX) / FX, (v - CY) / FY
    c, s = math.cos(state["yaw"]), math.sin(state["yaw"])
    directions = np.stack((c + s * right, s - c * right, -down), axis=-1)
    origin = np.array([state["x"] + c * CAMERA_OFFSET[0],
                       state["y"] + s * CAMERA_OFFSET[0], CAMERA_OFFSET[2]])
    distance = np.full((HEIGHT, WIDTH), 30., dtype=np.float32)
    color = np.full((HEIGHT, WIDTH, 3), [188, 205, 213], dtype=np.uint8)
    with np.errstate(divide="ignore", invalid="ignore"):
        ground = -origin[2] / directions[..., 2]
    ground_hit = (ground > 0) & (ground < distance)
    distance[ground_hit] = ground[ground_hit]
    safe_ground = np.where(ground_hit, ground, 0.)
    gx = origin[0] + safe_ground * directions[..., 0]
    gy = origin[1] + safe_ground * directions[..., 1]
    grid = ((np.floor(gx * 2) + np.floor(gy * 2)) % 2).astype(bool)
    color[ground_hit & grid] = [158, 173, 177]
    color[ground_hit & ~grid] = [180, 193, 195]
    boxes = list(obstacles) + [
        {"x": x, "y": y, "width": w, "depth": d, "height": .6}
        for x, y, w, d in ((-half_size-.05, 0, .1, 2*half_size+.2),
                          (half_size+.05, 0, .1, 2*half_size+.2),
                          (0, -half_size-.05, 2*half_size+.2, .1),
                          (0, half_size+.05, 2*half_size+.2, .1))]
    for box in boxes:
        low = np.array([box["x"]-box["width"]/2, box["y"]-box["depth"]/2, 0.])
        high = np.array([box["x"]+box["width"]/2, box["y"]+box["depth"]/2, box["height"]])
        near = np.full(distance.shape, -np.inf)
        far = np.full(distance.shape, np.inf)
        for axis in range(3):
            direction = directions[..., axis]
            parallel = np.abs(direction) < 1e-8
            safe = np.where(parallel, 1., direction)
            a, b = (low[axis]-origin[axis])/safe, (high[axis]-origin[axis])/safe
            near = np.maximum(near, np.where(parallel, -np.inf, np.minimum(a,b)))
            far = np.minimum(far, np.where(parallel, np.inf, np.maximum(a,b)))
            if not low[axis] <= origin[axis] <= high[axis]:
                far[parallel] = -np.inf
        hit = (far >= np.maximum(near, 0.)) & (near > 0.) & (near < distance)
        distance[hit] = near[hit]
        color[hit] = [99, 118, 129]
        z = origin[2] + np.where(hit, near, 0.) * directions[..., 2]
        color[hit & (z > box["height"]-.09) & (z < box["height"]-.055)] = [234, 132, 45]
    valid = (distance >= NEAR) & (distance <= FAR)
    depth = np.where(valid, np.rint(distance * 1000), 0).astype('<u2')
    t = np.clip((distance-NEAR)/(FAR-NEAR), 0, 1)
    channels = [np.clip(np.minimum(4*t-1.5,-4*t+4.5),0,1),
                np.clip(np.minimum(4*t-.5,-4*t+3.5),0,1),
                np.clip(np.minimum(4*t+.5,-4*t+2.5),0,1)]
    preview = (np.stack(channels,axis=-1)*255).astype(np.uint8)
    preview[~valid] = 0
    return color, depth, preview


def jpeg(array):
    output = io.BytesIO()
    Image.fromarray(array).save(output, format="JPEG", quality=80)
    return output.getvalue()
