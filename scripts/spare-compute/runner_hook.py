#!/usr/bin/env python3
"""Host-side GitHub runner hooks. Share one control directory across all runners."""
import argparse
import fcntl
import hashlib
import os
from pathlib import Path
import subprocess
import time


def transition(action, runner, control, compose):
    control.mkdir(parents=True, exist_ok=True)
    with (control / "lock").open("w") as lock:
        deadline = time.monotonic() + 90
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() > deadline:
                    raise TimeoutError("another runner hook still holds the lock")
                time.sleep(0.1)
        token = control / (hashlib.sha256(runner.encode()).hexdigest() + ".busy")
        if action == "started":
            # Persist before stop; failed hooks and crashed runners fail closed.
            token.write_text(runner + "\n")
            subprocess.run(compose + ["stop", "--timeout", "10"], check=True, timeout=60)
        elif action == "completed":
            if not token.exists():
                raise RuntimeError("no matching started marker; refusing to start background work")
            token.unlink()
        if action != "started" and not list(control.glob("*.busy")):
            # Containers must have been created beforehand. No builds, downloads,
            # or health waits on the runner's synchronous job hook path.
            subprocess.run(compose + ["start"], check=True, timeout=60)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["started", "completed", "resume"])
    parser.add_argument("--directory", type=Path, required=True, help="installed Compose directory")
    parser.add_argument("--control", type=Path, required=True, help="shared host control directory")
    parser.add_argument("--runner", default=os.environ.get("RUNNER_NAME"))
    parser.add_argument("--nvidia", action="store_true")
    args = parser.parse_args()
    if args.action != "resume" and not args.runner:
        parser.error("RUNNER_NAME or --runner is required")
    directory = args.directory.resolve()
    compose = ["docker", "compose", "--project-directory", str(directory),
               "--env-file", str(directory / ".env"), "-f", str(directory / "compose.yaml")]
    if args.nvidia:
        compose += ["-f", str(directory / "compose.nvidia.yaml")]
    transition(args.action, args.runner or "manual", args.control.resolve(), compose)


if __name__ == "__main__":
    main()
