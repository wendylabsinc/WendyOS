#!/usr/bin/env python3
"""Prepare only the caller's disposable, private cgroup namespace.

The launcher's Docker inspect receipt is the authority for isolation flags.
These additional guards reject common accidental host/shared launches. This is
not a general host provisioning tool and must never remount a host cgroup bind.
"""

import json
import os
from pathlib import Path
import platform
import subprocess
import sys
import time


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def unified_cgroup(pid):
    lines = Path(f"/proc/{pid}/cgroup").read_text().splitlines()
    require(len(lines) == 1 and lines[0].startswith("0::"), "unified cgroup v2 required")
    return lines[0][3:]


def status_value(name):
    for line in Path("/proc/self/status").read_text().splitlines():
        key, _, value = line.partition(":")
        if key == name:
            return value.strip()
    raise RuntimeError(f"missing process status {name}")


def prepare():
    require(platform.system() == "Linux" and os.geteuid() == 0, "Linux root required")
    require(Path("/.dockerenv").is_file(), "expected a disposable Docker node")
    require(unified_cgroup("self") == "/", "caller is not at its private cgroup namespace root")
    require(unified_cgroup(1) == "/", "PID 1 must share the private cgroup namespace root")
    require(os.readlink("/proc/self/ns/cgroup") == os.readlink("/proc/1/ns/cgroup"),
            "PID 1 and preflight must share a cgroup namespace")
    caps = int(status_value("CapEff"), 16)
    require(caps & (1 << 21), "SYS_ADMIN is required for private nested runtime mounts")
    require(caps & (1 << 12), "NET_ADMIN is required for private app CNI networking")
    require(not caps & (1 << 25), "SYS_TIME must not be granted")

    cgroot = Path("/sys/fs/cgroup")
    require(cgroot.is_dir() and not cgroot.is_symlink(), "missing cgroup mountpoint")
    # A NEW cgroup2 mount is rooted in this cgroup namespace. Stage it on a fresh
    # private path, then bind that mount at the standard runtime path. OrbStack
    # returns EBUSY when a new cgroup2 superblock mount directly covers the
    # inherited cgroup mount; the fresh-mount-and-bind sequence succeeds. This
    # never makes the inherited bind writable or exposes a host cgroup mount.
    staged = Path("/run/agentvm/cgroup")
    staged.mkdir(mode=0o700)
    subprocess.run(["mount", "-t", "cgroup2", "-o", "rw,nosuid,nodev,noexec",
                    "cgroup2", str(staged)], check=True)
    subprocess.run(["mount", "--bind", str(staged), str(cgroot)], check=True)
    # findmnt --target returns both the hidden inherited mount and the new one.
    # Resolve the active mount by opening the path and reading its kernel mount
    # ID, then match that ID in mountinfo instead of relying on listing order.
    fd = os.open(cgroot, os.O_PATH | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        fdinfo = Path(f"/proc/self/fdinfo/{fd}").read_text().splitlines()
        mount_id = next(line.split(":", 1)[1].strip() for line in fdinfo
                        if line.startswith("mnt_id:"))
        fields = next(line.split() for line in Path("/proc/self/mountinfo").read_text().splitlines()
                      if line.split()[0] == mount_id)
    finally:
        os.close(fd)
    separator = fields.index("-")
    require(fields[3] == "/" and fields[4] == str(cgroot)
            and fields[separator + 1] == "cgroup2" and "rw" in fields[5].split(","),
            "active namespace-root cgroup2 mount is not writable")
    result = "cgroup2 " + fields[5]
    require(unified_cgroup("self") == "/", "unexpected cgroup namespace changed during mount")

    # Keep the namespace root populated. Enabling domain subtree controllers
    # would make subsequent ordinary docker exec placement fail. These fixtures
    # do not request CPU/memory limits; the parent Docker node is resource-limited.
    # Child cgroups still provide exact service placement and device policies.
    probe = cgroot / f"agentvm-preflight-{os.getpid()}"
    probe.mkdir(mode=0o700)
    process = None
    try:
        process = subprocess.Popen(["sleep", "30"], close_fds=True)
        (probe / "cgroup.procs").write_text(str(process.pid))
        require(unified_cgroup(process.pid) == f"/{probe.name}", "child cgroup placement failed")
    finally:
        if process is not None:
            process.terminate()
            process.wait(timeout=5)
        probe.rmdir()

    return {
        "version": 1,
        "kernel": platform.release(),
        "machine": platform.machine(),
        "pid": os.getpid(),
        "cgroup_root": "/",
        "cgroup_mount": result,
        "cgroup_controllers_available": (cgroot / "cgroup.controllers").read_text().split(),
        "cgroup_controllers_enabled": (cgroot / "cgroup.subtree_control").read_text().split(),
        "child_cgroup_probe": "passed",
        "effective_capabilities_hex": status_value("CapEff"),
        "sys_time": False,
        "namespaces": {name: os.readlink(f"/proc/self/ns/{name}")
                       for name in ("cgroup", "pid", "net", "mnt", "user")},
        "prepared_unix_seconds": time.time(),
    }


if __name__ == "__main__":
    try:
        print(json.dumps(prepare(), sort_keys=True))
    except Exception as error:
        print(f"agentvm preflight refused: {error}", file=sys.stderr)
        sys.exit(1)
