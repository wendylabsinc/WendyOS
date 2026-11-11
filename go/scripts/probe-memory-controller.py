"""Read host cgroup v2 capabilities through the existing authenticated shell.

Exit 0 means memory is available, 2 means affirmatively absent, and 1 means
the probe failed. Only 2 permits skipping positive memory-limit fixtures.
"""

import re
import subprocess
import sys


TIMEOUT_SECONDS = 30
PREFIX = "WENDY_CGROUP_CONTROLLERS="
VERDICT = "PASS: host cgroup v2 controllers"
SCRIPT = """set -eu
test -r /sys/fs/cgroup/cgroup.controllers
controllers=$(cat /sys/fs/cgroup/cgroup.controllers)
printf 'WENDY_CGROUP_CONTROLLERS=%s\\n' "$controllers"
printf 'PASS: host cgroup v2 controllers\\n'
"""


def main(argv):
    if len(argv) != 2:
        print("FAIL: expected Wendy binary and device selector")
        return 1
    wendy, device = argv
    try:
        result = subprocess.run(
            [wendy, "device", "shell", "--device", device, "--", "sh", "-c", SCRIPT],
            stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, timeout=TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired as error:
        if error.output:
            print(error.output.decode(errors="replace") if isinstance(error.output, bytes) else error.output)
        print(f"FAIL: memory-controller probe exceeded {TIMEOUT_SECONDS}s")
        return 1
    except OSError as error:
        print(f"FAIL: cannot start authenticated device shell: {error}")
        return 1
    print(result.stdout, end="")
    lines = result.stdout.splitlines()
    records = [line[len(PREFIX):] for line in lines if line.startswith(PREFIX)]
    if result.returncode != 0 or lines.count(VERDICT) != 1 or len(records) != 1:
        print("FAIL: host memory-controller probe did not return a unique successful verdict")
        return 1
    # A missing/unreadable/non-v2 file must never look like an absent controller.
    # Reject empty or malformed records instead of treating them as capability data.
    if not re.fullmatch(r"[a-z][a-z0-9_]*(?: [a-z][a-z0-9_]*)*", records[0]):
        print("FAIL: malformed or empty host cgroup controller list")
        return 1
    controllers = records[0].split()
    if len(set(controllers)) != len(controllers):
        print("FAIL: duplicate host cgroup controllers")
        return 1
    if "memory" in controllers:
        print("Host memory controller: available")
        return 0
    print("Host memory controller: absent (positive memory-limit specs unavailable)")
    return 2


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
