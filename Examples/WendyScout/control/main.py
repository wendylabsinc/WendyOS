"""Trusted local supervisor for one Scout app. No model shell or arbitrary RPC/HTTP proxy."""

import datetime, http.client, http.server, json, os, pathlib, re, secrets, signal, socket, socketserver, struct, subprocess, threading, time, urllib.request
from common import (
    APP,
    PINS,
    STATE,
    context,
    releases,
    save,
    scrub,
    sha,
    append_log,
    cached_releases,
    cancel_commands,
)
from probes import Probes
from functional import (
    WORKFLOWS,
    CleanupError,
    confirmed_work,
    require_failed_experiment,
    model_observation,
)
from local import Local
from publisher import Publisher
import status_page
from inference import validate as validate_inference

STOP = threading.Event()
CHILDREN = []


def event(kind, **data):
    row = {
        "time": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "kind": kind,
        **data,
    }
    print(json.dumps(row), flush=True)
    append_log(STATE / "events.jsonl", (json.dumps(row) + "\n").encode())


def shutdown(*_):
    STOP.set()
    cancel_commands()
    for p in CHILDREN:
        try:
            os.killpg(p.pid, signal.SIGTERM)
        except ProcessLookupError:
            pass


signal.signal(signal.SIGTERM, shutdown)
signal.signal(signal.SIGINT, shutdown)


class Scout:
    def __init__(self):
        self.ready = False
        self.sessions = {}
        self.lock = threading.Lock()
        self.model_lock = threading.Lock()
        self.model = False
        self.refreshed = 0
        self.ctx = None
        self.worker_seen = 0

    def initialize(self):
        # Keep restart-abandoned runs bounded too; findings have independent storage.
        import shutil

        for old in sorted((STATE / "runs").glob("*"))[:-20]:
            if old.is_dir():
                shutil.rmtree(old)
        self.local = Local(os.environ.get("WENDY_AGENT_SOCKET", ""))
        self.target = self.local.verify_development()
        self.local.cleanup_scratch()  # only the fixed test-owned ID, including forced-kill leftovers
        cached = cached_releases()
        while not STOP.is_set():
            try:
                self.versions = cached or releases()
                break
            except Exception as e:
                event("release_fetch_backoff", reason=type(e).__name__, seconds=120)
                STOP.wait(120)
        if STOP.is_set():
            raise RuntimeError("Stopped during release discovery")
        self.refreshed = time.monotonic()
        self.probes = Probes(self.versions, self.local.scratch, self.local)
        info = self.probes.run("info", "stable", None)
        if info["exit"] != 0:
            raise RuntimeError("Local admin info preflight failed")
        self.info = info
        save(STATE / "release-pins.json", self.versions)
        if os.environ.get("SCOUT_BOOTSTRAP") != "1":
            self.start_model()
        self.ready = True
        event(
            "ready",
            connection=self.target,
            agent=self.probes.agent,
            versions={k: v["tag"] for k, v in self.versions.items()},
        )

    def start_model(self):
        m = PINS["model"]
        flash = m["id"] == "qwen3.8-flash-next"
        if flash:
            available_kib = int(
                next(
                    l.split()[1]
                    for l in pathlib.Path("/proc/meminfo").read_text().splitlines()
                    if l.startswith("MemAvailable:")
                )
            )
            if available_kib * 1024 < m["bytes"] + 12 * 1024**3:
                raise RuntimeError(
                    "Flash requires weight bytes plus 12 GiB available headroom"
                )
        models = STATE / "models"
        models.mkdir(exist_ok=True)
        # llama-server loads the remaining official shards beside the first.
        files = m.get("files", [m])
        missing = sum(
            f["bytes"]
            for f in files
            if not (models / f["file"]).exists()
            or (models / f["file"]).stat().st_size != f["bytes"]
        )
        if (
            os.statvfs(STATE).f_bavail * os.statvfs(STATE).f_frsize
            < missing + 10 * 1024**3
        ):
            raise RuntimeError("Insufficient persistent storage")
        for f in files:
            path = models / f["file"]
            path.parent.mkdir(parents=True, exist_ok=True)
            started = time.monotonic()
            if (
                not path.exists()
                or path.stat().st_size != f["bytes"]
                or path.with_suffix(".gguf.aria2").exists()
            ):
                url = f"https://huggingface.co/{m['repo']}/resolve/{m['revision']}/{f['file']}"
                event("model_download_start", file=f["file"], bytes=f["bytes"])
                subprocess.run(
                    [
                        "aria2c",
                        "-x8",
                        "-s8",
                        "-c",
                        "--file-allocation=none",
                        "--max-tries=5",
                        "--timeout=30",
                        "--connect-timeout=15",
                        "--summary-interval=30",
                        "-d",
                        str(path.parent),
                        "-o",
                        path.name,
                        url,
                    ],
                    check=True,
                    timeout=1800,
                )
                event(
                    "model_download_complete",
                    file=f["file"],
                    bytes=path.stat().st_size,
                    seconds=time.monotonic() - started,
                )
            if path.stat().st_size != f["bytes"] or sha(path) != f["sha256"]:
                raise RuntimeError("Model size/SHA-256 mismatch: " + f["file"])
            event(
                "model_verified",
                file=f["file"],
                seconds=time.monotonic() - started,
                sha256=f["sha256"],
            )
        path = models / m["file"]
        logpath = STATE / "model.log"
        logpath.write_text("")
        argv = [
            "/usr/lib/ollama/llama-server",
            "--model",
            str(path),
            "--alias",
            m["id"],
            "--host",
            "127.0.0.1",
            "--port",
            "18081",
            "-lv",
            "4",
            "-ngl",
            "999",
            "-c",
            "16384",
            "-np",
            "1",
            "--jinja",
            "--no-webui",
        ]
        if flash:
            # Trusted fixed profile, never a model-supplied backend override.
            argv += [
                "--chat-template-kwargs",
                '{"enable_thinking":false}',
                "--reasoning-budget",
                "0",
                "--top-k",
                "20",
                "--min-p",
                "0",
                "--lazy-mode",
                "off",
                "-b",
                "512",
                "-ub",
                "128",
                "-ot",
                "per_layer_token_embd=CPU",
            ]
        p = subprocess.Popen(
            argv,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        CHILDREN.append(p)
        started = time.monotonic()

        def pump():
            while block := p.stdout.read1(16384):
                append_log(logpath, block)

        threading.Thread(target=pump, daemon=True).start()
        deadline = started + (900 if flash else 120)
        while time.monotonic() < deadline:
            if STOP.is_set() or p.poll() is not None:
                raise RuntimeError("Model stopped during initialization")
            try:
                with urllib.request.urlopen(
                    "http://127.0.0.1:18081/health", timeout=2
                ) as r:
                    health = json.load(r)
                if health.get("status") == "ok":
                    break
            except Exception:
                pass
            STOP.wait(1)
        else:
            raise RuntimeError("Model startup deadline")
        match = re.search(
            r"offloaded (\d+)/(\d+) layers to GPU", (STATE / "model.log").read_text()
        )
        if not match or int(match[1]) == 0:
            raise RuntimeError("No GPU offload proof")
        self.model = True
        event(
            "gpu_ready",
            seconds=time.monotonic() - started,
            layers=int(match[1]),
            total=int(match[2]),
        )

    def begin(self, report):
        if getattr(self, "error", None):
            raise RuntimeError(self.error)
        if not self.ready or not self.model:
            return {
                "status": "waiting",
                "ready": self.ready,
                "bootstrap": os.environ.get("SCOUT_BOOTSTRAP") == "1",
                "versions": self.versions if self.ready else {},
            }
        # This root-supervisor diagnostic is not a security attestation. The
        # Agent enforces network:none outside Pi; the trusted worker installs
        # Landlock/seccomp before each test exec. SO_PEERCRED + mode 0600 keep
        # both model/test UIDs from forging this root-only startup call.
        if report.get("sandbox") != "passed":
            raise ValueError("Trusted supervisor startup diagnostic failed")
        with self.lock:
            previous = (
                list((STATE / "runs").glob("*")) if (STATE / "runs").exists() else []
            )
            if any(not x.get("finished") for x in self.sessions.values()):
                raise ValueError("A session is already active")
            token = secrets.token_hex(16)
            run = (
                STATE
                / "runs"
                / datetime.datetime.now(datetime.timezone.utc).strftime(
                    "%Y%m%dT%H%M%S.%fZ"
                )
            )  # directory is created only after context fetch succeeds
            cache = STATE / "context-cache.json"
            if (
                self.ctx is None
                and cache.exists()
                and time.time() - cache.stat().st_mtime < 1800
            ):
                cached_context = json.loads(cache.read_text())
                if cached_context.get("schema") == 1:
                    self.ctx = cached_context
            if self.ctx is None or time.monotonic() - self.refreshed >= 1800:
                try:
                    fresh = releases() if self.ctx is not None else self.versions
                    ctx = scrub(context())
                    self.versions = fresh
                    self.probes.versions = fresh
                    self.ctx = ctx
                    save(cache, ctx)
                    self.refreshed = time.monotonic()
                    import shutil

                    keep = {v["tag"] for v in fresh.values()}
                    for old in list((STATE / "tools").iterdir()) + list(
                        pathlib.Path("/opt/scout-tools").iterdir()
                    ):
                        if old.is_dir() and old.name not in keep:
                            shutil.rmtree(old)
                except Exception:
                    if self.ctx is None:
                        raise
                    event("context_refresh_backoff", seconds=300)
                    self.refreshed = time.monotonic() - 1500
            self.probes.deadline = None
            if self.probes.run("info", "stable", None)["exit"] != 0:
                raise RuntimeError("Self-device info preflight failed")
            self.probes.deadline = None
            ctx = self.ctx
            manifest = {
                "versions": self.versions,
                "agent": self.probes.agent,
                "target": self.target,
                "model": PINS["model"],
                "pi": PINS["pi"],
                "source_sha256": {
                    "control": __import__("hashlib")
                    .sha256(
                        b"".join(
                            p.read_bytes()
                            for p in sorted(pathlib.Path(__file__).parent.glob("*.py"))
                        )
                    )
                    .hexdigest(),
                    "worker": report.get("source_sha256", ""),
                },
            }
            save(run / "manifest.json", manifest)
            save(run / "context.json", ctx)
            self.sessions[token] = {
                "run": run,
                "calls": 0,
                "started": time.monotonic(),
                "finished": False,
                "model_calls": 0,
                "experiments": [],
            }
            # Only bounded sanitized summaries, never shared writable package/session stores.
            history = []
            for old in sorted(previous)[-2:]:
                if (old / "summary.json").exists():
                    item = json.loads((old / "summary.json").read_text())
                    item["summary"] = (
                        str(item.get("summary", ""))[:600]
                        if item.get("proof") is True
                        else "Historical narration omitted: fresh work proof was absent or unknown"
                    )
                    history.append(item)
            return {
                "status": "started",
                "session": token,
                "context": ctx,
                "previous": {
                    "evidence_kind": "untrusted_historical_model_summaries",
                    "items": history,
                },
                "versions": {
                    k: {
                        x: v[x]
                        for x in [
                            "tag",
                            "digest",
                            "binary_sha256",
                            "published_at",
                            "prerelease",
                        ]
                    }
                    for k, v in self.versions.items()
                },
                "agent": self.probes.agent,
                "workflows": list(WORKFLOWS),
            }

    def tool(self, data):
        if set(data) - {"session", "operation", "version", "kind"}:
            raise ValueError("Unknown tool argument")
        with self.lock:
            s = self.sessions[data["session"]]
            if (
                s["finished"]
                or s["calls"] >= 12
                or time.monotonic() - s["started"] > 300
            ):
                raise ValueError("Session budget exhausted")
            s["calls"] += 1
            self.probes.deadline = min(s["started"] + 300, time.monotonic() + 90)
            operation = data.get("operation")
            v = data.get("version", "past")
            kind = data.get("kind", "info")
            if getattr(self, "error", None):
                raise RuntimeError(self.error)
            if operation == "experiment":
                if kind not in WORKFLOWS:
                    raise ValueError("Unapproved functional workflow")
                try:
                    result = self.probes.run(kind, v, s["run"])
                except CleanupError as e:
                    self.error = str(e)
                    raise
                s["experiments"].append(result)
            elif operation == "reproduce":
                require_failed_experiment(
                    s["experiments"], kind, self.versions["nightly"]["tag"]
                )
                try:
                    result = self.probes.replay(kind, v, "nightly", s["run"])
                except CleanupError as e:
                    self.error = str(e)
                    raise
                save(s["run"] / f"candidate-{s['calls']}.json", result)
                if result.get("verified"):
                    findings = STATE / "findings"
                    findings.mkdir(exist_ok=True)
                    key = Publisher.fingerprint(result)
                    path = findings / (key + ".json")
                    if not path.exists() and len(list(findings.glob("*.json"))) >= 100:
                        raise ValueError(
                            "Retained finding limit reached; export required"
                        )
                    save(
                        path, result
                    )  # durable before any external request or run retention
                    try:
                        result["publication"] = Publisher(
                            STATE,
                            repo=os.environ.get(
                                "SCOUT_ISSUE_REPOSITORY", "wendylabsinc/WendyOS"
                            ),
                        ).publish(result)
                    except FileNotFoundError:
                        result["publication"] = {
                            "status": "pending",
                            "reason": "Scoped issues-only credential file not provisioned",
                        }
                    except Exception as e:
                        result["publication"] = {
                            "status": "held",
                            "reason": type(e).__name__,
                        }
                    save(s["run"] / f"finding-{s['calls']}.json", result)
                    save(path, result)
            else:
                raise ValueError("Unapproved operation")
            return scrub(model_observation(result))

    def finish(self, data):
        s = self.sessions[data["session"]]
        s["finished"] = True
        summary = {
            "exit": data.get("exit"),
            "proof": data.get("proof") is True
            and confirmed_work(s["experiments"], str(data.get("summary", ""))),
            "experiments": [
                {k: r[k] for k in ["id", "kind", "version", "outcome"]}
                for r in s["experiments"]
            ],
            "summary": scrub(str(data.get("summary", ""))[:4000]),
            "evidence_kind": "historical_model_summary",
            "seconds": time.monotonic() - s["started"],
        }
        save(s["run"] / "summary.json", summary)
        self.local.cleanup_scratch()
        event("session_finished", run=s["run"].name, exit=summary["exit"])
        # Keep metadata/findings separately; prune only completed run directories.
        import shutil

        completed = sorted(
            p for p in (STATE / "runs").glob("*") if (p / "summary.json").exists()
        )
        for p in completed[:-20]:
            shutil.rmtree(p)
        self.sessions.pop(data["session"], None)
        return {"status": "finished"}


scout = Scout()


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, *_):
        pass

    def reply(self, data, status=200):
        body = json.dumps(data).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        try:
            if self.path == "/scratch-lease":
                return self.reply(
                    {
                        "alive": not STOP.is_set()
                        and not getattr(scout, "error", None)
                        and time.monotonic() - scout.worker_seen < 4
                    }
                )
            if self.path == "/status":
                scout.worker_seen = time.monotonic()
                return self.reply(
                    {
                        "ready": scout.ready,
                        "model": scout.model,
                        "stopping": STOP.is_set(),
                        "network_namespace": os.readlink("/proc/self/ns/net"),
                    }
                )
            if self.path.startswith("/artifact/"):
                key = self.path.removeprefix("/artifact/")
                if key not in {"past", "stable", "nightly"}:
                    raise ValueError("Unapproved artifact")
                path = pathlib.Path(scout.versions[key]["binary"])
                self.send_response(200)
                self.send_header("Content-Length", str(path.stat().st_size))
                self.end_headers()
                with path.open("rb") as f:
                    while block := f.read(65536):
                        self.wfile.write(block)
                return
            raise ValueError("Unapproved route")
        except Exception as e:
            self.reply({"error": type(e).__name__}, 400)

    def do_POST(self):
        try:
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= 1024 * 1024:
                raise ValueError("Request size limit")
            data = json.loads(self.rfile.read(size))
            if self.path == "/begin":
                return self.reply(scout.begin(data))
            if self.path == "/tool":
                return self.reply(scout.tool(data))
            if self.path == "/finish":
                return self.reply(scout.finish(data))
            if self.path == "/v1/chat/completions":
                if not scout.model or STOP.is_set():
                    raise ValueError("Model unavailable")
                token = self.headers.get("X-Scout-Session", "")
                active = scout.sessions.get(token)
                if (
                    not active
                    or active["finished"]
                    or time.monotonic() - active["started"] > 300
                    or active["model_calls"] >= 24
                ):
                    raise ValueError("No active inference budget")
                try:
                    data = validate_inference(data, PINS["model"]["id"])
                except ValueError as e:
                    event(
                        "inference_refused",
                        reason=str(e),
                        fields=sorted(data) if isinstance(data, dict) else [],
                    )
                    raise
                active["model_calls"] += 1
                if not scout.model_lock.acquire(timeout=2):
                    raise ValueError("Model request already active")
                req = urllib.request.Request(
                    "http://127.0.0.1:18081/v1/chat/completions",
                    data=json.dumps(data).encode(),
                    headers={"Content-Type": "application/json"},
                )
                try:
                    with urllib.request.urlopen(req, timeout=240) as r:
                        self.send_response(200)
                        self.send_header(
                            "Content-Type",
                            r.headers.get("Content-Type", "application/json"),
                        )
                        self.end_headers()
                        total = 0
                        while block := r.read1(16384):
                            total += len(block)
                            if total > 1024 * 1024 or STOP.is_set():
                                raise ValueError("Model response budget")
                            self.wfile.write(block)
                            self.wfile.flush()
                except urllib.error.HTTPError as e:
                    event(
                        "model_request_failed",
                        code=e.code,
                        diagnostic=scrub(e.read(2048).decode(errors="replace")),
                    )
                    raise
                finally:
                    scout.model_lock.release()
                return
            raise ValueError("Unapproved route")
        except Exception as e:
            event("request_refused", route=self.path[:80], reason=scrub(str(e))[:300])
            try:
                self.reply({"error": type(e).__name__, "reason": scrub(str(e))}, 400)
            except BrokenPipeError:
                pass


class Server(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = True

    def verify_request(self, request, address):
        # Only trusted root supervisors can reach control, independently of the
        # filesystem mode. Model/test UIDs cannot forge a preflight or tool call.
        _pid, uid, _gid = struct.unpack(
            "3i", request.getsockopt(socket.SOL_SOCKET, socket.SO_PEERCRED, 12)
        )
        return uid == 0


def main():
    STATE.mkdir(exist_ok=True)
    os.chmod(STATE, 0o700)
    ipc = pathlib.Path("/ipc")
    ipc.mkdir(exist_ok=True)
    os.chmod(ipc, 0o700)
    path = ipc / "control.sock"
    path.unlink(missing_ok=True)
    server = Server(str(path), Handler)
    os.chmod(path, 0o600)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    page = status_page.start(
        "/work",
        lambda: {
            "ready": scout.ready,
            "stopping": STOP.is_set(),
            "worker_age": time.monotonic() - scout.worker_seen,
            "error": getattr(scout, "error", None),
        },
    )
    threading.Thread(target=page.serve_forever, daemon=True).start()
    try:
        scout.initialize()
        while not STOP.wait(1):
            if any(p.poll() is not None for p in CHILDREN):
                raise RuntimeError("GPU model exited")
    except Exception as e:
        scout.error = str(e)
        event("held", reason=str(e))
        # A held supervisor still serves diagnostics, never an unbounded loader.
        for p in CHILDREN:
            if p.poll() is None:
                try:
                    os.killpg(p.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                p.wait(timeout=5)
        while not STOP.wait(1):
            pass
    finally:
        shutdown()
        page.shutdown()
        page.server_close()
        server.shutdown()
        server.server_close()
        if hasattr(scout, "local"):
            try:
                scout.local.cleanup_scratch()
            except Exception:
                pass
        for p in CHILDREN:
            try:
                p.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(p.pid, 9)
                p.wait()


if __name__ == "__main__":
    main()
