"""Render explicit synthetic fire-camera inputs from the Go2's office geometry.

This is an optical stimulus fixture, not a combustion model. The ESP32 firmware
must still capture and transport these original JPEGs before perception uses
them. No robot motion or camera detection is implemented here.
"""
from __future__ import annotations

import argparse
import hashlib
import io
import json
import math
from pathlib import Path
import xml.etree.ElementTree as ET

from integration.xiaofire.perception import simulated_pinhole_contract

WIDTH, HEIGHT = 2048, 1536
FIRE_NAV = (6.6, 2.4, 0.02)
CAMERA_NAV = (5.5, 3.6, 2.3)
CAMERA_NAME = "xiao-fire-camera"
FIRE_GEOMS = ("xiao-fire-outer", "xiao-fire-core")


def _numbers(values):
    return " ".join(f"{v:.9g}" for v in values)


def _camera_axes(position, target):
    import numpy as np
    forward = np.array(target, dtype=float) - np.array(position, dtype=float)
    forward /= np.linalg.norm(forward)
    right = np.cross(forward, [0., 0., 1.])
    right /= np.linalg.norm(right)
    up = np.cross(right, forward)
    return _numbers([*right, *up])


def compose_office_file(original, output):
    """Add the camera and non-colliding fixture geoms; retain office contacts.

    Pass this exact output as PatrolPhysics's office argument. It is deliberately
    an office file, not a separately invented display scene.
    """
    original, output = Path(original).resolve(), Path(output).resolve()
    root = ET.parse(original).getroot()
    if root.find(f".//camera[@name='{CAMERA_NAME}']") is not None:
        raise ValueError("office already contains the fire fixture")
    asset, world = root.find("asset"), root.find("worldbody")
    if asset is None or world is None:
        raise ValueError("office needs assets and worldbody")
    visual = root.find("visual")
    if visual is None:
        visual = ET.SubElement(root, "visual")
    global_visual = visual.find("global")
    if global_visual is None:
        global_visual = ET.SubElement(visual, "global")
    global_visual.set("offwidth", str(WIDTH))
    global_visual.set("offheight", str(HEIGHT))
    # Triangular prisms make the declared cue shape visible from this camera.
    # The yellow front face is offset towards the camera to avoid z fighting.
    for suffix, width, height, depth in (("outer", .18, .70, .035), ("core", .10, .45, .045)):
        vertices = [(-width, -depth, 0), (width, -depth, 0), (0, -depth, height),
                    (-width, depth, 0), (width, depth, 0), (0, depth, height)]
        faces = [(0, 2, 1), (3, 4, 5), (0, 1, 4), (0, 4, 3),
                 (1, 2, 5), (1, 5, 4), (2, 0, 3), (2, 3, 5)]
        ET.SubElement(asset, "mesh", name="xiao-fire-" + suffix,
                      vertex=_numbers(v for p in vertices for v in p),
                      face=" ".join(str(v) for p in faces for v in p))
    ET.SubElement(asset, "material", name="xiao-fire-orange", rgba="1 .22 .025 1", emission="1", specular="0")
    ET.SubElement(asset, "material", name="xiao-fire-yellow", rgba="1 .85 .035 1", emission="1", specular="0")
    # The flame's front faces approximately toward the camera; outer width is X.
    flame = ET.SubElement(world, "body", name="xiao-fire-fixture", pos=_numbers(FIRE_NAV))
    for suffix, material, offset in (("outer", "orange", 0), ("core", "yellow", .06)):
        ET.SubElement(flame, "geom", name="xiao-fire-" + suffix, type="mesh",
                      mesh="xiao-fire-" + suffix, material="xiao-fire-" + material,
                      pos=f"0 {offset} 0", contype="0", conaffinity="0", group="2")
    ET.SubElement(flame, "geom", name="xiao-orange-distractor", type="box", size=".33 .035 .65",
                  pos="0 0 .65", rgba="1 .35 .025 0", contype="0", conaffinity="0", group="2")
    ET.SubElement(world, "camera", name=CAMERA_NAME, pos=_numbers(CAMERA_NAV),
                  xyaxes=_camera_axes(CAMERA_NAV, (FIRE_NAV[0], FIRE_NAV[1], .35)), fovy="16")
    # A visible camera housing belongs to the same scene, away from the route.
    housing_position = [CAMERA_NAV[0], CAMERA_NAV[1] + .12, CAMERA_NAV[2] + .1]
    housing = ET.SubElement(world, "body", name="xiao-camera-housing", pos=_numbers(housing_position))
    ET.SubElement(housing, "geom", name="xiao-camera-body", type="box", size=".065 .04 .04",
                  rgba=".1 .55 .5 1", contype="0", conaffinity="0", group="2")
    output.parent.mkdir(parents=True, exist_ok=True)
    ET.indent(root)
    ET.ElementTree(root).write(output, encoding="unicode")
    return {"office_sha256": hashlib.sha256(output.read_bytes()).hexdigest(),
            "original_office_sha256": hashlib.sha256(original.read_bytes()).hexdigest(),
            "fire_nav_m": list(FIRE_NAV), "camera_nav_m": list(CAMERA_NAV),
            "camera_name": CAMERA_NAME, "model": "synthetic emissive flame cue; no combustion or smoke physics"}


def set_cue(model, kind, phase=0):
    """Only change fixture visuals, never robot state, forces or observations."""
    if kind not in ("clean", "fire", "orange-distractor"):
        raise ValueError("unknown visual fixture")
    for name in FIRE_GEOMS:
        model.geom_rgba[model.geom(name).id, 3] = 1. if kind == "fire" else 0.
    model.geom_rgba[model.geom("xiao-orange-distractor").id, 3] = 1. if kind == "orange-distractor" else 0.
    # Vary only luminosity, not the declared floor point or camera calibration.
    outer = model.material("xiao-fire-orange").id
    core = model.material("xiao-fire-yellow").id
    pulse = .85 + .15 * math.sin(float(phase) * .83) ** 2
    model.mat_rgba[outer, :3] = [pulse, .22 * pulse, .025]
    model.mat_rgba[core, :3] = [1., .85 * pulse, .035]


def camera_calibration(model, data, run_id, model_sha256):
    import numpy as np
    camera_id = model.camera(CAMERA_NAME).id
    world_axes = np.array([[1., 0., 0.], [0., 0., 1.], [0., -1., 0.]])
    transform = np.eye(4)
    transform[:3, :3] = world_axes @ data.cam_xmat[camera_id].reshape(3, 3) @ np.diag([1., -1., -1.])
    transform[:3, 3] = world_axes @ data.cam_xpos[camera_id]
    focal = HEIGHT / (2 * math.tan(math.radians(float(model.cam_fovy[camera_id])) / 2))
    return {"schema": "wendy.xiao-fire-camera.v1", "origin": "simulated", "run_id": run_id,
            **simulated_pinhole_contract(WIDTH, HEIGHT),
            "width": WIDTH, "height": HEIGHT, "fx": focal, "fy": focal, "cx": WIDTH / 2, "cy": HEIGHT / 2,
            "camera_to_world": transform.reshape(-1).tolist(), "floor_y_m": 0.,
            "frame_id": "office-floor", "frame_revision": "office-" + model_sha256[:16],
            "revision": "xiao-synthetic-fire-camera-v1", "model_sha256": model_sha256,
            "model": "MuJoCo ideal pinhole; known simulated extrinsics; no measured lens calibration"}


def world_point_in_view(calibration, position, padding_px=0):
    """Projection check for the declared static optical replay boundary."""
    import numpy as np
    transform = np.array(calibration["camera_to_world"], dtype=float).reshape(4, 4)
    point = np.array(position, dtype=float)
    if point.shape != (3,) or not np.isfinite(point).all() or padding_px < 0:
        raise ValueError("invalid projection point")
    optical = transform[:3, :3].T @ (point - transform[:3, 3])
    if optical[2] <= 0:
        return False
    u = calibration["fx"] * optical[0] / optical[2] + calibration["cx"]
    v = calibration["fy"] * optical[1] / optical[2] + calibration["cy"]
    return bool(-padding_px <= u <= calibration["width"] + padding_px and
                -padding_px <= v <= calibration["height"] + padding_px)


def projected_visible(calibration, position, radius=.55):
    """Conservative world-axis cube projection for the robot replay boundary.

    A True result means the renderer must not omit that possibly visible robot.
    The caller must check the entire observed route, not only its destination.
    """
    import itertools
    center = tuple(float(value) for value in position)
    if len(center) != 3 or not all(math.isfinite(v) for v in center) or not math.isfinite(radius) or not 0 <= radius <= 10:
        raise ValueError("invalid projection bounds")
    transform = calibration["camera_to_world"]
    corners = []
    for delta in itertools.product((-radius, radius), repeat=3):
        offset = [center[i] + delta[i] - transform[4*i+3] for i in range(3)]
        corners.append([sum(transform[4*i+j] * offset[i] for i in range(3)) for j in range(3)])
    depth = [point[2] for point in corners]
    if max(depth) <= 0:
        return False
    # Test homogeneous frustum side planes before dividing by depth. A box
    # crossing the camera plane can still be wholly outside a side plane.
    # Each plane is linear, so its maximum over the box occurs at a corner.
    fx, fy, cx, cy = (calibration[key] for key in ("fx", "fy", "cx", "cy"))
    width, height = calibration["width"], calibration["height"]
    planes = ((fx, 0, cx), (-fx, 0, width-cx), (0, fy, cy), (0, -fy, height-cy))
    if any(max(sum(a*b for a, b in zip(plane, corner)) for corner in corners) < -1e-9
           for plane in planes):
        return False
    if min(depth) <= 0:
        return True  # The cube crosses the optical plane; do not call it unseen.
    u = [calibration["fx"] * p[0]/p[2] + calibration["cx"] for p in corners]
    v = [calibration["fy"] * p[1]/p[2] + calibration["cy"] for p in corners]
    return bool(max(u) >= 0 and min(u) <= calibration["width"] and
                max(v) >= 0 and min(v) <= calibration["height"])


def prepare(original, assets, out, run_id, clean_frames=15, fire_frames=600):
    """Prepare replayable optical input, independent of detection or dispatch."""
    import mujoco as mj
    from PIL import Image
    import sim.patrol_physics as physics
    from sim.patrol_physics import compose_office, INITIAL
    # Python searches the container working directory before PYTHONPATH. Fail
    # closed if a baked /world/sim shadows this checkout's scene composer.
    if Path(physics.__file__).resolve().parents[1] != Path(__file__).resolve().parents[2]:
        raise ValueError("scene renderer and physics composer must come from the same checkout; mount it at the container working directory")
    out = Path(out).resolve()
    if out.is_relative_to(Path(__file__).resolve().parents[2]):
        raise ValueError("generated media must stay outside the checkout")
    if not 1 <= clean_frames <= 100 or not 10 <= fire_frames <= 2000:
        raise ValueError("invalid bounded stimulus counts")
    out.mkdir(parents=True, exist_ok=False, mode=0o700)
    scene = compose_office_file(original, out / "office.xml")
    xml = compose_office(out / "office.xml", assets)
    digest = hashlib.sha256(xml.encode()).hexdigest()
    model = mj.MjModel.from_xml_string(xml)
    data = mj.MjData(model)
    data.qpos[:7] = INITIAL
    mj.mj_forward(model, data)
    calibration = camera_calibration(model, data, run_id, digest)
    (out / "calibration.json").write_text(json.dumps(calibration, indent=2) + "\n")
    frames, negatives = out / "frames", out / "negatives"
    frames.mkdir(); negatives.mkdir()
    manifest = []
    with mj.Renderer(model, height=HEIGHT, width=WIDTH) as renderer:
        def render(kind, phase=0):
            set_cue(model, kind, phase)
            renderer.update_scene(data, camera=CAMERA_NAME)
            rgb = renderer.render()
            output = io.BytesIO()
            Image.fromarray(rgb).save(output, "JPEG", quality=88)
            return output.getvalue()
        for kind in ("clean", "orange-distractor"):
            (negatives / (kind + ".jpg")).write_bytes(render(kind))
        clean = render("clean")
        fires = [render("fire", n) for n in range(11)]
        for n in range(clean_frames + fire_frames):
            kind = "clean" if n < clean_frames else "fire"
            raw = clean if kind == "clean" else fires[(n - clean_frames) % len(fires)]
            name = f"frame{n:05d}.jpg"
            (frames / name).write_bytes(raw)
            manifest.append({"path": "frames/" + name, "sha256": hashlib.sha256(raw).hexdigest(),
                             "stimulus_kind": kind})
    receipt = {"schema": "wendy.xiao-fire-stimuli.v1", "origin": "simulated", "run_id": run_id,
               "presentation": "Pre-rendered fixed-camera optical stimulus in the same office geometry as the Go2; no robot images synthesized during motion",
               "model_sha256": digest, "scene": scene, "width": WIDTH, "height": HEIGHT,
               "clean_frames": clean_frames, "fire_frames": fire_frames, "frames": manifest,
               "calibration_sha256": hashlib.sha256((out / "calibration.json").read_bytes()).hexdigest(),
               "limits": ["Synthetic visual cue, not physical combustion", "Ideal pinhole, no exposure or sensor-encoder model", "Go2 route and stand-off must remain outside this camera's field of view", "Clean prefix represents a startup optical buffer; cue visibility is not synchronized with the live 3D view"]}
    (out / "stimuli.json").write_text(json.dumps(receipt, indent=2) + "\n")
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("office", "assets", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--clean-frames", type=int, default=15)
    parser.add_argument("--fire-frames", type=int, default=600)
    args = parser.parse_args()
    receipt = prepare(args.office, args.assets, args.out, args.run_id, args.clean_frames, args.fire_frames)
    print(json.dumps({"out": str(args.out), "model_sha256": receipt["model_sha256"], "frames": len(receipt["frames"])}))


if __name__ == "__main__":
    main()
