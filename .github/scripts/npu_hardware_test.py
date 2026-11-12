#!/usr/bin/env python3
"""Offline checks of prepared NPU procedures, never NPU execution evidence."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def load_generator():
    spec = importlib.util.spec_from_file_location("npu_rehearsal", ROOT / ".github/regression-coverage/manual/qualcomm-npu-rehearsal.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class NPURehearsalTests(unittest.TestCase):
    def setUp(self):
        self.generator = load_generator()

    def inventory(self, root):
        cli = root / "wendy"
        cli.write_text("#!/bin/sh\nexit 99\n")
        cli.chmod(0o755)
        for name in ("agent", "recovery", "red.png", "blue.png"):
            (root / name).write_text(name)
        info = root / "info.json"
        info.write_text(json.dumps({"deviceType": "dragonwing-iq-9075", "os": "wendyos",
                                    "cpuArchitecture": "arm64", "version": "candidate", "osVersion": "fixture"}))
        path = root / "inventory.json"
        path.write_text(json.dumps({"cli_binary": str(cli), "agent_binary": str(root / "agent"),
                                    "agent_source_commit": "a" * 40, "recovery_record": str(root / "recovery"),
                                    "qualcomm_checkout": str(root), "endpoint": "http://reserved.invalid:9110",
                                    "images": [{"path": str(root / name), "expected": color}
                                               for name, color in (("red.png", "red triangle"), ("blue.png", "blue circle"))],
                                    "board": {"selector": "reserved.invalid", "serial": "fixture-serial",
                                              "owner": "fixture owner", "device_info": str(info)}}))
        return path

    def project(self):
        config = {"appId": "PROD-qwen-vlm", "entitlements": [{"type": "network", "mode": "host"},
                  {"type": "npu"}, {"type": "persist", "name": "prod-qwen-vlm-models", "path": "/data"},
                  {"type": "http", "port": 9110}]}
        return {Path("wendy.json"): json.dumps(config).encode(), Path("Dockerfile"): b"FROM fixture\n",
                Path("model.py"): b"# model fixture\n"}

    def test_bundle_is_isolated_and_runner_records_no_hardware_verdict(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory = self.inventory(root)
            out = root / "bundle"
            with patch.object(self.generator, "pinned_project", return_value=self.project()):
                evidence = self.generator.generate(inventory, out)
            self.assertEqual(len(evidence["cases"]), 7)
            self.assertEqual(evidence["audit_entries"], ["1988", "2547"])
            self.assertEqual(evidence["hardware_execution"], "not_run")
            self.assertTrue(all(c["status"] == "not_run" for c in evidence["cases"]))
            config = json.loads((out / "project/wendy.json").read_text())
            self.assertEqual(config["appId"], evidence["app_id"])
            self.assertNotEqual(config["appId"], "PROD-qwen-vlm")
            self.assertNotIn("prod-qwen-vlm-models", json.dumps(config))
            self.assertEqual(next(e["name"] for e in config["entitlements"] if e["type"] == "persist"), evidence["volume"])
            commands = json.loads((out / "commands.json").read_text())["qwen"]
            for stage in ("deploy", "start", "stop", "remove", "state", "info", "logs"):
                argv = commands[stage]
                self.assertEqual(argv[argv.index("--device") + 1], "reserved.invalid")
                self.assertNotIn("PROD-qwen-vlm", argv)
                self.assertEqual("--force" in argv, stage == "remove")
                self.assertNotIn("--cleanup", argv)
                self.assertNotIn("--delete-volumes", argv)
            self.assertIn("--no-restart", commands["deploy"])
            self.assertIn("--detach", commands["start"])
            subprocess.run(["sh", "-n", str(out / "console-capture.sh")], check=True)
            invalid_pid = subprocess.run(["sh", str(out / "console-capture.sh"), "7;bad", "12"], capture_output=True, timeout=5)
            self.assertEqual(invalid_pid.returncode, 2)
            evidence["dirty_preview"] = False
            (out / "evidence.json").write_text(json.dumps(evidence))
            # The executable is a local sentinel; a real device is never contacted.
            result = subprocess.run([sys.executable, str(out / "run-command.py"), "qwen", "stop", "offline"],
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 99, result.stderr)
            recorded = json.loads((out / "qwen-stop-offline.result.json").read_text())
            self.assertEqual(recorded["argv"], commands["stop"])
            self.assertEqual(recorded["timeout_seconds"], 60)
            self.assertEqual(json.loads((out / "evidence.json").read_text())["hardware_execution"], "not_run")
            (out / "project/model.py").write_text("# changed model fixture\n")
            result = subprocess.run([sys.executable, str(out / "run-command.py"), "qwen", "stop", "changed"],
                                    capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("artifact changed", result.stderr)
            self.assertFalse((out / "qwen-stop-changed.result.json").exists())

    def test_invalid_inventory_does_not_create_a_bundle(self):
        changes = [("endpoint", "http://reserved.invalid:9110/path"),
                   ("endpoint", "http://password@reserved.invalid:9110"),
                   ("endpoint", "https://reserved.invalid:443"),
                   ("agent_source_commit", "main"), ("images", [])]
        for key, value in changes:
            with self.subTest(key=key, value=value), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                inventory = self.inventory(root)
                raw = json.loads(inventory.read_text()); raw[key] = value
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    self.generator.generate(inventory, root / "bundle")
                self.assertFalse((root / "bundle").exists())
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory = self.inventory(root)
            info = root / "info.json"
            raw = json.loads(info.read_text()); raw["deviceType"] = "dragonwing-iq-8275"
            info.write_text(json.dumps(raw))
            with self.assertRaisesRegex(ValueError, "IQ-9075"):
                self.generator.generate(inventory, root / "bundle")

    def test_source_is_read_from_pinned_git_objects_not_dirty_files(self):
        with tempfile.TemporaryDirectory() as directory:
            repo = Path(directory)
            def git(*args):
                return subprocess.check_output(["git", "-C", str(repo), *args], stderr=subprocess.DEVNULL, text=True).strip()
            git("init")
            project = repo / "iq9-qwen"; project.mkdir()
            names = ("Dockerfile", "wendy.json", "server.py", "genie.py", "worker.py", "model.py", "preprocess.py", "index.html")
            for name in names:
                (project / name).write_text("pinned " + name)
            git("add", "iq9-qwen")
            git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
            commit = git("rev-parse", "HEAD")
            (project / "server.py").write_text("uncommitted wrong server")
            with patch.object(self.generator, "COMPANION_COMMIT", commit):
                files = self.generator.pinned_project(repo)
            self.assertEqual(files[Path("server.py")], b"pinned server.py")
            self.assertEqual(set(files), {Path(name) for name in names})
            (project / "bad-link").symlink_to("../outside")
            git("add", "iq9-qwen/bad-link")
            git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "symlink")
            with patch.object(self.generator, "COMPANION_COMMIT", git("rev-parse", "HEAD")):
                with self.assertRaisesRegex(ValueError, "non-regular"):
                    self.generator.pinned_project(repo)


class NPUProbeTests(unittest.TestCase):
    def setUp(self):
        self.probe = {}
        exec(compile(load_generator().PROBE, "npu-probe", "exec"), self.probe)

    def test_readiness_and_answers_fail_closed_without_becoming_hardware_passes(self):
        for stage, body, valid in (("status", {"state": "loading"}, False),
                                   ("status", {"state": "ready"}, True),
                                   ("infer", {"answer": ""}, False),
                                   ("infer", {"answer": "old text", "error": "backend died"}, False),
                                   ("infer", {"answer": "red triangle"}, True)):
            with self.subTest(stage=stage, body=body), tempfile.TemporaryDirectory() as directory:
                image = Path(directory) / "fixture.png"; image.write_bytes(b"non-sensitive fixture")
                class Response:
                    def __enter__(self): return self
                    def __exit__(self, *args): return False
                    def read(self, limit): return json.dumps(body).encode()
                with patch("urllib.request.build_opener") as opener, contextlib.redirect_stdout(io.StringIO()):
                    opener.return_value.open.return_value = Response()
                    argv = ["http://reserved.invalid:9110", stage, str(image)]
                    if valid:
                        self.assertEqual(self.probe["main"](argv), 0)
                    else:
                        with self.assertRaises(RuntimeError): self.probe["main"](argv)
                    request = opener.return_value.open.call_args.args[0]
                    self.assertEqual(request.get_method(), "POST" if stage == "infer" else "GET")
                    if stage == "infer":
                        self.assertEqual(request.data, image.read_bytes())
                        self.assertIn("/ask?q=", request.full_url)

    def test_inference_does_not_follow_redirects_or_swallow_transport_errors(self):
        redirect = self.probe["NoRedirect"]()
        with self.assertRaisesRegex(RuntimeError, "redirected"):
            redirect.redirect_request(None, None, 307, "redirect", {}, "https://elsewhere.invalid")
        with patch("urllib.request.build_opener") as opener:
            opener.return_value.open.side_effect = TimeoutError("backend did not reply")
            with self.assertRaises(TimeoutError):
                self.probe["main"](["http://reserved.invalid:9110", "status"])


if __name__ == "__main__":
    unittest.main()
