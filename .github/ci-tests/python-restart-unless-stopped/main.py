#!/usr/bin/env python3
"""Restart-unless-stopped policy test for Wendy CI.

On the first invocation the app exits with a non-zero status, simulating a
crash.  The UNLESS_STOPPED restart policy (sent by `wendy device apps start`
since PR #657) must cause the agent to restart the container.  On the second
invocation the app detects it has been restarted, prints a success message,
and exits 0 — which terminates the streaming session and lets the test pass.

A small sentinel file written to /tmp is used to distinguish first vs.
subsequent runs.  /tmp is ephemeral within the container lifetime but
persists across task restarts (the container is restarted, not recreated).
"""

import os
import sys

SENTINEL = "/tmp/.wendy_restart_sentinel"

if os.path.exists(SENTINEL):
    # Second (or later) run — the restart policy worked.
    print("PASS: container restarted by UNLESS_STOPPED policy")
    sys.exit(0)
else:
    # First run — create the sentinel and exit non-zero to trigger restart.
    with open(SENTINEL, "w") as f:
        f.write("1")
    print("First run: exiting with non-zero status to trigger restart...")
    sys.exit(1)
