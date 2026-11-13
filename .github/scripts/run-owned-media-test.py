#!/usr/bin/env python3
"""Run one fixture command and drain its owned process group within a deadline."""

import argparse
import os
import select
import signal
import subprocess
import sys
import time

# The guardian stays alive after the test exits and pins the process-group ID.
# Even an unexpectedly dead guardian remains unreaped until all group signals
# finish. Never poll/wait it earlier: a reaped PID/PGID can be reused.
GUARDIAN = r'''
import os
import signal
import subprocess
import sys

# Stay a live group leader throughout graceful child shutdown.
signal.signal(signal.SIGTERM, lambda signum, frame: None)
report = int(sys.argv[1])
try:
    child = subprocess.Popen(sys.argv[2:], close_fds=True)
    result = child.wait()
except BaseException:
    result = 125
try:
    os.write(report, (str(result) + "\n").encode())
except BrokenPipeError:
    # Cancellation closes the supervisor reader before graceful child exit.
    # Keep pinning the group until the owner sends its final group signal.
    pass
finally:
    os.close(report)
while True:
    signal.pause()
'''


def run(command, timeout, *, cwd=None, env=None, stdout=None):
    stopped = 0

    def stop(signum, frame):
        nonlocal stopped
        stopped = 128 + signum

    previous = {sig: signal.signal(sig, stop) for sig in (signal.SIGINT, signal.SIGTERM)}
    read_fd, write_fd = os.pipe()
    guardian = None
    result = 125
    try:
        deadline = time.monotonic() + timeout
        if stopped:
            return stopped
        guardian = subprocess.Popen([sys.executable, "-c", GUARDIAN, str(write_fd), *command],
                                    start_new_session=True, pass_fds=(write_fd,),
                                    cwd=cwd, env=env, stdout=stdout,
                                    stderr=subprocess.STDOUT if stdout is not None else None)
        os.close(write_fd)
        write_fd = None
        os.set_blocking(read_fd, False)
        report = bytearray()
        while not stopped:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                print(f"Media fixture exceeded its {timeout:g}-second wall deadline", file=sys.stderr)
                result = 124
                break
            readable, _, _ = select.select([read_fd], [], [], min(remaining, 0.1))
            if not readable:
                continue
            chunk = os.read(read_fd, 64)
            if not chunk:
                print("Media fixture guardian closed without an exit report", file=sys.stderr)
                break
            report.extend(chunk)
            if len(report) > 64:
                break
            if b"\n" in report:
                try:
                    code = int(report)
                    result = code if code >= 0 else 128 - code
                except ValueError:
                    pass
                break
        if stopped:
            result = stopped
    finally:
        os.close(read_fd)
        if write_fd is not None:
            os.close(write_fd)
        if guardian is not None:
            try:
                os.killpg(guardian.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            except OSError as error:
                print(f"Could not terminate owned media group: {error}", file=sys.stderr)
                result = 125
            # The leader is deliberately not reaped, even if TERM killed it.
            # Give children a bounded grace period before the final group signal.
            deadline = time.monotonic() + 3
            while time.monotonic() < deadline:
                time.sleep(min(0.05, max(0, deadline - time.monotonic())))
            try:
                os.killpg(guardian.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            except OSError as error:
                print(f"Could not kill owned media group: {error}", file=sys.stderr)
                result = 125
            # Only now can the PID become reusable; there are no later signals.
            try:
                guardian.wait(timeout=3)
            except subprocess.TimeoutExpired:
                print("Owned media guardian did not exit after SIGKILL", file=sys.stderr)
                result = 125
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    # Cancellation can arrive after the direct child reports success, while
    # its owned descendants are still draining. Never turn that into a pass.
    return stopped or result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--timeout", type=float, required=True)
    parser.add_argument("command", nargs=argparse.REMAINDER)
    args = parser.parse_args()
    command = args.command[1:] if args.command[:1] == ["--"] else args.command
    if not command or not 0 < args.timeout <= 600:
        parser.error("supply a command and a timeout in (0, 600] seconds")
    return run(command, args.timeout)


if __name__ == "__main__":
    sys.exit(main())
