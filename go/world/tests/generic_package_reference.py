"""Compare a completed generic run with independent MuJoCo 4 stepping.

This read-only integration oracle is for trusted bounded test fixtures. It does
not call the worker's execution or sample-building code and does not authorize
package execution. Use the constrained launcher for untrusted native inputs.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import math
from pathlib import Path
import sys

sys.dont_write_bytecode = True
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from sim.package_runner import canonical_bytes, decode, validate_request
from sim.run import compile_model, load_model_package, secure_read

ABSOLUTE_TOLERANCE = 1e-12
RELATIVE_TOLERANCE = 1e-10
MAX_EXECUTION_BYTES = 3 << 20


def compare(package_path, execution_path):
    execution_path = Path(execution_path).absolute()
    raw = secure_read(execution_path.parent, execution_path.name, MAX_EXECUTION_BYTES, "execution")
    execution = decode(raw)
    if type(execution) is not dict or execution.get("schema_version") != "wendy.world.simulation-execution.v1":
        raise ValueError("unsupported execution artifact")
    result = execution["result"]
    if result["schema_version"] != "wendy.world.simulation-result.v1":
        raise ValueError("unsupported result artifact")
    if hashlib.sha256(canonical_bytes(result)).hexdigest() != execution["result_sha256"]:
        raise ValueError("execution result digest mismatch")
    request = validate_request(result["request"])
    if result["request_sha256"] != hashlib.sha256(canonical_bytes(request)).hexdigest():
        raise ValueError("request digest mismatch")
    package = load_model_package(package_path)
    if package["_digest"] != request["package_sha256"] or result["package"]["sha256"] != package["_digest"]:
        raise ValueError("package digest mismatch")
    for field in ("schema_version", "site_id", "package_revision", "frame_revision"):
        if result["package"][field] != package[field]:
            raise ValueError("package metadata mismatch")
    # Import native code only after artifact/package identity is checked.
    import mujoco
    if mujoco.__version__ != "4.0.0" or result["runtime"]["mujoco_version"] != mujoco.__version__:
        raise ValueError("reference and reported engine must be MuJoCo 4.0.0")
    model = compile_model(package, mujoco)
    for field in ("nq", "nv", "nu", "na", "nbody", "ngeom", "njnt", "nsensor", "nsensordata"):
        if type(result["model"][field]) is not int or result["model"][field] != int(getattr(model, field)):
            raise ValueError("native model dimension mismatch: " + field)
    native_actuators = [mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_ACTUATOR, index) for index in range(model.nu)]
    if canonical_bytes(result["model"]["actuator_names"]) != canonical_bytes(native_actuators):
        raise ValueError("native actuator metadata mismatch")
    native_sensors = []
    for name in request["sensor_names"]:
        index = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_SENSOR, name)
        if index < 0:
            raise ValueError("native sensor binding missing")
        native_sensors.append({"name": name, "type": int(model.sensor_type[index]), "dimension": int(model.sensor_dim[index])})
    if canonical_bytes(result["model"]["selected_sensors"]) != canonical_bytes(native_sensors):
        raise ValueError("native sensor metadata mismatch")
    data = mujoco.MjData(model)
    mujoco.mj_resetData(model, data)
    controls = [0.0] * model.nu
    if request["control"]["mode"] == "fixed":
        for item in request["control"]["values"]:
            index = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_ACTUATOR, item["actuator"])
            if index < 0:
                raise ValueError("native actuator binding missing")
            controls[index] = float(item["value"])
    for index, value in enumerate(controls):
        if model.actuator_ctrllimited[index] and not model.actuator_ctrlrange[index][0] <= value <= model.actuator_ctrlrange[index][1]:
            raise ValueError("native control range mismatch")
    data.ctrl[:] = controls
    mujoco.mj_forward(model, data)
    comparisons = 0
    maximum_error = 0.0

    def numeric(actual, expected, label):
        nonlocal comparisons, maximum_error
        if type(actual) not in (int, float) or not math.isfinite(actual) or not math.isfinite(float(expected)):
            raise ValueError("invalid numeric sample: " + label)
        error = abs(actual - float(expected))
        maximum_error = max(maximum_error, error)
        comparisons += 1
        if not math.isclose(actual, float(expected), rel_tol=RELATIVE_TOLERANCE, abs_tol=ABSOLUTE_TOLERANCE):
            raise ValueError("native sample mismatch: " + label)

    def vector(actual, expected, label):
        if type(actual) is not list or len(actual) != len(expected):
            raise ValueError("native vector dimension mismatch: " + label)
        for index, value in enumerate(expected):
            numeric(actual[index], value, f"{label}[{index}]")

    def generalized(sample, label):
        for field in ("qpos", "qvel", "ctrl", "act"):
            vector(sample[field], getattr(data, field), label + "." + field)

    numeric(result["model"]["timestep_s"], model.opt.timestep, "timestep")
    generalized(result["initial_state"], "initial")
    samples = result["samples"]
    wanted_count = (request["steps"] + request["sample_every"] - 1) // request["sample_every"]
    if type(samples) is not list or len(samples) != wanted_count:
        raise ValueError("sample count mismatch")
    sample_index = 0
    for step in range(1, request["steps"] + 1):
        data.ctrl[:] = controls
        mujoco.mj_step(model, data)
        if any(int(w.number) for w in data.warning):
            raise ValueError("native reference simulation warning")
        if step % request["sample_every"] and step != request["steps"]:
            continue
        mujoco.mj_forward(model, data)
        sample = samples[sample_index]; sample_index += 1
        if type(sample["step"]) is not int or sample["step"] != step:
            raise ValueError("sample step mismatch")
        numeric(sample["simulation_time_s"], data.time, "sample time")
        generalized(sample, "sample")
        if len(sample["bodies"]) != len(request["body_names"]) or len(sample["sensors"]) != len(request["sensor_names"]):
            raise ValueError("selected element count mismatch")
        for entry, name in zip(sample["bodies"], request["body_names"]):
            index = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, name)
            if index < 0 or entry["name"] != name:
                raise ValueError("body binding mismatch")
            position = data.xpos[index]
            vector(entry["position_mujoco_m"], position, "body native")
            vector(entry["position_local_m"], [position[0], position[2], -position[1]], "body local")
        for entry, name in zip(sample["sensors"], request["sensor_names"]):
            index = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_SENSOR, name)
            if index < 0 or entry["name"] != name or type(entry["type"]) is not int or entry["type"] != int(model.sensor_type[index]):
                raise ValueError("sensor binding mismatch")
            start, size = int(model.sensor_adr[index]), int(model.sensor_dim[index])
            if type(entry["dimension"]) is not int or entry["dimension"] != size:
                raise ValueError("sensor dimension mismatch")
            vector(entry["values"], data.sensordata[start:start + size], "sensor")
    if (type(result["summary"]["completed_steps"]) is not int or result["summary"]["completed_steps"] != request["steps"] or
            type(result["summary"]["sample_count"]) is not int or result["summary"]["sample_count"] != sample_index):
        raise ValueError("completion summary mismatch")
    numeric(result["summary"]["final_simulation_time_s"], data.time, "final time")
    return {"matched": True, "engine": "MuJoCo", "version": mujoco.__version__, "steps": request["steps"],
            "samples_compared": sample_index, "numeric_values_compared": comparisons,
            "maximum_absolute_error": maximum_error, "absolute_tolerance": ABSOLUTE_TOLERANCE,
            "relative_tolerance": RELATIVE_TOLERANCE, "package_sha256": package["_digest"],
            "request_sha256": result["request_sha256"], "execution_sha256": hashlib.sha256(raw).hexdigest(),
            "reference_source_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest()}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", type=Path, required=True)
    parser.add_argument("--execution", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        result = compare(args.package, args.execution)
        code = 0
    except (ValueError, KeyError, TypeError, OSError, OverflowError) as exc:
        result, code = {"matched": False, "error": str(exc)[:512]}, 1
    output = json.dumps(result, sort_keys=True, allow_nan=False)
    if len(output.encode()) > 4096:
        output, code = '{"matched":false,"error":"reference output exceeded bound"}', 1
    print(output)
    return code


if __name__ == "__main__":
    raise SystemExit(main())
