#!/usr/bin/env python3
"""Real OTLP fault fixtures. The verifier collects a new workload independently."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

import fixtures as f

HERE = Path(__file__).resolve().parent


def completed_tools(events):
    """Read returned tool evidence only, excluding model prose and tool inputs."""
    pending = {}
    for event in events:
        kind = event.get("type")
        item = event.get("item", {})
        if kind == "item.completed":
            if item.get("type") == "command_execution":
                yield item.get("command", ""), item.get("aggregated_output", "")
            elif item.get("type") == "mcp_tool_call":
                yield item.get("tool", ""), json.dumps(item.get("result", {}))
        if kind == "assistant":
            for block in event.get("message", {}).get("content", []):
                if block.get("type") == "tool_use":
                    pending[block["id"]] = block.get("name", "") + " " + json.dumps(block.get("input", {}))
        if kind == "user":
            for block in event.get("message", {}).get("content", []):
                if block.get("type") == "tool_result" and not block.get("is_error"):
                    yield pending.get(block.get("tool_use_id"), ""), json.dumps(block.get("content", ""))
        if kind == "tool_start":
            pending[event["id"]] = event.get("tool", "") + " " + json.dumps(event.get("arguments", {}))
        if kind == "tool_result":
            yield pending.get(event.get("id"), event.get("tool", "")), event.get("text", "")


def require_telemetry_query(path, signal, run_id):
    events = [json.loads(line) for line in Path(path).read_text().splitlines() if line.strip()]
    marker = {"logs": "division by zero", "metrics": "eval.processing_ms", "traces": "eval.worker"}[signal]
    for command, output in completed_tools(events):
        query = "telemetry_" + signal in command or "telemetry-stream" in command
        query |= signal == "logs" and ("device logs" in command or "container_logs" in command)
        if query and marker in output and run_id in output:
            return
    raise RuntimeError(f"no returned {signal} query contains this trial's diagnostic evidence")


def exercise(args, challenge):
    probe = '''import json,urllib.request,urllib.error
results=[]
for value in [7,-13,23]:
    url=BASE + "/compute?value="+str(value)+"&challenge="+CHALLENGE
    try:
        r=urllib.request.urlopen(url,timeout=10)
        results.append([r.status,json.load(r)])
    except urllib.error.HTTPError as e:
        results.append([e.code,json.load(e)])
print(json.dumps(results))
'''
    code = f"BASE='http://127.0.0.1:{args.port}'\nCHALLENGE={challenge!r}\n" + probe
    return json.loads(f.cli(args, "--device", args.device, "device", "attach", args.app_id,
                            "--", "python3", "-c", code, timeout=60))


def collect(args, challenge):
    with tempfile.TemporaryFile(mode="w+") as output, tempfile.TemporaryFile(mode="w+") as errors:
        process = subprocess.Popen([args.wendy, "--device", args.device, "device", "telemetry-stream",
                                    "--app", args.app_id], stdout=output, stderr=errors, stdin=subprocess.DEVNULL)
        try:
            time.sleep(2)
            responses = exercise(args, challenge)
            time.sleep(2)
        finally:
            process.terminate()
            try:
                process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        output.seek(0)
        records = [json.loads(line) for line in output if line.strip()]
        records = [r for r in records if r.get("attributes", {}).get("eval.challenge") == challenge
                   and r.get("resource", {}).get("eval.run_id") == args.run_id]
        if not records:
            errors.seek(0)
            raise RuntimeError("no independent OTEL records for the fresh challenge: " + errors.read())
        return responses, records


def validate(responses, records, args, healthy):
    for value, (status, body) in zip((7, -13, 23), responses):
        if healthy and (status != 200 or body.get("result") != value * value or body.get("nonce") != args.run_id):
            raise RuntimeError(f"workload failed: {status}, {body}")
        if not healthy and args.signal == "logs" and status != 500:
            raise RuntimeError("logging fault was not reproduced")
    if len(responses) != 3:
        raise RuntimeError("incomplete workload")
    logs = [r for r in records if r.get("type") == "log"]
    metrics = [r for r in records if r.get("type") == "metric" and r.get("name") == "eval.processing_ms"]
    traces = [r for r in records if r.get("type") == "span"]
    roots = {r["spanId"]: r for r in traces if r.get("name") == "eval.request"}
    children = [r for r in traces if r.get("name") == "eval.worker"]
    if len(logs) < 3 or len(metrics) < 3 or len(children) < 3:
        raise RuntimeError("workload instrumentation is missing logs, metrics or traces")
    for span in children:
        root = roots.get(span.get("parentSpanId"))
        if not root or root["traceId"] != span["traceId"]:
            raise RuntimeError("uncorrelated parent/child spans")
    if healthy:
        if any(r.get("severityNumber", 0) >= 17 for r in logs):
            raise RuntimeError("fresh workload still emits errors")
        if any(r.get("unit") != "ms" or not 0 <= r.get("value", -1) < 100 for r in metrics):
            raise RuntimeError("processing time exceeds the 100 ms limit")
        if any(r.get("status", {}).get("code", "").endswith("ERROR") or not 0 <= r.get("durationMs", -1) < 100 for r in children):
            raise RuntimeError("worker spans still fail or exceed the 100 ms limit")
    elif args.signal == "logs":
        if not any("division by zero" in r.get("body", "") for r in logs):
            raise RuntimeError("expected diagnostic error was not exported")
    elif not any(r.get("value", 0) >= 200 for r in metrics):
        raise RuntimeError("latency fault was not reproduced")


def prepare(args):
    f.prepare(args)
    for name in ("app.py", "telemetry.py"):
        shutil.copy2(HERE / "fixture_apps/otel" / name, name)
    settings = json.loads(Path("settings.json").read_text())
    settings.update(calibration_divisor=0 if args.signal == "logs" else 1,
                    worker_delay_ms=0 if args.signal == "logs" else 250)
    Path("settings.json").write_text(json.dumps(settings, indent=2))
    Path("requirements.json").write_text(json.dumps({"behavior": "compute the square of the supplied integer",
        "max_processing_ms": 100, "app_id": args.app_id, "preserve": ["nonce", "port", "telemetry.py"]}))
    Path("Dockerfile").write_text('FROM python:3.12-alpine\nRUN pip install --no-cache-dir opentelemetry-proto==1.36.0\nWORKDIR /app\nCOPY app.py telemetry.py settings.json ./\nCMD ["python3","-u","app.py"]\n')
    f.cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=600)
    f.wait_health(args, "v1")
    responses, records = collect(args, uuid.uuid4().hex)
    validate(responses, records, args, healthy=False)
    print(json.dumps({"baseline_responses": responses, "baseline_telemetry": records}))


def verify(args):
    f.verify(args)
    digest = hashlib.sha256((HERE / "fixture_apps/otel/telemetry.py").read_bytes()).hexdigest()
    code = "import hashlib; print(hashlib.sha256(open('/app/telemetry.py','rb').read()).hexdigest())"
    actual = f.cli(args, "--device", args.device, "device", "attach", args.app_id, "--", "python3", "-c", code).strip()
    if actual != digest:
        raise RuntimeError("the deployed OTLP instrumentation was changed")
    responses, records = collect(args, uuid.uuid4().hex)
    validate(responses, records, args, healthy=True)
    if args.events:
        require_telemetry_query(args.events, args.signal, args.run_id)
    elif args.phase != "smoke":
        raise RuntimeError("agent event evidence is required for a scored diagnosis")
    print(json.dumps({"responses": responses, "telemetry": records}))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=["prepare", "verify", "cleanup", "smoke"])
    parser.add_argument("signal", choices=["logs", "metrics", "traces"])
    for name in ("wendy", "device", "run-id", "port"):
        parser.add_argument("--" + name, required=True, type=int if name == "port" else str)
    parser.add_argument("--events")
    args = parser.parse_args()
    if not re.fullmatch("[a-f0-9]{12}", args.run_id) or not 1024 < args.port < 65535:
        parser.error("invalid run ID or port")
    args.task = "deploy-http"
    args.app_id = "dev.wendy.eval." + args.run_id
    args.state = Path.cwd().parent / ("wendy-eval-state-" + args.run_id + ".json")
    try:
        if args.phase == "smoke":
            try:
                prepare(args)
                settings = json.loads(Path("settings.json").read_text())
                settings.update(calibration_divisor=1, worker_delay_ms=0)
                Path("settings.json").write_text(json.dumps(settings))
                f.cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=600)
                verify(args)
            finally:
                f.cleanup(args)
        elif args.phase == "cleanup":
            f.cleanup(args)
        else:
            globals()[args.phase](args)
    except (RuntimeError, ValueError, subprocess.TimeoutExpired) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
