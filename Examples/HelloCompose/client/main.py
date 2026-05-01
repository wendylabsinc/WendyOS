#!/usr/bin/env python3
"""Client service for HelloCompose example — polls the api and exits."""

import json
import sys
import time
import urllib.error
import urllib.request

API = "http://localhost:8080"

print("[client] Waiting for api to be ready...", flush=True)
for attempt in range(30):
    try:
        with urllib.request.urlopen(API, timeout=2) as resp:
            data = json.loads(resp.read())
        print(f"[client] Response: {data['message']}", flush=True)
        print(f"[client] API is running Python {data['python'].split()[0]} on {data['machine']}", flush=True)
        print("[client] Done.", flush=True)
        sys.exit(0)
    except (urllib.error.URLError, OSError):
        time.sleep(1)

print("[client] Timed out waiting for api", flush=True)
sys.exit(1)
