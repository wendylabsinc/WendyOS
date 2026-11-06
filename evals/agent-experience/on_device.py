"""Run one benchmark pass on the device it tests: fetch a pinned local model, serve it on the GPU,
check what `wendy chat` sends it, run benchmark.py, and leave the report on the device.
Python standard library only."""

import collections
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
LLAMA_SERVER = "/usr/lib/ollama/llama-server"
HOST, PORT = "127.0.0.1", 18081
HEADROOM = 12 * 2 ** 30
BASELINE_MAX_TOKENS = 128
HUGGINGFACE = "https://huggingface.co/{repo}/resolve/{revision}/{file}"


class UnknownModel(ValueError):
    pass


class FetchError(RuntimeError):
    pass


def load_model(path, model_id):
    pins = json.loads(Path(path).read_text())
    if model_id not in pins:
        raise UnknownModel(f"unknown model {model_id!r}; valid EVAL_MODEL values: {', '.join(sorted(pins))}")
    return {"id": model_id, **pins[model_id]}


def total_bytes(pin):
    return sum(f["bytes"] for f in pin["files"])


def server_argv(pin, context, models_dir):
    argv = [LLAMA_SERVER, "-m", str(Path(models_dir) / pin["entry_file"]), "-ngl", "999", "-c", str(context),
            "-np", "1", "--jinja", "--no-webui", "--host", HOST, "--port", str(PORT)]
    if pin.get("id"):
        argv += ["--alias", pin["id"]]
    return argv + list(pin.get("server_args", []))


def memory_ok(pin, meminfo_text):
    """The weights plus 12 GiB must fit in MemAvailable."""
    found = re.search(r"^MemAvailable:\s+(\d+)\s*kB", meminfo_text, re.M)
    return bool(found) and int(found.group(1)) * 1024 >= total_bytes(pin) + HEADROOM


def fetch_model(pin, models_dir, run=subprocess.run, attempts=30, pause=30, sleep=time.sleep):
    """Download every pinned file that is not already complete and checked. aria2c resumes and verifies
    the SHA-256; a `.verified` marker records that, so a restart doesn't hash 100 GB again. Hugging Face
    redirects to a signed link that expires mid-download (403), so a failed aria2c is run again with the
    same arguments: `-c` gets a fresh link and resumes from the .aria2 control file."""
    models_dir = Path(models_dir)
    models_dir.mkdir(parents=True, exist_ok=True)
    for entry in pin["files"]:
        target = models_dir / entry["file"]
        marker = models_dir / (entry["file"] + ".verified")
        if target.is_file() and target.stat().st_size == entry["bytes"] and \
                marker.is_file() and marker.read_text().strip() == entry["sha256"]:
            continue
        marker.unlink(missing_ok=True)
        if target.is_file() and target.stat().st_size > entry["bytes"]:
            target.unlink()
        argv = ["aria2c", "-x8", "-s8", "-c", f"--checksum=sha-256={entry['sha256']}", "-d", str(models_dir),
                "-o", entry["file"], HUGGINGFACE.format(repo=pin["repo"], revision=pin["revision"], file=entry["file"])]
        for attempt in range(1, attempts + 1):
            done = run(argv)
            if done.returncode == 0:
                break
            last = f"aria2c exited {done.returncode}"
            print(f"download of {entry['file']} failed ({last}), attempt {attempt} of {attempts}", file=sys.stderr, flush=True)
            if attempt < attempts:
                sleep(pause)
        else:
            raise FetchError(f"{entry['file']}: gave up after {attempts} attempts; last error: {last}")
        if not target.is_file() or target.stat().st_size != entry["bytes"]:
            raise FetchError(f"{entry['file']}: expected {entry['bytes']} bytes after the download")
        marker.write_text(entry["sha256"] + "\n")


class ModelServer:
    """llama-server, restarted whenever it exits until stop() is called."""

    def __init__(self, argv, log_path, health_url, restart_delay=2.0):
        self.argv, self.log_path, self.health_url, self.restart_delay = argv, Path(log_path), health_url, restart_delay
        self.starts = 0
        self._process, self._thread, self._stopping = None, None, threading.Event()

    def _launch(self):
        self.log_path.parent.mkdir(parents=True, exist_ok=True)
        with open(self.log_path, "ab") as log:
            log.write(f"{time.strftime('%FT%TZ', time.gmtime())} starting: {shlex.join(self.argv)}\n".encode())
            log.flush()
            self._process = subprocess.Popen(self.argv, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL)
        self.starts += 1

    def _supervise(self):
        while not self._stopping.is_set():
            self._process.wait()
            if self._stopping.wait(self.restart_delay):
                return
            try:
                self._launch()
            except OSError as error:  # keep trying: the next start may find the binary or the disk back
                print(f"model server did not start: {error}", file=sys.stderr, flush=True)

    def start(self):
        self._stopping.clear()
        self._launch()
        self._thread = threading.Thread(target=self._supervise, daemon=True)
        self._thread.start()

    def healthy(self):
        try:
            with urllib.request.urlopen(self.health_url, timeout=5) as reply:
                return reply.status == 200 and json.load(reply).get("status") == "ok"
        except urllib.error.HTTPError as error:  # 503 while the model loads
            error.close()
            return False
        except (OSError, ValueError):
            return False

    def wait_healthy(self, timeout=900, poll=2.0):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if self.healthy():
                return True
            time.sleep(poll)
        return False

    def stop(self):
        self._stopping.set()
        process = self._process
        if process is not None and process.poll() is None:
            process.terminate()
            try:
                process.wait(30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        if self._thread is not None:
            self._thread.join(10)


def _chunk(delta, finish=None):
    return {"id": "x", "object": "chat.completion.chunk", "created": 0, "model": "fake",
            "choices": [{"index": 0, "delta": delta, "finish_reason": finish}],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}}


def capture_first_request(wendy, env, timeout=120):
    """Run `wendy chat` once against a fake OpenAI endpoint and return the first request it sends,
    before any tool runs. The chat gets a private home and no agent socket."""
    requests = []

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            body = json.dumps({"object": "list", "data": [{"id": "fake", "object": "model"}]}).encode()
            self.send_response(200 if self.path.endswith("/models") else 404)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
            if not self.path.endswith("/chat/completions"):
                self.send_response(404)
                self.end_headers()
                return
            requests.append(request)
            self.send_response(200)
            if request.get("stream"):
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                for chunk in (_chunk({"role": "assistant", "content": "done"}), _chunk({}, "stop")):
                    self.wfile.write(b"data: " + json.dumps(chunk).encode() + b"\n\n")
                self.wfile.write(b"data: [DONE]\n\n")
            else:
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"choices": [{"index": 0, "finish_reason": "stop",
                                                          "message": {"role": "assistant", "content": "done"}}],
                                             "usage": {"prompt_tokens": 1, "completion_tokens": 1}}).encode())
            self.wfile.flush()

    server = ThreadingHTTPServer((HOST, 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    try:
        with tempfile.TemporaryDirectory(prefix="ctx-") as scratch:
            home, work = Path(scratch) / "home", Path(scratch) / "work"
            config = home / "wendy"
            config.mkdir(parents=True)
            work.mkdir()
            (config / "config.json").write_text(json.dumps({"analytics": {"enabled": False}, "completionInstalled": True,
                                                            "completionPromptDismissed": True}))
            private = {k: v for k, v in env.items() if k != "WENDY_AGENT_SOCKET"}
            private.update({"HOME": str(home), "WENDY_CONFIG_DIR": str(config), "WENDY_SECRET_STORE": "file",
                            "NO_COLOR": "1", "TERM": "dumb", "WENDY_ANALYTICS": "false"})
            done = subprocess.run(
                [wendy, "chat", "--no-memory", "--json", "--yes", "--provider", "local",
                 "--base-url", f"http://{HOST}:{server.server_address[1]}/v1", "--model", "fake",
                 "--prompt", "List the apps running on this device."],
                cwd=work, env=private, capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    finally:
        server.shutdown()
        server.server_close()
    if not requests:
        raise RuntimeError(f"wendy chat made no request to the capture endpoint (exit {done.returncode}): "
                           f"{(done.stderr or done.stdout)[-400:]}")
    return requests[0]


def _tool_name(tool):
    if isinstance(tool, str):
        return tool
    return (tool.get("function") or {}).get("name") or tool.get("name")


def request_tool_names(request):
    return [_tool_name(t) for t in request.get("tools") or []]


def unknown_tools(request, tools, builtins):
    """Tool names the request carries that neither the Wendy MCP server nor chat's built-ins explain.
    Each would end an attempt as unverified, so the pass refuses to start with any."""
    return set(request_tool_names(request)) - {_tool_name(t) for t in tools} - set(builtins)


def measure_baseline(url, request, timeout=900):
    """Send the captured first request once, unstreamed and capped, and read what llama-server reports."""
    body = {k: v for k, v in request.items() if k not in ("stream", "stream_options", "max_tokens")}
    body.update(stream=False, max_tokens=BASELINE_MAX_TOKENS)
    req = urllib.request.Request(url, json.dumps(body).encode(), {"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as reply:
        data = json.load(reply)
    tokens = (data.get("usage") or {}).get("prompt_tokens")
    if not isinstance(tokens, int):
        raise ValueError("the model server's reply has no usage.prompt_tokens")
    timings = data.get("timings") or {}
    return {"baseline_prompt_tokens": tokens, "prompt_per_second": timings.get("prompt_per_second"),
            "predicted_per_second": timings.get("predicted_per_second")}


def baseline_ok(tokens, context):
    return tokens <= context / 2


def pass_dir(results, model_id, revision, label):
    return Path(results) / f"{model_id}-{revision[:12]}-{label}"


def benchmark_argv(config, task_ids, repetitions, wendy, treatment_root, resume, limit=None):
    """The arguments to benchmark.py (not the interpreter or script), for one pass."""
    argv = ["--config", str(config), "--scenario", "all"]
    for task in task_ids:  # every task: the planner filters by environment and lists the rest with a reason
        argv += ["--task", task]
    argv += ["--target", "local", "--agent", "chat", "--repetitions", str(repetitions),
             "--treatment-root", str(treatment_root), "--wendy", str(wendy), "--resume", str(resume), "--execute"]
    if limit:
        argv += ["--limit", str(limit)]
    return argv


def mcp_tool_names(wendy, env):
    import benchmark
    return {t["name"] for t in benchmark.list_all_tools(wendy, dict(env))}


DEVICE_TREE_MODEL_PATHS = ("/proc/device-tree/model", "/sys/firmware/devicetree/base/model")


def device_facts(wendy, env):
    """Best-effort device description for the report header, from `device info --json` (`version` is the
    agent's); a field that can't be read stays None."""
    facts = {"agent_version": None, "os_version": None, "device": None}
    try:
        done = subprocess.run([wendy, "--json", "--device", "local", "device", "info"], env=env,
                              capture_output=True, text=True, timeout=60, stdin=subprocess.DEVNULL)
        data = json.loads(done.stdout)
        if isinstance(data, dict):
            for fact, key in (("agent_version", "version"), ("os_version", "osVersion"), ("device", "deviceType")):
                if data.get(key) not in (None, ""):
                    facts[fact] = str(data[key])
    except (OSError, ValueError, subprocess.SubprocessError):
        pass
    if not facts["device"]:
        for path in DEVICE_TREE_MODEL_PATHS:
            try:
                facts["device"] = Path(path).read_bytes().rstrip(b"\0").decode(errors="replace") or None
                break
            except OSError:
                continue
    return facts


def _write_json(path, value):
    temporary = Path(str(path) + ".tmp")
    temporary.write_text(json.dumps(value, indent=2) + "\n")
    os.replace(temporary, path)


def _point_latest(results, pdir):
    link = Path(results) / "latest"
    temporary = Path(results) / ".latest.tmp"
    temporary.unlink(missing_ok=True)
    os.symlink(str(pdir), temporary)
    os.replace(temporary, link)


def _print_header(pdir):
    try:
        lines = (pdir / "report.md").read_text().splitlines()
    except OSError:
        print(f"No report was written. Look in {pdir}.")
        return
    header = []
    for line in lines:
        if header and line.startswith("## "):
            break
        header.append(line)
    print("\n".join(header[:40]))
    print(f"\nFull report: {pdir / 'report.md'}")


def run_benchmark(argv, cwd, env, tail_lines=40):
    """Run benchmark.py with stderr passed through to the container log and its tail kept. Returns
    (exit code, tail)."""
    process = subprocess.Popen(argv, cwd=cwd, env=env, stderr=subprocess.PIPE, text=True, errors="replace")
    tail = collections.deque(maxlen=tail_lines)
    for line in process.stderr:
        sys.stderr.write(line)
        sys.stderr.flush()
        tail.append(line.rstrip("\n"))
    return process.wait(), "\n".join(tail)


def run_pass(environ):
    """One pass. Returns 0 when it finished, else a nonzero code with the reason in <results>/last-error.txt."""
    results = Path(environ.get("EVAL_RESULTS", "/results"))
    results.mkdir(parents=True, exist_ok=True)
    (results / "last-error.txt").unlink(missing_ok=True)
    server = None

    def fail(message):
        print(f"on-device pass stopped: {message}", file=sys.stderr, flush=True)
        (results / "last-error.txt").write_text(message + "\n")
        return 1

    try:
        model_id = environ.get("EVAL_MODEL", "qwen3.8-flash-next")
        models_dir = Path(environ.get("EVAL_MODELS_DIR", "/models"))
        harness = Path(environ.get("EVAL_HARNESS", HERE))
        treatment = Path(environ.get("EVAL_TREATMENT", "/opt/treatment"))
        wendy = environ.get("EVAL_WENDY", "/usr/local/bin/wendy")
        try:
            pin = load_model(harness / "models.json", model_id)
        except UnknownModel as error:
            return fail(str(error))
        try:
            context = int(environ.get("EVAL_CONTEXT") or pin["context"])
            repetitions = int(environ.get("EVAL_REPETITIONS") or 1)
            limit = int(environ["EVAL_LIMIT"]) if environ.get("EVAL_LIMIT") else None
            if min(context, repetitions) < 1 or (limit is not None and limit < 1):
                raise ValueError
        except ValueError:
            return fail("EVAL_CONTEXT, EVAL_REPETITIONS and EVAL_LIMIT must be positive whole numbers")
        try:
            cli_commit = (treatment / "REVISION").read_text().strip() or "unknown"
        except OSError:
            cli_commit = "unknown"
        label = f"pass{environ.get('EVAL_PASS') or 1}-{cli_commit[:12]}"
        pdir = pass_dir(results, model_id, pin["revision"], label)
        state = json.loads((pdir / "pass.json").read_text()) if (pdir / "pass.json").is_file() else {}
        if state.get("status") == "finished":
            _point_latest(results, pdir)
            print(f"Pass {pdir.name} already finished.")
            _print_header(pdir)
            return 0

        meminfo = Path(environ.get("EVAL_MEMINFO", "/proc/meminfo")).read_text()
        if not memory_ok(pin, meminfo):
            return fail(f"not enough free memory for {model_id}: it needs {total_bytes(pin) / 2 ** 30:.0f} GiB of "
                        "weights plus 12 GiB available (MemAvailable). Stop other apps or choose a smaller EVAL_MODEL.")
        try:
            fetch_model(pin, models_dir)
        except FetchError as error:
            return fail(f"model download failed: {error}")

        argv = server_argv(pin, context, models_dir)
        health_url = f"http://{HOST}:{PORT}/health"
        server = ModelServer(argv, results / "model-server.log", health_url)
        server.start()
        if not server.wait_healthy():
            return fail(f"the model server never became healthy; see {results / 'model-server.log'}")

        env = dict(environ)
        request = capture_first_request(wendy, env)
        unknown = unknown_tools(request, mcp_tool_names(wendy, env), tool_builtins())
        if unknown:
            return fail("wendy chat sent tools that neither Wendy's MCP server nor chat's built-ins explain: "
                        + ", ".join(sorted(unknown)) + ". Update CHAT_BUILTINS or the tool list before running.")
        baseline = measure_baseline(f"http://{HOST}:{PORT}/v1/chat/completions", {**request, "model": model_id})
        tokens = baseline["baseline_prompt_tokens"]
        if not baseline_ok(tokens, context):
            return fail(f"the baseline prompt takes {tokens} tokens, over half of the {context}-token context; "
                        "raise EVAL_CONTEXT or choose another EVAL_MODEL")

        facts = device_facts(wendy, env)
        sent = len(request_tool_names(request))
        pdir.mkdir(parents=True, exist_ok=True)
        import scenarios as sc
        shown = [a for i, a in enumerate(argv) if a != "-m" and (i == 0 or argv[i - 1] != "-m")]
        _write_json(pdir / "on_device.json", {
            "model": model_id, "revision": pin["revision"], "server": shlex.join(shown), "context": context,
            "baseline_prompt_tokens": tokens, "tokens_per_second": baseline["predicted_per_second"],
            "prompt_tokens_per_second": baseline["prompt_per_second"], "cli_commit": cli_commit,
            "agent_version": facts["agent_version"], "os_version": facts["os_version"], "device": facts["device"],
            "app_id": environ.get("WENDY_APP_ID") or sc.BENCHMARK_APP,
            "notes": [f"wendy chat sent {sent} tools on its first request, before wendy_tools was called",
                      f"the baseline prompt is {tokens} tokens of a {context}-token context"]})

        config = json.loads((harness / "config.on-device.json").read_text())
        config["agents"]["chat"]["model"] = model_id
        config_path = results / f"{pdir.name}.config.json"
        _write_json(config_path, config)
        task_ids = [t["id"] for t in json.loads((harness / "tasks.json").read_text())]
        _write_json(pdir / "pass.json", {"status": "running", "model": model_id, "label": label})
        _point_latest(results, pdir)  # readable from the first attempt on, and never another model's report
        returncode, tail = run_benchmark(
            [sys.executable, str(harness / "benchmark.py"),
             *benchmark_argv(config_path, task_ids, repetitions, wendy, treatment, pdir, limit)], harness, env)
        _write_json(pdir / "pass.json", {"status": "finished" if returncode == 0 else "halted",
                                         "exit_code": returncode, "model": model_id, "label": label})
        if returncode != 0 and not (pdir / "halt.json").exists():  # a halt explains itself; a crash must too
            (results / "last-error.txt").write_text(
                f"benchmark.py exited with exit code {returncode} and no halt reason. Last stderr:\n{tail}\n")
        _print_header(pdir)
        return returncode
    except Exception as error:  # a pass must leave a reason, not a traceback in a container that then idles
        return fail(f"{type(error).__name__}: {error}")
    finally:
        if server is not None:
            server.stop()


def tool_builtins():
    import tool_trace
    return tool_trace.CHAT_BUILTINS


if __name__ == "__main__":
    sys.exit(run_pass(os.environ))
