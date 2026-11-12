#!/usr/bin/env python3
"""Collect agent resource benchmarks on Linux, or compare reports anywhere."""

import argparse
from concurrent.futures import ThreadPoolExecutor, wait
import hashlib
import html
import json
import math
import os
from pathlib import Path
import platform
import re
import signal
import subprocess
import sys
import threading
import time

from compare import compare, markdown, validate_policy
from metrics import Process, key_values, read_text, service_pid, snapshot, summarize
import workloads


# Invalid input and environment failures exit 2, so they never look like a
# gated regression (exit 1).
INVALID_INPUT = (ValueError, KeyError, TypeError, AttributeError, IndexError, OSError,
                 subprocess.SubprocessError)


def write_json(path, value):
    path = Path(path)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(value, indent=2, allow_nan=False) + "\n")
    temporary.replace(path)


def load_policy(path):
    return validate_policy(json.loads(Path(path).read_text())) if path else None


def write_comparison(directory, result):
    write_json(directory / "comparison.json", result)
    text = markdown(result)
    (directory / "comparison.md").write_text(text)
    return text


def positive(value, name, allow_zero=False, integer=False):
    if (isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value)
            or value < 0 or (value == 0 and not allow_zero) or (integer and not isinstance(value, int))):
        raise ValueError(f"{name} must be a {'nonnegative' if allow_zero else 'positive'} {'integer' if integer else 'number'}")


def validate_profile(profile):
    if profile.get("schema") != 1:
        raise ValueError("profile schema must be 1")
    if set(profile) - {"schema", "repetitions", "sample_seconds", "settle_seconds", "cooldown_seconds", "scenarios"}:
        raise ValueError("unknown profile setting")
    positive(profile["repetitions"], "repetitions", integer=True)
    positive(profile["sample_seconds"], "sample_seconds")
    for key in ("settle_seconds", "cooldown_seconds"):
        positive(profile[key], key, allow_zero=True)
    if not profile["scenarios"]:
        raise ValueError("profile needs scenarios")
    names = set()
    for scenario in profile["scenarios"]:
        name = scenario["name"]
        if not re.fullmatch(r"[a-zA-Z0-9_-]+", name) or name in names:
            raise ValueError(f"invalid or duplicate scenario name: {name}")
        names.add(name)
        common = {"name", "kind", "setup", "cleanup", "ready", "verify", "cwd", "timeout_seconds"}
        specific = {"idle": {"seconds"}, "telemetry": {"requests", "requests_per_second", "batch", "payload_bytes", "endpoint", "max_lag_seconds"},
                    "command": {"command", "operations"}}
        if scenario["kind"] not in specific or set(scenario) - common - specific[scenario["kind"]]:
            raise ValueError(f"invalid settings in {name}")
        for key in ("command", "verify", "setup", "cleanup", "ready"):
            if key in scenario and (not isinstance(scenario[key], list) or not scenario[key]
                                    or not all(isinstance(v, str) and v for v in scenario[key])):
                raise ValueError(f"{name}.{key} must be a nonempty argument array")
        if scenario["kind"] == "idle":
            positive(scenario["seconds"], "seconds")
        elif scenario["kind"] == "telemetry":
            for key in ("requests", "batch", "payload_bytes"):
                positive(scenario[key], key, integer=True)
            positive(scenario["requests_per_second"], "requests_per_second")
            positive(scenario.get("max_lag_seconds", 1), "max_lag_seconds")
            if scenario["batch"] * (scenario["payload_bytes"] + 16) > 1024**2:
                raise ValueError("telemetry batch is too large")
        else:
            if "command" not in scenario or "verify" not in scenario:
                raise ValueError("command workloads require command and verify argument arrays")
            positive(scenario["operations"], "operations", integer=True)
        if any(key in scenario for key in ("command", "verify", "setup", "cleanup", "ready")):
            positive(scenario["timeout_seconds"], "timeout_seconds")


def binary_sha(pid):
    digest = hashlib.sha256()
    with open(f"/proc/{pid}/exe", "rb") as binary:
        for chunk in iter(lambda: binary.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def environment(label, metadata):
    cpu = read_text("/proc/cpuinfo") or ""
    cpu_model = next((line.split(":", 1)[1].strip() for line in cpu.splitlines()
                      if line.startswith(("model name", "Hardware"))), platform.machine())
    return {"label": label, "kernel": platform.release(), "architecture": platform.machine(),
            "cpu_count": os.cpu_count(), "cpu_model": cpu_model,
            "memory_kib": key_values(read_text("/proc/meminfo")).get("MemTotal"),
            "os_release": read_text("/etc/os-release"), "metadata": metadata}


def hook(scenario, name, log, stop):
    if name in scenario:
        workloads.command(scenario[name], scenario["timeout_seconds"], log, stop, scenario.get("cwd"))


def charts(path, runs):
    """Small standalone SVG charts; no network, chart libraries or active content."""
    parts = ["<!doctype html><meta charset='utf-8'><title>Agent benchmark</title>",
             "<style>body{font:16px system-ui;max-width:1050px;margin:32px auto;padding:16px}svg{width:100%;height:auto;background:#f5f7fa}path{fill:none;stroke-width:2}small{color:#555}</style>",
             "<h1>Agent benchmark</h1><p>Measured phase only. CPU uses 100% per core. Memory is process RSS. I/O is process storage accounting.</p>"]
    for run, samples in runs:
        parts.append(f"<h2>{html.escape(run['scenario'])}, repetition {run['repeat']}</h2>")
        t0 = samples[0]["time"]
        elapsed = max(.001, samples[-1]["time"] - t0)
        for title, keys, rate, scale in (
            ("CPU (%)", ["agent.process.cpu_seconds"], True, 100),
            ("Memory (MiB)", ["agent.process.rss_bytes"], False, 1/1024**2),
            ("Disk I/O (MiB/s), blue reads / orange writes", ["agent.process.read_bytes", "agent.process.write_bytes"], True, 1/1024**2),
        ):
            lines = []
            for key in keys:
                points = []
                for index, sample in enumerate(samples):
                    if key not in sample["values"] or (rate and index == 0):
                        continue
                    value = sample["values"][key]
                    if rate:
                        previous = samples[index - 1]
                        value = (value - previous["values"][key]) / (sample["time"] - previous["time"])
                    points.append((sample["time"]-t0, value * scale))
                lines.append(points)
            if not any(lines):
                parts.append(f"<p>{title}: unavailable</p>")
                continue
            maximum = max(.001, max(y for line in lines for _, y in line))
            parts.append(f"<p>{title} <small>0–{maximum:.3g}; 0–{elapsed:.1f} seconds</small></p><svg viewBox='0 0 960 160' role='img' aria-label='{html.escape(title)}'>")
            for points, color in zip(lines, ("#1565c0", "#d65f00")):
                d = " ".join(f"{'M' if i == 0 else 'L'}{10+940*x/elapsed:.2f},{150-140*y/maximum:.2f}" for i, (x, y) in enumerate(points))
                parts.append(f"<path stroke='{color}' d='{d}'/>")
            parts.append("</svg>")
    Path(path).write_text("\n".join(parts))


def collect(args):
    if sys.platform != "linux":
        raise ValueError("collection requires Linux; comparison works on any platform")
    profile = json.loads(args.profile.read_text())
    validate_profile(profile)
    metadata = json.loads(args.metadata.read_text()) if args.metadata else {}
    output = args.output
    output.mkdir(parents=True, exist_ok=False)
    report = {"schema": 1, "status": "running", "revision": args.revision, "runs": [],
              "compatibility": {"profile": profile, "environment": environment(args.environment, metadata),
                                "service": args.service if args.pid is None else None,
                                "observed_services": sorted(args.observe_service)},
              "notes": ["Missing metrics are unavailable, never zero.",
                        "OTLP HTTP success confirms receiver acceptance, not durable storage or cloud delivery.",
                        "Lifetime RSS high-water marks may predate this run; use sampled RSS peaks for comparisons."]}
    stop = threading.Event()
    plotted = []
    first_binary = None
    def interrupted(signum, frame):
        stop.set()
        raise KeyboardInterrupt(f"signal {signum}")
    old_handlers = {s: signal.signal(s, interrupted) for s in (signal.SIGINT, signal.SIGTERM)}
    write_json(output / "report.json", report)
    try:
        for repeat in range(1, profile["repetitions"] + 1):
            for scenario in profile["scenarios"]:
                log = output / f"{scenario['name']}-{repeat}.log"
                run = {"scenario": scenario["name"], "repeat": repeat, "status": "running"}
                report["runs"].append(run)
                print(f"{scenario['name']} {repeat}/{profile['repetitions']}", flush=True)
                try:
                    hook(scenario, "setup", log, stop)
                    hook(scenario, "ready", log, stop)
                    pid = args.pid if args.pid is not None else service_pid(args.service)
                    processes = {"agent": Process(pid, include_cgroup=args.pid is None)}
                    for service in args.observe_service:
                        processes[service] = Process(service_pid(service), include_cgroup=True)
                    digest = binary_sha(pid)
                    if first_binary is not None and first_binary != digest:
                        raise ValueError("agent executable changed between repetitions")
                    first_binary = digest
                    report["binary_sha256"] = digest
                    run["pid"] = pid
                    run["cgroups"] = {name: str(p.cgroup) if p.cgroup else None for name, p in processes.items()}
                    with (output / f"{scenario['name']}-{repeat}.jsonl").open("w") as raw:
                        def sample(phase):
                            value = snapshot(processes, phase)
                            raw.write(json.dumps(value, allow_nan=False) + "\n")
                            raw.flush()
                            return value
                        def wait_phase(seconds, phase):
                            deadline = time.monotonic() + seconds
                            values = [sample(phase)]
                            while time.monotonic() < deadline:
                                if stop.wait(min(profile["sample_seconds"], max(0, deadline-time.monotonic()))):
                                    raise ValueError("collection cancelled")
                                values.append(sample(phase))
                            return values
                        wait_phase(profile["settle_seconds"], "settle")
                        samples = [sample("measure")]
                        with ThreadPoolExecutor(max_workers=1) as pool:
                            future = pool.submit(workloads.run, scenario, log, stop)
                            try:
                                while not future.done():
                                    wait([future], timeout=profile["sample_seconds"])
                                    samples.append(sample("measure"))
                                workload_result = future.result()
                            finally:
                                if not future.done():
                                    stop.set()
                        # Include a final snapshot even for sub-interval workloads.
                        if len(samples) == 1:
                            samples.append(sample("measure"))
                        run["metrics"] = summarize(samples)
                        run["metrics"].update(workload_result)
                        operations = workload_result.get("workload.completed_operations")
                        if operations:
                            run["metrics"]["agent.process.cpu_seconds_per_operation"] = run["metrics"]["agent.process.cpu_seconds.total"] / operations
                        cooldown = wait_phase(profile["cooldown_seconds"], "cooldown")
                        if len(cooldown) > 1:
                            run["cooldown_metrics"] = summarize(cooldown)
                            # Totals used for storage comparisons include delayed writeback.
                            combined = summarize(samples + cooldown)
                            for key, value in combined.items():
                                if key.endswith(".total") and any(k in key for k in ("read_bytes", "write_bytes", ".io.", "host.disk.")):
                                    run["metrics"][key] = value
                        if operations:
                            for counter in ("read_bytes", "write_bytes"):
                                total = run["metrics"].get(f"agent.process.{counter}.total")
                                if total is not None:
                                    run["metrics"][f"agent.process.{counter}.per_operation"] = total / operations
                        if any(value > 0 for key, value in run["metrics"].items()
                               if key.endswith((".events.oom.total", ".events.oom_kill.total"))):
                            raise ValueError("OOM event occurred during measurement")
                        # Verify after cooldown so verification I/O is not charged
                        # to the workload's storage totals.
                        if scenario["kind"] != "command":
                            hook(scenario, "verify", log, stop)
                        run["status"] = "ok"
                        chart_name = f"{scenario['name']}-{repeat}.html"
                        charts(output / chart_name, [(run, samples)])
                        plotted.append(chart_name)
                except BaseException as error:
                    run.update(status="failed", error=str(error))
                    raise
                finally:
                    try:
                        hook(scenario, "cleanup", log, threading.Event())
                    finally:
                        write_json(output / "report.json", report)
        report["status"] = "ok"
    except BaseException as error:
        report.update(status="failed", error=str(error))
        raise
    finally:
        write_json(output / "report.json", report)
        links = "".join(f"<li><a href='{name}'>{name}</a></li>" for name in plotted)
        (output / "charts.html").write_text("<!doctype html><meta charset='utf-8'><title>Agent benchmark charts</title><h1>Agent benchmark charts</h1><ul>" + links + "</ul>")
        for sig, handler in old_handlers.items():
            signal.signal(sig, handler)
    print(output / "report.json")
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    run = commands.add_parser("run", help="collect a repeatable Linux benchmark")
    run.add_argument("--profile", type=Path, default=Path(__file__).with_name("profiles") / "standard.json")
    run.add_argument("--output", type=Path, required=True, help="new output directory, preferably on tmpfs")
    run.add_argument("--revision", required=True, help="commit/build identifier for the running agent")
    run.add_argument("--environment", required=True, help="stable device/storage/configuration identifier")
    run.add_argument("--metadata", type=Path, help="JSON describing build flags, storage, cloud state, power mode, etc.")
    target = run.add_mutually_exclusive_group()
    target.add_argument("--service", default="wendy-agent.service")
    target.add_argument("--pid", type=int, help="process-only mode; excludes cgroup accounting")
    run.add_argument("--observe-service", action="append", default=[])
    diff = commands.add_parser("compare", help="compare medians and emit a Markdown/JSON report")
    diff.add_argument("baseline", type=Path)
    diff.add_argument("candidate", type=Path)
    diff.add_argument("--policy", type=Path, help="JSON mapping metric names to absolute/relative thresholds")
    diff.add_argument("--output", type=Path, required=True, help="new comparison directory")
    diff.add_argument("--fail-on-regression", action="store_true")
    args = parser.parse_args()
    try:
        if args.action == "run":
            return collect(args)
        policy = load_policy(args.policy)
        result = compare(json.loads(args.baseline.read_text()), json.loads(args.candidate.read_text()), policy)
        args.output.mkdir(parents=True, exist_ok=False)
        print(write_comparison(args.output, result))
        return 1 if args.fail_on_regression and result["regressions"] else 0
    except INVALID_INPUT as error:
        print(f"benchmark: {error}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("benchmark interrupted; partial report retained", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
