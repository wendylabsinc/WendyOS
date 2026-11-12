#!/usr/bin/env python3
"""Offline preparation/capture checks. No serial devices or native tools run."""

import contextlib
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import io
import json
import os
from pathlib import Path
import runpy
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

PATH = Path(__file__).resolve().parents[1] / "regression-coverage/manual/so101-reader-acceptance.py"
SPEC = importlib.util.spec_from_file_location("so101_acceptance", PATH)
GENERATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GENERATOR)


def git_output(command, **_):
    if command == ["git", "rev-parse", "HEAD"]:
        return "a" * 40 + "\n"
    if command == ["git", "status", "--porcelain"]:
        return ""
    raise AssertionError(f"unexpected command during offline preparation: {command}")


class AcceptanceTests(unittest.TestCase):
    def inventory(self, directory, platform="macos"):
        capture = directory / "native-usb-inventory.txt"
        capture.write_text("SYNTHETIC INVENTORY FOR OFFLINE TEST; NO HARDWARE OBSERVED\n")
        raw = {"platform": platform, "hostname": "reserved-fixture-host", "os_version": "fixture-os", "architecture": "arm64",
               "python_version": "3.12.8", "lsof_version": "fixture-lsof", "captured_at": "2026-11-12T12:00:00Z",
               "owner": "fixture owner", "inventory_capture": str(capture),
               "adapter": {"port": "/dev/cu.fixture" if platform == "macos" else "/dev/serial/by-id/fixture-usb",
                           "vendor_id": "1a86", "product_id": "55d4", "serial_number": "SYNTHETIC-SERIAL",
                           "usb_location": "fixture-port-1", "physical_label": "synthetic fixture"},
               "motors": [{"id": i, "name": f"joint-{i}", "expected_model_number": 777} for i in range(1, 7)]}
        path = directory / "inventory.json"
        path.write_text(json.dumps(raw))
        return path, raw

    def generate(self, path, output):
        with patch.object(GENERATOR.subprocess, "check_output", side_effect=git_output), patch.object(GENERATOR.subprocess, "Popen", side_effect=AssertionError("device process during generation")):
            return GENERATOR.generate(path, output)

    def test_both_native_plans_hash_provenance_and_start_not_run(self):
        for platform in ("macos", "linux"):
            with self.subTest(platform=platform), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                inventory, raw = self.inventory(root, platform)
                output = root / "plan"
                plan = self.generate(inventory, output)
                self.assertEqual(plan["hardware_execution"], "not_run")
                self.assertTrue(all(case["status"] == "not_run" for case in plan["cases"]))
                self.assertEqual(plan["cleanup"]["status"], "not_run")
                self.assertFalse(plan["robot_identity_proven"])
                for item in [*plan["reader_source"], plan["inventory_artifact"], plan["inventory_capture"], plan["reader_config"], plan["historical_validation"]]:
                    self.assertEqual(item["sha256"], hashlib.sha256(Path(item["path"]).read_bytes()).hexdigest())
                config = json.loads((output / "reader-config.json").read_text())
                self.assertEqual((config["mode"], config["port"], config["baud"]), ("physical", raw["adapter"]["port"], 1_000_000))
                self.assertEqual(config["ids"], list(range(1, 7)))
                commands = json.loads((output / "commands.json").read_text())
                for command in commands.values():
                    self.assertEqual(command[command.index("--selected-port") + 1], raw["adapter"]["port"])
                compile((output / "capture-reader.py").read_text(), "capture-reader.py", "exec")
                procedure = (output / "procedure.txt").read_text()
                self.assertIn("ALL CASES ARE NOT RUN", procedure)
                self.assertIn("historical physical macOS acceptance", procedure)
                self.assertIn("returncode=0, forced_kill=false", procedure)

    def test_unsafe_or_ambiguous_inventory_is_rejected(self):
        changes = [lambda r: r.update(platform="windows"), lambda r: r.update(python_version="3.9.6"),
                   lambda r: r["adapter"].update(port="/dev/cu.*"), lambda r: r["adapter"].update(port="/dev/cu.foo/../bar"),
                   lambda r: r["adapter"].update(vendor_id="1A86"), lambda r: r["motors"][1].update(id=1),
                   lambda r: r["motors"][0].update(id=True), lambda r: r.update(http_port=80),
                   lambda r: r.update(captured_at="2026-11-12T12:00:00")]
        for change in changes:
            with self.subTest(change=change), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                inventory, raw = self.inventory(root)
                change(raw)
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    self.generate(inventory, root / "plan")
                self.assertFalse((root / "plan").exists())

    def run_capture(self, output, raw, process_factory):
        argv = [str(output / "capture-reader.py"), "--case", "native-baseline", "--selected-port", raw["adapter"]["port"], "--seconds", "10"]
        with patch.object(sys, "argv", argv), patch.object(sys, "version_info", (3, 12, 8)), \
                patch("platform.system", return_value="Darwin"), patch("platform.node", return_value=raw["hostname"]), \
                patch("subprocess.check_output", side_effect=git_output), patch("subprocess.Popen", side_effect=process_factory), \
                patch("subprocess.run", return_value=subprocess.CompletedProcess(["lsof"], 1, "", "")), \
                contextlib.redirect_stdout(io.StringIO()):
            runpy.run_path(str(output / "capture-reader.py"), run_name="__main__")

    def test_changed_config_and_inventory_refuse_before_process_start(self):
        for field in ("reader_config", "inventory_artifact", "inventory_capture"):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                inventory, raw = self.inventory(root)
                output = root / "plan"
                plan = self.generate(inventory, output)
                Path(plan[field]["path"]).write_text("changed after preparation")
                stderr = io.StringIO()
                with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as error:
                    self.run_capture(output, raw, lambda *a, **k: self.fail("reader started despite changed provenance"))
                self.assertEqual(error.exception.code, 2)
                self.assertIn("differs from the prepared plan", stderr.getvalue())

    def test_capture_http_bounds_slow_bodies_and_refuses_redirects_proxies_and_oversized_bodies(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            inventory, raw = self.inventory(root)
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                raw["http_port"] = reservation.getsockname()[1]
            inventory.write_text(json.dumps(raw))
            output = root / "plan"
            self.generate(inventory, output)
            paths = []

            class Handler(BaseHTTPRequestHandler):
                def do_GET(self):
                    paths.append(self.path)
                    self.send_response(307 if self.path == "/readyz" else 200)
                    if self.path == "/readyz":
                        self.send_header("Location", "/must-not-follow")
                    self.end_headers()
                    try:
                        if self.path == "/healthz":
                            # Every byte arrives sooner than the socket timeout;
                            # only a total wall deadline can bound this response.
                            for _ in range(50):
                                self.wfile.write(b" ")
                                self.wfile.flush()
                                time.sleep(0.1)
                        else:
                            self.wfile.write(b"x" * 1_048_577 if self.path == "/v1/observations" else b"{}")
                    except (BrokenPipeError, ConnectionResetError):
                        pass

                def log_message(self, *args):
                    pass

            class FakeProcess:
                pid = 424242  # Synthetic, never used in an OS signal call.
                polls = 0

                def poll(self):
                    self.polls += 1
                    return None if self.polls == 1 else 0

                def wait(self, timeout):
                    return 0

            servers = []

            def start_fake_reader(command, **kwargs):
                self.assertEqual(Path(command[-1]).name, "run.sh")
                self.assertEqual(Path(kwargs["env"]["SO101_CONFIG"]), (output / "reader-config.json").resolve())
                server = ThreadingHTTPServer(("127.0.0.1", raw["http_port"]), Handler)
                servers.append(server)
                threading.Thread(target=server.serve_forever, daemon=True).start()
                return FakeProcess()

            try:
                started = time.monotonic()
                with patch.dict(os.environ, {"HTTP_PROXY": "http://127.0.0.1:1", "http_proxy": "http://127.0.0.1:1", "NO_PROXY": "", "no_proxy": ""}):
                    self.run_capture(output, raw, start_fake_reader)
                self.assertLess(time.monotonic() - started, 4)
            finally:
                for server in servers:
                    server.shutdown()
                    server.server_close()
            self.assertEqual(paths, ["/healthz", "/readyz", "/v1/observations"])
            captures = list((output / "captures").iterdir())
            self.assertEqual(len(captures), 1)
            samples = [json.loads(line) for line in (captures[0] / "observations.jsonl").read_text().splitlines()]
            self.assertIn("2 second wall deadline", samples[0]["error"])
            self.assertEqual(samples[1]["status"], 307)
            self.assertIn("exceeded 1 MiB", samples[2]["error"])
            cleanup = json.loads((captures[0] / "cleanup.json").read_text())
            self.assertEqual(cleanup["status"], "needs_human_review")
            self.assertEqual(cleanup["returncode"], 0)
            self.assertFalse(cleanup["forced_kill"])
            self.assertEqual(json.loads((output / "evidence.json").read_text())["hardware_execution"], "not_run")

    def test_occupied_port_refuses_before_reader_start(self):
        with tempfile.TemporaryDirectory() as temporary, socket.socket() as occupied:
            root = Path(temporary)
            inventory, raw = self.inventory(root)
            occupied.bind(("127.0.0.1", 0))
            occupied.listen()
            raw["http_port"] = occupied.getsockname()[1]
            inventory.write_text(json.dumps(raw))
            output = root / "plan"
            self.generate(inventory, output)
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as error:
                self.run_capture(output, raw, lambda *a, **k: self.fail("reader started on occupied HTTP port"))
            self.assertEqual(error.exception.code, 2)
            self.assertIn("HTTP port is occupied", stderr.getvalue())
            self.assertFalse((output / "captures").exists())

    def test_changed_evidence_inventory_refuses_before_reader_start(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            inventory, raw = self.inventory(root)
            output = root / "plan"
            plan = self.generate(inventory, output)
            plan["inventory"]["motors"][0]["id"] = 42
            (output / "evidence.json").write_text(json.dumps(plan))
            stderr = io.StringIO()
            with contextlib.redirect_stderr(stderr), self.assertRaises(SystemExit) as error:
                self.run_capture(output, raw, lambda *a, **k: self.fail("reader started despite changed evidence"))
            self.assertEqual(error.exception.code, 2)
            self.assertIn("evidence inventory differs", stderr.getvalue())

    def test_child_failure_remains_unaccepted_and_retains_exit_code(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            inventory, raw = self.inventory(root)
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                raw["http_port"] = reservation.getsockname()[1]
            inventory.write_text(json.dumps(raw))
            output = root / "plan"
            self.generate(inventory, output)

            class FailedProcess:
                pid = 424242

                def poll(self):
                    return 1

                def wait(self, timeout):
                    return 1

            self.run_capture(output, raw, lambda *a, **k: FailedProcess())
            capture, = (output / "captures").iterdir()
            cleanup = json.loads((capture / "cleanup.json").read_text())
            self.assertEqual(cleanup["returncode"], 1)
            self.assertEqual(cleanup["status"], "needs_human_review")
            self.assertEqual((capture / "observations.jsonl").read_text(), "")
            evidence = json.loads((output / "evidence.json").read_text())
            self.assertEqual(evidence["hardware_execution"], "not_run")
            self.assertTrue(all(case["status"] == "not_run" for case in evidence["cases"]))

    def test_helper_sigterm_still_cleans_up_only_its_child(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            inventory, raw = self.inventory(root)
            with socket.socket() as reservation:
                reservation.bind(("127.0.0.1", 0))
                raw["http_port"] = reservation.getsockname()[1]
            inventory.write_text(json.dumps(raw))
            output = root / "plan"
            self.generate(inventory, output)
            signals = []

            class RunningProcess:
                pid = 424242

                def poll(self):
                    return None

                def send_signal(self, signum):
                    signals.append(signum)

                def wait(self, timeout):
                    return 0

            def start_and_interrupt(*args, **kwargs):
                # Invoke the installed helper handler, never signal an OS PID.
                signal.getsignal(signal.SIGTERM)(signal.SIGTERM, None)
                return RunningProcess()

            previous = signal.getsignal(signal.SIGTERM)
            self.run_capture(output, raw, start_and_interrupt)
            self.assertEqual(signal.getsignal(signal.SIGTERM), previous)
            self.assertEqual(signals, [signal.SIGTERM])
            capture, = (output / "captures").iterdir()
            self.assertEqual(json.loads((capture / "cleanup.json").read_text())["returncode"], 0)


if __name__ == "__main__":
    unittest.main()
