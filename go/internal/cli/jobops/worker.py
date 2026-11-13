"""Durable execution of a bounded job specification; one owned process group per step."""

import json
import os
from pathlib import Path
import select
import signal
import subprocess
import sys
import time


def group_has_workload(group):
    """Whether the workload's process group still has a live member."""
    proc = Path("/proc")
    if proc.is_dir():
        for entry in proc.iterdir():
            if not entry.name.isdigit():
                continue
            try:
                fields = (entry / "stat").read_text().rsplit(")", 1)[1].split()
                if int(fields[2]) == group and fields[0] != "Z":
                    return True
            except (FileNotFoundError, ProcessLookupError, PermissionError):
                # hidepid may conceal unrelated users; our unprivileged
                # workload shares the guard's uid and remains visible.
                pass
        return False
    # Local POSIX fixtures also run on macOS, whose process table is not /proc.
    result = subprocess.run(["ps", "-axo", "pid=,pgid=,stat="], capture_output=True,
                            text=True, check=True, timeout=2, start_new_session=True)
    for line in result.stdout.splitlines():
        _pid, pgid, state = line.split()
        if int(pgid) == group and not state.startswith("Z"):
            return True
    return False


def signal_owned_group(group, signum):
    try:
        os.killpg(group, signum)
    except ProcessLookupError:
        pass
    except PermissionError:
        # macOS answers EPERM when only the exited, unreaped leader is left.
        if group_has_workload(group):
            raise


def terminate_owned_group(group):
    """One SIGTERM, a bounded grace period, then SIGKILL for whatever is left."""
    signal_owned_group(group, signal.SIGTERM)
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        try:
            if not group_has_workload(group):
                break
        except (OSError, ValueError, subprocess.SubprocessError):
            # Uncertain membership is not permission to leave owned work alive.
            pass
        time.sleep(0.05)
    signal_owned_group(group, signal.SIGKILL)


def workload_exited(workload):
    """Observe the exit without reaping: the unreaped leader keeps its PID,
    and so the group ID the guard signals, from being reused."""
    if hasattr(os, "waitid"):
        return os.waitid(os.P_PID, workload.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT) is not None
    # ponytail: macOS before Python 3.13 has no waitid; local fixtures there
    # reap early and accept the reuse window. Linux hosts always have it.
    return workload.poll() is not None


def run_step_guard(liveness_fd, report_fd, argv):
    """Keep the owned group bounded even if the detached runner is SIGKILLed.

    Only the runner holds the pipe's write end; transport processes and the
    workload never inherit it. A lost observation therefore leaves work alive,
    while runner death produces EOF independently of its PID or any status file.

    The workload leads its own session, as it did before the guard existed:
    its setpgid(0, 0) stays a no-op and its group signals never reach the guard.
    """
    stopping = False

    def stop_guard(_signal, _frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGTERM, stop_guard)
    signal.signal(signal.SIGINT, stop_guard)
    workload = None
    with os.fdopen(report_fd, "w", buffering=1) as report:
        try:
            workload = subprocess.Popen(argv, stdin=subprocess.DEVNULL, start_new_session=True)
            report.write(json.dumps({"child_pid": workload.pid}) + "\n")
            terminated = False
            while not terminated and not workload_exited(workload):
                readable, _, _ = select.select([liveness_fd], [], [], 0.05)
                if stopping or (readable and not os.read(liveness_fd, 1)):
                    terminate_owned_group(workload.pid)
                    terminated = True
            descendants = not terminated and group_has_workload(workload.pid)
            if descendants:
                terminate_owned_group(workload.pid)
            # Reaped only after the last group signal; see workload_exited.
            code = workload.wait()
            report.write(json.dumps({"returncode": code, "descendants_remaining": descendants}) + "\n")
        except BaseException as exc:
            # Includes a closed report pipe when the runner died during launch.
            print("step guard failed: " + str(exc)[:800], file=sys.stderr, flush=True)
            try:
                report.write(json.dumps({"error": str(exc)[:800]}) + "\n")
            except OSError:
                pass
            if workload is not None:
                terminate_owned_group(workload.pid)


if len(sys.argv) > 1 and sys.argv[1] == "--step-guard":
    run_step_guard(int(sys.argv[2]), int(sys.argv[3]), json.loads(sys.argv[4]))
    sys.exit(0)

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
    # Only the supervision loop may reap/signal the guard. Keeping its PID
    # unreaped until the last group signal prevents process-group ID reuse.


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
            liveness_read, liveness_write = os.pipe()
            report_read, report_write = os.pipe()
            try:
                child = subprocess.Popen(
                    [sys.executable, str(root / "runner.py"), "--step-guard",
                     str(liveness_read), str(report_write), json.dumps(argv)],
                    cwd=spec["working_directory"],
                    env=environment,
                    stdin=subprocess.DEVNULL,
                    stdout=output,
                    stderr=subprocess.STDOUT,
                    pass_fds=(liveness_read, report_write),
                    start_new_session=True,
                )
            finally:
                os.close(liveness_read)
                os.close(report_write)
            try:
                os.set_blocking(report_read, False)
                report_bytes = bytearray()
                identity_received = False
                deadline = stage_started_monotonic + step["timeout_seconds"]
                timed_out = False

                def receive_report():
                    global identity_received
                    try:
                        chunk = os.read(report_read, 4097 - len(report_bytes))
                    except BlockingIOError:
                        return
                    report_bytes.extend(chunk)
                    if len(report_bytes) > 4096:
                        raise RuntimeError(name + " guard report exceeded its bound")
                    if not identity_received and b"\n" in report_bytes:
                        identity = json.loads(report_bytes.split(b"\n", 1)[0])
                        if "error" in identity:
                            raise RuntimeError(identity["error"])
                        # Preserve the public argv PID, not the guard's PID.
                        state["child_pid"] = identity["child_pid"]
                        identity_received = True
                        write()

                while child.poll() is None:
                    if stopping or time.monotonic() >= deadline:
                        timed_out = not stopping
                        try:
                            os.killpg(child.pid, signal.SIGTERM)
                        except ProcessLookupError:
                            pass
                        try:
                            child.wait(timeout=6)
                        except subprocess.TimeoutExpired:
                            os.killpg(child.pid, signal.SIGKILL)
                            child.wait()
                        break
                    readable, _, _ = select.select([report_read], [], [], 0.05)
                    if readable:
                        receive_report()
                code = child.wait()
                # All writers belong to the now-reaped guard. Workloads never
                # inherit this FD; a nonblocking drain also bounds bad reports.
                receive_report()
                lines = report_bytes.splitlines()
                descendants_remaining = False
                if len(lines) > 1:
                    result = json.loads(lines[1])
                    if "error" in result:
                        raise RuntimeError(result["error"])
                    code = result["returncode"]
                    descendants_remaining = result["descendants_remaining"]
                elif code == 0:
                    raise RuntimeError(name + " guard exited without a step result")
            finally:
                os.close(report_read)
                os.close(liveness_write)
                # Exceptions during identity/result handling still release work.
                try:
                    child.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    os.killpg(child.pid, signal.SIGKILL)
                    child.wait()
                except ChildProcessError:
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
