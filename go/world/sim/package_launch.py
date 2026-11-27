"""Run an explicitly requested verified package in a constrained local Docker VM.

Only manifest-declared bytes enter the container. The worker report and observed
Docker configuration are retained separately; neither is physical authority.
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import selectors
import signal
import subprocess
import tempfile
import time
import uuid

from .run import MAX_FILE, MAX_MANIFEST, PackageError, secure_read, load_model_package
from .package_runner import canonical_bytes, decode, validate_request, validate_result

MEMORY_BYTES = 2 << 30
COMMAND_BYTES = 2 << 20
CONTAINER_WALL_SECONDS = 40


class LaunchError(ValueError):
    pass


def command(args, timeout=10, limit=COMMAND_BYTES):
    """Bound both command output and wall time, including a noisy child image."""
    process = subprocess.Popen(args, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True)
    streams = {"stdout": bytearray(), "stderr": bytearray()}
    selector = selectors.DefaultSelector()
    for name in streams:
        selector.register(getattr(process, name), selectors.EVENT_READ, name)
    deadline = time.monotonic() + timeout
    try:
        while selector.get_map():
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise LaunchError("container command timed out; completion is unconfirmed")
            for key, _ in selector.select(min(remaining, .25)):
                chunk = os.read(key.fileobj.fileno(), 65536)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                streams[key.data].extend(chunk)
                if sum(map(len, streams.values())) > limit:
                    raise LaunchError("container command output exceeds bound")
        code = process.wait(timeout=max(.01, deadline-time.monotonic()))
        if code != 0:
            # Child diagnostics remain data. Do not turn their text into commands.
            detail = bytes(streams["stderr"])[-2048:].decode("utf-8", "replace")
            raise LaunchError(f"container command failed ({code}): {detail}")
        return bytes(streams["stdout"])
    finally:
        selector.close()
        # A departed CLI may leave descendants holding its pipes open.
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait(timeout=5)
        process.stdout.close()
        process.stderr.close()


def local_image(image):
    if not image or image.startswith("-") or len(image) > 256:
        raise LaunchError("explicit local image required")
    address = os.environ.get("DOCKER_HOST", "")
    if address and not address.startswith("unix://"):
        raise LaunchError("only a local Unix-socket Docker engine is supported")
    contexts = json.loads(command(["docker", "context", "inspect"]))
    if len(contexts) != 1 or not contexts[0]["Endpoints"]["docker"]["Host"].startswith("unix://"):
        raise LaunchError("only a local Docker context is supported")
    info = json.loads(command(["docker", "info", "--format", "{{json .}}"]))
    if info.get("OSType") != "linux" or not info.get("MemoryLimit") or not info.get("PidsLimit"):
        raise LaunchError("Linux Docker memory and PID limits are required")
    images = json.loads(command(["docker", "image", "inspect", image]))
    if len(images) != 1 or images[0].get("Os") != "linux" or images[0].get("Config", {}).get("Volumes"):
        raise LaunchError("image must be local Linux without declared writable volumes")
    identity = images[0].get("Id", "")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", identity):
        raise LaunchError("invalid local image identity")
    return identity


def stage_package(package_path, expected_digest, destination):
    """Reverify the original bytes while copying; never mount unrelated files."""
    package_path = Path(package_path).absolute()
    package = load_model_package(package_path)
    if package["_digest"] != expected_digest:
        raise LaunchError("package digest differs from requested run")
    root = package_path.parent.resolve()
    raw = secure_read(root, package_path.name, MAX_MANIFEST, "manifest")
    if hashlib.sha256(raw).hexdigest() != expected_digest:
        raise LaunchError("manifest changed during staging")
    destination.mkdir(mode=0o700)
    (destination / "world.json").write_bytes(raw)
    inventory = [(package["mujoco"]["path"], package["mujoco"]["sha256"], None)]
    inventory += [(a["path"], a["sha256"], a["size"]) for a in package.get("assets", [])]
    for relative, digest, size in inventory:
        data = secure_read(root, relative, MAX_FILE, "asset")
        if hashlib.sha256(data).hexdigest() != digest or (size is not None and len(data) != size):
            raise LaunchError("asset changed during staging")
        target = destination / relative
        target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
        target.write_bytes(data)
    return package


def inspect_policy(name, image, user, expected_mounts):
    records = json.loads(command(["docker", "inspect", name]))
    if len(records) != 1:
        raise LaunchError("container inspection unavailable")
    record = records[0]
    host, config = record["HostConfig"], record["Config"]
    if (record.get("Image") != image or config.get("User") != user or
        host.get("NetworkMode") != "none" or not host.get("ReadonlyRootfs") or
        host.get("Privileged") or host.get("Memory") != MEMORY_BYTES or
        host.get("MemorySwap") != MEMORY_BYTES or host.get("NanoCpus") != 1_000_000_000 or
        host.get("PidsLimit") != 64 or host.get("CapDrop") != ["ALL"] or
        "no-new-privileges" not in host.get("SecurityOpt", []) or
        host.get("Tmpfs") != {"/tmp": "rw,noexec,nosuid,size=16777216"} or
        host.get("CapAdd") or host.get("Devices") or host.get("DeviceRequests") or
        host.get("DeviceCgroupRules") or
        host.get("PidMode") or host.get("IpcMode") not in ("private", "") or host.get("UTSMode")):
        raise LaunchError("container policy differs from required offline limits")
    mounts = {m["Destination"]:m for m in record["Mounts"]}
    if (len(record["Mounts"]) != 3 or set(mounts) != {"/package", "/request.json", "/output"} or
        set(expected_mounts) != set(mounts) or any(
        mounts[p]["Type"] != "bind" or mounts[p]["RW"] != (p == "/output") or
        mounts[p].get("Source") != expected_mounts[p] for p in mounts)):
        raise LaunchError("container mount policy differs from declared inputs")
    return {"image": image, "network_mode": host["NetworkMode"], "read_only_root": True,
        "user": user, "memory_bytes": host["Memory"], "memory_and_swap_bytes": host["MemorySwap"],
        "nano_cpus": host["NanoCpus"], "pids_limit": host["PidsLimit"], "cap_drop": host["CapDrop"],
        "security_options": host["SecurityOpt"], "tmpfs": host["Tmpfs"],
        "mounts": [{"destination": p, "read_only": not mounts[p]["RW"]} for p in sorted(mounts)],
        "container_wall_limit_seconds": CONTAINER_WALL_SECONDS}


def publish_new(path, data):
    path = Path(path).absolute()
    parent = path.parent.resolve(strict=True)
    if path.exists() or path.is_symlink():
        raise LaunchError("output already exists")
    temporary = None
    try:
        fd, temporary = tempfile.mkstemp(prefix=".world-execution-", dir=parent)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.link(temporary, parent/path.name, follow_symlinks=False)
        directory = os.open(parent, os.O_RDONLY|os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary is not None:
            os.unlink(temporary)


def launch(package_path, request_path, image, output_path):
    output_path = Path(output_path)
    if output_path.exists() or output_path.is_symlink() or not output_path.parent.is_dir():
        raise LaunchError("output must be a new file in an existing directory")
    if os.getuid() == 0:
        raise LaunchError("launch from a non-root host user")
    request_path = Path(request_path).absolute()
    raw = secure_read(request_path.parent.resolve(), request_path.name, 64<<10, "request")
    request = validate_request(raw)
    identity = local_image(image)
    name = "world-package-" + uuid.uuid4().hex
    user = f"{os.getuid()}:{os.getgid()}"
    attempted = False
    with tempfile.TemporaryDirectory(prefix="wendy-package-run-") as temporary:
        folder = Path(temporary)
        package = stage_package(package_path, request["package_sha256"], folder/"package")
        (folder/"request.json").write_bytes(canonical_bytes(request))
        (folder/"output").mkdir(mode=0o700)
        try:
            args = ["docker", "create", "--name", name, "--label", "wendy.world.package-run="+name,
                "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
                "--memory", str(MEMORY_BYTES), "--memory-swap", str(MEMORY_BYTES), "--cpus", "1",
                "--pids-limit", "64", "--ulimit", "core=0:0", "--ulimit", "nofile=128:128",
                "--tmpfs", "/tmp:rw,noexec,nosuid,size=16777216", "--user", user,
                "--mount", f"type=bind,src={folder/'package'},dst=/package,readonly",
                "--mount", f"type=bind,src={folder/'request.json'},dst=/request.json,readonly",
                "--mount", f"type=bind,src={folder/'output'},dst=/output",
                identity, "--package", "/package/world.json", "--request", "/request.json", "--output", "/output/result.json"]
            attempted = True
            command(args)
            policy = inspect_policy(name, identity, user, {
                "/package": str(folder/"package"), "/request.json": str(folder/"request.json"),
                "/output": str(folder/"output")})
            command(["docker", "start", "--attach", name], timeout=CONTAINER_WALL_SECONDS)
            state = json.loads(command(["docker", "inspect", name]))[0]["State"]
            if (state.get("Running") is not False or type(state.get("ExitCode")) is not int or
                state["ExitCode"] != 0 or state.get("OOMKilled") is not False):
                raise LaunchError("container did not complete successfully")
            result_bytes = secure_read(folder/"output", "result.json", 2<<20, "worker report")
            report = validate_result(decode(result_bytes), request)
            if report["package"] != {"sha256": package["_digest"], **{key: package[key] for key in
                                     ("schema_version", "site_id", "package_revision", "frame_revision")}}:
                raise LaunchError("worker package metadata differs from staged package")
            execution = {"schema_version": "wendy.world.simulation-execution.v1", "container_policy": policy,
                "container_exit": {"exit_code": state["ExitCode"], "oom_killed": state["OOMKilled"]},
                "result": report, "result_sha256": hashlib.sha256(result_bytes).hexdigest(),
                "limitations": ["explicit synthetic package execution, not physical activation", "trusted local image and Docker engine", "resource policy is not a native-code security proof"]}
        finally:
            if attempted:
                command(["docker", "rm", "--force", name])
        publish_new(output_path, canonical_bytes(execution))
        return execution


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--package", required=True, type=Path)
    parser.add_argument("--request", required=True, type=Path)
    parser.add_argument("--image", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    try:
        result = launch(args.package, args.request, args.image, args.output)
        print(json.dumps({"saved": str(args.output), "image": result["container_policy"]["image"],
                          "package_sha256": result["result"]["package"]["sha256"]}))
        return 0
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        print(f"package execution failed: {error}", file=__import__("sys").stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
