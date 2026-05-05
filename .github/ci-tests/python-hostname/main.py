#!/usr/bin/env python3
"""Verify WENDY_HOSTNAME is the device mDNS hostname, not the app name."""

import os
import sys

hostname = os.environ.get("WENDY_HOSTNAME", "")

if not hostname:
    print("FAIL: WENDY_HOSTNAME is not set or empty")
    sys.exit(1)

if not hostname.endswith(".local"):
    print(f"FAIL: WENDY_HOSTNAME={hostname!r} does not end with '.local'")
    sys.exit(1)

# The device hostname should look like wendyos-<adjective>-<noun>.local
# It must NOT look like an app name (e.g. sh-wendy-ci-python-hostname.local
# or python-hostname.local).  We verify it does not contain the app's own
# bundle ID fragment as a simple sanity check.
if "python-hostname" in hostname:
    print(
        f"FAIL: WENDY_HOSTNAME={hostname!r} looks like the app name, "
        "not the device hostname"
    )
    sys.exit(1)

print(f"PASS: WENDY_HOSTNAME={hostname!r} is a valid device mDNS hostname")
