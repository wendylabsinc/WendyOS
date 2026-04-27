#!/usr/bin/env python3
"""Negative test: verify bluetooth is blocked without the bluetooth entitlement."""

import glob
import os
import socket
import sys

# Check sysfs — should NOT find controllers without the entitlement
bt_controllers = glob.glob("/sys/class/bluetooth/hci*")
if bt_controllers:
    names = [os.path.basename(p) for p in bt_controllers]
    print(f"FAIL: Bluetooth controllers visible without entitlement: {', '.join(names)}")
    sys.exit(1)

# Try raw HCI socket — should fail without the entitlement
AF_BLUETOOTH = 31
BTPROTO_HCI = 1
try:
    s = socket.socket(AF_BLUETOOTH, socket.SOCK_RAW, BTPROTO_HCI)
    s.close()
    print("FAIL: Bluetooth HCI socket opened without entitlement — expected denial")
    sys.exit(1)
except OSError as e:
    print(f"PASS: Bluetooth correctly blocked without entitlement ({e})")
