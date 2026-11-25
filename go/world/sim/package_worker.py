"""Trusted worker entry point. Install Linux limits before any application import.

References: Python 3.12 resource/subprocess documentation and MuJoCo 4.0.0
Python bindings. This worker supplies resource limits, not a security sandbox.
"""
from __future__ import annotations

import os
from pathlib import Path
import sys


def install_limits():
    if not sys.platform.startswith("linux"):
        raise RuntimeError("Linux worker required")
    import resource
    # These constants deliberately require no application or native import.
    limits = ((resource.RLIMIT_AS, 2 << 30), (resource.RLIMIT_CPU, 10),
              (resource.RLIMIT_FSIZE, 2 << 20), (resource.RLIMIT_CORE, 0))
    for kind, bound in limits:
        resource.setrlimit(kind, (bound, bound))
    for name in ("OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS", "MKL_NUM_THREADS"):
        os.environ[name] = "1"


def execute(package, request, mujoco):
    """Compile only verified bytes, then return copied bounded model samples."""
    import math
    import platform
    from sim.package_runner import (LIMITS, MAX_SAMPLES, MAX_STATE_VALUES, POLICY, RESULT_SCHEMA,
                                    RunnerError, digest, source_hashes)
    from sim.run import compile_model
    model = compile_model(package, mujoco)
    dimensions = {name: int(getattr(model, name)) for name in ("nq", "nv", "nu", "na", "nbody", "ngeom", "njnt", "nsensor", "nsensordata")}
    if any(n < 0 or n > 16384 for n in dimensions.values()):
        raise RunnerError("compiled model dimensions exceed bound", code="model_limit", stage="compilation")
    timestep = float(model.opt.timestep)
    if not math.isfinite(timestep) or not 0 < timestep <= 10:
        raise RunnerError("model timestep must be finite in (0,10] seconds", code="invalid_model", stage="compilation")

    def named(kind, names):
        result = []
        for name in names:
            index = mujoco.mj_name2id(model, kind, name)
            if index < 0:
                raise RunnerError("selected model element does not exist: " + name, code="missing_binding", stage="compilation")
            result.append((name, index))
        return result
    bodies = named(mujoco.mjtObj.mjOBJ_BODY, request["body_names"])
    sensors = named(mujoco.mjtObj.mjOBJ_SENSOR, request["sensor_names"])
    selected_sensors = [{"name": name, "type": int(model.sensor_type[index]), "dimension": int(model.sensor_dim[index])} for name, index in sensors]
    controls = [0.0] * model.nu
    if request["control"]["mode"] == "fixed":
        for item in request["control"]["values"]:
            index = named(mujoco.mjtObj.mjOBJ_ACTUATOR, [item["actuator"]])[0][1]
            controls[index] = float(item["value"])
    for index, value in enumerate(controls):
        if model.actuator_ctrllimited[index]:
            lower, upper = map(float, model.actuator_ctrlrange[index])
            if not math.isfinite(lower) or not math.isfinite(upper) or not lower <= value <= upper:
                raise RunnerError("control outside compiled actuator range", code="control_range", stage="compilation")
    sample_count = (request["steps"] + request["sample_every"] - 1) // request["sample_every"]
    vector_width = model.nq + model.nv + model.nu + model.na
    sample_width = vector_width + 6 * len(bodies) + sum(int(model.sensor_dim[index]) for _, index in sensors)
    if sample_count > MAX_SAMPLES or vector_width + sample_count * sample_width > MAX_STATE_VALUES:
        raise RunnerError("sampled state exceeds scalar budget", code="state_limit", stage="compilation")
    data = mujoco.MjData(model)
    mujoco.mj_resetData(model, data)
    data.ctrl[:] = controls

    def values(array):
        result = [float(value) for value in array]
        if any(not math.isfinite(value) for value in result):
            raise RunnerError("nonfinite simulated state", code="nonfinite_state", stage="simulation")
        return result

    def state():
        return {"qpos": values(data.qpos), "qvel": values(data.qvel), "ctrl": values(data.ctrl), "act": values(data.act)}

    def forward():
        try:
            mujoco.mj_forward(model, data)
        except Exception as exc:
            raise RunnerError("native state evaluation failed", code="native_failure", stage="simulation") from exc
        if any(int(warning.number) != 0 for warning in data.warning):
            raise RunnerError("MuJoCo reported a simulation warning", code="native_warning", stage="simulation")

    forward()
    initial = state()
    samples = []
    previous_time = float(data.time)
    for step in range(1, request["steps"] + 1):
        # Fixed requested inputs are reapplied; model-default qpos is preserved.
        data.ctrl[:] = controls
        try:
            mujoco.mj_step(model, data)
        except Exception as exc:
            raise RunnerError("native simulation step failed", code="native_failure", stage="simulation") from exc
        current_time = float(data.time)
        if not math.isfinite(current_time) or current_time <= previous_time:
            raise RunnerError("simulation time did not advance", code="invalid_time", stage="simulation")
        previous_time = current_time
        if any(int(warning.number) != 0 for warning in data.warning):
            raise RunnerError("MuJoCo reported a simulation warning", code="native_warning", stage="simulation")
        if step % request["sample_every"] and step != request["steps"]:
            continue
        forward()
        sample = {"step": step, "simulation_time_s": current_time, **state(), "bodies": [], "sensors": []}
        for name, index in bodies:
            x, y, z = values(data.xpos[index])
            sample["bodies"].append({"name": name, "position_mujoco_m": [x, y, z], "position_local_m": [x, z, -y]})
        for name, index in sensors:
            start, size = int(model.sensor_adr[index]), int(model.sensor_dim[index])
            sample["sensors"].append({"name": name, "type": int(model.sensor_type[index]), "dimension": size,
                                      "values": values(data.sensordata[start:start + size])})
        samples.append(sample)
    return {"schema_version": RESULT_SCHEMA, "request": request, "request_sha256": digest(request),
            "package": {"sha256": package["_digest"], "schema_version": package["schema_version"],
                        "site_id": package["site_id"], "package_revision": package["package_revision"], "frame_revision": package["frame_revision"]},
            "validation": {"inventory_verified": True, "native_compiled": True, "simulation_completed": True},
            "runtime": {"mujoco_version": mujoco.__version__, "python_version": platform.python_version(),
                        "resource_policy": POLICY, "limits": dict(LIMITS), "seed": None,
                        "isolation": "Linux process resource limits only; network and filesystem isolation are not attested here"},
            "model": {**dimensions, "timestep_s": timestep,
                      "actuator_names": [mujoco.mj_id2name(model, mujoco.mjtObj.mjOBJ_ACTUATOR, i) for i in range(model.nu)],
                      "selected_sensors": selected_sensors}, "initial_state": initial, "samples": samples,
            "summary": {"completed_steps": request["steps"], "sample_count": len(samples), "final_simulation_time_s": previous_time},
            "samples_sha256": digest(samples), "source_sha256": source_hashes(), "import_report": package["_import_report"],
            "limitations": ["synthetic package; no measured fidelity or hardware authority",
                            "fixed inputs and model-default state; no stochastic adapter or seed is applied",
                            "zero control does not imply zero servo force or zero motion",
                            "native sensor values retain MuJoCo semantics; units and calibration are not inferred",
                            "process limits are not a security sandbox; use the constrained container launcher",
                            "same-version native trajectory comparison is required; cross-platform bitwise determinism is not promised"]}


def main():
    # Nothing from sim, numpy or mujoco is imported before this call.
    install_limits()
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
    from sim.package_runner import MAX_REQUEST, MAX_RESULT, RunnerError, decode, encoded, validate_request
    from sim.run import load_model_package
    stage = "request"
    try:
        raw = sys.stdin.buffer.read(MAX_REQUEST + 8192 + 1)
        if len(raw) > MAX_REQUEST + 8192:
            raise RunnerError("worker input exceeds bound")
        envelope = decode(raw)
        if type(envelope) is not dict or set(envelope) != {"package", "request"} or type(envelope["package"]) is not str:
            raise RunnerError("invalid worker envelope")
        request = validate_request(envelope["request"])
        stage = "structural_validation"
        package = load_model_package(envelope["package"])
        if package["_digest"] != request["package_sha256"]:
            raise RunnerError("package digest does not match run request", code="package_mismatch", stage=stage)
        stage = "native_import"
        import mujoco
        if mujoco.__version__ != "4.0.0":
            raise RunnerError("MuJoCo 4.0.0 is required", code="native_version", stage=stage)
        stage = "compilation"
        report = execute(package, request, mujoco)
        output = encoded(report)
        if len(output) > MAX_RESULT:
            raise RunnerError("complete result exceeds byte bound", code="output_limit", stage="result")
    except Exception as exc:
        output = encoded({"error": {"stage": getattr(exc, "stage", stage), "code": getattr(exc, "code", "worker_failure"), "message": str(exc)[:512]}})
    sys.stdout.buffer.write(output)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
