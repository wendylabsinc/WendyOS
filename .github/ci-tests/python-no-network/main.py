#!/usr/bin/env python3
"""Negative test: verify network is blocked without the network entitlement."""

import sys
import urllib.request

url = "http://captive.apple.com/hotspot-detect.html"

try:
    response = urllib.request.urlopen(url, timeout=10)
    print(f"FAIL: Network request succeeded (HTTP {response.status}) — expected denial without entitlement")
    sys.exit(1)
except Exception as e:
    print(f"PASS: Network correctly blocked without entitlement ({type(e).__name__})")
