"""Exercise a running SIMULATION app only. Never targets a physical adapter."""
import argparse
import json
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", default="http://127.0.0.1:8892")
    parser.add_argument("--mode", choices=["policy", "hil", "expert"], default="policy")
    parser.add_argument("--steps", type=int, default=2400)
    parser.add_argument("--output")
    args = parser.parse_args()

    def get(path):
        with urllib.request.urlopen(args.url + path, timeout=20) as response:
            return json.load(response)

    status = get("/api/status")
    assert status["scene"] == "attempt-000001" and status["physical_commands_sent"] == 0
    token = get("/api/session")["token"]

    def post(action, body):
        request = urllib.request.Request(args.url + "/api/" + action, json.dumps(body).encode(), headers={"Content-Type": "application/json", "X-Coke-Token": token})
        with urllib.request.urlopen(request, timeout=20) as response:
            return json.load(response)

    post("run", {"mode": args.mode, "steps": args.steps})
    last_progress = -1
    deadline = time.monotonic() + 3600
    while time.monotonic() < deadline:
        status = get("/api/status")
        if status["phase"] == "error":
            raise RuntimeError(status["last_error"])
        if status["step"] // 200 != last_progress:
            print(json.dumps({key: status[key] for key in ("phase", "step", "simulation_seconds", "metrics")}), flush=True)
            last_progress = status["step"] // 200
        if status["phase"] == "completed":
            assert status["step"] == args.steps
            if args.output:
                from pathlib import Path
                Path(args.output).write_text(json.dumps(status, indent=2) + "\n")
            print(json.dumps({key: status[key] for key in ("phase", "step", "simulation_seconds", "metrics", "ros")}), flush=True)
            return
        time.sleep(1)
    raise TimeoutError("Simulation did not complete within one hour")


if __name__ == "__main__":
    main()
