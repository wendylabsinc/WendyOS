#!/usr/bin/env python3
"""Print usable physical ticks; refuse mock, stale, failed or faulted data."""

import json
import sys
import urllib.request

endpoint = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8876"
with urllib.request.urlopen(endpoint.rstrip("/") + "/v1/observations", timeout=3) as reply:
    observation = json.load(reply)
if observation["schema_version"] != 1:
    sys.exit("Unsupported reader schema")
if not observation["hardware_ready"] or observation["synthetic"]:
    sys.exit("Hardware observation unavailable: " + observation["connection"]["state"])
for joint in observation["joints"]:
    if joint["fresh"] and not joint["errors"]:
        print(joint["name"], joint["id"], joint["position"]["ticks"], joint["position"]["unit"])
