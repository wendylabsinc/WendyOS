"""Export one Crazyflie's visual surfaces for the browser.

Every drone shares the same meshes, so the viewer receives them once, in the
body frame (metres, Z up), with MuJoCo's own vertex normals. The propeller
mesh is split into its four rotors so the viewer can spin each one about its
motor axis.
"""

from __future__ import annotations

import gzip
import json

import mujoco
import numpy as np

from .choreography import PADS
from .model import N_DRONES, PROP_RADIUS, ROTOR_MAX_THRUST, ROTOR_SPIN, ROTOR_XY


def _rotation(quat: np.ndarray) -> np.ndarray:
    matrix = np.zeros(9)
    mujoco.mju_quat2Mat(matrix, quat)
    return matrix.reshape(3, 3)


def _triangles(model: mujoco.MjModel, geom: int) -> tuple[np.ndarray, np.ndarray]:
    """Non-indexed triangle corners and normals for a mesh geom, in the body frame."""
    mesh = int(model.geom_dataid[geom])
    rotation = _rotation(model.geom_quat[geom])
    vertices = model.mesh_vert[model.mesh_vertadr[mesh]:model.mesh_vertadr[mesh] + model.mesh_vertnum[mesh]]
    normals = model.mesh_normal[model.mesh_normaladr[mesh]:model.mesh_normaladr[mesh] + model.mesh_normalnum[mesh]]
    faces = model.mesh_face[model.mesh_faceadr[mesh]:model.mesh_faceadr[mesh] + model.mesh_facenum[mesh]]
    face_normals = model.mesh_facenormal[model.mesh_faceadr[mesh]:model.mesh_faceadr[mesh] + model.mesh_facenum[mesh]]
    corners = vertices[faces] @ rotation.T + model.geom_pos[geom]
    corner_normals = normals[face_normals] @ rotation.T
    return corners, corner_normals


def _part(name: str, rgba: np.ndarray, corners: np.ndarray, normals: np.ndarray, **extra) -> dict:
    return {
        "name": name,
        "rgba": np.round(rgba, 3).tolist(),
        "positions": np.round(corners.reshape(-1), 5).tolist(),
        "normals": np.round(normals.reshape(-1), 3).tolist(),
        **extra,
    }


def _motor_axes(model: mujoco.MjModel, geoms: list[int]) -> np.ndarray:
    """Centre of each motor can (the chrome mesh), per rotor, in the body frame."""
    for geom in geoms:
        material = mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_MATERIAL, int(model.geom_matid[geom]))
        if material == "burnished_chrome":
            corners, _ = _triangles(model, geom)
            points = corners.reshape(-1, 3)
            points = points[np.hypot(points[:, 0], points[:, 1]) > 0.025]
            axes = []
            for x, y in ROTOR_XY:
                mine = points[(np.sign(points[:, 0]) == np.sign(x)) & (np.sign(points[:, 1]) == np.sign(y))]
                axes.append([(mine[:, 0].min() + mine[:, 0].max()) / 2, (mine[:, 1].min() + mine[:, 1].max()) / 2, 0.0])
            return np.array(axes)
    return np.column_stack([ROTOR_XY, np.zeros(4)])


def export_scene(model: mujoco.MjModel) -> dict:
    body = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, "cf0")
    geoms = [geom for geom in range(model.ngeom)
             if model.geom_bodyid[geom] == body and model.geom_group[geom] == 2]
    hubs = _motor_axes(model, geoms)
    parts = []
    for geom in geoms:
        material = int(model.geom_matid[geom])
        name = mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_MATERIAL, material) or "surface"
        rgba = model.mat_rgba[material]
        corners, normals = _triangles(model, geom)
        if name != "propeller_plastic":
            parts.append(_part(name, rgba, corners, normals, metal=name in ("burnished_chrome", "polished_gold")))
            continue
        centroid = corners.mean(axis=1)
        for rotor, (x, y) in enumerate(ROTOR_XY):
            mine = (np.sign(centroid[:, 0]) == np.sign(x)) & (np.sign(centroid[:, 1]) == np.sign(y))
            hub = hubs[rotor]
            parts.append(_part(f"propeller_{rotor}", rgba, corners[mine] - hub, normals[mine],
                               rotor=rotor, hub=hub.tolist()))
    return {
        "version": 1,
        "drones": N_DRONES,
        "parts": parts,
        "pads": np.round(PADS, 4).tolist(),
        "rotorSpin": ROTOR_SPIN.tolist(),
        "propRadius": PROP_RADIUS,
        "maxThrust": ROTOR_MAX_THRUST,
    }


class SceneAsset:
    def __init__(self, model: mujoco.MjModel) -> None:
        self.description = export_scene(model)
        self.json = json.dumps(self.description, separators=(",", ":")).encode()
        self.gzip = gzip.compress(self.json, compresslevel=6, mtime=0)
