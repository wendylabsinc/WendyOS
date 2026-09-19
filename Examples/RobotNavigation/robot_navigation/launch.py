"""Supervise separate processes so perception/MCP stalls cannot bypass velocity expiry."""
import os
import signal
import subprocess
import sys
import time
from .settings import load_settings


def main():
    settings = load_settings()
    children = []
    stopping = False
    def stop(*_):
        nonlocal stopping
        stopping = True
    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    env = os.environ.copy()
    env["RGB_TOPIC"] = settings["topics"]["rgb"]
    env["DETECTIONS_TOPIC"] = settings["topics"]["detections"]
    commands = [[sys.executable, "-m", "robot_navigation.guard_node"],
                [sys.executable, "-m", "robot_navigation.motor"]]
    if settings["start_nav2"]:
        commands.append(["ros2", "launch", "/app/nav2.launch.py"])
    if settings["start_detector"]:
        commands.append([sys.executable, "-m", "robot_navigation.detector"])
    commands.append([sys.executable, "-m", "robot_navigation.server"])
    code = 0
    try:
        for command in commands:
            children.append(subprocess.Popen(command, env=env, start_new_session=True))
        while not stopping:
            failed = next((p for p in children if p.poll() is not None), None)
            if failed:
                print(f"navigation child exited ({failed.args}): {failed.returncode}", file=sys.stderr)
                code = 1
                break
            time.sleep(.1)
    finally:
        for child in reversed(children):
            try:
                os.killpg(child.pid, signal.SIGTERM)
            except ProcessLookupError:
                # Process group already gone during shutdown race.
                pass
        deadline = time.monotonic() + 3
        for child in children:
            try:
                child.wait(timeout=max(.01, deadline-time.monotonic()))
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(child.pid, signal.SIGKILL)
                except ProcessLookupError:
                    # Process group already gone during shutdown race.
                    pass
        for child in children:
            child.wait()
    return code


if __name__ == "__main__":
    raise SystemExit(main())
