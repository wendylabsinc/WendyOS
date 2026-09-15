#!/usr/bin/env python3
"""
ComposeEnv — client service

Waits for the server, fetches its identity/env response, then prints
what the server sees — specifically the values that docker-compose.yml
set via `environment:` and that Wendy now forwards (WDY-1268).
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

# On a Wendy device, the server is reachable on the device's mDNS hostname at
# port 8080.  On Docker Desktop, fall back to the compose service name.
_device = os.environ.get("WENDY_DEVICE_HOSTNAME")
SERVER = f"http://{_device}:8080" if _device else "http://server:8080"

print(f"[client] WENDY_HOSTNAME        = {os.environ.get('WENDY_HOSTNAME', '<not set>')}", flush=True)
print(f"[client] WENDY_DEVICE_HOSTNAME = {os.environ.get('WENDY_DEVICE_HOSTNAME', '<not set>')}", flush=True)
print(f"[client] waiting for server at {SERVER} ...", flush=True)

for attempt in range(30):
    try:
        with urllib.request.urlopen(SERVER, timeout=2) as resp:
            data = json.loads(resp.read())
        break
    except Exception:
        time.sleep(1)
else:
    print("[client] server never became ready", flush=True)
    sys.exit(1)

print("[client] server response:", flush=True)
print(json.dumps(data, indent=2), flush=True)

compose_env = data.get("from_compose_environment", {})
if os.environ.get("CHECK_CLI_OVERRIDES") == "1":
    expected = {"APP_MODE": "cli", "GREETING": "", "MAX_WORKERS": "7"}
    local = {key: os.environ.get(key) for key in expected}
    if local != expected or compose_env != expected:
        print(f"[client] CLI overrides mismatch: client={local}, server={compose_env}", flush=True)
        sys.exit(1)
    print("[client] CLI overrides reached both services (last key wins, empty preserved)", flush=True)
all_set = all(v != "<not set>" for v in compose_env.values())
if all_set:
    print("[client] ✓ all compose environment: vars reached the server container", flush=True)
else:
    missing = [k for k, v in compose_env.items() if v == "<not set>"]
    print(f"[client] ✗ missing vars: {missing}", flush=True)
    sys.exit(1)
