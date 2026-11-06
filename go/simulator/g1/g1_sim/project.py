"""Constrained user environments: no executable code or user-supplied MJCF.

Static world geoms preserve all robot/body/state indices. Files are an immutable,
bounded snapshot; no project path is ever opened by the runtime.
"""
import base64
import hashlib
import math
from pathlib import PurePosixPath
import re
import tempfile
import time
import xml.etree.ElementTree as ET

import mujoco
import numpy as np

MAX_BYTES = 8 * 1024 * 1024
MAX_FILE = 2 * 1024 * 1024
MAX_REQUEST = 12 * 1024 * 1024


def safe_path(name):
    if (not isinstance(name, str) or len(name) > 200 or "\\" in name or
            any(part in {"", ".", ".."} for part in name.split("/")) or
            PurePosixPath(name).is_absolute() or
            not re.fullmatch(r"[A-Za-z0-9_./-]+", name)):
        raise ValueError("asset paths must be relative, without traversal")
    return name


def vector(value, length, label, limit):
    if (not isinstance(value, list) or len(value) != length or
            any(type(v) not in {float, int} or not math.isfinite(v) or abs(v) > limit for v in value)):
        raise ValueError(f"{label} must contain {length} finite numbers bounded by {limit}")
    return value


def decode_project(encoded, kind):
    if not isinstance(encoded, dict) or not 1 <= len(encoded) <= 9 or "scene.json" not in encoded:
        raise ValueError("project requires scene.json and at most eight OBJ assets")
    files, total = {}, 0
    for name, value in encoded.items():
        safe_path(name)
        if name != "scene.json" and not (name.startswith("assets/") and name.endswith(".obj")):
            raise ValueError("only scene.json and assets/*.obj are supported")
        if not isinstance(value, str) or len(value) > (MAX_FILE * 4 // 3 + 4):
            raise ValueError(f"{name}: file exceeds 2 MiB")
        try:
            content = base64.b64decode(value, validate=True)
        except ValueError as exc:
            raise ValueError(f"{name}: invalid base64") from exc
        total += len(content)
        if len(content) > MAX_FILE or total > MAX_BYTES:
            raise ValueError("project exceeds file or total size limit")
        files[name] = content
    import json
    scene = json.loads(files["scene.json"])
    if (not isinstance(scene, dict) or set(scene) != {"version", "profile", "geoms"}
            or type(scene["version"]) is not int or scene["version"] != 1 or scene["profile"] != kind):
        raise ValueError(f"scene requires version 1, profile {kind}, and geoms; robot/controller edits are unsupported")
    geoms = scene["geoms"]
    if not isinstance(geoms, list) or len(geoms) > 64:
        raise ValueError("geoms must be a list of at most 64 static surfaces")
    names, used = set(), {"scene.json"}
    for geom in geoms:
        if not isinstance(geom, dict) or set(geom) - {"name", "type", "size", "position", "quaternion", "rgba", "mesh"}:
            raise ValueError("geom supports name, type, size, position, quaternion, rgba and mesh only")
        name = geom.get("name")
        if not isinstance(name, str) or not re.fullmatch(r"[A-Za-z][A-Za-z0-9_]{0,47}", name) or name in names:
            raise ValueError("geom names must be unique identifiers of at most 48 characters")
        names.add(name)
        shape = geom.get("type")
        dimensions = {"box": 3, "sphere": 1, "cylinder": 2, "capsule": 2, "ellipsoid": 3, "mesh": 3}
        if shape not in dimensions:
            raise ValueError(f"{name}: unsupported geometry type")
        size = vector(geom.get("size"), dimensions[shape], f"{name}.size", 4)
        if min(size) <= 0:
            raise ValueError(f"{name}: sizes must be positive")
        vector(geom.get("position"), 3, f"{name}.position", 20)
        quat = vector(geom.get("quaternion", [1, 0, 0, 0]), 4, f"{name}.quaternion (wxyz)", 1)
        if not math.isclose(sum(v*v for v in quat), 1, abs_tol=1e-5):
            raise ValueError(f"{name}: quaternion must have unit length")
        rgba = vector(geom.get("rgba", [0.6, 0.7, 0.8, 1]), 4, f"{name}.rgba", 1)
        if min(rgba) < 0 or rgba[3] <= 0:
            raise ValueError(f"{name}: colors must be in [0,1], with positive alpha")
        if shape == "mesh":
            asset = safe_path(geom.get("mesh"))
            if asset not in files or asset == "scene.json":
                raise ValueError(f"{name}: missing OBJ asset {asset}")
            used.add(asset)
            validate_obj(files[asset], asset)
        elif "mesh" in geom:
            raise ValueError(f"{name}: mesh is only allowed for mesh geometry")
    if used != set(files):
        raise ValueError("project contains unreferenced assets")
    digest = hashlib.sha256()
    for name in sorted(files):
        digest.update(name.encode() + b"\0" + files[name] + b"\0")
    return scene, files, digest.hexdigest()


def validate_obj(content, name):
    vertices, faces = 0, 0
    try:
        text = content.decode("utf-8")
        for line in text.splitlines():
            fields = line.split()
            if not fields or fields[0].startswith("#"):
                continue
            if fields[0] == "v":
                if len(fields) != 4:
                    raise ValueError("vertices require xyz")
                vector([float(v) for v in fields[1:]], 3, "OBJ vertex", 4)
                vertices += 1
            elif fields[0] == "f":
                if len(fields) != 4 or any(not v.isdigit() or not 1 <= int(v) <= vertices for v in fields[1:]):
                    raise ValueError("faces require three positive, already declared vertex indices")
                faces += 1
            else:
                raise ValueError("only v and triangular f records are supported (no materials or external files)")
            if vertices > 4096 or faces > 8192:
                raise ValueError("OBJ exceeds 4096 vertices or 8192 triangles")
        if vertices < 4 or faces < 4:
            raise ValueError("OBJ requires a closed three-dimensional mesh")
    except (ValueError, UnicodeError) as exc:
        raise ValueError(f"{name}: {exc}") from exc


def append_environment(scene, environment):
    """Called only by the trusted model builder, before MuJoCo compilation."""
    manifest, paths = environment
    assets = scene.find("asset")
    if assets is None:
        assets = ET.SubElement(scene, "asset")
    world = scene.find("worldbody")
    for geom in manifest["geoms"]:
        attrs = {"name": "project_" + geom["name"], "type": geom["type"], "group": "0",
                 "pos": " ".join(map(str, geom["position"])),
                 "quat": " ".join(map(str, geom.get("quaternion", [1, 0, 0, 0]))),
                 "rgba": " ".join(map(str, geom.get("rgba", [0.6, 0.7, 0.8, 1])))}
        if geom["type"] == "mesh":
            # Separate mesh per geom allows distinct scales with one source file.
            mesh_name = "project_mesh_" + geom["name"]
            ET.SubElement(assets, "mesh", name=mesh_name, file=str(paths[geom["mesh"]]),
                          scale=" ".join(map(str, geom["size"])))
            attrs["mesh"] = mesh_name
        else:
            attrs["size"] = " ".join(map(str, geom["size"]))
        ET.SubElement(world, "geom", **attrs)


def apply_project(runtime, body, kind):
    """Compile off the physics lock; commit only complete, compatible worlds."""
    if not runtime.project_lock.acquire(blocking=False):
        raise RuntimeError("another project update is in progress; retry")
    try:
        if body.get("vm_name") != __import__("os").environ.get(kind.upper() + "_VM_NAME", "") or body.get("source_digest") != runtime.source_digest:
            raise ValueError("project target does not match the managed runtime identity")
        project_id = body.get("project_id")
        if not isinstance(project_id, str) or not re.fullmatch(r"[a-f0-9]{32}", project_id):
            raise ValueError("project_id must be a 32-character hex identifier")
        current = runtime.project_status
        if current["project_id"] not in {None, project_id}:
            raise ValueError("VM is linked to a different project; use its local folder or restart the runtime to relink")
        if body.get("base_revision", "") != current["revision"]:
            raise ValueError("project revision changed; another watcher may be active, restart watch to reconcile")
        if "error" in body:
            current.update(error=str(body["error"])[:1000], watcher_seen=time.time())
            return {**current, "changed": False}
        if body.get("heartbeat") is True:
            current["watcher_seen"] = time.time()
            if body.get("clear_error") is True:
                current["error"] = None
            return {**current, "changed": False}
        try:
            manifest, files, revision = decode_project(body.get("files"), kind)
            if revision == current["revision"]:
                current.update(error=None, watcher_seen=time.time())
                return {**current, "changed": False}
            from .sensors import instrumented_model
            from .simulation import Simulation
            from .scene import BrowserScene
            with tempfile.TemporaryDirectory(prefix="wendy-scene-") as temporary:
                paths = {}
                for index, (name, content) in enumerate(files.items()):
                    if name != "scene.json":
                        from pathlib import Path
                        path = Path(temporary) / f"mesh-{index}.obj"
                        path.write_bytes(content)
                        paths[name] = path
                model = instrumented_model(runtime.sim.assets, environment=(manifest, paths))
                candidate = Simulation(runtime.sim.assets, model=model)
                visual = model
                if kind == "go2" and runtime.visual_detail == "balanced":
                    visual = instrumented_model(runtime.sim.assets, visual_mesh_dir=runtime.sim.assets / "visuals",
                                                environment=(manifest, paths))
            # Test actual physics and browser export before touching the live world.
            candidate.step()
            if not np.isfinite(candidate.data.qpos).all() or not np.isfinite(candidate.data.qvel).all():
                raise ValueError("scene produced non-finite physics")
            for geom in range(model.ngeom):
                name = mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_GEOM, geom) or ""
                if name.startswith("project_"):
                    radius = float(model.geom_rbound[geom])
                    if np.linalg.norm(model.geom_pos[geom, :2]) < 1.2 + radius:
                        raise ValueError(f"{name}: surface overlaps the robot's 1.2m spawn clearance")
            candidate.reset()
            candidate.pause()
            scene = BrowserScene(model, **({"visual_model": visual} if kind == "go2" else {}))
            if len(scene.json) > 16 * 1024 * 1024:
                raise ValueError("compiled browser scene exceeds 16 MiB")
            with runtime.observation_lock, runtime.lock:
                runtime.ensure_running()
                if runtime.error:
                    raise RuntimeError("recover the runtime fault before applying a project")
                if runtime.ros_commands:
                    runtime.ros_commands.revoke()
                candidate.epoch = runtime.sim.epoch + 1
                runtime.sim = candidate
                runtime.visual_model = visual
                runtime.scene = scene
                runtime.observation_generation += 1
                runtime.camera_frame = runtime.camera_jpeg = None
                runtime.pending_command = None
                current.update(project_id=project_id, revision=revision, error=None,
                               watcher_seen=time.time(), epoch=candidate.epoch)
            return {**current, "changed": True}
        except Exception as exc:
            current.update(error=str(exc)[:1000], watcher_seen=time.time())
            raise ValueError(f"project rejected; previous scene retained: {exc}") from exc
    finally:
        runtime.project_lock.release()
