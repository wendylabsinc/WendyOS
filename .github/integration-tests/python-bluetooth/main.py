#!/usr/bin/env python3
"""Bluetooth hardware access test for Wendy CI."""

import glob
import os
import socket
import sys

# Check sysfs for bluetooth controllers
bt_controllers = glob.glob("/sys/class/bluetooth/hci*")
if bt_controllers:
    names = [os.path.basename(p) for p in bt_controllers]
    print(f"Bluetooth controllers: {', '.join(names)}")
    print("PASS: Bluetooth entitlement verified")
    sys.exit(0)

# Fallback: try to open a raw Bluetooth HCI socket
AF_BLUETOOTH = 31
BTPROTO_HCI = 1
try:
    s = socket.socket(AF_BLUETOOTH, socket.SOCK_RAW, BTPROTO_HCI)
    s.close()
    print("PASS: Bluetooth HCI socket opened successfully")
except OSError as e:
    print(f"FAIL: Bluetooth not accessible")
    print(f"  /sys/class/bluetooth/hci*: not found")
    print(f"  HCI socket: {e}")
    sys.exit(1)
