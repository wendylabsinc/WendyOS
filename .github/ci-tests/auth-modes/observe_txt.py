"""Observe one explicitly selected DNS-SD instance; never browse the network."""

import argparse
import json
import re
import subprocess
import time


def observation(output, host, shape):
    """Both the resolved host and access TXT must belong to this answer."""
    resolved = re.search(
        r"can be reached at\s+([^\s:]+):([0-9]+)", output, re.IGNORECASE
    )
    if not resolved or resolved[1].rstrip(".").lower() != host.rstrip(".").lower():
        return None
    shapes = re.findall(r"\baccess=(open|closed|enrolled)\b", output)
    if not shapes or set(shapes) != {shape}:
        return None
    return {"resolvedHost": resolved[1].rstrip("."), "access": shapes[-1]}


def observe(instance, host, shape, timeout=20):
    # An exact instance resolution, not `dns-sd -B` or `wendy discover`.
    # The booking must identify this instance as the selected physical target.
    if not instance or any(c in instance for c in "\r\n\x00"):
        raise ValueError("an explicit DNS-SD instance is required")
    if not host.endswith(".local") or any(c in host for c in "\r\n\x00"):
        raise ValueError("a target .local hostname is required")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        child = subprocess.Popen(
            ["dns-sd", "-L", instance, "_wendyos._udp", "local."],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        try:
            try:
                output = child.communicate(timeout=min(3, max(0.1, deadline - time.monotonic())))[0]
            except subprocess.TimeoutExpired:
                child.terminate()
                output = child.communicate(timeout=2)[0]
        finally:
            if child.poll() is None:
                child.kill()
                child.communicate()
        result = observation(output, host, shape)
        if result:
            return {"instance": instance, **result}
        time.sleep(0.2)
    raise RuntimeError("target instance did not advertise the expected host and access shape")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--instance", required=True)
    parser.add_argument("--host", required=True)
    parser.add_argument("--shape", choices=("open", "closed", "enrolled"), required=True)
    args = parser.parse_args()
    print(json.dumps(observe(args.instance, args.host, args.shape)))
