#!/usr/bin/env python3
"""Build the integration matrix from devices accessible to the runner."""

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import re
import subprocess
import sys


REQUIRED = ("raspberry-pi", "jetson", "qualcomm")
MAX_DEVICES = 20
HOSTNAME = re.compile(
    r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?"
    r"(?:\.[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*\.?"
)


def log(message):
    print(message, file=sys.stderr, flush=True)


def platform(device_type):
    for prefix, family in (("raspberry-pi", "raspberry-pi"),
                           ("jetson", "jetson"),
                           ("dragonwing", "qualcomm"),
                           ("qualcomm", "qualcomm")):
        if device_type == prefix or device_type.startswith(prefix + "-"):
            return family
    return None


def cli_json(wendy, *args):
    result = subprocess.run(
        [wendy, *args, "--json"], stdin=subprocess.DEVNULL,
        capture_output=True, text=True, timeout=45, check=True,
    )
    return json.loads(result.stdout)


def candidates(discovery):
    hosts = set()
    for device in discovery.get("lanDevices") or []:
        if not device.get("isWendyDevice"):
            continue
        host = device.get("hostname", "")
        if not isinstance(host, str) or len(host) > 253 or not HOSTNAME.fullmatch(host):
            log(f"SKIPPED invalid hostname: {host!r}")
            continue
        hosts.add(host.lower())
    return sorted(hosts)


def probe(wendy, host):
    try:
        info = cli_json(wendy, "device", "info", "--device", host)
        # Check an authenticated app RPC as well as hardware info. Discovery
        # advertisements and public version information do not prove access.
        cli_json(wendy, "device", "apps", "list", "--device", host)
        family = platform(info.get("deviceType", ""))
        log(f"ACCESSIBLE: {host} ({info.get('deviceType') or 'unknown platform'})")
        return host, family
    except (subprocess.SubprocessError, ValueError, AttributeError) as error:
        # Do not echo CLI stderr, which can contain credential diagnostics.
        log(f"SKIPPED inaccessible device: {host} ({type(error).__name__})")
        return None


def select(accessible, require_platforms):
    # Reserve a slot for each required family before filling the bounded matrix.
    selected = []
    missing = []
    for family in REQUIRED:
        host = next((host for host, kind in accessible if kind == family), None)
        if host is None:
            missing.append(family)
        else:
            selected.append(host)
    if require_platforms and missing:
        raise ValueError("no accessible device for required platform(s): " + ", ".join(missing))
    for host, _ in accessible:
        if host not in selected and len(selected) < MAX_DEVICES:
            selected.append(host)
    log(f"Selected {len(selected)} of {len(accessible)} accessible devices (limit {MAX_DEVICES}).")
    return selected


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--wendy", required=True)
    parser.add_argument("--require-platforms", action="store_true")
    args = parser.parse_args()
    try:
        hosts = candidates(cli_json(args.wendy, "discover", "--timeout", "10s"))
        with ThreadPoolExecutor(max_workers=4) as pool:
            accessible = list(filter(None, pool.map(lambda host: probe(args.wendy, host), hosts)))
        print(json.dumps(select(accessible, args.require_platforms)))
    except (subprocess.SubprocessError, ValueError, AttributeError, OSError) as error:
        log(f"ERROR: integration discovery failed: {error}")
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
