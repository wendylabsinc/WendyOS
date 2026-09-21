"""Coherent MuJoCo rays for a virtual multi-ring lidar attachment.

This is a declared synthetic pattern, not a calibrated Unitree factory lidar:
360 azimuth samples on 25 rings from -30 to +30 degrees in 2.5-degree steps. The
middle ring uses the existing /scan angles exactly. All rays share one capture
time and the copied lidar site's pose; no rolling scan or motion distortion is
modeled. XYZ points use lidar_link coordinates (X forward, Y left, Z up).

Pinned MuJoCo 3.3.7's mj_multiRay spherical broadphase misses valid elevated
wall intersections. A private model copy disables that broadphase by setting
body_bvhadr to -1. The batch then dispatches the same exact geom intersections
as mj_ray in one native call, avoiding 9000 Python/GIL transitions per scan.
Only captured derived geom transforms are copied into the ray-only MjData;
this model is never used for dynamics and the shared physics model is untouched.
See src/engine/engine_ray.c (mju_multiRayPrepare and mju_singleRay) at tag 3.3.7.

The caller captures PhysicsSampler and owns pause/reset/publication fencing.
Sampling here neither reads live simulation data nor refreshes timestamps. One
worker owns each Lidar and PhysicsSampler instance. Robot visual/collision
groups 2/3 are excluded by the sampler's ray group; world group0 includes static
and moving obstacles. Missing, out-of-range, and dropped returns become +inf in
the 2D scan and are omitted from the point cloud, never invented clear-space
points at maximum range. Optional dropout uses a reproducible seeded stream.
"""

import copy
import math

import mujoco
import numpy as np

from .sensors import SCAN_ANGLES, SCAN_COUNT, SCAN_MAX, SCAN_MIN


ELEVATIONS_DEGREES = tuple(-30.0 + 2.5 * index for index in range(25))
HORIZONTAL_RING = ELEVATIONS_DEGREES.index(0.0)
RAY_COUNT = len(ELEVATIONS_DEGREES) * SCAN_COUNT


def ray_directions():
    horizontal = np.array([[math.cos(angle), math.sin(angle), 0.0] for angle in SCAN_ANGLES])
    rings = []
    for elevation in ELEVATIONS_DEGREES:
        angle = math.radians(elevation)
        directions = horizontal.copy()
        directions[:, :2] *= math.cos(angle)
        directions[:, 2] = math.sin(angle)
        rings.append(directions)
    result = np.concatenate(rings)
    result.flags.writeable = False
    return result


DIRECTIONS = ray_directions()


class Lidar:
    def __init__(self, *, dropout=0.0, seed=0):
        try:
            probability = float(dropout)
        except (TypeError, ValueError, OverflowError) as error:
            raise ValueError("lidar dropout must be a finite probability in [0,1]") from error
        if isinstance(dropout, (bool, str)) or not math.isfinite(probability) or not 0 <= probability <= 1:
            raise ValueError("lidar dropout must be a finite probability in [0,1]")
        if isinstance(seed, bool) or not isinstance(seed, (int, np.integer)) or seed < 0:
            raise ValueError("lidar seed must be a nonnegative integer")
        self.dropout = probability
        self.rng = np.random.default_rng(seed)
        self.distances = np.empty(RAY_COUNT, dtype=np.float64)
        self.geom_id = np.empty(RAY_COUNT, dtype=np.int32)
        self.ray_model = self.ray_data = self.source_model = None

    def sample(self, sampler):
        if sampler.epoch is None or not hasattr(sampler, "wall_timestamp_ns"):
            raise ValueError("capture PhysicsSampler before sampling lidar")
        data = sampler.data
        origin = data.site_xpos[sampler.lidar_id]
        rotation = data.site_xmat[sampler.lidar_id].reshape(3, 3)
        world_directions = np.ascontiguousarray(DIRECTIONS @ rotation.T)
        if self.source_model is not sampler.model:
            self.source_model = sampler.model
            self.ray_model = copy.copy(sampler.model)
            self.ray_model.body_bvhadr[:] = -1
            self.ray_data = mujoco.MjData(self.ray_model)
        self.ray_data.geom_xpos[:] = data.geom_xpos
        self.ray_data.geom_xmat[:] = data.geom_xmat
        self.ray_data.xipos[:] = data.xipos
        mujoco.mj_multiRay(self.ray_model, self.ray_data, origin, world_directions.reshape(-1),
                          sampler.geom_group, True, -1, self.geom_id, self.distances,
                          RAY_COUNT, SCAN_MAX)
        valid = (np.isfinite(self.distances) & (self.distances >= SCAN_MIN) &
                 (self.distances <= SCAN_MAX))
        if self.dropout:
            valid &= self.rng.random(RAY_COUNT) >= self.dropout
        first = HORIZONTAL_RING * SCAN_COUNT
        horizontal = slice(first, first + SCAN_COUNT)
        horizontal_valid = valid[horizontal]
        ranges = np.full(SCAN_COUNT, np.inf, dtype=np.float32)
        ranges[horizontal_valid] = self.distances[horizontal][horizontal_valid]
        # Return independent arrays: a later scan reuses our private buffers.
        indices = np.flatnonzero(valid).astype(np.int32)
        xyz = (DIRECTIONS[valid] * self.distances[valid, None]).astype(np.float32)
        return {"ranges": ranges, "xyz": xyz, "ray_indices": indices,
                "frame_id": "lidar_link", "world_frame_id": "simulation_world",
                "epoch": sampler.epoch, "time": float(data.time),
                "wall_timestamp_ns": sampler.wall_timestamp_ns}
