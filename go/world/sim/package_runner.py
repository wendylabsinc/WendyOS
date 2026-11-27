"""Run a verified package in a bounded Linux child, then publish a new report.

Process limits are not an OS security sandbox. Use the constrained container
launcher for network, filesystem, capabilities and aggregate process limits.
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
import subprocess
import sys
import tempfile
import time

from sim.run import PackageError, finite_float, invalid_constant, secure_read, unique_object

REQUEST_SCHEMA = "wendy.world.simulation-run.v1"
RESULT_SCHEMA = "wendy.world.simulation-result.v1"
POLICY = "linux-offline-v1"
MAX_REQUEST = 64 << 10
MAX_RESULT = 2 << 20
MAX_SAMPLES = 256
MAX_STATE_VALUES = 65536
LIMITS = {"address_space_bytes": 2 << 30, "cpu_seconds": 10, "wall_seconds": 15,
          "output_file_bytes": MAX_RESULT, "core_bytes": 0}
SOURCE_FILES = ("sim/run.py", "sim/package_runner.py", "sim/package_worker.py", "sim/requirements.txt")


class RunnerError(ValueError):
    def __init__(self, message, *, code="invalid_request", stage="request"):
        super().__init__(message)
        self.code, self.stage = code, stage


def encoded(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False).encode() + b"\n"


canonical_bytes = encoded


def digest(value):
    return hashlib.sha256(encoded(value)).hexdigest()


def decode(raw):
    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=unique_object,
                          parse_float=finite_float, parse_constant=invalid_constant)
    except (ValueError, UnicodeError, RecursionError) as exc:
        raise RunnerError("invalid finite UTF-8 JSON") from exc


def _shape(value, keys, label):
    if type(value) is not dict or set(value) != set(keys):
        raise RunnerError(f"invalid {label} fields")


def _name(value):
    return isinstance(value, str) and re.fullmatch(r"[\x20-\x7e]{1,128}", value) is not None and value == value.strip()


def validate_request(value):
    if isinstance(value, bytes):
        if len(value) > MAX_REQUEST:
            raise RunnerError("run request exceeds byte bound")
        value = decode(value)
    _shape(value, ("schema_version", "package_sha256", "run_id", "steps", "sample_every", "body_names",
                   "sensor_names", "initial_state", "control", "resource_policy"), "run request")
    if value["schema_version"] != REQUEST_SCHEMA or value["resource_policy"] != POLICY:
        raise RunnerError("unsupported request schema or resource policy")
    if not isinstance(value["package_sha256"], str) or re.fullmatch(r"[0-9a-f]{64}", value["package_sha256"]) is None:
        raise RunnerError("expected package digest required")
    if not isinstance(value["run_id"], str) or re.fullmatch(r"[A-Za-z0-9_-]{1,80}", value["run_id"]) is None:
        raise RunnerError("invalid run ID")
    steps, every = value["steps"], value["sample_every"]
    if type(steps) is not int or not 1 <= steps <= 10000 or type(every) is not int or not 1 <= every <= steps:
        raise RunnerError("invalid step or sampling bounds")
    if (steps + every - 1) // every > MAX_SAMPLES:
        raise RunnerError("sample count exceeds bound")
    if value["initial_state"] != "model-default":
        raise RunnerError("unsupported initial state")
    for field in ("body_names", "sensor_names"):
        names = value[field]
        if type(names) is not list or len(names) > 32 or any(not _name(n) for n in names) or len(set(names)) != len(names):
            raise RunnerError(f"invalid {field}")
    control = value["control"]
    if type(control) is not dict:
        raise RunnerError("invalid control")
    if control.get("mode") == "zero":
        _shape(control, ("mode",), "zero control")
    elif control.get("mode") == "fixed":
        _shape(control, ("mode", "values"), "fixed control")
        values = control["values"]
        if type(values) is not list or not 1 <= len(values) <= 64:
            raise RunnerError("fixed controls exceed bound")
        names = set()
        for item in values:
            _shape(item, ("actuator", "value"), "actuator control")
            name, number = item["actuator"], item["value"]
            if not _name(name) or name in names:
                raise RunnerError("invalid or duplicate actuator name")
            names.add(name)
            if type(number) not in (int, float) or not -1e9 <= number <= 1e9 or not math.isfinite(number):
                raise RunnerError("actuator control must be finite and bounded")
    else:
        raise RunnerError("unsupported control mode")
    raw = encoded(value)
    if len(raw) > MAX_REQUEST:
        raise RunnerError("run request exceeds byte bound")
    return decode(raw)


def source_hashes():
    root = Path(__file__).resolve().parents[1]
    return {name: hashlib.sha256((root / name).read_bytes()).hexdigest() for name in SOURCE_FILES}


def _finite(value):
    try:
        return type(value) in (int, float) and math.isfinite(value)
    except OverflowError:
        return False


def _validate_result(report, request):
    keys = ("schema_version", "request", "request_sha256", "package", "validation", "runtime", "model",
            "initial_state", "samples", "summary", "samples_sha256", "source_sha256", "import_report", "limitations")
    _shape(report, keys, "run result")
    if len(encoded(report)) > MAX_RESULT:
        raise RunnerError("worker result exceeds byte bound", code="output_limit", stage="result")
    if report["schema_version"] != RESULT_SCHEMA or encoded(report["request"]) != encoded(request) or report["request_sha256"] != digest(request):
        raise RunnerError("worker request binding mismatch")
    package = report["package"]
    _shape(package, ("sha256", "schema_version", "site_id", "package_revision", "frame_revision"), "result package")
    if package["sha256"] != request["package_sha256"] or package["schema_version"] not in ("wendy.world.package.v0", "wendy.world.package.v1"):
        raise RunnerError("worker package binding mismatch")
    for name in ("site_id", "package_revision", "frame_revision"):
        if type(package[name]) is not str or not 1 <= len(package[name].encode("utf-8")) <= 256:
            raise RunnerError("invalid result package identity")
    validation = {"inventory_verified": True, "native_compiled": True, "simulation_completed": True}
    if encoded(report["validation"]) != encoded(validation):
        raise RunnerError("worker did not complete validation, compilation and simulation")
    runtime = report["runtime"]
    _shape(runtime, ("mujoco_version", "python_version", "resource_policy", "limits", "seed", "isolation"), "runtime")
    if (runtime["mujoco_version"] != "4.0.0" or type(runtime["python_version"]) is not str or
            re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", runtime["python_version"]) is None or runtime["seed"] is not None or
            runtime["resource_policy"] != POLICY or encoded(runtime["limits"]) != encoded(LIMITS) or
            runtime["isolation"] != "Linux process resource limits only; network and filesystem isolation are not attested here"):
        raise RunnerError("worker runtime or policy mismatch")
    if encoded(report["source_sha256"]) != encoded(source_hashes()):
        raise RunnerError("worker source mismatch")
    model = report["model"]
    dimension_keys = ("nq", "nv", "nu", "na", "nbody", "ngeom", "njnt", "nsensor", "nsensordata")
    _shape(model, (*dimension_keys, "timestep_s", "actuator_names", "selected_sensors"), "model")
    for key in dimension_keys:
        if type(model[key]) is not int or not 0 <= model[key] <= 16384:
            raise RunnerError("invalid model dimension")
    if model["nbody"] < 1 or not _finite(model["timestep_s"]) or not 0 < model["timestep_s"] <= 10:
        raise RunnerError("invalid model timestep or body count")
    actuator_names = model["actuator_names"]
    if type(actuator_names) is not list or len(actuator_names) != model["nu"] or any(n is not None and not _name(n) for n in actuator_names):
        raise RunnerError("invalid actuator names")
    present_names = [n for n in actuator_names if n is not None]
    if len(set(present_names)) != len(present_names):
        raise RunnerError("duplicate model actuator names")
    controls = [0.0] * model["nu"]
    if request["control"]["mode"] == "fixed":
        for item in request["control"]["values"]:
            if item["actuator"] not in actuator_names:
                raise RunnerError("missing requested actuator")
            controls[actuator_names.index(item["actuator"])] = float(item["value"])
    selected_sensors = model["selected_sensors"]
    if type(selected_sensors) is not list or len(selected_sensors) != len(request["sensor_names"]):
        raise RunnerError("invalid selected sensors")
    for sensor, name in zip(selected_sensors, request["sensor_names"]):
        _shape(sensor, ("name", "type", "dimension"), "sensor metadata")
        if (sensor["name"] != name or type(sensor["type"]) is not int or not 0 <= sensor["type"] <= 1024 or
                type(sensor["dimension"]) is not int or not 1 <= sensor["dimension"] <= model["nsensordata"]):
            raise RunnerError("invalid sensor metadata")
    if len(selected_sensors) > model["nsensor"] or sum(s["dimension"] for s in selected_sensors) > model["nsensordata"]:
        raise RunnerError("selected sensors exceed native sensor dimensions")
    vector_width = model["nq"] + model["nv"] + model["nu"] + model["na"]
    sample_count = (request["steps"] + request["sample_every"] - 1) // request["sample_every"]
    width = vector_width + 6 * len(request["body_names"]) + sum(s["dimension"] for s in selected_sensors)
    if vector_width + width * sample_count > MAX_STATE_VALUES:
        raise RunnerError("state scalar budget exceeded")

    def vector(value, size):
        if type(value) is not list or len(value) != size or any(not _finite(n) for n in value):
            raise RunnerError("invalid state vector")

    def state(value):
        for field, dimension in (("qpos", "nq"), ("qvel", "nv"), ("ctrl", "nu"), ("act", "na")):
            vector(value[field], model[dimension])
        if value["ctrl"] != controls:
            raise RunnerError("state controls differ from request")
    _shape(report["initial_state"], ("qpos", "qvel", "ctrl", "act"), "initial state")
    state(report["initial_state"])
    samples = report["samples"]
    if type(samples) is not list or len(samples) != sample_count:
        raise RunnerError("worker sample count mismatch")
    previous_time = 0.0
    for index, sample in enumerate(samples, 1):
        _shape(sample, ("step", "simulation_time_s", "qpos", "qvel", "ctrl", "act", "bodies", "sensors"), "sample")
        step = min(index * request["sample_every"], request["steps"])
        sample_time = sample["simulation_time_s"]
        if (type(sample["step"]) is not int or sample["step"] != step or not _finite(sample_time) or sample_time <= previous_time or
                not math.isclose(sample_time, step * model["timestep_s"], rel_tol=1e-10, abs_tol=1e-12)):
            raise RunnerError("invalid sample step or simulation time")
        previous_time = sample_time
        state(sample)
        if type(sample["bodies"]) is not list or len(sample["bodies"]) != len(request["body_names"]):
            raise RunnerError("invalid body samples")
        for body, name in zip(sample["bodies"], request["body_names"]):
            _shape(body, ("name", "position_mujoco_m", "position_local_m"), "body sample")
            vector(body["position_mujoco_m"], 3); vector(body["position_local_m"], 3)
            x, y, z = body["position_mujoco_m"]
            if body["name"] != name or body["position_local_m"] != [x, z, -y]:
                raise RunnerError("body name or coordinate mismatch")
        if type(sample["sensors"]) is not list or len(sample["sensors"]) != len(selected_sensors):
            raise RunnerError("invalid sensor samples")
        for sample_sensor, selected in zip(sample["sensors"], selected_sensors):
            _shape(sample_sensor, ("name", "type", "dimension", "values"), "sensor sample")
            if encoded({k: sample_sensor[k] for k in selected}) != encoded(selected):
                raise RunnerError("sensor metadata changed")
            vector(sample_sensor["values"], selected["dimension"])
    summary = {"completed_steps": request["steps"], "sample_count": len(samples), "final_simulation_time_s": previous_time}
    if encoded(report["summary"]) != encoded(summary) or report["samples_sha256"] != digest(samples):
        raise RunnerError("worker result is incomplete")
    if type(report["import_report"]) is not dict or report["import_report"].get("schema_version") != "wendy.mujoco.import.v1":
        raise RunnerError("invalid import report")
    if type(report["limitations"]) is not list or not report["limitations"] or any(type(s) is not str or len(s) > 512 for s in report["limitations"]):
        raise RunnerError("invalid limitations")
    return report


def validate_result(report, request):
    """Check exact successful worker identity and state before publication."""
    request = validate_request(request)
    try:
        return _validate_result(report, request)
    except (KeyError, TypeError, AttributeError, OverflowError, UnicodeError, ValueError) as exc:
        raise RunnerError(str(exc)[:512], code="invalid_result", stage="result") from exc


def _run_child(value):
    """Only trusted code selects the executable; the request cannot override it."""
    with tempfile.TemporaryDirectory(prefix="world-package-run-") as tmp:
        folder = Path(tmp)
        with (folder / "input").open("w+b") as source, (folder / "output").open("w+b") as output, (folder / "error").open("w+b") as error:
            source.write(encoded(value)); source.seek(0)
            env = {"PATH": os.defpath, "LANG": "C.UTF-8", "OPENBLAS_NUM_THREADS": "1", "OMP_NUM_THREADS": "1", "MKL_NUM_THREADS": "1"}
            started = time.monotonic()
            child = subprocess.Popen([sys.executable, "-I", str(Path(__file__).with_name("package_worker.py").resolve())],
                                     stdin=source, stdout=output, stderr=error, env=env, cwd=tmp, start_new_session=True)
            try:
                while child.poll() is None:
                    if time.monotonic() - started >= LIMITS["wall_seconds"]:
                        raise RunnerError("worker exceeded wall deadline", code="wall_limit", stage="unknown")
                    if os.fstat(output.fileno()).st_size > MAX_RESULT or os.fstat(error.fileno()).st_size > MAX_RESULT:
                        raise RunnerError("worker output exceeded bound", code="output_limit", stage="unknown")
                    time.sleep(.01)
                if time.monotonic() - started >= LIMITS["wall_seconds"]:
                    raise RunnerError("worker exceeded wall deadline", code="wall_limit", stage="unknown")
            finally:
                # Also clean up a surviving child if the main worker exited first.
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                child.wait()
            if child.returncode != 0:
                raise RunnerError("worker exited unsuccessfully or exceeded a resource limit", code="worker_exit", stage="unknown")
            if os.fstat(error.fileno()).st_size >= MAX_RESULT:
                raise RunnerError("worker diagnostics reached output limit", code="output_limit", stage="unknown")
            if os.fstat(error.fileno()).st_size:
                raise RunnerError("worker emitted native diagnostics; result refused", code="native_diagnostics", stage="unknown")
            output.seek(0); raw = output.read(MAX_RESULT + 1)
            if len(raw) > MAX_RESULT:
                raise RunnerError("worker result exceeds byte bound", code="output_limit", stage="unknown")
    try:
        return decode(raw)
    except RunnerError as exc:
        raise RunnerError("worker returned invalid JSON", code="invalid_result", stage="result") from exc


def run_package(path, request):
    request = validate_request(request)
    if not sys.platform.startswith("linux"):
        raise RunnerError("package execution requires the bounded Linux worker", code="unsupported_platform", stage="startup")
    path = Path(path).absolute()
    if len(str(path).encode()) > 4096:
        raise RunnerError("package path exceeds bound")
    result = _run_child({"package": str(path), "request": request})
    if type(result) is dict and set(result) == {"error"}:
        failure = result["error"]
        if type(failure) is not dict or set(failure) != {"stage", "code", "message"}:
            raise RunnerError("invalid worker failure", code="invalid_result", stage="unknown")
        raise RunnerError(str(failure["message"])[:512], code=str(failure["code"])[:64], stage=str(failure["stage"])[:64])
    try:
        return validate_result(result, request)
    except (KeyError, TypeError, AttributeError, OverflowError) as exc:
        raise RunnerError("invalid worker result", code="invalid_result", stage="result") from exc


def install_report(report, destination):
    """Publish one complete file without replacing any existing path."""
    raw = encoded(report)
    if len(raw) > MAX_RESULT:
        raise RunnerError("report exceeds byte bound", code="output_limit", stage="publication")
    snapshot = decode(raw)
    validate_result(snapshot, validate_request(snapshot["request"]))
    destination = Path(destination).absolute()
    fd, temporary = tempfile.mkstemp(prefix=".world-run-", dir=destination.parent)
    published = False
    try:
        with os.fdopen(fd, "wb") as output:
            output.write(raw); output.flush(); os.fsync(output.fileno())
        os.link(temporary, destination, follow_symlinks=False)
        published = True
        directory = os.open(destination.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    except OSError as exc:
        if published:
            raise RunnerError("complete report published; directory durability unconfirmed; inspect without retry", code="publication_uncertain", stage="publication") from exc
        raise RunnerError("report destination unavailable or already exists", code="publication_refused", stage="publication") from exc
    finally:
        os.unlink(temporary)


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", type=Path, required=True)
    parser.add_argument("--request", type=Path, required=True)
    parser.add_argument("--output", "--out", dest="out", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        raw = secure_read(args.request.absolute().parent, args.request.name, MAX_REQUEST, "run request")
        report = run_package(args.package, decode(raw))
        install_report(report, args.out)
        print(json.dumps({"completed": True, "report_sha256": hashlib.sha256(encoded(report)).hexdigest(),
                          "package_sha256": report["package"]["sha256"], "request_sha256": report["request_sha256"]}, sort_keys=True))
        return 0
    except (RunnerError, PackageError, OSError, ValueError) as exc:
        print(json.dumps({"completed": False, "error": {"code": getattr(exc, "code", "runner_error"),
              "stage": getattr(exc, "stage", "unknown"), "message": str(exc)[:512]}}, sort_keys=True), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
