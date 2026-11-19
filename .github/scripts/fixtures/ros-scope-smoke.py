#!/usr/bin/env python3
"""Unprivileged image check, separate from the hosted scope assertions."""
import json
import subprocess
import sys
import uuid

nonce = uuid.uuid4().hex
publisher = subprocess.Popen([sys.executable, '/scope.py', 'publish', '--role', 'device', '--nonce', nonce],
                             stdout=subprocess.DEVNULL)
try:
    result = subprocess.run([sys.executable, '/scope.py', 'observe', '--nonce', nonce, '--seconds', '4'],
                            text=True, capture_output=True, timeout=10, check=True)
    value = json.loads(result.stdout)
    assert len(value['device']) >= 3 and not value['peripheral'] and not value['uplink'], value
    assert publisher.poll() is None, 'publisher died during observation'
    print('PASS: separate ROS processes received fresh nonce-bound DDS payloads', flush=True)
finally:
    if publisher.poll() is None:
        publisher.terminate()
    try:
        publisher.wait(timeout=3)
    except subprocess.TimeoutExpired:
        publisher.kill()
        publisher.wait(timeout=3)
