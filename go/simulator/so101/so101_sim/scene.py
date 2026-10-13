"""Static visual geometry and shared, finite browser pose snapshots.

MuJoCo's compiled mesh vertices already include asset scaling and centering.
They must be paired with the compiled geom pose, with no extra mesh transform.
The wire format uses metres, Z-up, and xyzw quaternions throughout.
"""

import hashlib
import json

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
