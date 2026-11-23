#!/usr/bin/env python3
"""Start a Go2-owned room on Linux without opening an Apple client."""
import argparse
import time
from ros_contributor import request

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('serial', help='A Go2 already paired and connected on the twin gateway')
parser.add_argument('--endpoint', default='http://127.0.0.1:8791')
args = parser.parse_args()
handle = request(args.endpoint, '/v1/robot', {'Operation': 'open', 'Serial': args.serial}, 'POST')['Handle']
try:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        state = request(args.endpoint, '/v1/robot', {'Operation': 'poll', 'Handle': handle}, 'POST')['Snapshot']
        if state['Pose']:
            result = request(args.endpoint, '/v1/robot', {'Operation': 'start-room', 'Handle': handle, 'Sequence': 1}, 'POST')
            print('Created room', result['RoomID'], flush=True)
            break
        time.sleep(.2)
    else:
        raise RuntimeError('No fresh robot pose; no room was created')
finally:
    # Release control. The Linux service keeps the observation subscription.
    request(args.endpoint, '/v1/robot', {'Operation': 'close', 'Handle': handle}, 'POST')
