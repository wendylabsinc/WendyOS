"""Bounded benchmark workloads. Commands are argument arrays, never implicit shell."""

import http.client
import os
from pathlib import Path
import signal
import subprocess
import time
from urllib.parse import urlsplit

from metrics import percentile


def command(argv, timeout, log, stop, cwd=None):
    with Path(log).open("ab") as output:
        process = subprocess.Popen(argv, cwd=cwd, stdout=output, stderr=subprocess.STDOUT,
                                   start_new_session=True)
        deadline = time.monotonic() + timeout
        try:
            while process.poll() is None:
                if stop.wait(.05):
                    raise ValueError("workload cancelled")
                if time.monotonic() >= deadline:
                    raise ValueError(f"command timed out after {timeout}s; see {log}")
            if process.returncode:
                raise ValueError(f"command exited {process.returncode}; see {log}")
        finally:
            stop_group(process)


def stop_group(process, grace=2):
    """Terminate the command's process group, including descendants left behind
    by a wrapper that already exited. Every member gets the grace period after
    SIGTERM, not just the group leader."""
    try:
        os.killpg(process.pid, signal.SIGTERM)
    except ProcessLookupError:
        process.wait()
        return
    deadline = time.monotonic() + grace
    while time.monotonic() < deadline:
        process.poll()  # Reap the leader so it does not keep the group alive.
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            break
        time.sleep(.05)
    else:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
    process.wait()


def varint(value):
    encoded = bytearray()
    while value > 127:
        encoded.append((value & 127) | 128)
        value >>= 7
    encoded.append(value)
    return bytes(encoded)


def field(number, value):
    return varint(number * 8 + 2) + varint(len(value)) + value


def log_payload(batch, size):
    """Encode only the fixed OTLP fixture, using field IDs in Proto/opentelemetry.

    ExportLogsServiceRequest -> ResourceLogs -> ScopeLogs -> LogRecord.body.
    No SDK, subprocess or package installation runs in the measurement window.
    """
    attribute = field(1, b"service.name") + field(2, field(1, b"wendy-agent-benchmark"))
    resource = field(1, field(1, attribute))
    record = field(5, field(1, b"x" * size))
    scope = field(2, record) * batch
    return field(1, resource + field(2, scope))


def telemetry(scenario, stop):
    endpoint = urlsplit(scenario.get("endpoint", "http://127.0.0.1:4318/v1/logs"))
    if endpoint.scheme != "http" or endpoint.hostname not in ("127.0.0.1", "localhost", "::1"):
        raise ValueError("telemetry endpoint must be local HTTP; the agent receiver is loopback-only")
    payload = log_payload(scenario["batch"], scenario["payload_bytes"])
    if len(payload) > 1024 * 1024:
        raise ValueError("telemetry batch exceeds the agent's 1 MiB segment-frame limit")
    connection = http.client.HTTPConnection(endpoint.hostname, endpoint.port or 80, timeout=5)
    latencies = []
    lags = []
    start = time.monotonic()
    try:
        for i in range(scenario["requests"]):
            due = start + i / scenario["requests_per_second"]
            if stop.wait(max(0, due - time.monotonic())):
                raise ValueError("telemetry cancelled")
            before = time.monotonic()
            lags.append(max(0, before - due))
            connection.request("POST", endpoint.path, payload, {"Content-Type": "application/x-protobuf"})
            response = connection.getresponse()
            body = response.read(4096)
            if response.status != 200 or body:
                raise ValueError(f"OTLP request failed: HTTP {response.status}, body={body!r}")
            latencies.append(time.monotonic() - before)
    finally:
        connection.close()
    # Do not silently compare lower resource usage caused by an unmet offered load.
    if max(lags) > scenario.get("max_lag_seconds", 1):
        raise ValueError(f"telemetry could not sustain offered load; max schedule lag {max(lags):.3f}s")
    # N requests at R/s occupy N/R seconds, not (N-1)/R; hold the last interval
    # so operations per second matches the offered rate.
    if stop.wait(max(0, start + scenario["requests"] / scenario["requests_per_second"] - time.monotonic())):
        raise ValueError("telemetry cancelled")
    return {"workload.completed_operations": scenario["requests"],
            "workload.accepted_records": scenario["requests"] * scenario["batch"],
            "workload.payload_bytes": len(payload) * scenario["requests"],
            "workload.latency_p95_seconds": percentile(latencies, .95),
            "workload.schedule_lag_p95_seconds": percentile(lags, .95)}


def run(scenario, log, stop):
    start = time.monotonic()
    kind = scenario["kind"]
    if kind == "idle":
        if stop.wait(scenario["seconds"]):
            raise ValueError("idle workload cancelled")
        result = {}
    elif kind == "telemetry":
        result = telemetry(scenario, stop)
    else:
        command(scenario["command"], scenario["timeout_seconds"], log, stop, scenario.get("cwd"))
        # Deployment success alone does not establish app readiness. Require an
        # explicit check for all custom workloads, inside the measured window.
        command(scenario["verify"], scenario["timeout_seconds"], log, stop, scenario.get("cwd"))
        result = {"workload.completed_operations": scenario["operations"]}
    result["workload.seconds"] = time.monotonic() - start
    if "workload.completed_operations" in result:
        result["workload.operations_per_second"] = result["workload.completed_operations"] / result["workload.seconds"]
    return result
