"""Durable execution of a bounded job specification; one owned process group per step."""

import json
import os
from pathlib import Path
import signal
import subprocess
import time

root = Path(__file__).resolve().parent
spec = json.loads((root / "spec.json").read_text())
state = {
    "state": "running",
    "stage": "starting",
    "runner_pid": os.getpid(),
    "started_unix": time.time(),
    "stages": [],
}
progress_files = {step["name"]: step["name"] + "/" + step["metrics_file"] for step in spec["steps"] if step.get("metrics_file")}
if progress_files:
    state["progress_files"] = progress_files
started_monotonic = time.monotonic()
child = None
stopping = False


def write():
    state["updated_unix"] = time.time()
    state["execution_seconds"] = round(time.monotonic() - started_monotonic, 6)
    temporary = root / "status.worker.tmp"
    temporary.write_text(json.dumps(state, separators=(",", ":")))
    temporary.replace(root / "status.json")


def stop(_signal, _frame):
    global stopping
    stopping = True
    if child is not None and child.poll() is None:
        try:
            os.killpg(child.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass


signal.signal(signal.SIGTERM, stop)
signal.signal(signal.SIGINT, stop)
try:
    write()
    for step in spec["steps"]:
        if stopping:
            break
        name = step["name"]
        step_dir = root / name
        step_dir.mkdir()
        argv = [
            value.replace("{job_dir}", str(root)).replace("{step_dir}", str(step_dir))
            for value in step["argv"]
        ]
        state.update(stage=name, child_pid=None)
        write()
        environment = dict(
            os.environ, WENDY_JOB_DIR=str(root), WENDY_STEP_DIR=str(step_dir)
        )
        stage_started_unix = time.time()
        stage_started_monotonic = time.monotonic()
        with (root / (name + ".log")).open("xb") as output:
            child = subprocess.Popen(
                argv,
                cwd=spec["working_directory"],
                env=environment,
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.STDOUT,
                start_new_session=True,
            )
            state["child_pid"] = child.pid
            write()
            deadline = time.monotonic() + step["timeout_seconds"]
            timed_out = False
            while child.poll() is None:
                if stopping or time.monotonic() >= deadline:
                    timed_out = not stopping
                    try:
                        os.killpg(child.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                    try:
                        child.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        os.killpg(child.pid, signal.SIGKILL)
                        child.wait()
                    break
                time.sleep(0.05)
            code = child.wait()
            # A successful foreground exit must not leave an owned training
            # process running invisibly. Daemonizing workloads need a managed app.
            descendants_remaining = False
            try:
                os.killpg(child.pid, 0)
                descendants_remaining = True
                os.killpg(child.pid, signal.SIGTERM)
                time.sleep(0.1)
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            except ProcessLookupError:
                pass
        state["stages"].append(
            {
                "label": name,
                "returncode": code,
                "timed_out": timed_out,
                "started_unix": stage_started_unix,
                "completed_unix": time.time(),
                "duration_seconds": round(time.monotonic() - stage_started_monotonic, 6),
            }
        )
        state["child_pid"] = None
        write()
        if stopping:
            break
        if descendants_remaining:
            raise RuntimeError(
                name + " left background descendants; owned process group terminated"
            )
        if timed_out or code:
            raise RuntimeError(
                name
                + (
                    " exceeded its deadline"
                    if timed_out
                    else " exited with code " + str(code)
                )
            )
    state.update(
        state="stopped" if stopping else "completed",
        stage="stopped" if stopping else "completed",
        completed_unix=time.time(),
    )
    write()
except Exception as exc:
    state.update(
        state="failed",
        error=str(exc)[:1000],
        completed_unix=time.time(),
        child_pid=None,
    )
    write()
    raise
