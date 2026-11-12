"""Linux resource accounting, kept independent of Wendy's telemetry pipeline."""

import math
import os
from pathlib import Path
import re
import statistics
import subprocess
import time


# A cgroup's io.stat lists a device only after the cgroup's first I/O to it, so a
# missing per-device counter (raw or summarized) is zero, not unavailable.
_CGROUP_IO_COUNTER = re.compile(r"\.service\.io\.\d+:\d+\.(?:rbytes|wbytes|rios|wios)(?:\.(?:total|per_second))?$")


def zero_when_absent(key):
    return _CGROUP_IO_COUNTER.search(key) is not None


def read_text(path):
    try:
        return Path(path).read_text()
    except (FileNotFoundError, PermissionError):
        return None


def key_values(text):
    return {parts[0].rstrip(":"): int(parts[1])
            for line in (text or "").splitlines()
            if len(parts := line.split()) >= 2 and parts[1].isdigit()}


def cgroup_path(pid):
    for line in (read_text(f"/proc/{pid}/cgroup") or "").splitlines():
        if line.startswith("0::"):
            path = Path("/sys/fs/cgroup") / line[3:].lstrip("/")
            if ".." not in path.parts and path.is_dir():
                return path
    return None


def service_pid(service):
    result = subprocess.run(
        ["systemctl", "show", service, "--property=MainPID", "--value"],
        check=True, capture_output=True, text=True, timeout=10)
    pid = int(result.stdout.strip())
    if pid <= 0:
        raise ValueError(f"{service} has no running MainPID")
    return pid


class Process:
    def __init__(self, pid, include_cgroup=False):
        self.pid = pid
        self.root = Path(f"/proc/{pid}")
        self.ticks = os.sysconf("SC_CLK_TCK")
        self.start = None
        self.cgroup = cgroup_path(pid) if include_cgroup else None
        if include_cgroup and self.cgroup is None:
            raise ValueError(f"PID {pid} has no cgroup v2 path under /sys/fs/cgroup; service accounting "
                             "needs the unified hierarchy. Use --pid for process-only measurement")
        if self.cgroup:
            own = cgroup_path(os.getpid())
            if own and (own == self.cgroup or self.cgroup in own.parents):
                raise ValueError("collector is inside the measured cgroup; run it in a separate service/session")
        self.sample()  # Fail before starting a workload if the PID is inaccessible.

    def sample(self):
        stat = (self.root / "stat").read_text()
        # comm can contain spaces and closing parentheses. Fields below start at #3.
        fields = stat[stat.rfind(")") + 2:].split()
        if fields[0] == "Z":
            raise ValueError(f"PID {self.pid} is a zombie")
        start = int(fields[19])
        if self.start is not None and start != self.start:
            raise ValueError(f"PID {self.pid} was reused")
        self.start = start
        if self.cgroup and cgroup_path(self.pid) != self.cgroup:
            raise ValueError(f"PID {self.pid} changed cgroup")
        status = key_values((self.root / "status").read_text())
        # Include reaped children (cutime/cstime), matching /proc/PID/io, so work
        # moved into helper commands still counts. A child counts once reaped.
        user, system = int(fields[11]) + int(fields[13]), int(fields[12]) + int(fields[14])
        data = {
            "process.cpu_seconds": (user + system) / self.ticks,
            "process.children_cpu_seconds": (int(fields[13]) + int(fields[14])) / self.ticks,
            "process.user_seconds": user / self.ticks,
            "process.system_seconds": system / self.ticks,
        }
        for key, name in (("VmRSS", "rss_bytes"), ("VmHWM", "lifetime_peak_rss_bytes"),
                          ("VmSwap", "swap_bytes")):
            if key in status:
                data[f"process.{name}"] = status[key] * 1024
        io = key_values(read_text(self.root / "io"))
        for key in ("read_bytes", "write_bytes", "cancelled_write_bytes", "rchar", "wchar", "syscr", "syscw"):
            if key in io:
                data[f"process.{key}"] = io[key]
        if self.cgroup:
            cpu = key_values(read_text(self.cgroup / "cpu.stat"))
            if "usage_usec" in cpu:
                data["service.cpu_seconds"] = cpu["usage_usec"] / 1e6
            for file, name in (("memory.current", "memory_bytes"), ("memory.swap.current", "swap_bytes")):
                value = read_text(self.cgroup / file)
                if value is not None:
                    data[f"service.{name}"] = int(value)
            memory = key_values(read_text(self.cgroup / "memory.stat"))
            for key in ("anon", "file", "kernel", "file_dirty", "file_writeback"):
                if key in memory:
                    data[f"service.{key}_bytes"] = memory[key]
            for key, value in key_values(read_text(self.cgroup / "memory.events")).items():
                data[f"service.events.{key}"] = value
            for line in (read_text(self.cgroup / "io.stat") or "").splitlines():
                device, *counters = line.split()
                for item in counters:
                    key, value = item.split("=")
                    if key in ("rbytes", "wbytes", "rios", "wios"):
                        data[f"service.io.{device}.{key}"] = int(value)
        return data


def host_sample(sysfs=Path("/sys")):
    data = {}
    # Keep devices separate: summing partitions, device-mapper and physical disks
    # would count the same I/O multiple times. diskstats sectors are 512 bytes.
    for line in (read_text("/proc/diskstats") or "").splitlines():
        f = line.split()
        if len(f) >= 14 and not f[2].startswith(("loop", "ram")):
            for index, key, scale in ((3, "reads", 1), (5, "read_bytes", 512),
                                      (6, "read_ms", 1), (7, "writes", 1),
                                      (9, "write_bytes", 512), (10, "write_ms", 1),
                                      (12, "busy_ms", 1)):
                data[f"disk.{f[2]}.{key}"] = int(f[index]) * scale
    for resource in ("cpu", "memory", "io"):
        for line in (read_text(f"/proc/pressure/{resource}") or "").splitlines():
            kind, *values = line.split()
            values = dict(v.split("=") for v in values)
            data[f"pressure.{resource}.{kind}_seconds"] = int(values["total"]) / 1e6
    for path in (sysfs / "class/thermal").glob("thermal_zone*/temp"):
        value = read_sensor(path)
        if value is not None:
            data[f"temperature.{path.parent.name}_c"] = value / 1000
    for path in (sysfs / "devices/system/cpu").glob("cpu[0-9]*/cpufreq/scaling_cur_freq"):
        value = read_sensor(path)
        if value is not None:
            data[f"frequency.{path.parent.parent.name}_khz"] = value
    return data


def read_sensor(path):
    """Optional host sensor. Drivers can fail reads (EIO, ENODATA, EAGAIN) or
    report non-numeric text; skip those rather than abort every snapshot."""
    try:
        return int(Path(path).read_text())
    except (OSError, ValueError):
        return None


def percentile(values, fraction):
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * fraction) - 1)]


def summarize(samples):
    """Summarize one phase. Never replace absent counters with zero, except
    cgroup io.stat devices, which are absent until the cgroup's first I/O."""
    if len(samples) < 2:
        raise ValueError("need at least two resource samples")
    elapsed = samples[-1]["time"] - samples[0]["time"]
    if elapsed <= 0:
        raise ValueError("sample times must increase")
    result = {"elapsed_seconds": elapsed}
    keys = set().union(*(s["values"] for s in samples))
    required = {key for key in keys if not zero_when_absent(key)}
    if any({key for key in s["values"] if not zero_when_absent(key)} != required for s in samples):
        raise ValueError("available metrics changed during the run")
    for key in sorted(keys):
        values = [s["values"].get(key, 0) for s in samples]
        gauge = (key.endswith("_bytes") and not any(x in key for x in ("read_bytes", "write_bytes")))
        gauge = gauge or "temperature." in key or "frequency." in key
        if gauge:
            result[f"{key}.mean"] = sum(
                (a["values"][key] + b["values"][key]) / 2 * (b["time"] - a["time"])
                for a, b in zip(samples, samples[1:])) / elapsed
            result[f"{key}.peak"] = max(values)
            if key.endswith("rss_bytes") or key.endswith("memory_bytes"):
                xs = [s["time"] - samples[0]["time"] for s in samples]
                xm, ym = statistics.mean(xs), statistics.mean(values)
                result[f"{key}.growth_per_second"] = sum((x-xm)*(y-ym) for x, y in zip(xs, values)) / sum((x-xm)**2 for x in xs)
        else:
            if any(b < a for a, b in zip(values, values[1:])):
                raise ValueError(f"counter reset: {key}")
            delta = values[-1] - values[0]
            result[f"{key}.total"] = delta
            result[f"{key}.per_second"] = delta / elapsed
            if key.endswith("cpu_seconds"):
                rates = [100 * (b["values"][key] - a["values"][key]) / (b["time"] - a["time"])
                         for a, b in zip(samples, samples[1:])]
                result[f"{key}.percent_mean"] = 100 * delta / elapsed
                result[f"{key}.percent_p95"] = percentile(rates, .95)
    return result


def snapshot(processes, phase):
    values = {}
    for name, process in processes.items():
        values.update({f"{name}.{k}": v for k, v in process.sample().items()})
    values.update({f"host.{k}": v for k, v in host_sample().items()})
    return {"time": time.monotonic(), "wall_time": time.time(), "phase": phase, "values": values}
