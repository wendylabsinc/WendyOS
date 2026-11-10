"""Create one named, idempotent job and retain its execution metadata."""

import hashlib
import json
import os
import stat
from pathlib import Path
import re
import subprocess
import sys
import time


def safe_relative(name):
    return isinstance(name, str) and 0 < len(name) <= 1024 and "\x00" not in name and not Path(name).is_absolute() and ".." not in Path(name).parts and str(Path(name)) == name and name != "."


def source_fd(working, name, directory=False):
    if not safe_relative(name):
        raise ValueError("source contracts require safe relative paths")
    parent = os.open(working, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    parts = Path(name).parts
    try:
        for part in parts[:-1]:
            following = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
            os.close(parent)
            parent = following
        flags = os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK
        if directory:
            flags |= os.O_DIRECTORY
        return os.open(parts[-1], flags, dir_fd=parent)
    finally:
        os.close(parent)


def source_file(working, name, limit=None):
    fd = source_fd(working, name)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or (limit is not None and info.st_size > limit):
            raise ValueError("source must be a bounded regular file: " + name)
        return os.fdopen(fd, "rb")
    except BaseException:
        os.close(fd)
        raise


def verify_sources(working, spec):
    files = dict(spec.get("source_hashes", {}))
    contract = spec.get("source_manifest")
    exact = []
    manifest_hash = None
    if contract:
        with source_file(working, contract['path'], 1024 * 1024) as stream:
            raw = stream.read(1024 * 1024 + 1)
        if len(raw) > 1024 * 1024:
            raise ValueError("source manifest exceeds read limit")
        manifest_hash = hashlib.sha256(raw).hexdigest()
        if manifest_hash != contract['sha256'].lower():
            raise ValueError("source manifest hash mismatch")
        manifest = json.loads(raw)
        if not isinstance(manifest, dict) or manifest.get('schema') != 'wendy.sources.v1' or set(manifest) - {'schema', 'files', 'exact_directories'}:
            raise ValueError("invalid source manifest schema")
        declared = manifest.get('files')
        exact = manifest.get('exact_directories', [])
        if not isinstance(declared, dict) or not 1 <= len(declared) <= 4096 or not isinstance(exact, list) or len(exact) > 64 or len(exact) != len(set(exact)):
            raise ValueError("source manifest inventory exceeds limits")
        for name, expected in declared.items():
            if name in files and files[name].lower() != expected.lower():
                raise ValueError("conflicting source hashes")
            files[name] = expected
    if len(files) > 4096:
        raise ValueError("source inventory exceeds 4096 files")
    for name, expected in files.items():
        if not isinstance(expected, str) or not re.fullmatch(r'[0-9a-fA-F]{64}', expected):
            raise ValueError("source contracts require SHA-256 digests")
        digest = hashlib.sha256()
        with source_file(working, name) as stream:
            for block in iter(lambda: stream.read(1024 * 1024), b''):
                digest.update(block)
        if digest.hexdigest() != expected.lower():
            raise ValueError("source hash mismatch: " + name)
    for name in exact:
        if not safe_relative(name):
            raise ValueError("exact source directory must be relative")
        expected = {Path(p).name for p in files if Path(p).parent == Path(name)}
        actual = set()
        fd = source_fd(working, name, directory=True)
        try:
            with os.scandir(fd) as entries:
                for index, entry in enumerate(entries):
                    if index >= 4096:
                        raise ValueError("exact source directory exceeds scan limit")
                    if entry.is_symlink():
                        raise ValueError("exact source directory contains a symlink")
                    if not entry.is_dir(follow_symlinks=False):
                        actual.add(entry.name)
        finally:
            os.close(fd)
        if expected != actual:
            raise ValueError("source directory differs from manifest: " + name)
    return {"verified_files": len(files), "manifest_sha256": manifest_hash, "exact_directories": len(exact), "scope": "original_submission"}


def main():
    request = json.loads(sys.argv[1])
    spec = request["spec"]
    name = spec["name"]
    if not re.fullmatch("[a-z0-9][a-z0-9_-]{0,63}", name):
        raise ValueError("invalid job name")
    # Step logs share the job directory with the supervisor's runner.log.
    if any(step["name"] == "runner" for step in spec["steps"]):
        raise ValueError('step name "runner" is reserved for the job supervisor')
    working = Path(spec["working_directory"]).resolve(strict=True)
    if not working.is_dir() or working == Path("/"):
        raise ValueError("working_directory must be an existing non-root directory")
    spec["working_directory"] = str(working)
    encoded = json.dumps(spec, sort_keys=True, separators=(",", ":")).encode()
    digest = hashlib.sha256(encoded).hexdigest()
    parent = working
    for part in [".wendy", "jobs"]:
        parent = parent / part
        if parent.is_symlink():
            raise ValueError("job storage must not contain symlinks")
    root = parent / name
    if root.exists():
        if root.is_symlink() or (root / "spec.json").read_bytes() != encoded:
            raise ValueError("job name already exists with another specification")
        receipt = json.loads((root / "receipt.json").read_text())
        receipt["reused"] = True
        try:
            current = json.loads((root / "status.json").read_text())
            receipt["reported_state"] = current.get("state", "unknown")
            receipt["stage"] = current.get("stage", "unknown")
        except (OSError, ValueError):
            receipt["reported_state"] = "unknown"
        print("WENDY_JOB " + json.dumps(receipt, separators=(",", ":")), flush=True)
        return
    source_check = verify_sources(working, spec)
    parent = working
    for part in [".wendy", "jobs"]:
        parent = parent / part
        if parent.is_symlink():
            raise ValueError("job storage must not contain symlinks")
        parent.mkdir(exist_ok=True)
    root.mkdir(mode=0o700)
    (root / "spec.json").write_bytes(encoded)
    (root / "runner.py").write_text(request["worker_source"])
    (root / "status.json").write_text(
        json.dumps(
            {"state": "starting", "stage": "starting", "started_unix": time.time()}
        )
    )
    with (root / "runner.log").open("xb") as log:
        process = subprocess.Popen(
            ["python3", "-u", str(root / "runner.py")],
            stdin=subprocess.DEVNULL,
            stdout=log,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
    receipt = {
        "job_path": str(root),
        "name": name,
        "runner_pid": process.pid,
        "spec_sha256": digest,
        "reported_state": "starting",
        "reused": False,
        "source_check": source_check,
        "runner_sha256": hashlib.sha256(request["worker_source"].encode()).hexdigest(),
    }
    (root / "receipt.json").write_text(json.dumps(receipt, separators=(",", ":")))
    print("WENDY_JOB " + json.dumps(receipt, separators=(",", ":")), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(
            "WENDY_JOB " + json.dumps({"error": str(exc)[:800]}, separators=(",", ":")),
            flush=True,
        )
        sys.exit(1)
