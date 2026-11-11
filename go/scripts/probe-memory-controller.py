"""Read host memory-controller capabilities through authenticated shell.

Exit 0 plus the final WENDY_MEMORY_CONTROLLER record reports a known result;
any nonzero exit means the probe failed, including interpreter startup errors.
"""

import re
import subprocess
import sys


TIMEOUT_SECONDS = 30
PREFIX = "WENDY_CGROUP_CONTROLLERS="
V1_PREFIX = "WENDY_CGROUP_V1_MEMORY_LIMIT="
VERDICT = "PASS: host memory controller probe"
SCRIPT = """set -eu
cgroup_root=/sys/fs/cgroup
test -d "$cgroup_root"
test -r "$cgroup_root"
test -x "$cgroup_root"
memory_root="$cgroup_root/memory"
if test -e "$memory_root" || test -L "$memory_root"; then
    test -d "$memory_root"
    test -r "$memory_root"
    test -x "$memory_root"
    test -r "$memory_root/memory.limit_in_bytes"
    limit=$(cat "$memory_root/memory.limit_in_bytes")
    printf 'WENDY_CGROUP_V1_MEMORY_LIMIT=%s\\n' "$limit"
else
    test -r "$cgroup_root/cgroup.controllers"
    controllers=$(cat "$cgroup_root/cgroup.controllers")
    printf 'WENDY_CGROUP_CONTROLLERS=%s\\n' "$controllers"
fi
printf 'PASS: host memory controller probe\\n'
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
    records = [line for line in lines if line.startswith((PREFIX, V1_PREFIX))]
    if result.returncode != 0 or lines.count(VERDICT) != 1 or len(records) != 1:
        print("FAIL: host memory-controller probe did not return a unique successful verdict")
        return 1
    record_index = lines.index(records[0])
    if lines[record_index + 1:] != [VERDICT]:
        print("FAIL: unexpected lines in host cgroup controller record")
        return 1
    # A v1 hierarchy takes precedence on hybrid hosts where memory is bound
    # to v1 and therefore absent from the v2 controller list.
    if records[0].startswith(V1_PREFIX):
        if not re.fullmatch(r"[0-9]+", records[0][len(V1_PREFIX):]):
            print("FAIL: malformed host cgroup v1 memory limit")
            return 1
        print("Host memory controller: available (v1 hierarchy)")
        print("WENDY_MEMORY_CONTROLLER=available")
        return 0
    controllers_text = records[0][len(PREFIX):]
    # Missing/unreadable inventories and malformed records remain unknown.
    if not re.fullmatch(r"[a-z][a-z0-9_]*(?: [a-z][a-z0-9_]*)*", controllers_text):
        print("FAIL: malformed or empty host cgroup controller list")
        return 1
    controllers = controllers_text.split()
    if len(set(controllers)) != len(controllers):
        print("FAIL: duplicate host cgroup controllers")
        return 1
    if "memory" in controllers:
        print("Host memory controller: available")
        print("WENDY_MEMORY_CONTROLLER=available")
        return 0
    print("Host memory controller: absent (positive memory-limit specs unavailable)")
    print("WENDY_MEMORY_CONTROLLER=absent")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
