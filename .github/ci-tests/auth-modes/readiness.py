"""Read-only app readiness after an acknowledged access-setting restart."""

import argparse
import json
import os
import subprocess
import time


def transient_transport(error):
    message = error.get("message", "").lower()
    if any(word in message for word in ("permission", "unauth", "does not trust", "certificate", "identity", "pin mismatch")):
        return False
    if error.get("code") == "device_unreachable" and error.get("retryable") is True:
        return True
    return error.get("code") in ("grpc_unavailable", "connection_closed", "unexpected_eof") and any(
        text in message for text in ("eof", "connection reset", "connection refused", "connection closed", "transport is closing")
    )


def expected_key_refusal(error):
    return error.get("code") == "device_auth_required" and "does not trust this computer's key" in error.get("message", "")


def error_body(stderr):
    for line in reversed(stderr.splitlines()):
        try:
            value = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(value, dict) and isinstance(value.get("error"), dict):
            return value["error"]
    raise RuntimeError("app readiness failed without a structured CLI diagnostic")


def wait_for_apps(device, config, allowed, timeout=40):
    # AgentRestart's default AckDelay + bounded Stop is 0.5 + 5 seconds.
    # Two read observations after six seconds cannot both be supplied by its
    # outgoing listener. This is a readiness barrier, not proof of process ID.
    start = time.monotonic()
    consecutive = 0
    attempts = 0
    while time.monotonic() - start < timeout:
        attempts += 1
        result = subprocess.run(
            ["wendy", "--json", "--device", device, "device", "apps", "list"],
            env=dict(os.environ, WENDY_CONFIG_DIR=config, WENDY_RUN_NO_CLOUD_FALLBACK="1"),
            text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10,
        )
        if result.returncode == 0:
            inventory = json.loads(result.stdout)
            if not isinstance(inventory, list):
                raise RuntimeError("app readiness received an invalid inventory")
            decision = "allowed"
            expected = allowed
        else:
            error = error_body(result.stderr)
            if not allowed and expected_key_refusal(error):
                decision, expected = "denied", True
            elif transient_transport(error):
                decision, expected = "transport_retry", False
            else:
                raise RuntimeError("app readiness refused a non-transport failure: " + error.get("code", "unknown"))
        elapsed = time.monotonic() - start
        print(json.dumps({"appRead": decision, "attempt": attempts, "elapsedSeconds": round(elapsed, 2)}), flush=True)
        if expected and elapsed >= 6:
            consecutive += 1
            if consecutive == 2:
                return
        else:
            consecutive = 0
        time.sleep(0.5)
    raise RuntimeError("read-only app readiness did not settle across the scheduled restart")


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--device", required=True)
    parser.add_argument("--config", required=True)
    parser.add_argument("--expect", choices=("allowed", "denied"), required=True)
    args = parser.parse_args()
    wait_for_apps(args.device, args.config, args.expect == "allowed")
