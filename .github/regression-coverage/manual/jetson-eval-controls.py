#!/usr/bin/env python3
"""Run explicitly inside the admitted disposable evaluator container, never at generation."""
import argparse
import hashlib
import http.server
import json
import os
from pathlib import Path
import subprocess
import sys
import threading

HERE = Path(__file__).resolve().parent


def digest(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda: source.read(4 * 1024**2), b""):
            h.update(chunk)
    return h.hexdigest()


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def admit(plan):
    require(sys.platform == "linux" and os.environ.get("WENDY_APP_ID") == plan["app_id"], "foreign app")
    require(Path("/proc/sys/kernel/random/boot_id").read_text().strip() == plan["board"]["boot_id"], "different admitted boot")
    require(digest("/usr/local/bin/wendy") == plan["binaries"]["staged_cli"]["sha256"], "CLI bytes changed")
    require((Path("/opt/treatment") / "REVISION").read_text().strip() == plan["source_commit"], "treatment revision changed")
    require(json.loads((HERE / "models.json").read_text())[plan["model_id"]] == plan["model"], "model pin changed")


def verify(plan):
    # Independently hash every byte, even when production's .verified download marker exists.
    files = []
    for item in plan["model"]["files"]:
        path = Path("/models") / item["file"]
        require(path.is_file() and not path.is_symlink() and path.stat().st_size == item["bytes"], "model size/type mismatch")
        require(digest(path) == item["sha256"], "model byte hash mismatch")
        files.append(item)
    latest = Path("/results/latest").resolve(strict=True)
    require(latest.parent == Path("/results") and plan["trial"] in latest.name, "foreign pass directory")
    state = json.loads((latest / "pass.json").read_text())
    require(state["status"] == "finished" and state["exit_code"] == 0, "pass unfinished or halted")
    facts = json.loads((latest / "on_device.json").read_text())
    require(facts["model"] == plan["model_id"] and facts["revision"] == plan["model"]["revision"]
            and facts["cli_commit"] == plan["source_commit"] and facts["app_id"] == plan["app_id"], "pass identity mismatch")
    require(0 < facts["baseline_prompt_tokens"] <= facts["context"] / 2 and facts["tokens_per_second"] > 0,
            "missing actual model baseline")
    rows = [json.loads(p.read_text()) for p in latest.glob("*/result.json")]
    require(len(rows) == 1 and rows[0]["target"] == "local", "expected exactly one local attempt")
    require(rows[0]["status"] not in ("setup_error", "timeout", "interrupted", "model_server_down", "context_exceeded"),
            "normal model attempt did not complete")
    require(rows[0].get("cleanup_ok") is True and rows[0].get("metrics", {}).get("usage_complete") is True,
            "missing cleanup or actual model usage")
    require((latest / "report.md").is_file() and (latest / "report.json").is_file(), "model reports missing")
    result = {"case": "local-gpu-pass", "automated_checks": "passed", "model_files": files,
              "on_device": facts, "attempt_status": rows[0]["status"],
              "human_gpu_offload_and_identity_review": "required", "physical_acceptance": "pending_review"}
    Path("/results/rehearsal-model-verification.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


def timeout_trial(b, sc, plan, output, cli, shim, tools, agent_seconds=30):
    """Real chat process/request and production trial/report; only HTTP response is withheld."""
    received, release = threading.Event(), threading.Event()
    requests = []

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            body = json.dumps({"object": "list", "data": [{"id": "deadline-control", "object": "model"}]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            size = int(self.headers.get("Content-Length", 0))
            if not self.path.endswith("/chat/completions") or not 0 < size <= 4 * 1024**2:
                self.send_error(400)
                return
            self.connection.settimeout(5)
            requests.append(json.loads(self.rfile.read(size)))
            received.set()
            release.wait(agent_seconds + 10)
            self.close_connection = True

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.daemon_threads = False
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    try:
        config = json.loads((HERE / "config.on-device.json").read_text())
        config.pop("model_health_url", None)  # controlled endpoint is intentionally silent, not GPU health
        config["max_agent_seconds"] = agent_seconds
        config["agents"]["chat"]["model"] = "deadline-control"
        command = config["agents"]["chat"]["command"]
        command[command.index("--base-url") + 1] = f"http://127.0.0.1:{server.server_port}/v1"
        task = {"id": "physical-cli-deadline-control", "prompt": "List this device's apps.",
                "prepare": [sys.executable, "-c", "pass"], "verify": [sys.executable, "-c", "raise SystemExit(1)"],
                "cleanup": [sys.executable, "-c", "pass"], "cleanup_timeout_seconds": 5,
                "setup_timeout_seconds": 5, "verify_timeout_seconds": 5}
        row = b.trial(config, "chat", "local", task, output, cli,
                      treatment_root=Path("/opt/treatment"), shim=shim, tools_path=tools)
    finally:
        release.set()
        server.shutdown()
        server.server_close()
        thread.join(5)
        require(not thread.is_alive(), "controlled HTTP peer did not drain")
    require(received.is_set() and requests and requests[0].get("messages"), "deadline expired before actual CLI model request")
    require(row["status"] == "timeout" and row["success"] is False and row["cleanup_ok"] is True
            and row["stages"]["agent"]["timed_out"] is True, "timeout result misreported")
    b.report([row], output)
    summary = json.loads((output / "summary.json").read_text())
    require(len(summary) == 1 and summary[0]["timeouts"] == 1 and summary[0]["successes"] == 0
            and summary[0]["unscored"] == 1 and summary[0]["attempts"] == 0, "local timeout incorrectly scored")
    (output / "timeout-request.json").write_text(json.dumps(requests[0], indent=2) + "\n")
    return {"status": "passed", "result": row["id"], "real_cli_http_request_observed": True,
            "controlled_http_response": "withheld", "model_performance_claim": False}


def controls(plan):
    sys.path.insert(0, str(HERE))
    import benchmark as b
    import eval_wendy
    import on_device as od
    import scenarios as sc
    from mcp_client import StdioMCP
    output = Path("/results/rehearsal-controls")
    output.mkdir(mode=0o700, exist_ok=False)
    cli = "/usr/local/bin/wendy"
    env = dict(os.environ, WENDY_ANALYTICS="false")
    tools = output / "tools.json"
    tools.write_text(json.dumps(b.list_all_tools(cli, env)))
    request = od.capture_first_request(cli, env)
    names = {item["name"] for item in json.loads(tools.read_text())}
    require(not od.unknown_tools(request, names, od.tool_builtins()), "advertised tool catalog unexplained")
    (output / "advertised-request.json").write_text(json.dumps(request, indent=2) + "\n")
    shim = eval_wendy.write_shim(output)
    scenario = output / "scenario.json"
    sc.write_scenario_config(scenario, real_wendy=cli, shim=str(shim), tools_path=tools,
                            canned_dir=HERE / "canned-local", canned_set="tasks", canned=[], allow_real=[],
                            block=sc.guard_matchers(plan["app_id"]), cli_canned=[], device="local", vm_name="unused",
                            proxy_log=output / "proxy.jsonl", cli_log=output / "cli.jsonl", passthrough=True)
    env["WENDY_EVAL_SCENARIO"] = str(scenario)
    # If a guard regresses, only the explicitly owned evaluator app is targeted.
    result = subprocess.run([str(shim), "device", "apps", "stop", plan["app_id"]], env=env,
                            capture_output=True, text=True, timeout=30, stdin=subprocess.DEVNULL)
    logs = b.read_jsonl(output / "cli.jsonl")
    require(result.returncode == 0 and len(logs) == 1 and logs[0].get("canned") is True
            and not logs[0].get("error"), "CLI self-stop was forwarded or policy failed")
    with StdioMCP([str(shim), "mcp", "serve", "--tool-groups", "all"], env=env) as peer:
        peer.initialize()
        require("container_stop" in {t["name"] for t in peer.list_tools()}, "real MCP catalog lacks stop tool")
        peer.call("container_stop", {"app_name": plan["app_id"]})
    logs = b.read_jsonl(output / "proxy.jsonl")
    require(len(logs) == 1 and logs[0]["tool"] == "container_stop" and logs[0]["canned"] is True,
            "MCP self-stop was forwarded")
    timeout = timeout_trial(b, sc, plan, output, cli, shim, tools)
    result = {"guard": "passed", "refusal_semantics": "canned, not forwarded; not necessarily an error response",
              "timeout": timeout, "physical_gpu_claim": False}
    (output / "controls.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=("verify", "controls"))
    args = parser.parse_args()
    plan = json.loads((HERE / "rehearsal-plan.json").read_text())
    admit(plan)
    {"verify": verify, "controls": controls}[args.action](plan)
