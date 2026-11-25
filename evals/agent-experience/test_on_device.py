import contextlib
import io
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

import benchmark as b
import on_device as od
import scenarios as sc
import tool_trace

HERE = Path(__file__).resolve().parent
REAL_MEASURE_BASELINE = od.measure_baseline
FLASH = od.load_model(HERE / "models.json", "qwen3.8-flash-next")
GIB = 2 ** 30
FLASH_BYTES = sum(f["bytes"] for f in FLASH["files"])


def meminfo(available_bytes=None, kb=None):
    kb = kb if kb is not None else -(-available_bytes // 1024)
    return f"MemTotal:  131072000 kB\nMemAvailable:  {kb} kB\n"


class ModelPins(unittest.TestCase):
    def test_load_model_rejects_a_typo_and_names_the_choices(self):
        with self.assertRaises(od.UnknownModel) as caught:
            od.load_model(HERE / "models.json", "qwen3.8-flash")
        message = str(caught.exception)
        self.assertIn("qwen3.8-flash-next", message)
        self.assertIn("qwen3-coder-30b-a3b", message)

    def test_server_argv_flash(self):
        argv = od.server_argv(FLASH, 65536, "/models")
        self.assertEqual(argv[0], "/usr/lib/ollama/llama-server")
        pairs = list(zip(argv, argv[1:]))
        for pair in [("-ngl", "999"), ("-c", "65536"), ("-np", "1"), ("--temp", "0.7"), ("--host", "127.0.0.1"),
                     ("--port", "18081"), ("-m", "/models/" + FLASH["entry_file"])]:
            self.assertIn(pair, pairs)
        for flag in ("--jinja", "--no-webui"):
            self.assertIn(flag, argv)
        self.assertIn(("--chat-template-kwargs", '{"enable_thinking":false}'), pairs)

    def test_server_argv_uses_the_given_context(self):
        argv = od.server_argv(FLASH, 32768, "/m")
        self.assertIn(("-c", "32768"), list(zip(argv, argv[1:])))

    def test_memory_ok_needs_weights_plus_12_gib(self):
        need = FLASH_BYTES + 12 * GIB
        kb = -(-need // 1024)  # the smallest MemAvailable, in kB, that covers it
        self.assertTrue(od.memory_ok(FLASH, meminfo(kb=kb)))
        self.assertFalse(od.memory_ok(FLASH, meminfo(kb=kb - 1)))
        self.assertFalse(od.memory_ok(FLASH, "MemTotal: 1 kB\n"))

    def test_baseline_ok_boundary(self):
        self.assertTrue(od.baseline_ok(32768, 65536))
        self.assertFalse(od.baseline_ok(32769, 65536))

    def test_pass_dir_changes_with_model_revision_and_label(self):
        base = od.pass_dir("/results", "m", "a" * 40, "p1")
        self.assertEqual(base, Path("/results/m-aaaaaaaaaaaa-p1"))
        self.assertNotEqual(base, od.pass_dir("/results", "n", "a" * 40, "p1"))
        self.assertNotEqual(base, od.pass_dir("/results", "m", "b" * 40, "p1"))
        self.assertNotEqual(base, od.pass_dir("/results", "m", "a" * 40, "p2"))


class Fetch(unittest.TestCase):
    PIN = {"repo": "o/r", "revision": "rev", "entry_file": "a.gguf",
           "files": [{"file": "a.gguf", "bytes": 4, "sha256": "aa"}, {"file": "b.gguf", "bytes": 6, "sha256": "bb"}]}

    def setUp(self):
        patch = mock.patch.object(sys, "stderr", io.StringIO())
        patch.start()
        self.addCleanup(patch.stop)

    def test_fetch_skips_complete_files_and_raises_on_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            models = Path(directory)
            calls = []

            def run(argv, **kwargs):
                calls.append(argv)
                name = argv[argv.index("-o") + 1]
                (models / name).write_bytes(b"x" * {"a.gguf": 4, "b.gguf": 6}[name])
                return mock.Mock(returncode=0)

            od.fetch_model(self.PIN, models, run=run)
            self.assertEqual(len(calls), 2)
            self.assertIn("--checksum=sha-256=aa", calls[0])
            self.assertIn("https://huggingface.co/o/r/resolve/rev/a.gguf", calls[0])
            self.assertEqual(calls[0][calls[0].index("-d") + 1], str(models))
            od.fetch_model(self.PIN, models, run=run)  # both are complete and verified
            self.assertEqual(len(calls), 2)
            (models / "b.gguf").write_bytes(b"xx")  # a partial file is fetched again
            od.fetch_model(self.PIN, models, run=run)
            self.assertEqual(len(calls), 3)
            self.assertIn("b.gguf", calls[2])
            (models / "b.gguf").unlink()
            with self.assertRaises(od.FetchError) as caught:
                od.fetch_model(self.PIN, models, run=lambda argv, **kw: mock.Mock(returncode=7), attempts=1)
            self.assertIn("b.gguf", str(caught.exception))

    def test_fetch_retries_after_a_failed_attempt(self):
        with tempfile.TemporaryDirectory() as directory:
            models = Path(directory)
            calls, pauses = [], []

            def run(argv, **kwargs):
                calls.append(argv)
                if len(calls) <= 2:
                    return mock.Mock(returncode=22)
                (models / "a.gguf").write_bytes(b"x" * 4)
                return mock.Mock(returncode=0)

            pin = dict(self.PIN, files=self.PIN["files"][:1])
            od.fetch_model(pin, models, run=run, sleep=pauses.append)
            self.assertEqual(len(calls), 3)
            self.assertEqual(calls[0], calls[1])
            self.assertEqual(calls[1], calls[2])
            self.assertIn("-c", calls[0])
            self.assertEqual(pauses, [30, 30])

    def test_fetch_gives_up_after_the_attempt_limit(self):
        with tempfile.TemporaryDirectory() as directory:
            calls = []
            with self.assertRaises(od.FetchError) as caught:
                od.fetch_model(self.PIN, Path(directory), run=lambda argv, **kw: calls.append(argv) or mock.Mock(returncode=22),
                               attempts=30, sleep=lambda s: None)
            self.assertEqual(len(calls), 30)
            self.assertIn("a.gguf", str(caught.exception))
            self.assertIn("22", str(caught.exception))

    def test_a_checksum_mismatch_stops_at_once_and_says_so(self):
        with tempfile.TemporaryDirectory() as directory:
            calls, pauses = [], []
            with self.assertRaises(od.FetchError) as caught:
                od.fetch_model(self.PIN, Path(directory), run=lambda argv, **kw: calls.append(argv) or mock.Mock(returncode=32),
                               attempts=30, sleep=pauses.append)
            self.assertEqual(len(calls), 1)
            self.assertEqual(pauses, [])
            self.assertIn("checksum", str(caught.exception))
            self.assertIn("a.gguf", str(caught.exception))

    def test_a_file_of_the_right_size_without_a_checksum_pass_is_fetched_again(self):
        with tempfile.TemporaryDirectory() as directory:
            models = Path(directory)
            (models / "a.gguf").write_bytes(b"x" * 4)
            (models / "b.gguf").write_bytes(b"x" * 6)
            calls = []
            od.fetch_model(self.PIN, models, run=lambda argv, **kw: calls.append(argv) or mock.Mock(returncode=0))
            self.assertEqual(len(calls), 2)


class Server(unittest.TestCase):
    def test_model_server_restarts_after_exit(self):
        with tempfile.TemporaryDirectory() as directory:
            server = od.ModelServer([sys.executable, "-c", "pass"], Path(directory) / "model.log",
                                    "http://127.0.0.1:9/health", restart_delay=0.05)
            server.start()
            try:
                deadline = time.monotonic() + 10
                while server.starts < 3 and time.monotonic() < deadline:
                    time.sleep(0.05)
                self.assertGreaterEqual(server.starts, 3)
            finally:
                server.stop()
            count = server.starts
            time.sleep(0.3)
            self.assertEqual(server.starts, count)  # stopped means stopped

    def test_a_garbled_health_reply_is_not_healthy_and_does_not_raise(self):
        import http.client
        server = od.ModelServer(["x"], "/dev/null", "http://127.0.0.1:9/health")
        with mock.patch.object(od.urllib.request, "urlopen", side_effect=http.client.BadStatusLine("garbage")):
            self.assertFalse(server.healthy())
        with mock.patch.object(od.urllib.request, "urlopen", side_effect=http.client.IncompleteRead(b"x")):
            self.assertFalse(server.healthy())

    def test_stop_ends_a_server_restarted_while_it_was_stopping(self):
        server = od.ModelServer(["x"], "/dev/null", "http://127.0.0.1:9/health")
        exited = mock.Mock()
        exited.poll.return_value = 0
        fresh = mock.Mock()
        fresh.poll.return_value = None
        server._process = exited

        class Thread:
            def join(self_, timeout=None):
                server._process = fresh  # the supervisor finished a launch after stop() read the old process

        server._thread = Thread()
        server.stop()
        fresh.terminate.assert_called_once()

    def test_wait_healthy_reads_the_health_endpoint(self):
        from http.server import BaseHTTPRequestHandler, HTTPServer
        import threading
        state = {"ready": False}

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_GET(self):
                body = json.dumps({"status": "ok" if state["ready"] else "loading model"}).encode()
                self.send_response(200 if state["ready"] else 503)
                self.end_headers()
                self.wfile.write(body)

        httpd = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        try:
            with tempfile.TemporaryDirectory() as directory:
                server = od.ModelServer([sys.executable, "-c", "import time; time.sleep(30)"],
                                        Path(directory) / "m.log", f"http://127.0.0.1:{httpd.server_address[1]}/health")
                server.start()
                try:
                    self.assertFalse(server.wait_healthy(timeout=0.5, poll=0.1))
                    state["ready"] = True
                    self.assertTrue(server.wait_healthy(timeout=5, poll=0.1))
                finally:
                    server.stop()
        finally:
            httpd.shutdown()
            httpd.server_close()


FAKE_CHAT = """#!{python}
import json, sys, urllib.request
args = sys.argv[1:]
base = args[args.index("--base-url") + 1]
assert "--provider" in args and "--no-memory" in args
body = {{"model": "fake", "stream": True, "messages": [{{"role": "system", "content": "s"}}, {{"role": "user", "content": "hi"}}],
        "tools": [{{"type": "function", "function": {{"name": n, "parameters": {{}}}}}} for n in ("device_info", "workspace_exec", "made_up")]}}
req = urllib.request.Request(base + "/chat/completions", json.dumps(body).encode(), {{"Content-Type": "application/json"}})
print(urllib.request.urlopen(req).read().decode()[:80])
"""


class Capture(unittest.TestCase):
    def fake_chat(self, directory):
        path = Path(directory) / "wendy"
        path.write_text(FAKE_CHAT.format(python=sys.executable))
        path.chmod(path.stat().st_mode | stat.S_IXUSR)
        return str(path)

    def test_capture_first_request_reads_tools(self):
        with tempfile.TemporaryDirectory() as directory:
            env = {"PATH": os.environ["PATH"], "WENDY_AGENT_SOCKET": "/run/wendy.sock"}
            request = od.capture_first_request(self.fake_chat(directory), env)
        names = [t["function"]["name"] for t in request["tools"]]
        self.assertEqual(names, ["device_info", "workspace_exec", "made_up"])
        self.assertEqual(request["messages"][0]["role"], "system")

    def test_capture_fails_when_chat_never_calls_the_endpoint(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "wendy"
            path.write_text("#!/bin/sh\nexit 0\n")
            path.chmod(0o755)
            with self.assertRaises(RuntimeError):
                od.capture_first_request(str(path), {"PATH": os.environ["PATH"]})

    def test_unknown_tools_lists_what_would_stop_the_run(self):
        request = {"tools": [{"type": "function", "function": {"name": n}}
                             for n in ("device_info", "workspace_exec", "made_up", "also_made_up")]}
        self.assertEqual(od.unknown_tools(request, [{"name": "device_info"}], tool_trace.CHAT_BUILTINS),
                         {"made_up", "also_made_up"})
        self.assertEqual(od.unknown_tools(request, ["device_info", "made_up", "also_made_up"], tool_trace.CHAT_BUILTINS),
                         set())

    def test_chat_builtins_cover_what_chat_sends(self):
        # profile_skill is a chat built-in; if it is missing, runs stop as mcp_unverified.
        self.assertIn("profile_skill", tool_trace.CHAT_BUILTINS)


class DeviceFacts(unittest.TestCase):
    def test_reads_what_device_info_json_emits(self):
        info = {"cliVersion": "dev", "deviceType": "jetson-agx-thor", "osVersion": "WendyOS-0.20.1",
                "version": "0.20.1-agent", "cudaVersion": "13.2", "memTotalBytes": 131857190912}
        with mock.patch.object(od.subprocess, "run", return_value=mock.Mock(stdout=json.dumps(info))) as run:
            facts = od.device_facts("/w", {})
        self.assertEqual(facts, {"agent_version": "0.20.1-agent", "os_version": "WendyOS-0.20.1",
                                 "device": "jetson-agx-thor"})
        self.assertIn("info", run.call_args.args[0])

    def test_unreadable_output_leaves_the_fields_empty(self):
        with mock.patch.object(od.subprocess, "run", return_value=mock.Mock(stdout="not json")), \
                mock.patch.object(od, "DEVICE_TREE_MODEL_PATHS", ()):
            facts = od.device_facts("/w", {})
        self.assertEqual(facts, {"agent_version": None, "os_version": None, "device": None})


class Baseline(unittest.TestCase):
    def serve(self, reply):
        from http.server import BaseHTTPRequestHandler, HTTPServer
        import threading
        seen = []

        class Handler(BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                seen.append(json.loads(self.rfile.read(int(self.headers["Content-Length"]))))
                body = json.dumps(reply).encode()
                self.send_response(200)
                self.end_headers()
                self.wfile.write(body)

        httpd = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=httpd.serve_forever, daemon=True).start()
        self.addCleanup(httpd.server_close)
        self.addCleanup(httpd.shutdown)
        return f"http://127.0.0.1:{httpd.server_address[1]}/v1/chat/completions", seen

    def test_measure_baseline_reads_llama_server_usage_and_timings(self):
        url, seen = self.serve({"usage": {"prompt_tokens": 17000, "completion_tokens": 8},
                                "timings": {"prompt_per_second": 410.5, "predicted_per_second": 21.2}})
        result = od.measure_baseline(url, {"model": "m", "stream": True, "stream_options": {"include_usage": True},
                                           "max_tokens": 4096, "messages": [{"role": "user", "content": "hi"}]})
        self.assertEqual(result, {"baseline_prompt_tokens": 17000, "prompt_per_second": 410.5,
                                  "predicted_per_second": 21.2})
        self.assertIs(seen[0]["stream"], False)
        self.assertNotIn("stream_options", seen[0])
        self.assertLessEqual(seen[0]["max_tokens"], 256)

    def test_measure_baseline_refuses_a_reply_without_usage(self):
        url, _ = self.serve({"choices": []})
        with self.assertRaises(ValueError):
            od.measure_baseline(url, {"messages": []})


class FakeProcess:
    """What subprocess.Popen returns for the benchmark: stderr lines, then an exit code."""

    def __init__(self, returncode=0, stderr=()):
        self.returncode, self.stderr = returncode, iter(stderr)

    def wait(self):
        return self.returncode


class RunPass(unittest.TestCase):
    def setUp(self):
        self.dir = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(self.dir, ignore_errors=True))
        self.results, self.models, self.treatment = self.dir / "results", self.dir / "models", self.dir / "treatment"
        self.treatment.mkdir()
        (self.treatment / "REVISION").write_text("c" * 40 + "\n")
        self.meminfo = self.dir / "meminfo"
        self.meminfo.write_text(meminfo(500 * GIB))
        self.env = {"EVAL_RESULTS": str(self.results), "EVAL_MODELS_DIR": str(self.models),
                    "EVAL_TREATMENT": str(self.treatment), "EVAL_MEMINFO": str(self.meminfo),
                    "EVAL_WENDY": "/bin/true", "WENDY_AGENT_SOCKET": "/run/wendy.sock", "PATH": os.environ["PATH"]}

    def pdir(self, model="qwen3.8-flash-next"):
        pin = od.load_model(HERE / "models.json", model)
        settings = od.settings_fingerprint(pin, pin['context'], self.env, HERE)
        return od.pass_dir(self.results, model, pin["revision"], "pass1-" + "c" * 12 + "-" + settings)

    def last_error(self):
        return (self.results / "last-error.txt").read_text()

    def quiet(self, env):
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            return od.run_pass(env)

    def test_bad_model_leaves_an_error_and_returns_nonzero_without_raising(self):
        self.assertNotEqual(self.quiet({**self.env, "EVAL_MODEL": "qwen3.8-flash"}), 0)
        self.assertIn("qwen3.8-flash", self.last_error())
        self.assertIn("qwen3-coder-30b-a3b", self.last_error())

    def test_too_little_memory_leaves_an_error(self):
        self.meminfo.write_text(meminfo(20 * GIB))
        with mock.patch.object(od, "fetch_model") as fetch:
            self.assertNotEqual(self.quiet(self.env), 0)
        fetch.assert_not_called()
        self.assertIn("memory", self.last_error().lower())

    def test_failed_download_leaves_an_error(self):
        with mock.patch.object(od, "fetch_model", side_effect=od.FetchError("a.gguf: aria2c exited 7")), \
                mock.patch.object(od, "ModelServer") as server:
            self.assertNotEqual(self.quiet(self.env), 0)
        server.assert_not_called()
        self.assertIn("a.gguf", self.last_error())

    def test_a_crash_inside_the_pass_is_an_error_not_an_exception(self):
        with mock.patch.object(od, "fetch_model", side_effect=OSError("disk gone")):
            self.assertNotEqual(self.quiet(self.env), 0)
        self.assertIn("disk gone", self.last_error())

    def healthy_pass(self, exit_code=0, notes_tools=79, baseline=17000, stderr=()):
        """Patch the heavy parts: the download, the server, the capture and the baseline."""
        server = mock.Mock()
        server.wait_healthy.return_value = True
        request = {"model": "m", "tools": [{"type": "function", "function": {"name": f"t{i}"}} for i in range(notes_tools)]}
        patches = [
            mock.patch.object(od, "fetch_model"),
            mock.patch.object(od, "ModelServer", return_value=server),
            mock.patch.object(od, "capture_first_request", return_value=request),
            mock.patch.object(od, "mcp_tool_names", return_value={f"t{i}" for i in range(notes_tools)}),
            mock.patch.object(od, "measure_baseline", return_value={
                "baseline_prompt_tokens": baseline, "prompt_per_second": 400.0, "predicted_per_second": 20.0}),
            mock.patch.object(od, "hardware_identity", return_value={"architecture": "aarch64", "gpu": "Thor"}),
            mock.patch.object(od, "device_facts", return_value={"agent_version": "1.2", "os_version": "0.20", "device": "thor"}),
            mock.patch.object(od.subprocess, "Popen", return_value=FakeProcess(exit_code, stderr)),
        ]
        mocks = [p.start() for p in patches]
        for p in patches:
            self.addCleanup(p.stop)
        return server, mocks[-1]

    def test_finished_pass_does_not_rerun(self):
        server, run = self.healthy_pass()
        pdir = self.pdir()
        pdir.mkdir(parents=True)
        (pdir / "pass.json").write_text(json.dumps({"status": "finished"}))
        self.assertEqual(self.quiet(self.env), 0)
        run.assert_not_called()
        server.start.assert_not_called()
        self.assertEqual(os.readlink(self.results / "latest"), str(pdir))

    def test_halted_pass_resumes(self):
        server, run = self.healthy_pass()
        pdir = self.pdir()
        pdir.mkdir(parents=True)
        (pdir / "pass.json").write_text(json.dumps({"status": "halted"}))
        self.assertEqual(self.quiet(self.env), 0)
        argv = run.call_args.args[0]
        self.assertEqual(argv[argv.index("--resume") + 1], str(pdir))
        self.assertIn("--execute", argv)
        self.assertEqual(json.loads((pdir / "pass.json").read_text())["status"], "finished")
        server.stop.assert_called()

    def test_context_change_starts_a_new_pass(self):
        _, run = self.healthy_pass()
        old = self.pdir()
        old.mkdir(parents=True)
        (old / "pass.json").write_text(json.dumps({"status": "finished"}))
        self.assertEqual(self.quiet({**self.env, "EVAL_CONTEXT": "131072"}), 0)
        run.assert_called()
        self.assertNotEqual(Path(os.readlink(self.results / "latest")), old)

    def test_exit_code_decides_finished_or_halted(self):
        for code, status in ((0, "finished"), (3, "halted"), (1, "halted")):
            with self.subTest(code=code):
                shutil.rmtree(self.results, ignore_errors=True)
                self.healthy_pass(exit_code=code)
                self.quiet(self.env)
                self.assertEqual(json.loads((self.pdir() / "pass.json").read_text())["status"], status)
                mock.patch.stopall()

    def test_manifest_records_what_the_report_reads(self):
        self.healthy_pass()
        self.quiet(self.env)
        info = json.loads((self.pdir() / "on_device.json").read_text())
        self.assertEqual(info["model"], "qwen3.8-flash-next")
        self.assertEqual(info["revision"], FLASH["revision"])
        self.assertEqual(info["context"], 65536)
        self.assertEqual(info["baseline_prompt_tokens"], 17000)
        self.assertEqual(info["tokens_per_second"], 20.0)
        self.assertEqual(info["cli_commit"], "c" * 40)
        self.assertEqual(info["app_id"], sc.BENCHMARK_APP)
        self.assertEqual(info["benchmark_paths"], {"files": [str(od.HERE), str(self.results)], "real_cli": "/bin/true"})
        self.assertEqual(info["os_version"], "0.20")
        self.assertIn("-c 65536", info["server"])
        self.assertIn("wendy chat sent 79 tools on its first request, before wendy_tools was called", info["notes"])
        self.assertTrue(any("17000" in note for note in info["notes"]))

    def test_the_per_pass_config_names_the_model(self):
        _, run = self.healthy_pass()
        self.quiet({**self.env, "EVAL_MODEL": "qwen3-coder-30b-a3b"})
        argv = run.call_args.args[0]
        config = json.loads(Path(argv[argv.index("--config") + 1]).read_text())
        self.assertEqual(config["agents"]["chat"]["model"], "qwen3-coder-30b-a3b")

    def test_unknown_tools_and_a_big_baseline_stop_the_pass(self):
        server, run = self.healthy_pass()
        with mock.patch.object(od, "mcp_tool_names", return_value={"t0"}):
            self.assertNotEqual(self.quiet(self.env), 0)
        self.assertIn("t1", self.last_error())
        run.assert_not_called()
        server.stop.assert_called()
        mock.patch.stopall()
        server, run = self.healthy_pass(baseline=40000)
        self.assertNotEqual(self.quiet(self.env), 0)
        self.assertIn("40000", self.last_error())
        run.assert_not_called()

    def test_an_unhealthy_server_stops_the_pass(self):
        server, run = self.healthy_pass()
        server.wait_healthy.return_value = False
        self.assertNotEqual(self.quiet(self.env), 0)
        self.assertIn("healthy", self.last_error())
        run.assert_not_called()
        server.stop.assert_called()

    def test_latest_points_at_the_pass_while_the_benchmark_runs(self):
        seen = []

        def popen(argv, **kwargs):
            seen.append(os.path.realpath(self.results / "latest"))
            return FakeProcess(0)

        _, run = self.healthy_pass()
        run.side_effect = popen
        self.quiet(self.env)
        self.assertEqual(seen, [os.path.realpath(self.pdir())])

    def test_latest_already_points_at_the_pass_during_the_model_fetch(self):
        seen = []
        server, _ = self.healthy_pass()
        od.fetch_model.side_effect = lambda *a, **k: seen.append(os.path.realpath(self.results / "latest"))
        previous = self.results / "old-pass"
        previous.mkdir(parents=True)
        od._point_latest(self.results, previous)
        self.quiet(self.env)
        self.assertEqual(seen, [os.path.realpath(self.pdir())])

    def test_a_benchmark_crash_leaves_its_reason_and_still_reaches_the_container_log(self):
        self.healthy_pass(exit_code=2, stderr=["usage: benchmark.py\n", "benchmark.py: error: bad flag\n"])
        err = io.StringIO()
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err):
            self.assertEqual(od.run_pass(self.env), 2)
        text = self.last_error()
        self.assertIn("exit code 2", text)
        self.assertIn("error: bad flag", text)
        self.assertIn("error: bad flag", err.getvalue())

    def test_a_halt_keeps_its_own_reason_in_halt_json(self):
        _, run = self.healthy_pass(exit_code=3, stderr=["noise\n"])
        pdir = self.pdir()
        run.side_effect = lambda argv, **kw: (pdir.joinpath("halt.json").write_text('{"reason": "r"}'), FakeProcess(3, ["noise\n"]))[1]
        self.quiet(self.env)
        self.assertFalse((self.results / "last-error.txt").exists())

    def http_error(self, code, body):
        import urllib.error
        return urllib.error.HTTPError("http://x/v1/chat/completions", code, "err", {}, io.BytesIO(body.encode()))

    def test_a_baseline_that_overflows_the_context_says_so(self):
        server, run = self.healthy_pass()
        body = json.dumps({"error": {"code": 400, "type": "exceed_context_size_error", "n_prompt_tokens": 19166, "n_ctx": 8192,
                                     "message": "request (19166 tokens) exceeds the available context size (8192 tokens), try increasing it"}})
        with mock.patch.object(od, "measure_baseline", REAL_MEASURE_BASELINE), \
                mock.patch.object(od.urllib.request, "urlopen", side_effect=self.http_error(400, body)):
            self.assertNotEqual(self.quiet({**self.env, "EVAL_CONTEXT": "16384"}), 0)
        text = self.last_error()
        for needle in ("EVAL_CONTEXT", "19166", "8192", "baseline"):
            self.assertIn(needle, text)
        self.assertNotIn("Bad Request", text)
        run.assert_not_called()
        server.stop.assert_called()

    def test_another_http_error_in_the_baseline_shows_status_and_body(self):
        server, run = self.healthy_pass()
        with mock.patch.object(od, "measure_baseline", REAL_MEASURE_BASELINE), \
                mock.patch.object(od.urllib.request, "urlopen", side_effect=self.http_error(500, "boom " * 100)):
            self.assertNotEqual(self.quiet(self.env), 0)
        text = self.last_error()
        self.assertIn("500", text)
        self.assertIn("boom boom", text)
        self.assertLess(len(text), 600)
        run.assert_not_called()
        server.stop.assert_called()

    def test_eval_limit_and_repetitions_reach_the_benchmark(self):
        _, run = self.healthy_pass()
        self.quiet({**self.env, "EVAL_LIMIT": "5", "EVAL_REPETITIONS": "2"})
        argv = run.call_args.args[0]
        self.assertEqual(argv[argv.index("--limit") + 1], "5")
        self.assertEqual(argv[argv.index("--repetitions") + 1], "2")

    def test_context_override_changes_the_server_and_the_manifest(self):
        server, _ = self.healthy_pass()
        self.quiet({**self.env, "EVAL_CONTEXT": "32768"})
        self.assertIn(("-c", "32768"), list(zip(od.ModelServer.call_args.args[0], od.ModelServer.call_args.args[0][1:])))


class AppManifest(unittest.TestCase):
    def test_wendy_json_declares_the_entitlements_and_namespaced_volumes(self):
        app = json.loads((HERE / "wendy.json").read_text())
        self.assertEqual(app["appId"], sc.BENCHMARK_APP)
        types = {e["type"] for e in app["entitlements"]}
        self.assertLessEqual({"gpu", "admin", "build"}, types)
        persist = {e["name"]: e["path"] for e in app["entitlements"] if e["type"] == "persist"}
        self.assertEqual(persist, {"model-tool-report-models": "/models", "model-tool-report-results": "/results",
                                   "model-tool-report-buildkit": "/var/lib/buildkit"})
        self.assertTrue(all(name.startswith("model-tool-report-") for name in persist))


class PlanOnly(unittest.TestCase):
    def argv(self):
        tasks = [t["id"] for t in json.loads((HERE / "tasks.json").read_text())]
        argv = od.benchmark_argv(HERE / "config.on-device.json", tasks, repetitions=1, wendy="/usr/local/bin/wendy",
                                 treatment_root="/opt/treatment", resume=Path("/results/x"), limit=None)
        self.assertIn("--execute", argv)
        argv = [a for a in argv if a != "--execute"]
        i = argv.index("--resume")
        return argv[:i] + argv[i + 2:], tasks

    def test_benchmark_argv_names_every_task_and_the_local_chat(self):
        tasks = [t["id"] for t in json.loads((HERE / "tasks.json").read_text())]
        argv = od.benchmark_argv("c.json", tasks, repetitions=3, wendy="/w", treatment_root="/t",
                                 resume=Path("/r"), limit=7)
        self.assertEqual(argv[argv.index("--scenario") + 1], "all")
        self.assertEqual([argv[i + 1] for i, a in enumerate(argv) if a == "--task"], tasks)
        for flag, value in (("--target", "local"), ("--agent", "chat"), ("--repetitions", "3"),
                            ("--treatment-root", "/t"), ("--wendy", "/w"), ("--resume", "/r"), ("--limit", "7")):
            self.assertEqual(argv[argv.index(flag) + 1], value)
        self.assertNotIn("--limit", od.benchmark_argv("c", tasks, 1, "/w", "/t", Path("/r"), limit=None))

    def test_on_device_plan_only(self):
        argv, tasks = self.argv()
        out = io.StringIO()
        with contextlib.redirect_stdout(out), mock.patch.dict(os.environ, {}, clear=False):
            os.environ.pop("WENDY_AGENT_SOCKET", None)
            self.assertEqual(b.main(argv), 0)
        text = out.getvalue()
        decoder, position, values = json.JSONDecoder(), 0, []
        while True:
            start = text.find("{", position)
            starts = [i for i in (start, text.find("[", position)) if i >= 0]
            if not starts:
                break
            value, position = decoder.raw_decode(text, min(starts))
            values.append(value)
        coverage = next(v for v in values if isinstance(v, dict) and "coverage" in v)["coverage"]
        plan = next(v for v in values if isinstance(v, list))
        self.assertNotIn("ota-update", {p["task"] for p in plan})
        self.assertNotIn("install-physical", {p["task"] for p in plan})
        for task in ("ota-update", "install-physical"):
            rows = [c for c in coverage if c["task"] == task]
            self.assertTrue(rows)
            self.assertTrue(all(c["status"] == "unconfigured" and c["reason"] for c in rows))
        self.assertTrue(plan)

    def test_execute_without_a_health_url_is_refused(self):
        config = json.loads((HERE / "config.on-device.json").read_text())
        del config["model_health_url"]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.json"
            path.write_text(json.dumps(config))
            argv = od.benchmark_argv(path, ["deploy-http"], 1, "/bin/true", "/tmp", Path(directory) / "pass", None)
            err = io.StringIO()
            with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock"}), \
                    contextlib.redirect_stderr(err), contextlib.redirect_stdout(io.StringIO()), self.assertRaises(SystemExit) as caught:
                b.main(argv)
        self.assertEqual(caught.exception.code, 2)
        self.assertIn("model_health_url", err.getvalue())


class InitScript(unittest.TestCase):
    def test_the_log_line_reports_the_real_exit_code_despite_set_e(self):
        text = (HERE / "on-device-init.sh").read_text()
        start = text.index("(\n  cd /opt/harness")
        block = text[start:text.index("\n) &", start) + 2].replace("cd /opt/harness", "cd /").replace(
            "python3 on_device.py", "sh -c 'exit 3'")
        done = subprocess.run(["sh", "-ec", block], capture_output=True, text=True, timeout=30)
        self.assertIn("pass exited with code 3", done.stderr)


if __name__ == "__main__":
    unittest.main()

