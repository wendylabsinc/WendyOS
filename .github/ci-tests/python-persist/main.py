#!/usr/bin/env python3
"""Persist entitlement test for Wendy CI.

Verifies that the declared /data volume is mounted and writable inside the
container.  A file written on one run should survive; on a fresh first run the
directory must at least exist and accept writes.
"""

import os
import sys

MOUNT = "/data"
PROBE = os.path.join(MOUNT, "wendy-ci-persist-probe.txt")

# 1. Mount point must exist.
if not os.path.isdir(MOUNT):
    print(f"FAIL: {MOUNT} directory does not exist — persist volume not mounted")
    sys.exit(1)

# 2. Must be writable.
try:
    with open(PROBE, "w") as f:
        f.write("wendy-ci-persist\n")
except OSError as e:
    print(f"FAIL: Cannot write to {PROBE}: {e}")
    sys.exit(1)

# 3. Must be readable back.
try:
    content = open(PROBE).read().strip()
except OSError as e:
    print(f"FAIL: Cannot read back {PROBE}: {e}")
    sys.exit(1)

if content != "wendy-ci-persist":
    print(f"FAIL: Unexpected content in probe file: {content!r}")
    sys.exit(1)

print(f"PASS: Persist entitlement verified — {MOUNT} is mounted and writable")
