#!/usr/bin/env python3
"""Restart-policy probe for Wendy CI.

Prints a timestamped banner and exits 0 immediately.  The test harness in
test-ci.sh is responsible for:
  1. Deploying this image with --restart-unless-stopped.
  2. Waiting for the monitor to restart it (container should reappear running).
  3. Issuing `wendy container stop` and verifying the container is NOT restarted.
"""

import datetime
import sys

print(f"RESTART-PROBE start at {datetime.datetime.utcnow().isoformat()}Z")
print("PASS: restart probe exiting 0")
sys.exit(0)
