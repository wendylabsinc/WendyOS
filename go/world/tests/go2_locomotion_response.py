"""Offline pinned-policy characterization; never a live motion qualification."""
import argparse
import hashlib
import importlib
import json
import math
import os
from pathlib import Path
import stat
import struct
import sys

SCHEMA = "wendy.world.go2-offline-pins.v1"
SOURCE_DIGEST = "sha256:2869d018ae55e4c83318329bb20cc670e16f6f62838989bb5192829002af8d2b"
POLICY = "go2-moe-cts-164k-0.6715-v1"
ENGINES = {"mujoco": "4.0.0", "onnxruntime": "1.23.2"}
SPEEDS = (0.0, 0.05, 0.1, 0.2, 0.4)
MATRICES = {"baseline": SPEEDS, "commissioning-window": (0.25, 0.30, 0.35)}
STEP_NS = 2_000_000
PHASES = (("warmup", 1500), ("command", 10000), ("zero", 2500))
MAX_INPUT = 128 << 20


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def strict(raw):
    def pairs(items):
        out = {}
        for k, v in items:
            if k in out:
                raise ValueError("duplicate JSON key")
            out[k] = v
        return out
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=lambda _: (_ for _ in ()).throw(ValueError("nonfinite JSON")))


def read(path, limit=MAX_INPUT):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or not 0 <= info.st_size <= limit:
            raise ValueError("input must be a bounded regular file")
    except BaseException:
        os.close(fd)
        raise
    with os.fdopen(fd, "rb") as stream:
        raw = stream.read(limit + 1)
    if len(raw) > limit:
        raise ValueError("input exceeded bound")
    return raw


def relative(root, name):
    if not isinstance(name, str) or not name or len(name) > 512 or "\\" in name:
        raise ValueError("invalid inventory path")
    p = Path(name)
    if p.is_absolute() or any(x in (".", "..") for x in p.parts) or p.as_posix() != name:
        raise ValueError("invalid inventory path")
    for i in range(1, len(p.parts) + 1):
        if (root.joinpath(*p.parts[:i])).is_symlink():
            raise ValueError("symlink inventory path")
    return root / p


def check_inventory(root, inventory):
    if not isinstance(inventory, dict) or not 1 <= len(inventory) <= 256:
        raise ValueError("inventory size")
    output = {}
    for name, expected in sorted(inventory.items()):
        if not isinstance(expected, dict) or set(expected) != {"sha256", "bytes"} or type(expected["bytes"]) is not int:
            raise ValueError("inventory entry")
        raw = read(relative(root, name))
        item = {"sha256": sha(raw), "bytes": len(raw)}
        if item != expected:
            raise ValueError("inventory digest mismatch")
        output[name] = item
    return output


def validate_inputs(source, assets, pins_path, mujoco_version="4.0.0"):
    if mujoco_version not in ("4.0.0", "3.3.7"):
        raise ValueError("unsupported MuJoCo version")
    engines = {**ENGINES, "mujoco": mujoco_version}
    raw = read(pins_path, 256 << 10)
    pins = strict(raw)
    if not isinstance(pins, dict) or set(pins) != {"schema", "source_digest", "source_files", "asset_files", "engines", "policy_bundle"}:
        raise ValueError("pin schema")
    if pins["schema"] != SCHEMA or pins["source_digest"] != SOURCE_DIGEST or pins["policy_bundle"] != POLICY or pins["engines"] != engines:
        raise ValueError("unsupported source, policy or engines")
    source = source.resolve(strict=True)
    assets = assets.resolve(strict=True)
    files = check_inventory(source, pins["source_files"])
    actual = set()
    for p in source.rglob("*"):
        if p.is_symlink():
            raise ValueError("source symlink")
        if p.is_file():
            actual.add(p.relative_to(source).as_posix())
    if actual != set(files):
        raise ValueError("source inventory is not complete")
    h = hashlib.sha256(b"wendy-go2-source-v1\0")
    for name in sorted(files):
        for value in (name.encode(), read(source / name)):
            h.update(struct.pack(">Q", len(value)))
            h.update(value)
    if "sha256:" + h.hexdigest() != SOURCE_DIGEST:
        raise ValueError("canonical source digest mismatch")
    lock = strict(read(source / "assets.lock.json", 256 << 10))
    expected = {x["path"]: {"sha256": x["sha256"], "bytes": x["size"]} for x in lock["files"]}
    if lock["bundle"] != POLICY or pins["asset_files"] != expected:
        raise ValueError("assets differ from pinned source lock")
    asset_files = check_inventory(assets, expected)
    return source, assets, {"pins_sha256": sha(raw), "source_digest": SOURCE_DIGEST, "source_files": files,
                            "asset_files": asset_files, "engines": engines, "policy_bundle": POLICY}


class VirtualClock:
    def __init__(self):
        self.ticks = 0

    def now(self):
        return self.ticks * STEP_NS / 1e9

    def advance(self):
        self.ticks += 1


def measurement(sim, np):
    # mj_step integrates qpos; cached xmat can still describe the pre-step pose.
    # Derive attitude from this sample's qpos so all reported vectors agree.
    w,x,y,z = [float(v) for v in sim.data.qpos[3:7]]
    n = math.sqrt(w*w+x*x+y*y+z*z)
    if not math.isfinite(n) or n <= 0:
        raise ValueError("invalid sampled quaternion")
    w,x,y,z = w/n,x/n,y/n,z/n
    rotation = np.array([[1-2*(y*y+z*z),2*(x*y-z*w),2*(x*z+y*w)],
                         [2*(x*y+z*w),1-2*(x*x+z*z),2*(y*z-x*w)],
                         [2*(x*z-y*w),2*(y*z+x*w),1-2*(x*x+y*y)]])
    return {"simulation_time_s": float(sim.data.time), "position_xyz_m": sim.data.qpos[:3].tolist(),
            "quaternion_wxyz": sim.data.qpos[3:7].tolist(),
            "body_velocity_mps": (rotation.T @ sim.data.qvel[:3]).tolist(),
            "body_angular_velocity_rps": sim.data.qvel[3:6].tolist(),
            "roll_rad": math.atan2(rotation[2, 1], rotation[2, 2]),
            "pitch_rad": math.asin(max(-1.0, min(1.0, float(-rotation[2, 0])))),
            "command_target": sim.command.tolist(), "applied_target": sim.applied_command.tolist(),
            "mode": sim.mode, "policy_updates": sim.policy.updates}


def finite_measurement(row):
    def visit(v):
        if isinstance(v, dict):
            return all(visit(x) for x in v.values())
        if isinstance(v, list):
            return all(visit(x) for x in v)
        if isinstance(v, (int, float)):
            return not isinstance(v, bool) and math.isfinite(v)
        return isinstance(v, str)
    if not visit(row):
        raise ValueError("nonfinite measurement")


def integral(rows, field, axis):
    # Command is zero-order held after each sampled boundary; other fields use
    # a sampled trapezoid estimate, not exact continuous integration.
    return math.fsum((b["virtual_monotonic_s"]-a["virtual_monotonic_s"])*
               (a[field][axis] if field == "command_target" else (a[field][axis]+b[field][axis])*.5)
               for a,b in zip(rows,rows[1:]))


def response_window(rows):
    elapsed=rows[-1]["virtual_monotonic_s"]-rows[0]["virtual_monotonic_s"]
    return {"first_phase_time_s":rows[0]["phase_time_s"],"last_phase_time_s":rows[-1]["phase_time_s"],
            "elapsed_s":elapsed,"samples":len(rows),
            "sampled_command_forward_integral_m":integral(rows,"command_target",0),
            "sampled_applied_forward_integral_m":integral(rows,"applied_target",0),
            "sampled_measured_body_forward_integral_m":integral(rows,"body_velocity_mps",0),
            "mean_body_velocity_mps":[integral(rows,"body_velocity_mps",i)/elapsed if elapsed else None for i in range(3)],
            "max_abs_body_velocity_mps":[max(abs(x["body_velocity_mps"][i]) for x in rows) for i in range(3)],
            "mean_body_yaw_rate_rps":integral(rows,"body_angular_velocity_rps",2)/elapsed if elapsed else None,
            "ending_position_xyz_m":rows[-1]["position_xyz_m"],
            "ending_body_velocity_mps":rows[-1]["body_velocity_mps"]}


def phase_summary(rows):
    first, last = rows[0], rows[-1]
    dx = [b-a for a, b in zip(first["position_xyz_m"], last["position_xyz_m"])]
    elapsed = last["virtual_monotonic_s"] - first["virtual_monotonic_s"]
    q = first["quaternion_wxyz"]
    yaw = math.atan2(2*(q[0]*q[3]+q[1]*q[2]), 1-2*(q[2]*q[2]+q[3]*q[3]))
    forward = math.cos(yaw)*dx[0] + math.sin(yaw)*dx[1]
    lateral = -math.sin(yaw)*dx[0] + math.cos(yaw)*dx[1]
    result = {"samples": len(rows), "elapsed_s": elapsed, "world_displacement_xyz_m": dx,
              "initial_heading_forward_displacement_m": forward, "initial_heading_lateral_displacement_m": lateral,
              "mean_initial_heading_forward_mps": forward/elapsed if elapsed else None,
              "sampled_planar_path_length_m": sum(math.dist(a["position_xyz_m"][:2], b["position_xyz_m"][:2]) for a,b in zip(rows,rows[1:])),
              "max_abs_roll_rad": max(abs(x["roll_rad"]) for x in rows),
              "max_abs_pitch_rad": max(abs(x["pitch_rad"]) for x in rows),
              "max_body_speed_mps": max(math.sqrt(sum(v*v for v in x["body_velocity_mps"])) for x in rows),
              "max_abs_body_yaw_rate_rps": max(abs(x["body_angular_velocity_rps"][2]) for x in rows),
              "modes": sorted({x["mode"] for x in rows})}
    result["response"] = response_window(rows)
    if first["phase"] == "command":
        result["after_first_two_seconds"] = response_window([x for x in rows if x["phase_time_s"] >= 2.0])
    if first["phase"] == "zero":
        # Descriptive thresholds only, never a safety or braking guarantee.
        quiet = [math.sqrt(sum(v*v for v in x["body_velocity_mps"])) <= .02 and
                 math.sqrt(sum(v*v for v in x["body_angular_velocity_rps"])) <= .05 for x in rows]
        suffix = True
        first_quiet = None
        for i in range(len(rows)-1, -1, -1):
            suffix = suffix and quiet[i]
            if suffix:
                first_quiet = i
        result["sampled_settling"] = {"linear_speed_threshold_mps": .02, "angular_speed_threshold_rps": .05,
             "minimum_remaining_observation_s": 1.0, "time_s": rows[first_quiet]["phase_time_s"] if first_quiet is not None and last["phase_time_s"]-rows[first_quiet]["phase_time_s"] >= 1.0 else None,
             "scope": "All later 100 Hz samples quiet with at least 1 s remaining; not continuous settling proof"}
    return result


def run_case(speed, factory, sample, emit, matrix="baseline"):
    if matrix not in MATRICES or speed not in MATRICES[matrix] or isinstance(speed, bool):
        raise ValueError("unsupported fixed matrix speed")
    clock = VirtualClock()
    sim = factory(clock.now)
    owner = sim.arm()
    summaries = {}
    count = refreshes = 0
    for phase, ticks in PHASES:
        rows = []
        velocity = speed if phase == "command" else 0.0
        for tick in range(ticks + 1):
            if tick < ticks and tick % 50 == 0:
                sim.command_velocity(velocity, 0.0, 0.0, token=owner)
                refreshes += 1
            if tick % 5 == 0:
                row = dict(sample(sim), phase=phase, phase_time_s=tick*STEP_NS/1e9,
                           virtual_monotonic_s=clock.now(), scheduled_forward_mps=velocity)
                finite_measurement(row)
                if row["mode"] not in ("standing", "moving"):
                    raise ValueError("simulator left supported walking mode")
                emit(row)
                rows.append(row)
                count += 1
            if tick < ticks:
                clock.advance()
                sim.step()
        summaries[phase] = phase_summary(rows)
    sim.stop(token=owner)
    return {"matrix_name": matrix, "speed_mps": speed, "counterfactual_above_live_profile": speed > .05,
            "status": "measured", "samples": count, "command_refreshes": refreshes, "phases": summaries,
            "model": {"variant": "pinned robot/flat.xml infinite floor; no imported scene or sandbox",
                      "nq": int(sim.model.nq), "nv": int(sim.model.nv), "nu": int(sim.model.nu),
                      "ngeom": int(sim.model.ngeom), "nbody": int(sim.model.nbody),
                      "total_body_mass_kg": float(sum(sim.model.body_mass))},
            "safety_pass": None, "live_commissioning": False}


def write_json(path, value):
    raw = (json.dumps(value, indent=2, sort_keys=True, allow_nan=False) + "\n").encode()
    with path.open("xb") as out:
        out.write(raw)
    return {"sha256": sha(raw), "bytes": len(raw)}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--matrix", choices=tuple(MATRICES), default="baseline")
    parser.add_argument("--mujoco-version", choices=("4.0.0", "3.3.7"), default="4.0.0",
                        help="3.3.7 is only for reproducing historical reports in a separate environment")
    parser.add_argument("--source", required=True, type=Path)
    parser.add_argument("--assets", required=True, type=Path)
    parser.add_argument("--pins", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()
    sys.dont_write_bytecode = True
    source, assets, identity = validate_inputs(args.source, args.assets, args.pins, args.mujoco_version)
    if "go2_sim" in sys.modules:
        raise ValueError("simulator already imported")
    sys.path.insert(0, str(source))
    np = importlib.import_module("numpy")
    mujoco = importlib.import_module("mujoco")
    ort = importlib.import_module("onnxruntime")
    if {"mujoco": mujoco.__version__, "onnxruntime": ort.__version__} != identity["engines"]:
        raise ValueError("installed engine versions differ")
    module = importlib.import_module("go2_sim.simulation")
    policy = importlib.import_module("go2_sim.policy")
    if Path(module.__file__).resolve() != source / 'go2_sim/simulation.py' or Path(policy.__file__).resolve() != source / 'go2_sim/policy.py' or module.TIMESTEP != .002:
        raise ValueError("loaded simulator differs")
    args.out.mkdir(mode=0o700, parents=False, exist_ok=False)
    report = {"schema": "wendy.world.go2-offline-response.v1", "simulation": True,
              "operational_authority": False, "live_commissioning": False, "identity": identity,
              "numpy_version": np.__version__, "python_version": sys.version, "matrix_name": args.matrix, "matrix_mps": MATRICES[args.matrix], "phases_s": {k:n*STEP_NS/1e9 for k,n in PHASES},
              "sample_hz":100, "command_refresh_ms":100, "clock":"virtual monotonic advances with physics steps",
              "limits":["Offline isolated physics only; no DDS, network, managed VM load or mission authority exercised.",
                        "Speeds above 0.05 m/s are counterfactual model characterization, not live authorization.",
                        "Sampled maxima and settling are not continuous dynamics bounds or safety results.",
                        "Fresh simulator for each case; reset is the sole body pose initialization."],"cases":[],"complete":False}
    try:
        for index,speed in enumerate(MATRICES[args.matrix]):
            path=args.out/f"case-{index}.ndjson"
            with path.open("xb") as output:
                def emit(row):
                    output.write((json.dumps(row,separators=(",",":"),allow_nan=False)+"\n").encode())
                result=run_case(speed,lambda clock:module.Simulation(asset_dir=assets,monotonic=clock),lambda sim:measurement(sim,np),emit,matrix=args.matrix)
            result["sample_file"]={"path":path.name,"sha256":sha(read(path,16<<20)),"bytes":path.stat().st_size}
            write_json(args.out/f"case-{index}.json",result)
            report["cases"].append(result)
        report["complete"]=True
    except BaseException as exc:
        report["failure"]={"case_index":len(report["cases"]),"error_class":type(exc).__name__,"partial_samples_retained":True}
        raise
    finally:
        write_json(args.out/"report.json",report)


if __name__ == "__main__":
    main()
