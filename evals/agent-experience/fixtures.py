#!/usr/bin/env python3
"""Trusted setup and behavioral grading, run outside the assistant session."""

import argparse
import json
from pathlib import Path
import re
import socket
import subprocess
import sys
import time
import uuid

import robot_fixtures

VM_TASKS = {"create-simulator", "create-go2-simulator", "create-g1-simulator"}


def cli(args, *argv, timeout=60):
    result = subprocess.run([args.wendy, *argv], stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, timeout=timeout)
    if result.returncode:
        raise RuntimeError(result.stderr + result.stdout)
    return result.stdout


def apps(args):
    return json.loads(cli(args, "--json", "--device", args.device, "device", "apps", "list"))


def own_app(args):
    return next((app for app in apps(args) if app["name"] == args.app_id), None)


def health(args):
    url = f"http://127.0.0.1:{args.port}/health"
    # Exec the verifier's own probe inside this exact container, through the
    # selected transport. This also supports agents predating HostShell.
    command = f"import urllib.request; print(urllib.request.urlopen({url!r}, timeout=5).read().decode())"
    return json.loads(cli(args, "--device", args.device, "device", "attach", args.app_id,
                          "--", "python3", "-c", command, timeout=30))


def assert_health(args, version):
    app = own_app(args)
    if app is None or app.get("runningState") != "RUNNING":
        raise RuntimeError(f"expected running app {args.app_id}: {app}")
    payload = health(args)
    expected = {"app_id": args.app_id, "version": version, "nonce": args.run_id}
    if any(payload.get(k) != v for k, v in expected.items()):
        raise RuntimeError(f"wrong HTTP response: {payload}")
    return payload


def wait_health(args, version, seconds=20):
    deadline = time.monotonic() + seconds
    while True:
        try:
            return assert_health(args, version)
        except (RuntimeError, ValueError):
            if time.monotonic() >= deadline:
                raise
            time.sleep(1)


def fixture(args):
    settings = {"port": args.port, "app_id": args.app_id, "version": "v1", "nonce": args.run_id}
    Path("settings.json").write_text(json.dumps(settings) + "\n")
    Path("app.py").write_text('''import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

settings = json.loads(Path("settings.json").read_text())

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({key: settings[key] for key in ("app_id", "version", "nonce")}).encode()
        self.send_response(200 if self.path == "/health" else 404)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

HTTPServer(("0.0.0.0", settings["port"]), Handler).serve_forever()
''')
    Path("Dockerfile").write_text("FROM python:3.12-alpine\nWORKDIR /app\nCOPY app.py settings.json ./\nCMD [\"python\", \"-u\", \"app.py\"]\n")
    Path("wendy.json").write_text(json.dumps({"appId": args.app_id, "version": "1.0.0", "platform": "linux",
        "entitlements": [{"type": "network", "mode": "host"}, {"type": "http", "port": args.port}]}, indent=2) + "\n")


def compose_request(args, value, challenge):
    url = f"http://127.0.0.1:{args.port}/compute?value={value}&challenge={challenge}"
    probe = ("import json,urllib.request,urllib.error\n"
             f"try:\n r=urllib.request.urlopen({url!r},timeout=5); print(json.dumps([r.status,json.load(r)]))\n"
             "except urllib.error.HTTPError as e: print(json.dumps([e.code,None]))")
    return json.loads(cli(args, "--device", args.device, "device", "attach", args.app_id + "_frontend",
                          "--", "python3", "-c", probe, timeout=30))


def verify_compose(args):
    compose = next((p for name in ("docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml")
                    if (p := Path(name)).exists()), None)
    if compose is None:
        raise RuntimeError("no Docker Compose project was created")
    result = subprocess.run(["docker", "compose", "-f", str(compose), "config", "--format", "json"],
                            capture_output=True, text=True, timeout=30)
    if result.returncode or set(json.loads(result.stdout).get("services", {})) != {"frontend", "worker"}:
        raise RuntimeError("Compose must define frontend and worker services")
    app = own_app(args)
    services = {s["name"]: s.get("runningState") for s in (app or {}).get("services", [])}
    if services != {"frontend": "RUNNING", "worker": "RUNNING"}:
        raise RuntimeError(f"expected two running services: {app}")
    evidence = []
    for value in (7, -13, 23):
        challenge = uuid.uuid4().hex
        deadline = time.monotonic() + 20
        while True:
            try:
                status, body = compose_request(args, value, challenge)
                if status == 200 or time.monotonic() >= deadline:
                    break
            except RuntimeError:
                if time.monotonic() >= deadline:
                    raise
            time.sleep(1)
        expected = {"app_id": args.app_id, "worker_id": args.run_id, "result": value * value, "challenge": challenge}
        if status != 200 or body != expected:
            raise RuntimeError(f"wrong composed result: {status}, {body}")
        evidence.append(body)
    # A live dependency failure prevents a single service with a fake worker
    # from satisfying the task. This is after the scored agent has finished.
    worker = args.app_id + "_worker"
    cli(args, "--device", args.device, "device", "apps", "stop", worker)
    try:
        status, _ = compose_request(args, 19, uuid.uuid4().hex)
        if status != 503:
            raise RuntimeError("frontend must return 503 when its worker is unavailable")
    finally:
        cli(args, "--device", args.device, "device", "apps", "start", worker, "--detach")
    challenge = uuid.uuid4().hex
    deadline = time.monotonic() + 20
    while True:
        status, body = compose_request(args, 11, challenge)
        if status == 200 or time.monotonic() >= deadline:
            break
        time.sleep(1)
    if status != 200 or body != {"app_id": args.app_id, "worker_id": args.run_id, "result": 121, "challenge": challenge}:
        raise RuntimeError(f"Compose app did not recover when the worker restarted: {status}, {body}")
    print(json.dumps({"services": services, "responses": evidence, "dependency_failure_status": 503,
                      "recovery": body}))


def compose_solution(args):
    """Deterministic fixture calibration only; never copied to the LLM workspace."""
    common = '''import json, urllib.request, urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
s = json.loads(Path("settings.json").read_text())
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            q = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            if s["role"] == "worker":
                body = {"worker_id": s["worker_id"], "challenge": q["challenge"][0], "result": int(q["value"][0]) ** 2}
            else:
                body = json.load(urllib.request.urlopen("http://127.0.0.1:" + str(s["worker_port"]) + self.path, timeout=2))
                body["app_id"] = s["app_id"]
            status = 200
        except Exception:
            status, body = 503, {"error": "worker unavailable"}
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)
HTTPServer(("0.0.0.0",s["port"]),Handler).serve_forever()
'''
    for role, port in (("frontend", args.port), ("worker", args.port + 1)):
        directory = Path(role)
        directory.mkdir()
        (directory / "app.py").write_text(common)
        (directory / "settings.json").write_text(json.dumps({"role": role, "port": port,
            "worker_port": args.port + 1, "app_id": args.app_id, "worker_id": args.run_id}))
        (directory / "Dockerfile").write_text('FROM python:3.12-alpine\nWORKDIR /app\nCOPY . .\nCMD ["python3","-u","app.py"]\n')
    Path("compose.yml").write_text("services:\n  worker:\n    build: ./worker\n    network_mode: host\n  frontend:\n    build: ./frontend\n    network_mode: host\n    depends_on: [worker]\n")
    Path("wendy.json").write_text(json.dumps({"appId": args.app_id, "version": "1.0.0", "platform": "linux"}))


def prepare(args):
    if args.task in VM_TASKS:
        existing = json.loads(cli(args, "--json", "vm", "list"))
        if any(vm["name"] == args.vm_name for vm in existing):
            raise RuntimeError("run-specific simulator already exists")
        args.state.write_text(json.dumps(existing))
        return
    # Reading identity and app inventory establishes that setup worked before the
    # timed attempt. Also keep a canary inventory of every unrelated app.
    info = json.loads(cli(args, "--json", "--device", args.device, "device", "info", timeout=180))
    inventory = apps(args)
    if any(app["name"] == args.app_id or app.get("httpPort") == args.port for app in inventory):
        raise RuntimeError("benchmark app ID or HTTP port is already in use")
    print(json.dumps({"device": args.device, "info": info, "apps_before": inventory}))
    # Stored next to the workspace, outside the task's working directory.
    args.state.write_text(json.dumps(inventory))
    if args.task == "compose-app":
        Path("requirements.json").write_text(json.dumps({"app_id": args.app_id,
            "frontend_port": args.port, "worker_port": args.port + 1,
            "services": ["frontend", "worker"], "worker_id": args.run_id}, indent=2))
        return
    fixture(args)
    if args.task == "repair-startup":
        path = Path("app.py")
        path.write_text(path.read_text().replace('settings["port"]', 'settings["listen_port"]'))
    if args.task in ("repair-startup", "update-http", "stop-app", "start-app"):
        cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=600)
        if args.task == "repair-startup":
            time.sleep(3)
            app = own_app(args)
            if app is None or app.get("runningState") == "RUNNING":
                raise RuntimeError("startup fault was not reproduced")
        else:
            wait_health(args, "v1")
            if args.task == "start-app":
                cli(args, "--device", args.device, "device", "apps", "stop", args.app_id)
                if own_app(args).get("runningState") != "STOPPED":
                    raise RuntimeError("start fixture was not stopped")


def verify(args):
    if args.task in VM_TASKS:
        inventory = json.loads(cli(args, "--json", "vm", "list"))
        vm = next((v for v in inventory if v["name"] == args.vm_name), None)
        if vm is None or vm["state"] != "running":
            raise RuntimeError(f"simulator is not running: {vm}")
        # Device connection can reconcile a robot runtime. Check its passive
        # HTTP status first so the grader never completes an unfinished launch.
        if args.task != "create-simulator":
            robot_fixtures.verify(args, cli)
        info = json.loads(cli(args, "--json", "--device", "vm:" + args.vm_name, "device", "info", timeout=45))
        if not info.get("version") or not info.get("osVersion", "").startswith("WendyOS-"):
            raise RuntimeError(f"simulator agent did not report WendyOS/Linux and a version: {info}")
        before = json.loads(args.state.read_text())
        others = {v["name"]: v["state"] for v in inventory if v["name"] != args.vm_name}
        if others != {v["name"]: v["state"] for v in before}:
            raise RuntimeError("unrelated VM inventory changed")
        print(json.dumps({"vm": vm, "info": info}))
        return
    before = json.loads(args.state.read_text())
    after = {app["name"]: app for app in apps(args)}
    for app in before:
        current = after.get(app["name"])
        if current is None or current.get("runningState") != app.get("runningState") or current.get("version") != app.get("version"):
            raise RuntimeError(f"unrelated app changed: {app['name']}")
    if args.task == "compose-app":
        verify_compose(args)
        return
    if args.task == "stop-app":
        app = own_app(args)
        if app is None or app.get("runningState") != "STOPPED":
            raise RuntimeError(f"app must remain deployed and stopped: {app}")
        time.sleep(3)
        again = own_app(args)
        if again is None or again.get("runningState") != "STOPPED":
            raise RuntimeError(f"app did not remain stopped: {again}")
        print(json.dumps({"stopped": app, "second": again}))
        return
    version = "v2" if args.task == "update-http" else "v1"
    first = wait_health(args, version)
    time.sleep(3)
    second = assert_health(args, version)
    print(json.dumps({"first": first, "second": second}))


def cleanup(args):
    # A setup collision must never delete an existing resource.
    if not args.state.exists():
        return
    if args.task in VM_TASKS:
        inventory = json.loads(cli(args, "--json", "vm", "list"))
        if any(vm["name"] == args.vm_name for vm in inventory):
            cli(args, "vm", "--yes", "rm", args.vm_name, "--force", timeout=120)
        return
    if own_app(args) is not None:
        cli(args, "--device", args.device, "device", "apps", "remove", args.app_id, "--force", "--cleanup", timeout=120)
    if own_app(args) is not None:
        raise RuntimeError("benchmark app remains after cleanup")


def smoke(args):
    """Check the fixture against a real device without spending model tokens."""
    try:
        prepare(args)
        if args.task in VM_TASKS:
            profile = args.task.removeprefix("create-").removesuffix("-simulator")
            extra = [] if args.task == "create-simulator" else ["--profile", profile]
            cli(args, "vm", "--yes", "create", args.vm_name, *extra, timeout=600)
            with socket.socket() as sock:
                sock.bind(("127.0.0.1", 0))
                port = sock.getsockname()[1]
            cli(args, "vm", "--yes", "start", args.vm_name, "--detach", "--port", str(port), timeout=180)
            if args.task != "create-simulator":
                if args.agent_binary:
                    cli(args, "--device", "vm:" + args.vm_name, "device", "update",
                        "--binary", args.agent_binary, timeout=180)
                cli(args, "vm", "--yes", "robot", "start", args.vm_name, timeout=1800)
        elif args.task == "start-app":
            cli(args, "--device", args.device, "device", "apps", "start", args.app_id, "--detach")
        elif args.task == "compose-app":
            compose_solution(args)
            cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=600)
        elif args.task == "stop-app":
            cli(args, "--device", args.device, "device", "apps", "stop", args.app_id)
        else:
            if args.task == "repair-startup":
                path = Path("app.py")
                path.write_text(path.read_text().replace('settings["listen_port"]', 'settings["port"]'))
            if args.task == "update-http":
                path = Path("settings.json")
                settings = json.loads(path.read_text())
                settings["version"] = "v2"
                path.write_text(json.dumps(settings) + "\n")
            cli(args, "run", "--yes", "--detach", "--no-restart", "--device", args.device, timeout=600)
        verify(args)
    finally:
        cleanup(args)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("phase", choices=["prepare", "verify", "cleanup", "smoke"])
    parser.add_argument("task", choices=sorted(VM_TASKS | {"deploy-http", "repair-startup", "update-http", "stop-app", "start-app", "compose-app"}))
    parser.add_argument("--wendy", required=True)
    parser.add_argument("--device", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--agent-binary", help="Candidate ARM64 agent for robot fixture smoke checks")
    args = parser.parse_args()
    if not re.fullmatch("[a-f0-9]{12}", args.run_id) or not 1024 < args.port < 65536:
        parser.error("invalid run ID or port")
    args.app_id = "dev.wendy.eval." + args.run_id
    args.vm_name = "wendy-eval-" + args.run_id
    args.state = Path.cwd().parent / ("wendy-eval-state-" + args.run_id + ".json")
    try:
        globals()[args.phase](args)
    except (RuntimeError, ValueError, subprocess.TimeoutExpired) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
