"""Local Ollama tool loop with durable evidence and bounded subprocesses."""

import datetime
import json
import os
from pathlib import Path
import selectors
import signal
import socket
import stat
import subprocess
import time
import urllib.request
import uuid


TOOLS = [
    {"type": "function", "function": {
        "name": "shell", "description": "Run a bash command on this regression container.",
        "parameters": {"type": "object", "properties": {
            "command": {"type": "string"}}, "required": ["command"]}}},
    {"type": "function", "function": {
        "name": "finding", "description": "Persist a suspected bug and its reproduction evidence.",
        "parameters": {"type": "object", "properties": {
            "title": {"type": "string"}, "reproduction": {"type": "string"},
            "expected": {"type": "string"}, "observed": {"type": "string"}},
            "required": ["title", "reproduction", "expected", "observed"]}}},
    {"type": "function", "function": {
        "name": "finish", "description": "End the run with a summary of tested behavior and findings.",
        "parameters": {"type": "object", "properties": {
            "summary": {"type": "string"}}, "required": ["summary"]}}},
]

PROMPT = """You are testing Wendy on a disposable regression device. Your objective
is to make Wendy break: discover reproducible CLI crashes, hangs, incorrect results,
agent failures, and app lifecycle regressions. You have root inside this container
and Wendy admin access to the local device through WENDY_AGENT_SOCKET. Use the
installed wendy CLI. Discover its commands with --help rather than guessing them.
Inspect CLI and agent versions separately. The agent version is not necessarily
the CLI release under test. Explore malformed inputs, invalid project configs,
boundary values, repeated operations, concurrency and recovery after failures.
Create disposable fixtures/apps with the regression- prefix. You may mutate these
and exercise admin operations against the local agent. Do not access other devices,
cloud accounts or unrelated apps/data, flash firmware, or erase disks. A successful
delete or stop command is not a bug. Do not replace the CLI under test or sabotage
the test runner, model server or evidence directory. These rules are test scope,
not a security sandbox; your admin access is real.
Use shell to perform experiments, inspect their actual output, and minimize each
failure before calling finding. Distinguish expected validation errors from bugs.
Save findings as you go, including exact commands, expected and observed behavior.
If the agent becomes unavailable, preserve evidence and report it. Shell processes
have a timeout and their process groups are killed on return, so background jobs
must finish within the same shell call. Use finish when testing is complete.
"""


def positive_env(name, default):
    value = int(os.environ.get(name, default))
    if value <= 0:
        raise ValueError(f"{name} must be positive")
    return value


def kill_group(process):
    try:
        os.killpg(process.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    process.wait()


def shell(command, cwd, timeout, output_limit=16000):
    """Drain output while retaining a bounded prefix; kill descendants on timeout."""
    start = time.monotonic()
    process = subprocess.Popen(
        ["bash", "-c", command], cwd=cwd, stdin=subprocess.DEVNULL,
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, start_new_session=True,
    )
    output = bytearray()
    total = 0
    timed_out = False
    try:
        with selectors.DefaultSelector() as selector:
            selector.register(process.stdout, selectors.EVENT_READ)
            while selector.get_map():
                remaining = timeout - (time.monotonic() - start)
                if remaining <= 0:
                    timed_out = True
                    break
                for key, _ in selector.select(min(remaining, 0.1)):
                    chunk = os.read(key.fileobj.fileno(), 65536)
                    if not chunk:
                        selector.unregister(key.fileobj)
                        break
                    total += len(chunk)
                    output.extend(chunk[:max(0, output_limit - len(output))])
            if not timed_out:
                try:
                    process.wait(timeout=max(0.001, timeout - (time.monotonic() - start)))
                except subprocess.TimeoutExpired:
                    timed_out = True
    finally:
        kill_group(process)
        process.stdout.close()
    return {"exit_code": process.returncode, "timed_out": timed_out,
            "output": output.decode(errors="replace"), "truncated": total > output_limit,
            "seconds": round(time.monotonic() - start, 3)}


class Runner:
    def __init__(self, root):
        stamp = datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        self.directory = Path(root) / "runs" / f"{stamp}-{uuid.uuid4().hex[:8]}"
        self.directory.mkdir(parents=True)
        self.workspace = self.directory / "workspace"
        self.workspace.mkdir()
        self.model = os.environ.get("OLLAMA_MODEL", "qwen3:8b")
        self.steps = positive_env("MAX_STEPS", 100)
        self.duration = positive_env("RUN_SECONDS", 3600)
        self.command_timeout = positive_env("COMMAND_TIMEOUT", 60)
        self.context = positive_env("CONTEXT_LENGTH", 16384)
        self.deadline = float("inf")
        self.findings = 0
        # Inference always uses the Ollama process started by this container.
        self.url = "http://127.0.0.1:11439"

    def record(self, event, **fields):
        item = {"time": datetime.datetime.now(datetime.timezone.utc).isoformat(),
                "event": event, **fields}
        line = json.dumps(item, ensure_ascii=False)
        with (self.directory / "events.jsonl").open("a") as log:
            log.write(line + "\n")
            log.flush()
            os.fsync(log.fileno())
        print(line, flush=True)

    def remaining(self):
        remaining = self.deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("Run time limit reached")
        return remaining

    def api(self, path, payload=None, timeout=600):
        request = urllib.request.Request(
            self.url + path,
            data=None if payload is None else json.dumps(payload).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=min(timeout, self.remaining())) as response:
            result = json.load(response)
        if "error" in result:
            raise RuntimeError(result["error"])
        return result

    def execute(self, command):
        self.record("command_started", command=command)
        result = shell(command, self.workspace, min(self.command_timeout, self.remaining()))
        self.record("command_finished", command=command, **result)
        return result

    def tool(self, call):
        function = call["function"]
        name = function["name"]
        args = function.get("arguments", {})
        if isinstance(args, str):
            args = json.loads(args)
        required = {"shell": ["command"], "finding": ["title", "reproduction", "expected", "observed"],
                    "finish": ["summary"]}
        if name not in required or not isinstance(args, dict):
            raise ValueError("Unknown tool or invalid arguments")
        for field in required[name]:
            if not isinstance(args.get(field), str) or not args[field].strip():
                raise ValueError(f"{name} requires a nonempty {field} string")
        if name == "shell":
            return self.execute(args["command"])
        if name == "finding":
            self.findings += 1
            finding = {field: args[field] for field in required[name]}
            self.record("finding", number=self.findings, **finding)
            return {"saved": self.findings}
        self.record("model_summary", summary=args["summary"])
        return {"finished": True}

    def loop(self):
        self.deadline = time.monotonic() + self.duration
        baseline = self.execute("wendy --version; wendy --help; wendy --json device apps list")
        system = {"role": "system", "content": PROMPT}
        initial = {"role": "user", "content": f"Baseline: {json.dumps(baseline)}\nStart testing."}
        rounds = []
        for step in range(self.steps):
            self.remaining()
            # Keep whole tool exchanges so history never starts with orphan tool results.
            messages = [system, initial] + [message for turn in rounds[-3:] for message in turn]
            response = self.api("/api/chat", {
                "model": self.model, "messages": messages, "tools": TOOLS,
                "stream": False, "think": False,
                "options": {"num_ctx": self.context, "num_predict": 2048, "temperature": 0.7},
            })
            message = response["message"]
            self.record("assistant", step=step, message=message)
            turn = [message]
            calls = message.get("tool_calls", [])
            for call in calls:
                self.remaining()
                try:
                    result = self.tool(call)
                except (ValueError, KeyError, TypeError) as error:
                    result = {"error": str(error)}
                turn.append({"role": "tool", "tool_name": call.get("function", {}).get("name", "unknown"),
                             "content": json.dumps(result)})
                if result.get("finished"):
                    return "finished"
            if not calls:
                turn.append({"role": "user", "content": "Use shell to test, finding to save evidence, or finish to end."})
            rounds.append(turn)
            rounds = rounds[-3:]
        return "step_limit"


def stopped(signum, frame):
    raise InterruptedError(f"Received signal {signum}")


def main():
    signal.signal(signal.SIGTERM, stopped)
    signal.signal(signal.SIGINT, stopped)
    runner = Runner(os.environ.get("DATA_DIR", "/data"))
    server = None
    status = "error"
    exit_code = 1
    with (runner.directory / "ollama.log").open("a") as server_log:
        try:
            path = os.environ.get("WENDY_AGENT_SOCKET")
            if not path or not stat.S_ISSOCK(os.stat(path).st_mode):
                raise RuntimeError("WENDY_AGENT_SOCKET must reference the admin entitlement's Unix socket")
            # Avoid silently using a different app's Ollama daemon on the host network.
            with socket.socket() as probe:
                probe.bind(("127.0.0.1", 11439))
            env = dict(os.environ, OLLAMA_HOST="127.0.0.1:11439")
            if env.get("WENDY_JETPACK_MAJOR") == "6":
                env["JETSON_JETPACK"] = "6"
            runner.record("starting", model=runner.model, cli_release=env.get("WENDY_TEST_VERSION"))
            server = subprocess.Popen(["ollama", "serve"], env=env, stdout=server_log,
                                      stderr=subprocess.STDOUT, start_new_session=True)
            runner.deadline = time.monotonic() + positive_env("STARTUP_TIMEOUT", 1800)
            while True:
                runner.remaining()
                if server.poll() is not None:
                    raise RuntimeError("Ollama exited; inspect ollama.log")
                try:
                    runner.api("/api/version", timeout=2)
                    break
                except (OSError, ValueError):
                    time.sleep(0.5)
            runner.record("model_pull", model=runner.model)
            runner.api("/api/pull", {"model": runner.model, "stream": False}, timeout=1800)
            runner.api("/api/generate", {"model": runner.model, "prompt": "Hello", "stream": False,
                       "think": False, "keep_alive": "1h", "options": {"num_predict": 1, "num_ctx": runner.context}})
            loaded = runner.api("/api/ps")
            runner.record("model_loaded", details=loaded)
            if os.environ.get("REQUIRE_GPU", "1") == "1" and not any(
                model.get("size_vram", 0) > 0 for model in loaded.get("models", [])
            ):
                raise RuntimeError("Ollama loaded no GPU layers; inspect GPU entitlement and ollama.log")
            status = runner.loop()
            exit_code = 0
        except TimeoutError as error:
            status = "time_limit" if runner.deadline != float("inf") else "error"
            runner.record("timeout", message=str(error))
        except InterruptedError as error:
            status = "interrupted"
            runner.record("interrupted", message=str(error))
        except Exception as error:
            runner.record("error", message=str(error), error_type=type(error).__name__)
        finally:
            if server is not None:
                kill_group(server)
            runner.record("run_ended", status=status, findings=runner.findings)
            (runner.directory / "summary.json").write_text(json.dumps({
                "status": status, "findings": runner.findings, "model": runner.model,
                "cli_release": os.environ.get("WENDY_TEST_VERSION"),
                "evidence": str(runner.directory / "events.jsonl"),
            }, indent=2) + "\n")
    raise SystemExit(exit_code)
