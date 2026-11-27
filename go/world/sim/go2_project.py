"""Convert selected static World geometry into an existing Go2 project overlay.

Native compilation is Linux-only and runs in a resource-limited child. This is
not a general MJCF importer, an operating-system security sandbox, or permission
to activate a simulator. No robot commands or HTTP requests are made.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import sys
import tempfile

SCHEMA = "wendy.world.go2-project-conversion.v1"
MAX_RESULT = 2 << 20
MAX_REQUEST = 64 << 10
MAX_MODEL_ITEMS = 4096
LIMITS = {"address_space_bytes": 2 << 30, "cpu_seconds": 10,
          "wall_seconds": 15, "result_bytes": MAX_RESULT}
SHAPES = {"box": 3, "sphere": 1, "cylinder": 2, "capsule": 2, "ellipsoid": 3}


class ConversionError(ValueError):
    pass


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=True, allow_nan=False).encode() + b"\n"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def request(package, names, run_id):
    if (not isinstance(run_id, str) or
            not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", run_id)):
        raise ConversionError("explicit bounded simulation run ID required")
    if (not isinstance(names, list) or not 1 <= len(names) <= 64 or
            any(not isinstance(n, str) or not n or len(n) > 256 or
                any(ord(c) < 32 or ord(c) > 126 for c in n) for n in names) or
            len(set(names)) != len(names)):
        raise ConversionError("select 1 to 64 distinct bounded geom names")
    value = {"package": str(Path(package).absolute()), "names": sorted(names), "run_id": run_id}
    if len(encoded(value)) > MAX_REQUEST:
        raise ConversionError("conversion request exceeds bound")
    return value


def _static(model, body):
    while body:
        if model.body_jntnum[body] or model.body_mocapid[body] >= 0:
            return False
        body = int(model.body_parentid[body])
    return True


def _numbers(values):
    result = [float(x) for x in values]
    if not all(math.isfinite(x) for x in result):
        raise ConversionError("nonfinite compiled geometry")
    return [0.0 if x == 0 else x for x in result]


def _compile(value):
    # Called only after the worker installs all resource limits. Import no native
    # dependency in the parent. Source asset closure comes from the shared loader.
    from sim.run import compile_model, load_package
    import mujoco
    import numpy as np

    if mujoco.__version__ != "4.0.0":
        raise ConversionError("MuJoCo 4.0.0 is required")
    package = load_package(value["package"])
    model = compile_model(package, mujoco)
    if any(int(getattr(model, k)) > MAX_MODEL_ITEMS for k in
           ("ngeom", "nbody", "njnt", "nu", "nsensor", "ncam", "nmat", "ntex")):
        raise ConversionError("compiled model exceeds report item bound")
    data = mujoco.MjData(model)
    mujoco.mj_forward(model, data)
    name = lambda kind, i: mujoco.mj_id2name(model, kind, i) or ""
    types = {int(getattr(mujoco.mjtGeom, "mjGEOM_" + k.upper())): k for k in SHAPES}
    names = {name(mujoco.mjtObj.mjOBJ_GEOM, i): i for i in range(model.ngeom)}
    geoms, selected, losses = [], [], []
    for selected_name in value["names"]:
        if selected_name not in names:
            raise ConversionError("selected geom not found: " + selected_name)
        i = names[selected_name]
        if not _static(model, int(model.geom_bodyid[i])):
            raise ConversionError("selected geom has moving or mocap ancestry: " + selected_name)
        shape = types.get(int(model.geom_type[i]))
        if shape is None:
            raise ConversionError("selected geom type unsupported: " + selected_name)
        size = _numbers(model.geom_size[i][:SHAPES[shape]])
        position = _numbers(data.geom_xpos[i])
        quat = np.zeros(4)
        mujoco.mju_mat2Quat(quat, data.geom_xmat[i])
        quaternion = _numbers(quat)
        # q and -q represent the same rotation; choose a deterministic spelling.
        if next((x for x in quaternion if x != 0), 1) < 0:
            quaternion = [-x if x else 0.0 for x in quaternion]
        rgba = _numbers(model.geom_rgba[i])
        if (min(size) <= 0 or max(size) > 4 or max(map(abs, position)) > 20 or
                min(rgba) < 0 or max(rgba) > 1 or rgba[3] <= 0 or
                not math.isclose(sum(x*x for x in quaternion), 1, abs_tol=1e-5)):
            raise ConversionError("selected geom exceeds Go2 project bounds: " + selected_name)
        radius = float(model.geom_rbound[i])
        if not math.isfinite(radius) or math.hypot(*position[:2]) < 1.2 + radius:
            raise ConversionError("selected geom overlaps Go2 spawn clearance: " + selected_name)
        target_name = "world_geom_%03d" % len(geoms)
        geom = {"name": target_name, "type": shape, "size": size,
                "position": position, "quaternion": quaternion, "rgba": rgba}
        geoms.append(geom)
        selected.append({"source_geom_index": i, "source_geom_name": selected_name,
                         "project_geom_name": target_name, **{k: v for k, v in geom.items() if k != "name"}})
        losses.append({"source_geom_name": selected_name,
                       "material_index": int(model.geom_matid[i]),
                       "material_name": name(mujoco.mjtObj.mjOBJ_MATERIAL, int(model.geom_matid[i])) if model.geom_matid[i] >= 0 else "",
                       "source_contact": {"friction": _numbers(model.geom_friction[i]),
                                          "solref": _numbers(model.geom_solref[i]),
                                          "solimp": _numbers(model.geom_solimp[i]),
                                          "contype": int(model.geom_contype[i]),
                                          "conaffinity": int(model.geom_conaffinity[i]),
                                          "condim": int(model.geom_condim[i]),
                                          "margin": float(model.geom_margin[i]),
                                          "gap": float(model.geom_gap[i]),
                                          "priority": int(model.geom_priority[i])},
                       "target_contact": "Go2 runtime MJCF defaults; source contact properties are not transferred",
                       "appearance": "compiled geom RGBA only; material, texture, lighting and camera appearance are not preserved"})
    selected_ids = {g["source_geom_index"] for g in selected}
    excluded = {"geoms": [{"index": i, "name": name(mujoco.mjtObj.mjOBJ_GEOM, i),
                           "reason": "not explicitly selected"} for i in range(model.ngeom) if i not in selected_ids]}
    for key, obj, count in [("bodies", "BODY", model.nbody), ("joints", "JOINT", model.njnt),
                            ("actuators", "ACTUATOR", model.nu), ("sensors", "SENSOR", model.nsensor),
                            ("cameras", "CAMERA", model.ncam), ("materials", "MATERIAL", model.nmat),
                            ("textures", "TEXTURE", model.ntex)]:
        excluded[key] = [{"index": i, "name": name(getattr(mujoco.mjtObj, "mjOBJ_" + obj), i),
                          "reason": "not transferred; target uses its existing Go2 runtime"}
                         for i in range(1 if key == "bodies" else 0, count)]
    scene = {"version": 1, "profile": "go2", "geoms": geoms}
    raw = encoded(scene)
    report = {"schema": SCHEMA, "domain": "simulation", "operational": False,
              "source": {"package_sha256": package["_digest"], "site_id": package["site_id"],
                         "package_revision": package["package_revision"], "frame_revision": package["frame_revision"],
                         "provenance": package["provenance"], "run_id": value["run_id"],
                         "clock_domain": "simulation:" + value["run_id"],
                         "geographic_anchor": package["geographic_anchor"], "local_frame": package["local_frame"],
                         "model_to_local_position": package["mujoco"]["to_local_position"]},
              "target": {"profile": "go2", "coordinates": {"units": "metres", "up": "Z", "quaternion": "wxyz"},
                         "project_revision": digest(b"scene.json\0" + raw + b"\0"),
                         "files": [{"path": "scene.json", "size": len(raw), "sha256": digest(raw)}]},
              "selection": selected, "exclusions": excluded, "selected_property_differences": losses,
              "differences": ["Static primitives only; no source robot, dynamics, controller or sensors transferred",
                              "Overlay retains the existing Go2 sandbox walls, floor, obstacle, robot and virtual sensors",
                              "Spawn clearance alone does not prove absence of overlap with the existing sandbox or other imported geoms",
                              "Go2 project application resets and pauses physics and revokes previous command ownership",
                              "Source engine world positions and rotations are preserved in metres, Z-up; this is not the Y-up site frame",
                              "No real office, surveyed placement, calibrated collision or physical robot parity is established",
                              "All unselected geometry is excluded, including source floor planes",
                              "Gas model, scenario, initial state, forces, lights and source simulation settings are not transferred"],
              "compiler": {"name": "MuJoCo", "version": mujoco.__version__, "limits": LIMITS,
                           "isolation": "Linux process resource limits; not an OS security sandbox"},
              "source_import_report": package["_import_report"]}
    result = {"scene": scene, "report": report}
    if len(encoded(result)) > MAX_RESULT:
        raise ConversionError("conversion report exceeds output bound")
    return result


def _worker():
    if not sys.platform.startswith("linux"):
        return 2
    import resource
    for which, limit in ((resource.RLIMIT_AS, LIMITS["address_space_bytes"]),
                         (resource.RLIMIT_CPU, LIMITS["cpu_seconds"]),
                         (resource.RLIMIT_FSIZE, MAX_RESULT), (resource.RLIMIT_CORE, 0)):
        resource.setrlimit(which, (limit, limit))
    raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
    try:
        if len(raw) > MAX_REQUEST:
            raise ConversionError("conversion request exceeds bound")
        value = json.loads(raw)
        checked = request(value["package"], value["names"], value["run_id"])
        sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
        result = _compile(checked)
    except Exception as exc:
        # Error text is bounded; do not include native diagnostic output wholesale.
        result = {"error": str(exc)[:512], "schema": SCHEMA}
    sys.stdout.buffer.write(encoded(result))
    return 0


def convert(package, names, run_id):
    value = request(package, names, run_id)
    if not sys.platform.startswith("linux"):
        raise ConversionError("native conversion requires Linux resource-limited worker")
    with tempfile.TemporaryDirectory(prefix="world-go2-compile-") as tmp:
        tmp = Path(tmp)
        # Regular files plus RLIMIT_FSIZE bound native output without an unlimited
        # communicate() buffer or pipe deadlock. Nothing is read until child exit.
        with (tmp / "input").open("w+b") as source, (tmp / "output").open("w+b") as output, (tmp / "error").open("w+b") as error:
            source.write(encoded(value)); source.seek(0)
            env = {"PATH": os.defpath, "LANG": "C.UTF-8", "OPENBLAS_NUM_THREADS": "1", "OMP_NUM_THREADS": "1", "MKL_NUM_THREADS": "1"}
            child = subprocess.Popen([sys.executable, "-I", str(Path(__file__).resolve()), "--worker"],
                                     stdin=source, stdout=output, stderr=error, env=env, start_new_session=True,
                                     cwd=str(Path(__file__).resolve().parents[1]))
            try:
                child.wait(timeout=LIMITS["wall_seconds"])
            except subprocess.TimeoutExpired:
                raise ConversionError("native compilation exceeded wall deadline") from None
            finally:
                if child.poll() is None:
                    try:
                        os.killpg(child.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    child.wait()
            if child.returncode != 0:
                raise ConversionError("native compilation failed or exceeded resource bound")
            output.seek(0); raw = output.read(MAX_RESULT + 1)
            if len(raw) > MAX_RESULT:
                raise ConversionError("native result exceeds bound")
    try:
        result = json.loads(raw)
        if "error" in result:
            raise ConversionError(result["error"])
        if set(result) != {"scene", "report"} or result["report"]["schema"] != SCHEMA:
            raise ValueError()
        return result
    except (ValueError, KeyError, TypeError) as exc:
        if isinstance(exc, ConversionError):
            raise
        raise ConversionError("invalid native conversion result") from None


def install(result, destination):
    """Create a fresh output only; an interrupted write can leave invalid partial files."""
    destination = Path(destination)
    if ".." in destination.parts:
        raise ConversionError("output path must not contain parent traversal")
    # The destination parent is trusted local storage, but stable symlink aliases
    # are refused. This is not protection from a privileged directory replacement.
    for parent in reversed(destination.absolute().parents):
        info = parent.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise ConversionError("output parent must be a real directory without symlink aliases")
    destination.mkdir(mode=0o700)  # Never overwrite, merge or follow an existing output.
    project = destination / "project"
    project.mkdir(mode=0o700)
    for path, value in ((project / "scene.json", result["scene"]), (destination / "conversion.json", result["report"])):
        raw = encoded(value)
        if len(raw) > MAX_RESULT:
            raise ConversionError("output exceeds bound")
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as out:
            out.write(raw); out.flush(); os.fsync(out.fileno())
    for directory in (project, destination, destination.parent):
        fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True)
    parser.add_argument("--geom", action="append", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--out", required=True)
    args = parser.parse_args(argv)
    result = convert(args.package, args.geom, args.run_id)
    install(result, args.out)
    print(json.dumps({"schema": SCHEMA, "result": "converted", "operational": False,
                      "project_revision": result["report"]["target"]["project_revision"],
                      "report_sha256": digest(encoded(result["report"]))}, sort_keys=True))


if __name__ == "__main__":
    if sys.argv[1:] == ["--worker"]:
        raise SystemExit(_worker())
    try:
        main()
    except (ConversionError, OSError) as exc:
        print("conversion refused: " + str(exc), file=sys.stderr)
        raise SystemExit(1)
