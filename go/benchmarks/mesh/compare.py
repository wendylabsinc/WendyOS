#!/usr/bin/env python3
"""Compare the real mesh protocol at two revisions in isolated worktrees."""

import argparse
import csv
from contextlib import ExitStack
import hashlib
import json
import os
from pathlib import Path
import platform
import random
import re
import statistics
import subprocess
import tempfile


HERE = Path(__file__).resolve().parent
PACKAGE = "go/internal/agent/localmesh"


def run(command, cwd, **kwargs):
    return subprocess.run(command, cwd=cwd, check=True, text=True, **kwargs)


def select_adapter(sync, batch):
    if "func (s *Synchronizer) seedAnnouncements(" in sync:
        if "func (s *Synchronizer) receiveHaveBatch(" in batch:
            return "batching"
        return "head"
    if "func (s *Synchronizer) ReconcileBounded(" in sync:
        return "base"
    raise ValueError("unsupported synchronizer")


def parse(text):
    metrics = {}
    case = ""
    for line in text.splitlines():
        if line.startswith("=== RUN"):
            name = line.split("=== RUN", 1)[1].strip()
            case = name.replace("TestVirtualMeshRegression/", "").replace(
                "TestVirtualMeshImmediateWarmProbe/", "immediate-warm-probe/")
        if "MESH_RESULT " in line:
            result = json.loads(line.split("MESH_RESULT ", 1)[1])
            prefix = f"simulation/{case}/{result.pop('scenario')}"
            kinds = result.pop("frames_by_kind")
            for name, value in result.items():
                metrics[f"{prefix}/{name}"] = value
            # A missing kind means zero, reconciled across samples below.
            for name, value in kinds.items():
                metrics[f"{prefix}/kind/{name}"] = value
        if line.startswith("BenchmarkVirtual"):
            parts = line.split()
            if len(parts) < 4 or not parts[1].isdigit():
                continue
            name = re.sub(r"-\d+$", "", parts[0])
            for index in range(2, len(parts) - 1, 2):
                metrics[f"benchmark/{name}/{parts[index+1]}"] = float(parts[index])
    if not any(key.startswith("simulation/") for key in metrics):
        raise ValueError("simulation results missing")
    if not any(key.startswith("benchmark/") for key in metrics):
        raise ValueError("benchmark results missing")
    return metrics


def compare(samples, threshold):
    keys = set().union(*(sample.keys() for runs in samples.values() for sample in runs))
    rng = random.Random(3115)
    rows = []
    for key in sorted(keys):
        # Missing frame kinds are zero; missing benchmark/scenario results are
        # incomplete measurements, never silently interpreted as improvements.
        values = {}
        for label, runs in samples.items():
            if "/kind/" not in key and any(key not in sample for sample in runs):
                raise ValueError(f"missing metric: {label} {key}")
            values[label] = [sample.get(key, 0) for sample in runs]
        base, head = values["base"], values["head"]
        b, h = statistics.mean(base), statistics.mean(head)
        delta = 100 * (h / b - 1) if b else (0 if not h else None)
        # Paired bootstrap by round. Alternate run order to reduce drift bias.
        draws = []
        for _ in range(4000):
            indexes = rng.choices(range(len(base)), k=len(base))
            denominator = statistics.mean(base[i] for i in indexes)
            if denominator:
                draws.append(100 * (statistics.mean(head[i] for i in indexes) / denominator - 1))
        draws.sort()
        low = draws[int(len(draws) * .025)] if draws else None
        high = draws[min(len(draws)-1, int(len(draws) * .975))] if draws else None
        # Throughput is the only metric where an increase is beneficial.
        worse = (high is not None and high < -threshold) if key.endswith("/MB/s") else (
            (low is not None and low > threshold) or (b == 0 and h > 0))
        rows.append(dict(metric=key, base_mean=b, head_mean=h, change_pct=delta,
                         ci95_low_pct=low, ci95_high_pct=high, regression=bool(worse)))
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True)
    parser.add_argument("--head", required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--repeat", type=int, default=7)
    parser.add_argument("--benchtime", default="500ms")
    parser.add_argument("--threshold", type=float, default=10)
    parser.add_argument("--fail-on-regression", action="store_true")
    args = parser.parse_args()
    if args.repeat < 3:
        parser.error("use at least three paired rounds")
    root = Path(run(["git", "rev-parse", "--show-toplevel"], HERE, capture_output=True).stdout.strip())
    revisions = {label: run(["git", "rev-parse", f"{ref}^{{commit}}"], root, capture_output=True).stdout.strip()
                 for label, ref in (("base", args.base), ("head", args.head))}
    args.out = args.out.resolve()
    args.out.mkdir(parents=True, exist_ok=True)
    # Refuse to overwrite a prior measurement set.
    metadata = args.out / "metadata.json"
    with metadata.open("x") as stream:
        json.dump(dict(revisions=revisions, platform=platform.platform(),
                       go=run(["go", "version"], root, capture_output=True).stdout.strip(),
                       repeat=args.repeat, benchtime=args.benchtime, threshold_pct=args.threshold,
                       gomaxprocs=1,
                       have_batches=os.environ.get("WENDY_MESH_HAVE_BATCHES", "1"),
                       fixtures={p.name: hashlib.sha256(p.read_bytes()).hexdigest()
                                 for p in sorted((HERE / "testdata").glob("*.go"))}), stream, indent=2)
    samples = {"base": [], "head": []}
    env = dict(os.environ, GOMAXPROCS="1")
    with tempfile.TemporaryDirectory(prefix="wendy-mesh-compare-") as temporary, ExitStack() as cleanup:
        temp = Path(temporary).resolve()
        binaries = {}
        for label, sha in revisions.items():
            checkout = temp / label
            run(["git", "worktree", "add", "--detach", str(checkout), sha], root, stdout=subprocess.DEVNULL)
            cleanup.callback(run, ["git", "worktree", "remove", "--force", str(checkout)], root)
            destination = checkout / PACKAGE
            # Fail closed if a future implementation no longer matches
            # either adapter; don't guess the announcement schedule.
            sync = (destination / "sync.go").read_text()
            batch = destination / "have_batch.go"
            adapter = select_adapter(sync, batch.read_text() if batch.exists() else "")
            for name, source in (("virtual_mesh_test.go", "virtual_mesh_test.go"),
                                 ("virtual_mesh_adapter_test.go", f"{adapter}_adapter.go")):
                with (destination / name).open("x") as stream:
                    stream.write((HERE / "testdata" / source).read_text())
            binaries[label] = temp / f"{label}.test"
            print(f"Building {label} {sha[:12]} with {adapter} adapter", flush=True)
            run(["go", "test", "-c", "-o", str(binaries[label]), f"./{PACKAGE}"], checkout, env=env)
        for round_number in range(args.repeat):
            order = ("base", "head") if round_number % 2 == 0 else ("head", "base")
            for label in order:
                print(f"Round {round_number + 1}/{args.repeat}: {label}", flush=True)
                log = args.out / f"{label}-{round_number + 1}.txt"
                with log.open("x") as stream:
                    run([str(binaries[label]), "-test.run=^TestVirtualMesh(Regression|ImmediateWarmProbe)$", "-test.v",
                         "-test.bench=^BenchmarkVirtual", f"-test.benchtime={args.benchtime}",
                         "-test.cpu=1", "-test.timeout=5m"], root, env=env,
                        stdout=stream, stderr=subprocess.STDOUT)
                samples[label].append(parse(log.read_text()))
    (args.out / "samples.json").write_text(json.dumps(samples, indent=2) + "\n")
    rows = compare(samples, args.threshold)
    (args.out / "comparison.json").write_text(json.dumps(rows, indent=2) + "\n")
    with (args.out / "comparison.csv").open("w", newline="") as stream:
        writer = csv.DictWriter(stream, fieldnames=list(rows[0]))
        writer.writeheader()
        writer.writerows(rows)
    flags = [row for row in rows if row["regression"]]
    print(f"Compared {len(rows)} metrics; {len(flags)} exceed the {args.threshold:g}% regression screen.")
    for row in flags:
        print(f"  {row['metric']}: {row['change_pct']}%")
    print(f"Raw measurements and comparison: {args.out}")
    return 1 if flags and args.fail_on_regression else 0


if __name__ == "__main__":
    raise SystemExit(main())
