"""Local MuJoCo 4 worker for the private SwiftUI/SceneKit office demo.

The protocol uses atomic JSON files inside one private run directory. Geometry
is compiled MuJoCo state, with XY floor / Z up and row-major local-to-world
rotation matrices. Sizes are MuJoCo half-sizes: box XYZ, ellipsoid radii XYZ,
sphere radius X, cylinder/capsule radius X and half-length Y along local Z.
Plane XY sizes are finite display extents; collision planes remain infinite.

The shell reads geometry.json once and state.json as its sequence advances.
Commands are commands/000001.json onward, with exactly schema, run_id,
sequence, action and target. Actions are start, stop, toggle_edge,
toggle_available and select_map. Start/stop have an empty target. Receipts use
the same numbered filename. A missing number blocks later commands. No
command, mission or process is resumed after restart. The shell owns overlays
and screenshots; selecting a replica only changes which existing map is read.

This is synthetic local physics and an in-process modeled radio mesh, not
hardware control or native Wendy mesh verification. Unix time is display-only;
the snapshot's simulation clock is unchanged. A shell must mark stale state
when its worker sequence stops advancing, regardless of authorized=true.
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
import threading
import time
import uuid

from sim.collaborative_runtime import SceneRuntime


MAX_JSON_BYTES = 8 * 1024 * 1024
MAX_COMMAND_BYTES = 4096
MAX_COMMANDS = 4096
MAX_GEOMS = 12000
COMMAND_SCHEMA = "wendy.macos-demo.command.v1"
STATE_SCHEMA = "wendy.macos-demo.state.v1"
ACTIONS = frozenset(("start", "stop", "toggle_edge", "toggle_available", "select_map"))
FRAME = {"units": "metres", "up": "Z", "handedness": "right",
         "rotation": "row-major local-to-world"}


def json_bytes(value, limit=MAX_JSON_BYTES):
    data = (json.dumps(value, allow_nan=False, sort_keys=True, separators=(",", ":")) + "\n").encode()
    if len(data) > limit:
        raise ValueError("JSON size limit exceeded")
    return data


def _unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON field")
        result[key] = value
    return result


class SessionFiles:
    """Anchored private output directory; exclusive marker fences restarts."""
    def __init__(self, directory, run_id):
        self.fd = self.commands_fd = self.receipts_fd = None
        path = Path(directory)
        self.fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            self._check(os.fstat(self.fd), directory=True)
            self.write("bridge-session.json", {"schema": "wendy.macos-demo.session.v1",
                "run_id": run_id, "pid": os.getpid(), "restart": "refused"})
            for name in ("commands", "receipts"):
                os.mkdir(name, 0o700, dir_fd=self.fd)
            self.commands_fd = os.open("commands", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=self.fd)
            self.receipts_fd = os.open("receipts", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=self.fd)
        except BaseException:
            self.close()
            raise

    @staticmethod
    def _check(info, directory=False):
        expected = stat.S_ISDIR if directory else stat.S_ISREG
        if not expected(info.st_mode) or info.st_uid != os.geteuid() or info.st_mode & 0o077:
            raise ValueError("session path must be private and owned")

    def write(self, name, value, *, replace=False, receipt=False):
        data = json_bytes(value)
        directory = self.receipts_fd if receipt else self.fd
        temporary = ".bridge-" + uuid.uuid4().hex
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=directory)
        try:
            with os.fdopen(fd, "wb") as stream:
                stream.write(data)
                stream.flush()
            if replace:
                self._check(os.stat(name, dir_fd=directory, follow_symlinks=False))
                os.replace(temporary, name, src_dir_fd=directory, dst_dir_fd=directory)
            else:
                os.link(temporary, name, src_dir_fd=directory, dst_dir_fd=directory, follow_symlinks=False)
        finally:
            try:
                os.unlink(temporary, dir_fd=directory)
            except FileNotFoundError:
                pass
        return hashlib.sha256(data).hexdigest()

    def command(self, sequence):
        name = f"{sequence:06d}.json"
        try:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=self.commands_fd)
        except FileNotFoundError:
            return None
        try:
            info = os.fstat(fd)
            self._check(info)
            if info.st_size > MAX_COMMAND_BYTES:
                raise ValueError("command too large")
            with os.fdopen(fd, "rb", closefd=False) as stream:
                raw = stream.read(MAX_COMMAND_BYTES + 1)
            if len(raw) > MAX_COMMAND_BYTES:
                raise ValueError("command too large")
            return json.loads(raw, object_pairs_hook=_unique_object,
                              parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))
        finally:
            os.close(fd)

    def stop_requested(self):
        try:
            info = os.stat("stop", dir_fd=self.fd, follow_symlinks=False)
        except FileNotFoundError:
            return False
        # A bad sentinel fails closed too, without following or reading it.
        if not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid():
            raise ValueError("invalid stop sentinel")
        return True

    def close(self):
        for name in ("commands_fd", "receipts_fd", "fd"):
            fd = getattr(self, name, None)
            if fd is not None:
                os.close(fd)
                setattr(self, name, None)


def _floats(value):
    result = [float(v) for v in value]
    if not all(math.isfinite(v) for v in result):
        raise ValueError("nonfinite compiled scene")
    return result


def _transform(runtime, geom_id):
    return {"id": geom_id, "position": _floats(runtime.data.geom_xpos[geom_id]),
            "rotation": _floats(runtime.data.geom_xmat[geom_id])}


def _presentation(config):
    source = config.get("presentation", {})
    result = {}
    for key in ("name", "subtitle"):
        if isinstance(source.get(key), str) and len(source[key]) <= 300:
            result[key] = source[key]
    labels = source.get("labels", [])
    if not isinstance(labels, list) or len(labels) > 100:
        raise ValueError("invalid presentation labels")
    result["labels"] = []
    for label in labels:
        position = label.get("position") if isinstance(label, dict) else None
        text = label.get("text") if isinstance(label, dict) else None
        if (not isinstance(text, str) or not 0 < len(text) <= 100
                or not isinstance(position, list) or len(position) != 3
                or any(type(v) not in (int, float) for v in position)):
            raise ValueError("invalid presentation label")
        result["labels"].append({"text": text, "position": _floats(position)})
    return result


def geometry(runtime):
    """Export compiled transforms, never infer collision sizes from XML text."""
    model, mj = runtime.model, runtime.mj
    if not 0 < model.ngeom <= MAX_GEOMS:
        raise ValueError("geometry count exceeds bound")
    kinds = {int(getattr(mj.mjtGeom, "mjGEOM_" + name.upper())): name
             for name in ("plane", "sphere", "capsule", "ellipsoid", "cylinder", "box")}
    robots = {binding["body"]: name for name, binding in runtime.bindings.items()}
    geoms, dynamic_ids = [], []
    textures = {}
    for geom_id in range(model.ngeom):
        kind = kinds.get(int(model.geom_type[geom_id]))
        if kind is None:
            raise ValueError("unsupported compiled geometry")
        body = int(model.geom_bodyid[geom_id])
        ancestor, robot_id, dynamic = body, None, False
        while ancestor:
            robot_id = robots.get(ancestor, robot_id)
            dynamic |= bool(model.body_dofnum[ancestor])
            ancestor = int(model.body_parentid[ancestor])
        material_id = int(model.geom_matid[geom_id])
        rgba = _floats(model.geom_rgba[geom_id])
        material = {"emission": 0., "specular": .5, "shininess": .5, "reflectance": 0.}
        if material_id >= 0:
            if all(abs(a - b) < 1e-6 for a, b in zip(rgba, (.5, .5, .5, 1.))):
                rgba = _floats(model.mat_rgba[material_id])
            for name in material:
                material[name] = float(getattr(model, "mat_" + name)[material_id])
            # Only color textures affect this fallback. Normal/roughness maps
            # must not be mistaken for a surface color.
            for role in (mj.mjtTextureRole.mjTEXROLE_RGB, mj.mjtTextureRole.mjTEXROLE_RGBA):
                texture_id = int(model.mat_texid[material_id, int(role)])
                if texture_id < 0:
                    continue
                if texture_id not in textures:
                    count = int(model.tex_width[texture_id]) * int(model.tex_height[texture_id])
                    channels, address = int(model.tex_nchannel[texture_id]), int(model.tex_adr[texture_id])
                    if count > 4 * 1024 * 1024 or channels not in (3, 4):
                        raise ValueError("unsupported compiled color texture")
                    pixels = model.tex_data[address:address + count * channels].reshape(count, channels)
                    textures[texture_id] = _floats(pixels[:, :3].mean(axis=0) / 255.)
                material["texture_average_rgb"] = textures[texture_id]
                material["texture_approximation"] = True
                break
        geom = {**_transform(runtime, geom_id), "name": mj.mj_id2name(model, mj.mjtObj.mjOBJ_GEOM, geom_id) or f"geom-{geom_id}",
                "type": kind, "size": _floats(model.geom_size[geom_id]), "rgba": rgba,
                "body_id": body, "body_name": mj.mj_id2name(model, mj.mjtObj.mjOBJ_BODY, body) or "world",
                "robot_id": robot_id, "dynamic": bool(dynamic), "group": int(model.geom_group[geom_id]),
                "material_name": mj.mj_id2name(model, mj.mjtObj.mjOBJ_MATERIAL, material_id) if material_id >= 0 else None,
                "material": material}
        geoms.append(geom)
        if dynamic:
            dynamic_ids.append(geom_id)
    lights = [{"id": i, "type": int(model.light_type[i]), "active": bool(model.light_active[i]),
               "position": _floats(runtime.data.light_xpos[i]), "direction": _floats(runtime.data.light_xdir[i]),
               "ambient": _floats(model.light_ambient[i]), "diffuse": _floats(model.light_diffuse[i]),
               "specular": _floats(model.light_specular[i]), "cast_shadow": bool(model.light_castshadow[i])}
              for i in range(model.nlight)]
    result = {"schema": "wendy.macos-demo.geometry.v1", "run_id": runtime.run_id,
              "model_sha256": runtime.config["model"]["sha256"], "coordinate_frame": FRAME,
              "engine": "MuJoCo 4.0.0", "synthetic": True, "geoms": geoms, "lights": lights,
              "presentation": _presentation(runtime.config),
              "semantics": "Compiled ground-truth geometry; map voxels are selected-replica occupied ray hits"}
    json_bytes(result)
    return result, dynamic_ids


class Bridge:
    def __init__(self, runtime, files, fusion=None):
        if runtime.authorized or runtime.stopped:
            raise ValueError("bridge requires a fresh unstarted runtime")
        self.runtime, self.files, self.fusion = runtime, files, fusion
        self.map_node = "hub"
        self.command_sequence = self.sequence = 0
        compiled, self.dynamic_ids = geometry(runtime)
        self.geometry_sha256 = files.write("geometry.json", compiled)
        self.export()
        files.write("bridge-ready.json", {"schema": "wendy.macos-demo.ready.v1", "run_id": runtime.run_id,
            "pid": os.getpid(), "geometry_sha256": self.geometry_sha256, "state_sequence": self.sequence,
            "max_json_bytes": MAX_JSON_BYTES, "max_commands": MAX_COMMANDS, "state_interval_ms": 100})

    def export(self, *, closed=False, reason=None):
        self.sequence += 1
        snapshot = self.runtime.snapshot(self.map_node)
        if self.fusion is not None:
            snapshot['fusion'] = self.fusion.snapshot(self.map_node)
        state = {"schema": STATE_SCHEMA, "run_id": self.runtime.run_id, "sequence": self.sequence,
                 "updated_unix_ms": time.time_ns() // 1000000, "worker_status": "closed" if closed else "running",
                 "exit_reason": reason, "last_command_sequence": self.command_sequence,
                 "snapshot": snapshot, "dynamic_geoms": [_transform(self.runtime, i) for i in self.dynamic_ids]}
        self.files.write("state.json", state, replace=self.sequence > 1)
        if closed:
            self.files.write("final-state.json", state)
        return state

    def commands(self):
        # At most eight operations between ticks; a queued burst cannot starve
        # physics or create an unbounded render/command loop.
        for _ in range(min(8, MAX_COMMANDS - self.command_sequence)):
            sequence = self.command_sequence + 1
            try:
                command = self.files.command(sequence)
                if command is None:
                    break
            except (OSError, ValueError, UnicodeError, RecursionError):
                command = {}
            action = command.get("action") if isinstance(command, dict) else None
            error = self._validate(command, sequence)
            accepted = False
            if error is None:
                target = command["target"]
                try:
                    if action == "start":
                        accepted = self.runtime.start_mission()
                        error = None if accepted else "start_refused"
                    elif action == "stop":
                        self.runtime.stop_mission()
                        accepted = True
                    elif action == "toggle_edge":
                        self.runtime.toggle_edge(target)
                        accepted = True
                    elif action == "toggle_available":
                        self.runtime.toggle_available(target)
                        accepted = True
                    else:
                        self.map_node = target
                        accepted = True
                except Exception:
                    self.runtime.stop_mission()
                    error = "unknown_outcome"
            self.command_sequence = sequence
            self.files.write(f"{sequence:06d}.json", {"schema": "wendy.macos-demo.command-result.v1",
                "run_id": self.runtime.run_id, "sequence": sequence,
                "action": action if isinstance(action, str) and action in ACTIONS else "invalid",
                "accepted": bool(accepted), "error": error}, receipt=True)
            if error == "unknown_outcome":
                raise RuntimeError("command outcome unknown")

    def _validate(self, value, sequence):
        if (not isinstance(value, dict) or set(value) != {"schema", "run_id", "sequence", "action", "target"}
                or value["schema"] != COMMAND_SCHEMA or value["run_id"] != self.runtime.run_id
                or type(value["sequence"]) is not int or value["sequence"] != sequence
                or not isinstance(value["action"], str) or value["action"] not in ACTIONS
                or not isinstance(value["target"], str) or len(value["target"]) > 100):
            return "invalid_command"
        action, target = value["action"], value["target"]
        if action in ("start", "stop"):
            return None if target == "" else "invalid_target"
        if action == "toggle_edge":
            return None if target in self.runtime.mesh.edges or target == self.runtime.mesh.internet["id"] else "unknown_target"
        if action == "toggle_available":
            return None if target in self.runtime.links else "unknown_target"
        return None if target in self.runtime.mesh.maps else "unknown_target"


def run(scene, coordinator, out, run_id, duration, *, stop_event=None, runtime_factory=SceneRuntime,
        fusion_binary=None):
    if (not isinstance(run_id, str) or re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,99}", run_id) is None
            or type(duration) not in (int, float) or not math.isfinite(duration) or not 0 < duration <= 3600):
        raise ValueError("invalid bounded session")
    stop_event = stop_event or threading.Event()
    runtime = bridge = fusion = None
    files = SessionFiles(out, run_id)
    reason, phase_before_close, failure = "duration", None, False
    try:
        runtime = runtime_factory(scene, coordinator, run_id)
        if fusion_binary is not None:
            from sim.office_fusion import OfficeFusion
            fusion = OfficeFusion(runtime, fusion_binary, out)
        bridge = Bridge(runtime, files, fusion)
        start = previous = last_export = time.monotonic()
        while time.monotonic() - start < duration:
            if stop_event.is_set():
                reason = "signal"
                break
            if files.stop_requested():
                reason = "stop_file"
                break
            bridge.commands()
            now = time.monotonic()
            runtime.tick(max(.000001, min(.02, now - previous)))
            if fusion is not None:
                fusion.tick()
            previous = now  # Never catch up a stalled worker's elapsed wall time.
            if runtime.fault:
                reason, failure = "runtime_fault", True
                break
            if now - last_export >= .1:
                bridge.export()
                last_export = now
            stop_event.wait(.01)
    except Exception:
        reason, failure = "bridge_fault", True
    finally:
        try:
            if fusion is not None:
                fusion.close()
            if runtime is not None:
                phase_before_close = runtime.phase
                runtime.stop_mission()
                runtime.close()
            if bridge is not None:
                bridge.export(closed=True, reason=reason)
            files.write("bridge-result.json", {"schema": "wendy.macos-demo.result.v1", "run_id": run_id,
                "status": "failed" if failure else "closed", "exit_reason": reason,
                "mission_phase_before_close": phase_before_close,
                "completed": runtime.completed if runtime else 0,
                "total": len(runtime.config["jobs"]) if runtime else 0,
                "last_command_sequence": bridge.command_sequence if bridge else 0,
                "coordinator_closed": bool(runtime and runtime.closed)})
        finally:
            # Even an output failure must close our owned coordinator.
            if runtime is not None and not runtime.closed:
                runtime.close()
            files.close()
    return 1 if failure else 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scene", type=Path, required=True)
    parser.add_argument("--coordinator", type=Path, required=True)
    parser.add_argument("--fusion", type=Path, help="ordinary fusion app for live office sensors")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--duration", type=float, default=1800)
    args = parser.parse_args(argv)
    stopped = threading.Event()
    for sig in (signal.SIGTERM, signal.SIGINT):
        signal.signal(sig, lambda *_: stopped.set())
    try:
        return run(args.scene, args.coordinator, args.out, args.run_id, args.duration,
                   stop_event=stopped, fusion_binary=args.fusion)
    except (OSError, ValueError, RuntimeError):
        # No scene paths or arbitrary subprocess errors in terminal output.
        print("Native demo bridge refused or failed; inspect its bounded session receipt", flush=True)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
