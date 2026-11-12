#!/usr/bin/env python3
"""Compare two Linux agent binaries using alternating, fresh Docker fixtures.

Exercises unenrolled idle and local telemetry without containerd or cloud access.
Use benchmark.py with the real systemd service for full device measurements.
"""

import argparse
import copy
import json
from pathlib import Path
import subprocess
import sys
import uuid

from benchmark import validate_profile, write_json
from compare import compare, markdown, validate
from smoke import HERE, IMAGE, agent_fixture


def merge(reports, profile):
    merged = copy.deepcopy(reports[0])
    merged["runs"] = []
    for index, report in enumerate(reports, 1):
        validate(report)
        if report["compatibility"] != reports[0]["compatibility"] or report["binary_sha256"] != merged["binary_sha256"]:
            raise ValueError("environment or binary changed between repetitions")
        for run in copy.deepcopy(report["runs"]):
            run["repeat"] = index
            merged["runs"].append(run)
    merged["compatibility"]["profile"] = profile
    validate(merged)
    return merged


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path)
    parser.add_argument("--candidate", type=Path)
    parser.add_argument("--baseline-revision", default="baseline")
    parser.add_argument("--candidate-revision", default="candidate")
    parser.add_argument("--profile", type=Path, default=HERE / "profiles/ci.json")
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--fail-on-regression", action="store_true")
    parser.add_argument("--policy", type=Path, help="custom comparison thresholds")
    parser.add_argument("--inside", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.inside:
        with agent_fixture(args.output) as agent:
            subprocess.run([sys.executable, str(HERE / "benchmark.py"), "run", "--pid", str(agent.pid),
                            "--profile", str(args.profile), "--environment", "docker-unenrolled-no-containerd",
                            "--revision", args.candidate_revision, "--output", str(args.output / "run")], check=True)
        return 0
    if not args.baseline or not args.candidate or not args.baseline.is_file() or not args.candidate.is_file():
        parser.error("--baseline and --candidate must name Linux agent binaries matching Docker's architecture")
    profile = json.loads(args.profile.read_text())
    validate_profile(profile)
    if any(s["kind"] not in ("idle", "telemetry") for s in profile["scenarios"]):
        parser.error("Docker pairing supports idle and telemetry; use benchmark.py on a device for custom workloads")
    args.output.mkdir(parents=True, exist_ok=False)
    single = dict(profile, repetitions=1)
    write_json(args.output / "profile.json", single)
    reports = {"baseline": [], "candidate": []}
    workload_seconds = sum(s["seconds"] if s["kind"] == "idle" else s["requests"] / s["requests_per_second"] for s in single["scenarios"])
    timeout = 90 + workload_seconds + len(single["scenarios"]) * (single["settle_seconds"] + single["cooldown_seconds"])
    for repeat in range(1, profile["repetitions"] + 1):
        order = ("baseline", "candidate") if repeat % 2 else ("candidate", "baseline")
        for label in order:
            directory = args.output / label / str(repeat)
            directory.mkdir(parents=True)
            name = "wendy-agent-pair-" + uuid.uuid4().hex[:12]
            try:
                subprocess.run(["docker", "run", "--rm", "--name", name, "--network", "none",
                                "--mount", f"type=bind,src={getattr(args, label).resolve()},dst=/test-agent,readonly",
                                "--mount", f"type=bind,src={HERE},dst=/benchmark,readonly",
                                "--mount", f"type=bind,src={(args.output / 'profile.json').resolve()},dst=/profile.json,readonly",
                                "--mount", f"type=bind,src={directory.resolve()},dst=/results",
                                IMAGE, "python3", "/benchmark/pair.py", "--inside", "--profile", "/profile.json",
                                "--output", "/results", "--candidate-revision", getattr(args, label + "_revision")],
                               check=True, timeout=timeout)
            finally:
                subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)
            reports[label].append(json.loads((directory / "run/report.json").read_text()))
    merged = {label: merge(values, profile) for label, values in reports.items()}
    for label, value in merged.items():
        write_json(args.output / label / "report.json", value)
    result = compare(merged["baseline"], merged["candidate"],
                     json.loads(args.policy.read_text()) if args.policy else None)
    write_json(args.output / "comparison.json", result)
    (args.output / "comparison.md").write_text(markdown(result))
    print(markdown(result))
    return 1 if args.fail_on_regression and result["regressions"] else 0


if __name__ == "__main__":
    sys.exit(main())
