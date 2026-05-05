#!/usr/bin/env python3
"""Verify that Wendy runtime env vars are injected into containers (WDY-1126).

Checks:
  - WENDY_HOSTNAME is set and ends with .local
  - WENDY_DEVICE_HOSTNAME is set and ends with .local
"""

import os
import sys

errors = []

wendy_hostname = os.environ.get("WENDY_HOSTNAME", "")
if not wendy_hostname:
    errors.append("WENDY_HOSTNAME is not set")
elif not wendy_hostname.endswith(".local"):
    errors.append(f"WENDY_HOSTNAME={wendy_hostname!r} does not end with '.local'")
else:
    print(f"PASS: WENDY_HOSTNAME={wendy_hostname!r}")

device_hostname = os.environ.get("WENDY_DEVICE_HOSTNAME", "")
if not device_hostname:
    errors.append("WENDY_DEVICE_HOSTNAME is not set (expected to be injected by agent)")
elif not device_hostname.endswith(".local"):
    errors.append(f"WENDY_DEVICE_HOSTNAME={device_hostname!r} does not end with '.local'")
else:
    print(f"PASS: WENDY_DEVICE_HOSTNAME={device_hostname!r}")

if errors:
    for e in errors:
        print(f"FAIL: {e}")
    sys.exit(1)

print("PASS: all Wendy runtime env vars present and well-formed")
sys.exit(0)
