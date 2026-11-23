#!/usr/bin/env python3
"""Select the newest public runtime supporting the CI suite's compact iPhone."""

import json
import sys

DEVICE_TYPE = 'com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro'


def select(runtimes):
    available = [runtime for runtime in runtimes
                 if runtime['isAvailable'] and not runtime.get('isInternal', False)
                 and '.iOS-' in runtime['identifier']
                 and int(runtime['version'].split('.')[0]) >= 26
                 and any(device['identifier'] == DEVICE_TYPE
                         for device in runtime['supportedDeviceTypes'])]
    if not available:
        raise ValueError('No public iOS >=26 simulator runtime supports iPhone 17 Pro')
    runtime = max(available, key=lambda item: tuple(map(int, item['version'].split('.'))))
    return runtime['identifier'], DEVICE_TYPE


if __name__ == '__main__':
    print(*select(json.load(sys.stdin)['runtimes']))
