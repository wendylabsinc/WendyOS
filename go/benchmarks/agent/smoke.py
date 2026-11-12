#!/usr/bin/env python3
"""Exercise collection against a real Linux agent in an isolated Docker container.

This validates the benchmark tooling, not hardware performance. No ports, devices,
host service configuration or credentials are exposed to the container.
"""

import argparse
from contextlib import contextmanager
import http.client
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import uuid


HERE = Path(__file__).resolve().parent
IMAGE = "python:3.13-slim@sha256:e48f59cdba69a3cbd9ed2cf0f348d90d45a8d350d98134cf1070bc106ee9ed90"


@contextmanager
def agent_fixture(output):
    env = dict(os.environ, WENDY_LOCAL_SOCKET="off", WENDY_CONTAINERD_ADDR="/tmp/no-containerd.sock",
               WENDY_TELEMETRY_DIR="/tmp/telemetry", WENDY_DATA_DIR="/tmp/data")
    with (output / "agent.log").open("w") as log:
        agent = subprocess.Popen(["/test-agent"], env=env, stdout=log, stderr=log)
    try:
        deadline = time.monotonic() + 30
        while True:
            if agent.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("agent did not become ready: " + (output / "agent.log").read_text())
            conn = http.client.HTTPConnection("127.0.0.1", 4318, timeout=.5)
            try:
                conn.request("POST", "/v1/logs", b"", {"Content-Type": "application/x-protobuf"})
                response = conn.getresponse()
                response.read()
                if response.status == 200:
                    break
            except OSError:
                pass
            finally:
                conn.close()
            time.sleep(.1)
        yield agent
    finally:
        if agent.poll() is None:
            agent.kill()
        agent.wait()


def inside():
    output = Path("/results")
    with agent_fixture(output) as agent:
        for label in ("baseline", "candidate"):
            subprocess.run([sys.executable, str(HERE / "benchmark.py"), "run", "--pid", str(agent.pid),
                            "--profile", str(HERE / "profiles/smoke.json"), "--environment", "docker-smoke",
                            "--revision", label, "--output", str(output / label)], check=True, timeout=30)
            report = json.loads((output / label / "report.json").read_text())
            assert report["status"] == "ok"
            assert len(report["runs"]) == 4
            for run in report["runs"]:
                assert run["metrics"]["agent.process.rss_bytes.peak"] > 0
                if run["scenario"] == "telemetry":
                    assert run["metrics"]["workload.accepted_records"] == 20
                    assert run["metrics"]["agent.process.wchar.total"] > 0
        # HTTP acknowledgement alone is not a persistence check. Verify the real
        # protobuf fixture reached the agent's disk buffer during this smoke test.
        assert any(b"wendy-agent-benchmark" in p.read_bytes() for p in Path("/tmp/telemetry").glob("logs-*.bin"))
        subprocess.run([sys.executable, str(HERE / "benchmark.py"), "compare",
                        str(output / "baseline/report.json"), str(output / "candidate/report.json"),
                        "--output", str(output / "comparison")], check=True, timeout=10)
        agent.terminate()
        agent.wait(timeout=15)
        result = subprocess.run([sys.executable, str(HERE / "benchmark.py"), "run", "--pid", str(agent.pid),
                                 "--profile", str(HERE / "profiles/smoke.json"), "--environment", "docker-smoke",
                                 "--revision", "stopped-agent", "--output", str(output / "failed")], timeout=10)
        assert result.returncode == 2
        assert json.loads((output / "failed/report.json").read_text())["status"] == "failed"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--agent", type=Path)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--inside", action="store_true", help=argparse.SUPPRESS)
    args = parser.parse_args()
    if args.inside:
        inside()
        return
    if args.agent is None or not args.agent.is_file() or args.output is None:
        parser.error("--agent and --output are required; binary must match Docker's Linux architecture")
    args.output.mkdir(parents=True, exist_ok=False)
    name = "wendy-agent-perf-" + uuid.uuid4().hex[:12]
    try:
        subprocess.run(["docker", "run", "--rm", "--name", name, "--network", "none",
                        "--mount", f"type=bind,src={args.agent.resolve()},dst=/test-agent,readonly",
                        "--mount", f"type=bind,src={HERE},dst=/benchmark,readonly",
                        "--mount", f"type=bind,src={args.output.resolve()},dst=/results",
                        IMAGE, "python3", "/benchmark/smoke.py", "--inside"],
                       check=True, timeout=120)
    finally:
        subprocess.run(["docker", "rm", "-f", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, timeout=15)


if __name__ == "__main__":
    main()
