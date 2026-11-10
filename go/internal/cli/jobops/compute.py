"""Bounded compute inventory. Never installs tools or changes GPU workloads."""

import csv, io, json, os, shutil, socket, subprocess, sys


def query(arguments):
    try:
        p = subprocess.run(arguments, capture_output=True, text=True, timeout=4)
        if p.returncode:
            return None, p.stderr.strip()[:200] or "query failed"
        return list(csv.reader(io.StringIO(p.stdout.strip()))), None
    except (OSError, subprocess.TimeoutExpired) as exc:
        return None, str(exc)[:200]


def number(value):
    try:
        return float(value.strip().split()[0])
    except (ValueError, IndexError):
        return None


def main():
    gpus, error = query(
        [
            "nvidia-smi",
            "--query-gpu=uuid,name,utilization.gpu,memory.used,memory.total",
            "--format=csv,noheader,nounits",
        ]
    )
    processes, process_error = query(
        [
            "nvidia-smi",
            "--query-compute-apps=pid,process_name,used_memory",
            "--format=csv,noheader,nounits",
        ]
    )
    rows = []
    for row in (gpus or [])[:8]:
        if len(row) != 5:
            continue
        rows.append(
            {
                "uuid": row[0].strip(),
                "name": row[1].strip(),
                "utilization_percent": number(row[2]),
                "memory_used_mib": number(row[3]),
                "memory_total_mib": number(row[4]),
            }
        )
    active = []
    for row in (processes or [])[:8]:
        if len(row) != 3:
            continue
        active.append(
            {
                "pid": int(row[0]) if row[0].strip().isdigit() else None,
                "name": row[1].strip()[:160],
                "memory_mib": number(row[2]),
            }
        )
    out = {
        "hostname": socket.gethostname(),
        "cpu_count": os.cpu_count(),
        "gpus": rows,
        "gpu_processes": active,
        "gpu_process_count": None if processes is None else len(processes),
        "gpu_state": "unknown"
        if processes is None or not rows
        else "busy"
        if processes
        else "idle",
    }
    try:
        mem = {}
        with open("/proc/meminfo") as f:
            for line in f:
                name, value = line.split(":", 1)
                if name in ["MemTotal", "MemAvailable"]:
                    mem[name] = int(value.strip().split()[0]) * 1024
        out["memory"] = {
            "total_bytes": mem.get("MemTotal"),
            "available_bytes": mem.get("MemAvailable"),
        }
    except (OSError, ValueError):
        out["memory"] = {"status": "unknown"}
    disk = shutil.disk_usage("/")
    out["disk"] = {"total_bytes": disk.total, "available_bytes": disk.free}
    warnings = [v for v in [error, process_error] if v]
    if len(gpus or []) > 8 or len(processes or []) > 8:
        warnings.append(
            "GPU detail limited to eight records; total process count retained"
        )
    if warnings:
        out["warnings"] = warnings
    print("WENDY_JOB " + json.dumps(out, separators=(",", ":")), flush=True)


if __name__ == "__main__":
    main()
