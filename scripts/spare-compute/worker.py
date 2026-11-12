#!/usr/bin/env python3
"""Bounded background jobs against an immutable git archive. No CI credentials."""
import argparse
import collections
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import time

STOP = False


def stop(_signum, _frame):
    global STOP
    STOP = True


def atomic_json(path, value):
    tmp = path.with_suffix(".tmp")
    tmp.write_text(json.dumps(value, indent=2) + "\n")
    tmp.replace(path)


def contained(root, relative):
    path = (root / relative).resolve()
    if not path.is_relative_to(root.resolve()):
        raise ValueError(f"path outside source: {relative}")
    return path


def shard(key, index, count):
    return int(hashlib.sha256(key.encode()).hexdigest(), 16) % count == index


def terminate(proc):
    # Kill the whole session, including test subprocesses and compiler workers.
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    deadline = time.monotonic() + 3
    while proc.poll() is None and time.monotonic() < deadline:
        time.sleep(0.05)
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    proc.wait()


def run_process(argv, cwd, env, report, seconds, min_free):
    started = time.monotonic()
    log = report / "output.log"
    status = "completed"
    with log.open("wb") as output:
        proc = subprocess.Popen(argv, cwd=cwd, env=env, stdout=output,
                                stderr=subprocess.STDOUT, start_new_session=True)
        try:
            while proc.poll() is None:
                if STOP:
                    status = "interrupted"
                elif time.monotonic() - started >= seconds:
                    status = "timeout"
                elif log.stat().st_size > 8 * 1024**2:
                    status = "output_limit"
                elif shutil.disk_usage(report).free < min_free:
                    status = "disk_pressure"
                if status != "completed":
                    break
                time.sleep(0.2)
        finally:
            terminate(proc)  # Also remove descendants left behind by an exited parent.
    if status == "completed" and proc.returncode:
        status = "failed"
    return {"status": status, "exit_code": proc.returncode,
            "seconds": round(time.monotonic() - started, 2), "command": argv}


def jobs_for(root, config, modes, index, count):
    jobs = collections.defaultdict(list)
    for mode in ("fuzz", "mutation", "race"):
        for item in config.get(mode, []):
            package = item["package"] if mode == "fuzz" else item
            if not contained(root, package).is_dir():
                raise ValueError(f"missing package: {package}")
            if mode == "fuzz" and not re.fullmatch(r"Fuzz[A-Za-z0-9_]+", item["target"]):
                raise ValueError("invalid fuzz target")
            job = {"mode": mode, "package": package}
            if mode == "fuzz":
                job["target"] = item["target"]
            jobs[mode].append(job)
    if "review" in modes:
        files = set()
        for name in config.get("review_roots", []):
            for path in contained(root, name).rglob("*.go"):
                if path.is_symlink() or path.name.endswith(("_test.go", ".pb.go")):
                    continue
                if "Code generated" in path.read_text(errors="replace")[:1024]:
                    continue
                files.add(path.relative_to(root).as_posix())
        for name in sorted(files):
            lines = contained(root, name).read_text(errors="replace").splitlines()
            for start in range(0, len(lines), 200):
                jobs["review"].append({"mode": "review", "file": name,
                                       "start": start + 1, "end": min(start + 240, len(lines))})
    for mode in list(jobs):
        jobs[mode] = [j for j in jobs[mode] if shard(json.dumps(j, sort_keys=True), index, count)]
    return jobs


def command_for(job, cpus, config, report):
    package = "./" + job.get("package", "")
    test = ["go", "test", "-p", str(cpus), "-count=1", "-timeout=5m"]
    if job["mode"] == "fuzz":
        return test + [package, "-run=^$", f"-fuzz=^{job['target']}$",
                       f"-fuzztime={config['fuzz_seconds']}s", f"-parallel={cpus}"]
    if job["mode"] == "mutation":
        # v0.6.0 passes its --test-cpu value to Go as one malformed argument.
        # Bound CPU through GOMAXPROCS/GOFLAGS and the container instead.
        return ["gremlins", "unleash", "--workers", "1",
                "--output", str(report / "mutations.json"), package]
    if job["mode"] == "race":
        return test + [package, "-race", "-shuffle=on"]
    return [sys.executable, str(Path(__file__).with_name("review.py")),
            "--file", job["file"], "--start", str(job["start"]), "--end", str(job["end"]),
            "--output", str(report / "review.json")]


def extract(archive, root):
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True)
    with tarfile.open(archive) as source:
        # Reject links entirely: jobs and model input must remain in this snapshot.
        for member in source:
            if not (member.isfile() or member.isdir()):
                continue
            destination = contained(root, member.name)
            if member.isdir():
                destination.mkdir(parents=True, exist_ok=True)
            else:
                destination.parent.mkdir(parents=True, exist_ok=True)
                with source.extractfile(member) as src, destination.open("wb") as dst:
                    shutil.copyfileobj(src, dst)
                destination.chmod(0o755 if member.mode & 0o111 else 0o644)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path, default=Path("/input/source.tar"))
    parser.add_argument("--state", type=Path, default=Path("/state"))
    parser.add_argument("--config", type=Path, default=Path(__file__).with_name("config.json"))
    parser.add_argument("--once", action="store_true", help="run one scheduled job")
    parser.add_argument("--list", action="store_true", help="print this shard's jobs and exit")
    args = parser.parse_args()
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    os.nice(19)
    state = args.state.resolve()
    state.mkdir(parents=True, exist_ok=True)
    # Prevent two workers corrupting one persistent volume.
    import fcntl
    lock = (state / "worker.lock").open("w")
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    config = json.loads(args.config.read_text())
    modes = os.environ.get("MODES", "fuzz,mutation,race").split(",")
    if not modes or set(modes) - {"fuzz", "mutation", "review", "race"}:
        raise ValueError("MODES must contain fuzz, mutation, review or race")
    index, count = int(os.environ.get("WORKER_INDEX", 0)), int(os.environ.get("WORKER_COUNT", 1))
    if count < 1 or not 0 <= index < count:
        raise ValueError("require 0 <= WORKER_INDEX < WORKER_COUNT")
    cpus = max(1, math.floor(float(os.environ.get("WORKER_CPUS", 2))))
    min_free = int(float(os.environ.get("MIN_FREE_GIB", 10)) * 1024**3)
    keep = int(os.environ.get("MAX_REPORTS", 200))
    if keep < 1 or min_free < 0 or not 0 < config["fuzz_seconds"] < config["job_seconds"]:
        raise ValueError("invalid retention, disk reserve or job duration")
    with tarfile.open(args.archive) as source:
        revision = source.pax_headers.get("comment", "")
    if not re.fullmatch(r"[a-f0-9]{40,64}", revision):
        raise ValueError("source must be a git archive of a commit, with its revision header")
    root = state / "work"
    results = state / "reports"
    for path in (results, state / "home", state / "cache", state / "corpus"):
        path.mkdir(exist_ok=True)
    extract(args.archive, root)
    jobs = jobs_for(root, config, modes, index, count)
    if args.list:
        print(json.dumps({m: jobs[m] for m in modes}, indent=2))
        return
    key = hashlib.sha256(json.dumps([revision, config, modes, index, count,
                                   os.environ.get("MODEL_NAME"),
                                   os.environ.get("MODEL_BASE_URL")], sort_keys=True).encode()).hexdigest()
    checkpoint = state / "checkpoint.json"
    progress = json.loads(checkpoint.read_text()) if checkpoint.exists() else {}
    if progress.get("key") != key:
        progress = {"key": key, "turn": 0, "cursors": {}}
    env = {**os.environ, "GOMAXPROCS": str(cpus), "GOFLAGS": f"-p={cpus}",
           "GOCACHE": str(state / "cache/build"), "GOMODCACHE": str(state / "cache/mod"),
           "GOTMPDIR": str(state / "tmp"), "TMPDIR": str(state / "tmp"),
           "HOME": str(state / "home"), "SOURCE_REVISION": revision}
    print(json.dumps({"event": "started", "revision": revision, "shard": [index, count]}), flush=True)
    while not STOP:
        # A full disk pauses work without deleting findings or fuzz corpora.
        if shutil.disk_usage(state).free < min_free:
            print('{"event":"waiting_for_disk"}', flush=True)
            if args.once:
                return
            time.sleep(5)
            continue
        available = [m for m in modes if jobs[m] and
                     (m in ("fuzz", "race") or progress["cursors"].get(m, 0) < len(jobs[m]))]
        if not available:
            print('{"event":"all_finite_jobs_completed"}', flush=True)
            return
        mode = available[progress["turn"] % len(available)]
        cursor = progress["cursors"].get(mode, 0)
        job = jobs[mode][cursor % len(jobs[mode])]
        # Every attempt gets pristine source, including after a killed mutant.
        extract(args.archive, root)
        shutil.rmtree(state / "tmp", ignore_errors=True)
        (state / "tmp").mkdir()
        for old in sorted(results.iterdir())[:max(0, len(list(results.iterdir())) - keep + 1)]:
            shutil.rmtree(old)
        report = results / f"{time.time_ns()}-{mode}"
        report.mkdir()
        corpus = None
        if mode == "fuzz":
            corpus = state / "corpus" / hashlib.sha256(json.dumps(job, sort_keys=True).encode()).hexdigest()[:16]
            destination = contained(root, job["package"]) / "testdata/fuzz" / job["target"]
            if corpus.exists():
                shutil.copytree(corpus, destination, dirs_exist_ok=True)
        argv = command_for(job, cpus, config, report)
        metadata = {"revision": revision, "job": job, "shard": [index, count], "status": "running"}
        atomic_json(report / "result.json", metadata)
        print(json.dumps({"event": "job_started", **metadata}), flush=True)
        outcome = run_process(argv, root, env, report, config["job_seconds"], min_free)
        if corpus is not None and destination.exists():
            shutil.copytree(destination, corpus, dirs_exist_ok=True)
            shutil.copytree(destination, report / "corpus")
        atomic_json(report / "result.json", {**metadata, **outcome})
        print(json.dumps({"event": "job_finished", **metadata, **outcome}), flush=True)
        # Failed jobs are visible and do not stop other workloads. Finite failed jobs
        # are retried on a fresh sweep only after the operator removes checkpoint.json.
        retry_model = mode == "review" and outcome["exit_code"] == 75
        if outcome["status"] not in ("interrupted", "disk_pressure"):
            if not retry_model:
                progress["cursors"][mode] = cursor + 1
            progress["turn"] += 1
            atomic_json(checkpoint, progress)
        if args.once or STOP:
            break
        # Interruptible delay avoids hammering a model that is still loading.
        for _ in range(30 if retry_model else 2):
            if STOP:
                break
            time.sleep(1)


if __name__ == "__main__":
    main()
