"""Fixed read-only host probe, executed through the local Wendy admin socket."""
import csv
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import xml.etree.ElementTree as ET


def cgroup(pid):
    for line in Path(f"/proc/{pid}/cgroup").read_text().splitlines():
        if line.startswith("0::"):
            return line[3:]
    raise ValueError("cgroup v2 required")


def query(fields, kind="gpu"):
    result = subprocess.run(["nvidia-smi", f"--query-{kind}={fields}",
                             "--format=csv,noheader,nounits"], capture_output=True,
                            text=True, timeout=2, check=True)
    return [[x.strip() for x in row] for row in csv.reader(result.stdout.splitlines())]


def gpu_processes():
    # XML includes graphics/MPS/other contexts that --query-compute-apps misses.
    result = subprocess.run(["nvidia-smi", "-q", "-x"], capture_output=True,
                            text=True, timeout=2, check=True)
    if len(result.stdout) > 4 * 1024**2:
        raise ValueError("GPU inventory exceeds bound")
    processes = {}
    for gpu in ET.fromstring(result.stdout).findall("gpu"):
        uuid = gpu.findtext("uuid")
        entries = gpu.find("processes")
        if not uuid or entries is None or (entries.text or "").strip():
            raise ValueError("GPU process inventory unavailable")
        processes[uuid] = [int(p.findtext("pid")) for p in entries.findall("process_info")]
    return processes


def snapshot(app_id):
    groups = set()
    # Identify our containers by the agent-injected identity, then account by
    # cgroup, including children that replace their environment.
    identity = ("WENDY_APP_ID=" + app_id).encode()
    for proc in Path("/proc").iterdir():
        if not proc.name.isdigit():
            continue
        try:
            with (proc / "environ").open("rb") as environment:
                owned = identity in environment.read(65536).split(b"\0")
            if owned:
                groups.add(cgroup(proc.name))
        except (FileNotFoundError, ProcessLookupError):
            continue
    if not groups or "/" in groups:
        raise ValueError("cannot identify isolated app cgroups")
    own_usec = 0
    for group in groups:
        values = dict(line.split() for line in (Path("/sys/fs/cgroup") / group.lstrip("/") / "cpu.stat").read_text().splitlines())
        own_usec += int(values["usage_usec"])
    cpu = [int(v) for v in Path("/proc/stat").read_text().splitlines()[0].split()[1:9]]
    mem = {}
    for line in Path("/proc/meminfo").read_text().splitlines():
        name, value = line.split(":", 1)
        if name in ("MemAvailable", "MemTotal"):
            mem[name] = int(value.split()[0]) * 1024
    result = {"time": time.monotonic(), "boot_id": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
              "cpu_total": sum(cpu), "cpu_idle": cpu[3] + cpu[4], "cpu_count": os.cpu_count(),
              "own_cpu_usec": own_usec, "groups": sorted(groups),
              "memory_available": mem["MemAvailable"], "gpus": [], "gpu_known": False,
              "gpu_present": Path("/proc/driver/nvidia/version").exists()}
    try:
        rows = query("index,uuid,utilization.gpu,memory.free,memory.total,display_active,mig.mode.current")
        processes = gpu_processes()
        if set(processes) != {row[1] for row in rows}:
            raise ValueError("GPU inventories disagree")
        foreign = set()
        for gpu, pid in ((gpu, pid) for gpu, pids in processes.items() for pid in pids):
            try:
                owned = cgroup(int(pid)) in groups
            except (OSError, ValueError):
                owned = False
            if not owned:
                foreign.add(gpu)
        def number(value):
            try:
                return float(value)
            except ValueError:
                return None
        for index, uuid, utilization, free, total, display, mig in rows:
            result["gpus"].append({"index": int(index), "uuid": uuid,
                "utilization": number(utilization), "free_mib": number(free),
                "total_mib": number(total), "foreign": uuid in foreign,
                # Display and MIG devices need separate admission policies.
                "supported": display == "Disabled" and mig in ("Disabled", "[N/A]", "N/A")})
        result["gpu_known"] = bool(rows)
    except (OSError, ValueError, TypeError, ET.ParseError, subprocess.SubprocessError):
        pass
    return result


if __name__ == "__main__":
    os.nice(19)
    while True:
        try:
            result = snapshot(sys.argv[1])
        except Exception as error:
            result = {"error": str(error)}
        print("SPARE_SAMPLE " + json.dumps(result), flush=True)
        time.sleep(2)
