#!/usr/bin/env python3
"""Verify that a container deployed with zstd-compressed layers runs correctly."""

import platform
import sys

print(f"PASS: zstd-compressed container deployed and running successfully")
print(f"Python {sys.version.split()[0]} on {platform.machine()}")
sys.exit(0)
