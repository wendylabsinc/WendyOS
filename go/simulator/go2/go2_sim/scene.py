"""Static visual geometry and shared, finite browser pose snapshots.

MuJoCo's compiled mesh vertices already include asset scaling and centering.
They must be paired with the compiled geom pose, with no extra mesh transform.
The wire format uses metres, Z-up, and xyzw quaternions throughout.
"""

import gzip
import hashlib
import json
import threading
import time

import mujoco
import numpy as np


STATE_PERIOD = 1 / 30
GEOM_TYPES = {int(getattr(mujoco.mjtGeom, "mjGEOM_" + name.upper())): name
              for name in ("plane", "sphere", "capsule", "ellipsoid", "cylinder", "box", "mesh")}


def compact_json(value):
    return json.dumps(value, allow_nan=False, separators=(",", ":")).encode()


def numbers(values):
    return np.round(np.asarray(values, dtype=np.float64), 7).reshape(-1).tolist()


def xyzw(quaternion):
    return numbers(np.asarray(quaternion)[..., [1, 2, 3, 0]])


def export_scene(model):
    """Export visible surfaces once; collision proxies never reach the viewer."""
    robot_body = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, "base")
    result = {
        "version": 1,
        "bodies": [{"id": body, "name": mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_BODY, body) or ""}
                   for body in range(model.nbody)],
        "geoms": [], "meshes": [], "robot_body": robot_body,
        "camera": None,
    }
    mesh_ids = set()
    for geom in range(model.ngeom):
        body = int(model.geom_bodyid[geom])
        group = int(model.geom_group[geom])
        # Instrumented models explicitly put proxies in group 3. Pinned flat
        # models retain group 0 proxies, but robot surfaces are always group 2.
        if group == 3 or (robot_body >= 0 and model.body_rootid[body] == robot_body and group != 2):
            continue
        kind = GEOM_TYPES.get(int(model.geom_type[geom]))
        if kind is None:
            raise ValueError(f"unsupported browser geometry type: {model.geom_type[geom]}")
        material = int(model.geom_matid[geom])
        rgba = model.geom_rgba[geom]
        if material >= 0 and np.array_equal(rgba, [0.5, 0.5, 0.5, 1.0]):
            rgba = model.mat_rgba[material]
        if rgba[3] <= 0:
            continue
        surface = {
            "name": mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_GEOM, geom) or "",
            "body": body, "type": kind, "size": numbers(model.geom_size[geom]),
            "position": numbers(model.geom_pos[geom]),
            "quaternion": xyzw(model.geom_quat[geom]), "rgba": numbers(rgba),
        }
        if kind == "mesh":
            mesh = int(model.geom_dataid[geom])
            surface["mesh"] = mesh
            mesh_ids.add(mesh)
        result["geoms"].append(surface)
    for mesh in sorted(mesh_ids):
        vertex = int(model.mesh_vertadr[mesh])
        face = int(model.mesh_faceadr[mesh])
        result["meshes"].append({
            "id": mesh,
            "positions": numbers(model.mesh_vert[vertex:vertex + model.mesh_vertnum[mesh]]),
            "indices": model.mesh_face[face:face + model.mesh_facenum[mesh]].reshape(-1).tolist(),
        })
    camera = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_CAMERA, "front")
    if camera >= 0:
        result["camera"] = {
            "body": int(model.cam_bodyid[camera]), "position": numbers(model.cam_pos[camera]),
            "quaternion": xyzw(model.cam_quat[camera]), "fovy": float(model.cam_fovy[camera]),
        }
    result["id"] = hashlib.sha256(compact_json(result)).hexdigest()
    return result


class BrowserScene:
    """One private kinematics buffer and a 30 Hz cache shared by all clients.

    Static surfaces may use simplified meshes with different compiled geom
    frames. Body poses always come from the original physics model.
    A request copies only integration positions while holding the physics lock.
    Kinematics and JSON encoding happen outside it. Lifecycle identity is checked
    again before publication, so a reset cannot publish an old epoch as new.
    """

    def __init__(self, model, *, visual_model=None):
        self.model = model
        visual_model = model if visual_model is None else visual_model
        if visual_model is not model:
            names = lambda source: [mujoco.mj_id2name(source, mujoco.mjtObj.mjOBJ_BODY, body)
                                    for body in range(source.nbody)]
            if (names(model) != names(visual_model)
                    or any(not np.array_equal(getattr(model, field), getattr(visual_model, field))
                           for field in ("body_parentid", "body_mocapid", "body_pos", "body_quat"))):
                raise ValueError("browser visual model must preserve physics body IDs and frames")
        self.description = export_scene(visual_model)
        self.json = compact_json(self.description)
        self.gzip = gzip.compress(self.json, compresslevel=5, mtime=0)
        self.data = mujoco.MjData(model)
        self.lock = threading.Lock()
        self.cached = None
        self.cached_json = None
        self.last_capture = -float("inf")
        self.samples = 0
        # A finite model-default fallback also covers a fault before first GET.
        mujoco.mj_kinematics(model, self.data)
        self.last_positions = numbers(self.data.xpos)
        self.last_quaternions = xyzw(self.data.xquat)
        self.last_time = 0.0

    def state_json(self, runtime):
        with self.lock:
            for _ in range(4):
                with runtime.lock:
                    sim = runtime.sim
                    identity = (sim.epoch, runtime.observation_generation, sim.mode)
                    now = time.monotonic()
                    if (self.cached is not None and now - self.last_capture < STATE_PERIOD
                            and identity == (self.cached["epoch"], self.cached["generation"], self.cached["mode"])):
                        return self.cached_json
                    self.data.qpos[:] = sim.data.qpos
                    self.data.mocap_pos[:] = sim.data.mocap_pos
                    self.data.mocap_quat[:] = sim.data.mocap_quat
                    sampled_time = float(sim.data.time)
                valid = bool(np.isfinite(self.data.qpos).all()
                             and np.isfinite(self.data.mocap_pos).all()
                             and np.isfinite(self.data.mocap_quat).all()
                             and np.isfinite(sampled_time))
                if valid:
                    mujoco.mj_kinematics(self.model, self.data)
                    valid = bool(np.isfinite(self.data.xpos).all() and np.isfinite(self.data.xquat).all())
                positions = numbers(self.data.xpos) if valid else self.last_positions
                quaternions = xyzw(self.data.xquat) if valid else self.last_quaternions
                state = {
                    "scene_id": self.description["id"], "epoch": identity[0],
                    "generation": identity[1], "time": sampled_time if valid else self.last_time,
                    "mode": identity[2], "valid": valid,
                    "positions": positions, "quaternions": quaternions,
                }
                payload = compact_json(state)
                with runtime.lock:
                    if identity != (sim.epoch, runtime.observation_generation, sim.mode):
                        continue
                    self.cached = state
                    self.cached_json = payload
                    self.last_capture = now
                    self.samples += 1
                    if valid:
                        self.last_positions, self.last_quaternions = positions, quaternions
                        self.last_time = sampled_time
                    return payload
            raise RuntimeError("scene state changed during capture; retry")
