"""Run bounded children with automatic admission or explicit always-on mode."""
import json
import os
from pathlib import Path
import signal
import subprocess
import time

STOP = False
UID = 10001


def admission_mode():
    mode = os.environ.get("ADMISSION_MODE", "auto")
    if mode not in ("auto", "always"):
        raise ValueError("ADMISSION_MODE must be auto or always")
    return mode


def permitted(path, lane, mode):
    if mode == "always":
        return True
    if mode != "auto":
        raise ValueError("unknown admission mode")
    return lease_valid(path, lane)


def lease_valid(path, lane, now=None, boot_id=None):
    try:
        lease = json.loads(Path(path).read_text())
        now = time.monotonic() if now is None else now
        boot_id = Path("/proc/sys/kernel/random/boot_id").read_text().strip() if boot_id is None else boot_id
        return (lease.get(lane) is True and lease.get("boot_id") == boot_id
                and 0 <= now - lease["issued"] < 8)
    except (OSError, ValueError, KeyError, TypeError):
        return False


def child_pids():
    # Wendy gives each service a private PID namespace. All workload processes
    # use this UID; the root monitor is excluded, even if a child daemonizes.
    for path in Path("/proc").iterdir():
        if path.name.isdigit():
            try:
                if path.stat().st_uid == UID:
                    yield int(path.name)
            except FileNotFoundError:
                pass


def stop_children(process):
    for pid in child_pids():
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass
    deadline = time.monotonic() + 4
    while time.monotonic() < deadline and any(child_pids()):
        process.poll()
        time.sleep(0.1)
    for pid in child_pids():
        try:
            os.kill(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait(timeout=2)


def main(command, lane, state="/state"):
    if os.getuid() != 0:
        raise RuntimeError("lease monitor must start as root to isolate and reap workload UID")
    mode = admission_mode()
    print(json.dumps({"event": "admission_mode", "mode": mode, "lane": lane}), flush=True)
    def stop(_signum, _frame):
        global STOP
        STOP = True
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    for name in (state, "/work"):
        path = Path(name)
        path.mkdir(parents=True, exist_ok=True)
        os.chown(path, UID, UID)
    os.nice(19)
    env = {k: v for k, v in os.environ.items() if k != "WENDY_AGENT_SOCKET"}
    deadline = 0
    process = None
    started = 0
    session_seconds = float(os.environ.get("SESSION_SECONDS", 21600))
    try:
        while not STOP:
            granted = permitted("/control/lease.json", lane, mode)
            if process is not None and (not granted or process.poll() is not None or time.monotonic() - started > session_seconds):
                completed = process.poll() == 0
                stop_children(process)
                process = None
                deadline = time.monotonic() + (3600 if completed else 60)
                print(json.dumps({"event": "yielded", "lane": lane}), flush=True)
            if granted and process is None and time.monotonic() >= deadline:
                started = time.monotonic()
                process = subprocess.Popen(command, env=env, user=UID, group=UID,
                                           extra_groups=[], start_new_session=True)
                print(json.dumps({"event": "resumed", "lane": lane}), flush=True)
            time.sleep(0.25)
    finally:
        if process is not None:
            stop_children(process)


if __name__ == "__main__":
    main(json.loads(os.environ["WORKLOAD_COMMAND"]), os.environ.get("LANE", "gpu"),
         os.environ.get("WORKLOAD_STATE", "/cache"))
