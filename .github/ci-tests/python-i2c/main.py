#!/usr/bin/env python3
"""I2C entitlement test for Wendy CI.

Verifies that the declared i2c-1 device node is present inside the container.
Actual I2C communication is not attempted — the test only confirms the device
node was mounted, proving that the entitlement pipeline reached the container.
"""

import os
import sys

DEVICE = "/dev/i2c-1"

if not os.path.exists(DEVICE):
    print(f"FAIL: {DEVICE} not found — i2c entitlement did not mount the device node")
    sys.exit(1)

print(f"PASS: I2C entitlement verified — {DEVICE} is present in the container")
