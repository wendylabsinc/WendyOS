#!/usr/bin/env python3
"""Prepare an additive, credential-free history service bundle; never deploy it.

The baseline remains a separately pinned seven-service stage. A future runner
must validate this bundle against that baseline, copy only the reader context,
and check expected_previous_manifest_sha256 before installing each manifest.
Both manifests keep the seven original service definitions. Their only
difference is the reader's two fresh persistence volumes.

This bundle contains no reader configuration or gate. Those require the actual
deployment generation and a successful, locally verified fusion acknowledgment.
The future history scenario must explicitly request 600000 ms result retention;
this staging step changes no retention setting or the scene's 600-second limit.
"""
import argparse
import copy
import hashlib
import json
import os
from pathlib import Path
import re
import stat

import stage

READER = "proof-reader"
PHASES = ("before_restart", "after_restart")
ORDER = ("camera-a", "sensorlink-bridge", "gas", "audio", "fusion")
MAX_BINARY = 64 << 20
MAX_JSON = 1 << 20
CONTEXT = "project/services/" + READER
MANIFESTS = {phase: "manifests/" + phase + ".wendy.json" for phase in PHASES}
FILES = {
    "plan.json": (MAX_JSON, 0o600),
    **{path: (MAX_JSON, 0o644) for path in MANIFESTS.values()},
    CONTEXT + "/Dockerfile": (4096, 0o644),
    CONTEXT + "/app": (MAX_BINARY, 0o755),
    CONTEXT + "/app-runner": (MAX_BINARY, 0o755),
}


def _require(condition, message):
    if not condition:
        raise stage.Refused(message)


def _pin(value):
    _require(isinstance(value, str) and re.fullmatch(r"[a-f0-9]{64}", value),
             "explicit lowercase SHA256 pin required")
    return value


def _path(value):
    path = Path(value)
    _require(path.is_absolute() and path == Path(os.path.abspath(path)),
             "clean absolute path required")
    return path


def _open(path, limit):
    """Open an owned bounded file through directories that are not symlinks."""
    path = _path(path)
    directory = os.open("/", os.O_RDONLY | os.O_DIRECTORY)
    try:
        for part in path.parts[1:-1]:
            child = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                            dir_fd=directory)
            os.close(directory)
            directory = child
        fd = os.open(path.name, os.O_RDONLY | os.O_NOFOLLOW, dir_fd=directory)
    finally:
        os.close(directory)
    try:
        info = os.fstat(fd)
        _require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and
                 not info.st_mode & 0o022 and 0 < info.st_size <= limit,
                 "input owner, permissions, type or size refused")
        return os.fdopen(fd, "rb"), info
    except BaseException:
        os.close(fd)
        raise


def _bytes(path, limit):
    source, before = _open(path, limit)
    with source:
        raw = source.read(limit + 1)
        after = os.fstat(source.fileno())
    _require(len(raw) == before.st_size <= limit and
             (before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) ==
             (after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns),
             "input changed while reading")
    return raw


def _sha(raw):
    return hashlib.sha256(raw).hexdigest()


def _json(path):
    return stage.strict_json(_bytes(path, MAX_JSON), MAX_JSON)


def _binary(path, destination=None):
    source, before = _open(path, MAX_BINARY)
    target = None
    digest = hashlib.sha256()
    size = 0
    header = b""
    try:
        if destination is not None:
            target = os.fdopen(os.open(destination, os.O_WRONLY | os.O_CREAT |
                                      os.O_EXCL | os.O_NOFOLLOW, 0o755), "wb")
        with source:
            for block in iter(lambda: source.read(1 << 20), b""):
                size += len(block)
                _require(size <= MAX_BINARY, "binary grew beyond its bound")
                if not header:
                    header = block[:64]
                digest.update(block)
                if target is not None:
                    target.write(block)
            after = os.fstat(source.fileno())
        _require(size == before.st_size and
                 (before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) ==
                 (after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns),
                 "binary changed while copying")
        _require(len(header) == 64 and header[:6] == b"\x7fELF\x02\x01" and
                 int.from_bytes(header[18:20], "little") == 183,
                 "Linux arm64 ELF reader and runner required")
        if target is not None:
            target.flush()
            os.fsync(target.fileno())
            os.fchmod(target.fileno(), 0o755)
        return {"sha256": digest.hexdigest(), "size": size}
    finally:
        source.close()
        if target is not None:
            target.close()


def _baseline(root, plan_sha256, inventory_sha256):
    root = _path(root)
    plan_raw = _bytes(root / "plan.json", MAX_JSON)
    inventory_raw = _bytes(root / "files.json", MAX_JSON)
    _require(_sha(plan_raw) == _pin(plan_sha256) and
             _sha(inventory_raw) == _pin(inventory_sha256),
             "baseline plan or inventory pin differs")
    # Strict parsing happens before the existing validator. No Docker calls are
    # made by validate_stage; it checks the local source/rootfs/binary inventory.
    plan = stage.strict_json(plan_raw, MAX_JSON)
    inventory = stage.strict_json(inventory_raw, MAX_JSON)
    _require(stage.validate_stage(root) == plan,
             "baseline stage changed during validation")
    project_raw = _bytes(root / "project/wendy.json", MAX_JSON)
    project = stage.strict_json(project_raw, MAX_JSON)
    _require(project == stage.project_manifest(plan["run_id"]) and
             plan["services"] == list(stage.SERVICES),
             "exact seven-service baseline required")
    _require(inventory["files"]["project/wendy.json"]["sha256"] == _sha(project_raw) and
             inventory["files"]["project/wendy.json"]["size"] == len(project_raw),
             "baseline manifest changed after inventory validation")
    runner_path = root / "project/services/fusion/app-runner"
    runner = _binary(runner_path)
    _require(runner == plan["binaries"]["app-runner"],
             "baseline runner differs from its inventory")
    pins = {"plan_sha256": plan_sha256, "inventory_sha256": inventory_sha256,
            "project_sha256": _sha(project_raw)}
    return root, plan, project, runner_path, runner, pins


def phase_volumes(run_id, phase):
    _require(phase in PHASES, "unknown history phase")
    _require(isinstance(run_id, str) and
             re.fullmatch(r"[a-z0-9][a-z0-9-]{0,39}", run_id), "invalid run ID")
    return {kind: stage.volume_name(run_id, READER + "-" + phase.replace("_", "-"), kind)
            for kind in ("config", "proofs")}


def _manifest(project, run_id, phase):
    result = copy.deepcopy(project)
    volumes = phase_volumes(run_id, phase)
    subscribe = [
        {"app": stage.APP, "service": service,
         "componentType": stage.RESULT if service == "fusion" else stage.CAPTURE,
         "stream": "hazard" if service == "fusion" else stage.SENSORS[service]}
        for service in ORDER
    ]
    result["services"][READER] = {
        "context": "services/" + READER,
        "entitlements": [
            {"type": "persist", "name": volumes["config"], "path": "/config"},
            {"type": "persist", "name": volumes["proofs"], "path": "/proofs"},
            {"type": "world", "subscribe": subscribe},
        ],
    }
    return stage.json_bytes(result)


def _contents(base_plan, project, runner, pins, reader):
    manifests = {phase: _manifest(project, base_plan["run_id"], phase) for phase in PHASES}
    previous = pins["project_sha256"]
    phases = []
    for phase in PHASES:
        digest = _sha(manifests[phase])
        phases.append({"name": phase, "manifest": MANIFESTS[phase],
                       "manifest_sha256": digest,
                       "expected_previous_manifest_sha256": previous,
                       "volumes": phase_volumes(base_plan["run_id"], phase)})
        previous = digest
    plan = {"version": 1, "kind": "wendy.agentmultimodal.history-stage.v1",
            "simulation_only": True, "app": stage.APP, "run_id": base_plan["run_id"],
            "service": READER, "baseline": pins,
            "binaries": {READER: reader, "app-runner": runner},
            "phases": phases, "scene_lifetime_seconds": 600,
            "deployment_verified": False}
    dockerfile = stage.dockerfile(READER, base_plan["run_id"], base_plan["anchor_unix_ns"],
                                 base_plan["seed"], {})
    return plan, manifests, dockerfile


def _inventory(root):
    """Reject every unlisted path before reading or hashing its contents."""
    root = _path(root)
    _require(not root.is_symlink() and root.is_dir() and
             root.stat().st_uid == os.geteuid() and not root.stat().st_mode & 0o022,
             "owned protected bundle directory required")
    allowed = set(FILES) | {"files.json"}
    directories = {str(p) for name in allowed for p in Path(name).parents if str(p) != "."}
    found = set()
    for directory, dirs, files in os.walk(root, followlinks=False):
        for name in dirs:
            path = Path(directory) / name
            rel = str(path.relative_to(root))
            info = path.lstat()
            _require(rel in directories and stat.S_ISDIR(info.st_mode) and
                     info.st_uid == os.geteuid() and not info.st_mode & 0o022,
                     "unlisted, symlinked or writable bundle directory")
        for name in files:
            rel = str((Path(directory) / name).relative_to(root))
            _require(rel in allowed, "unlisted bundle file")
            found.add(rel)
    _require(found == allowed, "bundle file set is incomplete")
    result = {}
    for name, (limit, mode) in FILES.items():
        path = root / name
        if name in (CONTEXT + "/app", CONTEXT + "/app-runner"):
            info = _binary(path)
        else:
            raw = _bytes(path, limit)
            info = {"sha256": _sha(raw), "size": len(raw)}
        _require(stat.S_IMODE(path.stat().st_mode) == mode, "bundle file mode differs")
        result[name] = {**info, "mode": mode}
    _require(stat.S_IMODE((root / "files.json").stat().st_mode) == 0o600,
             "bundle inventory mode differs")
    return result


def prepare(baseline_stage, output, reader_binary, *, reader_sha256,
            baseline_plan_sha256, baseline_inventory_sha256, staging=stage.STAGING):
    """Write a new sibling bundle; refuse reuse and leave failed output intact."""
    reader = _binary(reader_binary)
    _require(reader["sha256"] == _pin(reader_sha256), "reader binary pin differs")
    base, base_plan, project, runner_path, runner, pins = _baseline(
        baseline_stage, baseline_plan_sha256, baseline_inventory_sha256)
    output, staging = _path(output), _path(staging)
    _require(not output.is_relative_to(base) and not base.is_relative_to(output),
             "history output must not contain or modify the baseline")
    for parent in (staging, output.parent):
        _require(parent.is_dir() and not parent.is_symlink() and
                 parent.stat().st_uid == os.geteuid() and not parent.stat().st_mode & 0o022,
                 "existing owned private output parent required")
    plan, manifests, dockerfile = _contents(base_plan, project, runner, pins, reader)
    out = stage.safe_output(output, staging)
    for phase, raw in manifests.items():
        stage.write_new(out / MANIFESTS[phase], raw, 0o644)
    stage.write_new(out / CONTEXT / "Dockerfile", dockerfile, 0o644)
    _require(_binary(reader_binary, out / CONTEXT / "app") == reader,
             "reader changed after pin validation")
    _require(_binary(runner_path, out / CONTEXT / "app-runner") == runner,
             "runner changed after baseline validation")
    stage.write_new(out / "plan.json", stage.json_bytes(plan))
    # The exact inventory is generated once. Validation checks it against the
    # pinned baseline again, including refusal of baseline files added mid-copy.
    files = stage.inventory(out)
    stage.write_new(out / "files.json", stage.json_bytes({"version": 1, "files": files}))
    return validate_bundle(out, base, reader_sha256=reader_sha256,
                           baseline_plan_sha256=baseline_plan_sha256,
                           baseline_inventory_sha256=baseline_inventory_sha256)


def validate_bundle(bundle, baseline_stage, *, reader_sha256,
                    baseline_plan_sha256, baseline_inventory_sha256):
    root = _path(bundle)
    actual = _inventory(root)
    inventory = _json(root / "files.json")
    _require(stage.json_bytes(inventory) == stage.json_bytes({"version": 1, "files": actual}),
             "bundle inventory differs")
    _, base_plan, project, _, runner, pins = _baseline(
        baseline_stage, baseline_plan_sha256, baseline_inventory_sha256)
    reader = _binary(root / CONTEXT / "app")
    _require(reader["sha256"] == _pin(reader_sha256), "bundled reader pin differs")
    _require(_binary(root / CONTEXT / "app-runner") == runner, "bundled runner differs")
    expected, manifests, dockerfile = _contents(base_plan, project, runner, pins, reader)
    plan = _json(root / "plan.json")
    _require(stage.json_bytes(plan) == stage.json_bytes(expected), "history plan differs")
    for phase, raw in manifests.items():
        # Require the generated bytes as well as strict parsing. Updating the
        # inventory cannot authorize a changed grant, service or volume.
        _json(root / MANIFESTS[phase])
        _require(_bytes(root / MANIFESTS[phase], MAX_JSON) == raw,
                 "history manifest changes an unapproved definition")
    _require(_bytes(root / CONTEXT / "Dockerfile", 4096) == dockerfile,
             "history launcher differs")
    return plan


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    for name in ("prepare", "verify"):
        command = commands.add_parser(name)
        command.add_argument("--baseline", type=Path, required=True)
        command.add_argument("--baseline-plan-sha256", required=True)
        command.add_argument("--baseline-inventory-sha256", required=True)
        command.add_argument("--reader-sha256", required=True)
        if name == "prepare":
            command.add_argument("--reader", type=Path, required=True)
            command.add_argument("--output", type=Path, required=True)
        else:
            command.add_argument("--bundle", type=Path, required=True)
    args = parser.parse_args(argv)
    pins = {name: getattr(args, name) for name in
            ("reader_sha256", "baseline_plan_sha256", "baseline_inventory_sha256")}
    try:
        if args.command == "prepare":
            result = prepare(args.baseline, args.output, args.reader, **pins)
        else:
            result = validate_bundle(args.bundle, args.baseline, **pins)
    except (stage.Refused, OSError, ValueError, KeyError, TypeError):
        parser.exit(1, "history stage refused; no runtime action was attempted\n")
    print(json.dumps(result, sort_keys=True))


if __name__ == "__main__":
    main()
