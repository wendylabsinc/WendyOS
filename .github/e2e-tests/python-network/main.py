#!/usr/bin/env python3
"""Network connectivity test for Wendy CI."""

import sys
import urllib.request

url = "http://captive.apple.com/hotspot-detect.html"

try:
    response = urllib.request.urlopen(url, timeout=10)
    if response.status == 200:
        print(f"PASS: Network connectivity verified (HTTP {response.status} from {url})")
    else:
        print(f"FAIL: Unexpected HTTP status {response.status}")
        sys.exit(1)
except Exception as e:
    print(f"FAIL: Network request failed: {e}")
    sys.exit(1)
