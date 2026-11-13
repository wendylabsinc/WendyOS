"""What the browser draws: the G1's visual meshes, the warehouse fixtures and the boxes.

The robot's 49 visual meshes (about 630k triangles) are merged on a 2 mm grid,
to about 190k triangles, and sent once as one binary blob: float32 vertices and
uint32 triangle indices per mesh. Everything that moves is a MuJoCo body, so the
live stream only carries body poses.
"""

from __future__ import annotations

import gzip
import json

import mujoco
import numpy as np

from .catalog import BAY_PITCH, BAYS, CART, CART_SLOTS, SHELF_TOP, ZONES
from .world import RACK_DEPTH, RACK_WIDTH, boxes, cart_slot, rack_slot, stock

GRID = 0.002  # vertex-merge cell for the robot meshes (m)
MESH = mujoco.mjtGeom.mjGEOM_MESH
SHAPES = {mujoco.mjtGeom.mjGEOM_BOX: "box", mujoco.mjtGeom.mjGEOM_CYLINDER: "cylinder"}


def _simplify(vertices: np.ndarray, faces: np.ndarray, cell: float = GRID) -> tuple[np.ndarray, np.ndarray]:
    """Merge vertices that share a grid cell and drop the triangles that collapse."""
    cells = np.floor(vertices / cell).astype(np.int64)
    _, inverse, counts = np.unique(cells, axis=0, return_inverse=True, return_counts=True)
    inverse = inverse.reshape(-1)
    merged = np.zeros((counts.size, 3))
    np.add.at(merged, inverse, vertices)
    merged /= counts[:, None]
    triangles = inverse[faces]
    keep = ((triangles[:, 0] != triangles[:, 1]) & (triangles[:, 1] != triangles[:, 2])
            & (triangles[:, 0] != triangles[:, 2]))
    triangles = triangles[keep]
    _, first = np.unique(np.sort(triangles, axis=1), axis=0, return_index=True)
    return merged.astype(np.float32), triangles[np.sort(first)].astype(np.uint32)


def _xyzw(wxyz: np.ndarray) -> list[float]:
    return [round(float(v), 6) for v in (wxyz[1], wxyz[2], wxyz[3], wxyz[0])]


def _slot(slot) -> list[float]:
    return [round(slot.x, 4), round(slot.y, 4), round(slot.z, 4), round(slot.facing, 6)]


def render_bodies(model: mujoco.MjModel) -> list[int]:
    """Bodies the viewer draws and the stream moves: robot links with visual meshes, then boxes
    (the ones the robot moves, then the fixed stock on the top shelves)."""
    robot = sorted({int(model.geom_bodyid[g]) for g in range(model.ngeom)
                    if model.geom_group[g] == 1 and model.geom_type[g] == MESH})
    return robot + [model.body(name).id for name, _, _ in boxes() + stock()]


def export_scene(model: mujoco.MjModel) -> tuple[dict, bytes]:
    data = mujoco.MjData(model)
    mujoco.mj_forward(model, data)
    bodies = render_bodies(model)
    slot = {body: i for i, body in enumerate(bodies)}
    robot_root = model.body("pelvis").id
    box_bodies = {model.body(name).id for name, _, _ in boxes() + stock()}

    chunks: list[bytes] = []
    offset = 0
    meshes: list[dict] = []
    mesh_slot: dict[int, int] = {}
    parts = []
    for geom in range(model.ngeom):
        if model.geom_group[geom] != 1 or model.geom_type[geom] != MESH:
            continue
        mesh = int(model.geom_dataid[geom])
        if mesh not in mesh_slot:
            vertices = model.mesh_vert[model.mesh_vertadr[mesh]:model.mesh_vertadr[mesh] + model.mesh_vertnum[mesh]]
            faces = model.mesh_face[model.mesh_faceadr[mesh]:model.mesh_faceadr[mesh] + model.mesh_facenum[mesh]]
            vertices, triangles = _simplify(vertices.astype(np.float64), faces)
            entry = {"name": model.mesh(mesh).name, "vertexOffset": offset, "vertexCount": int(len(vertices))}
            chunks.append(vertices.tobytes())
            offset += vertices.nbytes
            entry.update(indexOffset=offset, indexCount=int(triangles.size))
            chunks.append(triangles.tobytes())
            offset += triangles.nbytes
            mesh_slot[mesh] = len(meshes)
            meshes.append(entry)
        parts.append({
            "body": slot[int(model.geom_bodyid[geom])],
            "mesh": mesh_slot[mesh],
            "pos": np.round(model.geom_pos[geom], 6).tolist(),
            "quat": _xyzw(model.geom_quat[geom]),
            "rgba": np.round(model.geom_rgba[geom], 3).tolist(),
        })

    fixtures = []
    for geom in range(model.ngeom):
        body = int(model.geom_bodyid[geom])
        kind = SHAPES.get(int(model.geom_type[geom]))
        if kind is None or model.body_rootid[body] == robot_root or body in box_bodies:
            continue
        quat = np.zeros(4)
        mujoco.mju_mat2Quat(quat, data.geom_xmat[geom])
        fixtures.append({
            "name": model.geom(geom).name,
            "shape": kind,
            "size": np.round(model.geom_size[geom], 4).tolist(),
            "pos": np.round(data.geom_xpos[geom], 4).tolist(),
            "quat": _xyzw(quat),
        })

    description = {
        "version": 1,
        "bodies": [model.body(b).name for b in bodies],
        "meshes": meshes,
        "parts": parts,
        "fixtures": fixtures,
        "boxes": [{"name": name, "body": slot[model.body(name).id], "label": item.label,
                   "size": list(item.size), "color": item.color} for name, item, _ in boxes() + stock()],
        "zones": [{"key": z.key, "name": z.name, "x": z.x, "y": z.y, "facing": z.facing} for z in ZONES],
        "rack": {"width": RACK_WIDTH, "depth": RACK_DEPTH, "bays": BAYS, "bayPitch": BAY_PITCH, "shelf": SHELF_TOP},
        "cart": CART,
        # where a box rests in each rack bay and cart slot: x, y, surface height, facing
        "slots": {**{z.key: [_slot(rack_slot(z, bay)) for bay in range(BAYS)] for z in ZONES},
                  "cart": [_slot(cart_slot(i)) for i in range(len(CART_SLOTS))]},
    }
    return description, b"".join(chunks)


class SceneAsset:
    """The scene description and mesh blob, encoded once for every client."""

    def __init__(self, model: mujoco.MjModel) -> None:
        self.description, self.meshes = export_scene(model)
        self.json = json.dumps(self.description, separators=(",", ":")).encode()
        self.json_gzip = gzip.compress(self.json, compresslevel=6, mtime=0)
        self.meshes_gzip = gzip.compress(self.meshes, compresslevel=6, mtime=0)
