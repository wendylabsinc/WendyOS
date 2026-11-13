"""Fixed, read-only host-job probe. No user supplied code is evaluated."""

import hashlib, itertools, json, math, os, re, socket, stat, sys, time
from pathlib import Path

MAX_STATUS = 262144
MAX_TAIL = 65536


def read_regular(path, limit, tail=False, confined_root=None):
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    if confined_root is None:
        fd = os.open(str(path), flags)
    else:
        parts = path.relative_to(confined_root).parts
        if not parts or any(part in [".", ".."] for part in parts):
            raise ValueError("invalid job evidence path")
        parent = os.open(confined_root, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
        try:
            for part in parts[:-1]:
                following = os.open(part, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=parent)
                os.close(parent)
                parent = following
            fd = os.open(parts[-1], flags, dir_fd=parent)
        finally:
            os.close(parent)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode):
            raise ValueError("expected a regular file: " + str(path))
        if not tail and info.st_size > limit:
            raise ValueError("file exceeds read limit: " + str(path))
        start = max(0, info.st_size - limit) if tail else 0
        os.lseek(fd, start, os.SEEK_SET)
        data = os.read(fd, limit)
        if tail and start:
            data = data.split(b"\n", 1)[-1]
        return data, info.st_mtime
    finally:
        os.close(fd)


def process(root, pid, started):
    if not isinstance(pid, int) or isinstance(pid, bool) or pid <= 0:
        return {"status": "unknown", "reason": "runner_pid is unavailable"}
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return {"status": "missing", "pid": pid}
    except PermissionError:
        return {"status": "unknown", "pid": pid, "reason": "permission denied"}
    p = Path("/proc") / str(pid)
    if not p.exists():
        return {
            "status": "unknown",
            "pid": pid,
            "reason": "process identity is unavailable",
        }
    try:
        with (p / "cmdline").open("rb") as f:
            command = f.read(65537)
        if len(command) > 65536:
            return {
                "status": "unknown",
                "pid": pid,
                "reason": "process command exceeds identity limit",
            }
        command = command.split(b"\0")
        expected = str(root / "runner.py").encode()
        if expected not in command:
            return {"status": "identity_mismatch", "pid": pid}
        # /proc/PID/stat comm may contain spaces or parentheses.
        fields = (p / "stat").read_text().rsplit(")", 1)[1].split()
        state = fields[0]
        if (
            not isinstance(started, (int, float))
            or isinstance(started, bool)
            or not math.isfinite(started)
        ):
            return {
                "status": "unknown",
                "pid": pid,
                "reason": "recorded start time missing; current command matches",
            }
        boot = next(
            float(line.split()[1])
            for line in Path("/proc/stat").read_text().splitlines()
            if line.startswith("btime ")
        )
        actual_start = boot + int(fields[19]) / os.sysconf("SC_CLK_TCK")
        if abs(actual_start - started) > 5:
            return {
                "status": "identity_mismatch",
                "pid": pid,
                "reason": "process start time differs from recorded job",
            }
        return {
            "status": (
                "exited"
                if state == "Z"
                else "paused"
                if state in ["T", "t"]
                else "live"
            ),
            "pid": pid,
        }
    except (OSError, ValueError, IndexError, StopIteration):
        return {
            "status": "unknown",
            "pid": pid,
            "reason": "process identity changed during inspection",
        }


def snapshot(root, target, include_steps=False):
    raw, mtime = read_regular(root / "status.json", MAX_STATUS)
    data = json.loads(raw)
    if not isinstance(data, dict):
        raise ValueError("status.json must contain an object")
    warnings = []
    progress = {}
    progress_times = {}
    latest = mtime
    # Never recurse into datasets or checkpoints. Only bounded scalar progress.
    entries = list(itertools.islice(root.iterdir(), 129))
    directories = sorted(p for p in entries if not p.is_symlink() and p.is_dir())
    sources = {}
    declared = data.get("progress_files", {})
    if not isinstance(declared, dict):
        declared = {}
    if len(declared) > 32:
        warnings.append("declared progress scan limited to 32 files")
    if isinstance(declared, dict):
        for label, relative in itertools.islice(declared.items(), 32):
            if (not isinstance(label, str) or not re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", label)
                    or not isinstance(relative, str) or len(relative) > 1100
                    or "\x00" in relative or Path(relative).is_absolute() or ".." in Path(relative).parts
                    or not relative.startswith(label + "/")):
                warnings.append("invalid declared progress path; no file opened")
                continue
            sources[label] = root / relative
    for directory in directories[:32]:
        if directory.name not in sources and len(sources) < 32:
            sources[directory.name] = directory / "metrics.jsonl"
    started_labels = {data.get("stage")} if isinstance(data.get("stage"), str) else set()
    stages = data.get("stages", [])
    if isinstance(stages, list):
        started_labels.update(step.get("label") for step in stages if isinstance(step, dict) and isinstance(step.get("label"), str))
    for label, path in sources.items():
        try:
            tail, mt = read_regular(path, MAX_TAIL, True, root)
            latest = max(latest, mt)
            rows = tail.splitlines()
            value = None
            for row in reversed(rows[-16:]):
                try:
                    value = json.loads(row)
                    if not isinstance(value, dict):
                        value = None
                        continue
                    break
                except (ValueError, UnicodeError):
                    warnings.append(label + ": incomplete metrics record")
            if value is not None:
                progress_times[label] = mt
                progress[label] = {
                    k: v
                    for k, v in value.items()
                    if k
                    in [
                        "iteration",
                        "step",
                        "transitions",
                        "optimizer_steps_total",
                        "elapsed_s",
                        "loss",
                        "initial_chunk_kl",
                        "maximum_chunk_kl",
                    ]
                    and isinstance(v, (int, float, bool))
                }
        except FileNotFoundError:
            if label in declared and (not started_labels or label in started_labels):
                warnings.append(label + ": declared progress file unavailable")
        except (OSError, ValueError) as exc:
            warnings.append(label + ": " + str(exc)[:160])
    if len(directories) > 32 or len(entries) > 128:
        warnings.append("directory scan limited; detailed inventory required")
    observed = time.time()
    proc = process(root, data.get("runner_pid"), data.get("started_unix"))
    out = {
        "target": target,
        "hostname": socket.gethostname(),
        "reported_state": str(data.get("state", "unknown"))[:80],
        "stage": str(data.get("stage", "unknown"))[:120],
        "process": proc,
        "health": "not_running"
        if proc["status"] in ["missing", "exited", "paused", "identity_mismatch"]
        else "unknown",
        "progress": dict(
            sorted(
                progress.items(), key=lambda item: (-progress_times[item[0]], item[0])
            )[:8]
        ),
        "progress_omitted": max(0, len(progress) - 8),
        "report": {
            "path": str(root / "status.json"),
            "sha256": hashlib.sha256(raw).hexdigest(),
        },
    }
    duration = data.get("execution_seconds")
    if type(duration) in (int, float) and math.isfinite(duration) and duration >= 0:
        key = "execution_seconds" if data.get("state") in ["completed", "failed", "stopped"] else "recorded_execution_seconds"
        out[key] = duration
    for key in ["robot_commands_sent", "child_pid"]:
        if isinstance(data.get(key), int):
            out[key] = data[key]
    stages = data.get("stages")
    if include_steps:
        outcomes = []
        if isinstance(stages, list):
            for step in stages[-32:]:
                if not isinstance(step, dict):
                    continue
                code = step.get("returncode")
                outcomes.append({
                    "label": str(step.get("label", "unknown"))[:80],
                    "returncode": code if isinstance(code, int) and not isinstance(code, bool) else None,
                    "timed_out": step.get("timed_out") if isinstance(step.get("timed_out"), bool) else None,
                    "duration_seconds": step.get("duration_seconds") if type(step.get("duration_seconds")) in (int,float) and math.isfinite(step["duration_seconds"]) and step["duration_seconds"] >= 0 else None,
                })
        out["step_outcomes"] = outcomes
        out["step_outcomes_omitted"] = max(0, len(stages) - len(outcomes)) if isinstance(stages, list) else 0
        if out["reported_state"] == "failed":
            failed = next((step for step in reversed(outcomes)
                           if step["timed_out"] is True or (step["returncode"] is not None and step["returncode"] != 0)), None)
            if failed is not None:
                label = failed["label"]
                diagnostic = {"step": label, "tail_max_bytes": 2048}
                out["diagnostic"] = diagnostic
                if re.fullmatch(r"[a-z0-9][a-z0-9_-]{0,63}", label):
                    log = root / (label + ".log")
                    diagnostic["log_path"] = str(log)
                    try:
                        tail, _ = read_regular(log, 2048, True, root)
                        excerpt = tail.decode("utf-8", errors="replace").encode("utf-8")[:2048].decode("utf-8", errors="ignore")
                        diagnostic["excerpt"] = excerpt
                        diagnostic["excerpt_sha256"] = hashlib.sha256(excerpt.encode("utf-8")).hexdigest()
                    except (OSError, ValueError) as exc:
                        diagnostic["unavailable"] = str(exc)[:160]
                else:
                    diagnostic["unavailable"] = "invalid step label; no log opened"
    if isinstance(stages, list) and stages and isinstance(stages[-1], dict):
        out["last_stage"] = {
            k: (v[:160] if isinstance(v, str) else v)
            for k, v in stages[-1].items()
            if k in ["label", "returncode", "completed_unix"]
            and isinstance(v, (str, int, float))
        }
    if data.get("error"):
        out["error"] = str(data["error"])[:500]
    if warnings:
        out["warnings"] = warnings[:8]
    # Cursor deliberately excludes wall-clock age. A poll sees changed process,
    # metrics, state, error or report content even when the stage is unchanged.
    cursor = hashlib.sha256(
        json.dumps({key: value for key, value in out.items() if key not in ["step_outcomes", "step_outcomes_omitted", "diagnostic"]}, sort_keys=True, separators=(",", ":")).encode()
    ).hexdigest()[:24]
    out.update(
        cursor=cursor,
        changed=True,
        sample_age_seconds=round(max(0, observed - latest), 1),
    )
    return out


def main():
    request = json.loads(sys.argv[1])
    path = request.get("path", "")
    if (
        not isinstance(path, str)
        or not path.startswith("/")
        or len(path) > 4096
        or "\x00" in path
    ):
        raise ValueError("path must be an absolute job directory")
    root = Path(path).resolve(strict=True)
    if root == Path("/"):
        raise ValueError("root directory is not a job")
    since = request.get("since", "")
    wait = request.get("wait_seconds", 0)
    until_terminal = request.get("until_terminal", False)
    if not isinstance(until_terminal, bool):
        raise ValueError("until_terminal must be boolean")
    if not isinstance(since, str) or len(since) > 128:
        raise ValueError("invalid cursor")
    if isinstance(wait, bool) or not isinstance(wait, int) or not 0 <= wait <= 30:
        raise ValueError("wait_seconds must be an integer from 0 to 30")
    if wait and not since and not until_terminal:
        raise ValueError("wait_seconds requires since")
    deadline = time.monotonic() + wait
    while True:
        out = snapshot(root, str(request.get("target", ""))[:4096], until_terminal)
        if (
            (not until_terminal and out["cursor"] != since)
            or time.monotonic() >= deadline
            or out["reported_state"] in ["completed", "failed", "stopped", "cancelled"]
        ):
            break
        time.sleep(min(0.25, max(0, deadline - time.monotonic())))
    if out["cursor"] == since and not until_terminal:
        out = {
            k: out[k]
            for k in [
                "target",
                "cursor",
                "reported_state",
                "process",
                "health",
                "sample_age_seconds",
            ]
        }
        out["changed"] = False
    print(
        "WENDY_JOB " + json.dumps(out, separators=(",", ":"), allow_nan=False),
        flush=True,
    )


if __name__ == "__main__":
    try:
        main()
    except Exception as exc:
        print(
            "WENDY_JOB "
            + json.dumps(
                {"error": str(exc)[:800], "status": "unknown"}, separators=(",", ":")
            ),
            flush=True,
        )
        sys.exit(1)
