#!/usr/bin/env python3
"""Offline validation of the physical TensorRT rehearsal; no GPU/device access."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]


class TensorRTRestartRehearsalTests(unittest.TestCase):
    def setUp(self):
        path = ROOT / ".github/regression-coverage/manual/jetson-tensorrt-restart.py"
        spec = importlib.util.spec_from_file_location("trt_restart", path)
        self.generator = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.generator)

    def inventory(self, root):
        files = {}
        for name in ("cli_binary", "agent_binary", "model_onnx", "recovery_record"):
            file = root / name
            file.write_text("#!/bin/sh\nexit 99\n" if name == "cli_binary" else name + " fixture\n")
            if name == "cli_binary":
                file.chmod(0o755)
            files[name] = str(file)
        info = root / "device-info.json"
        info.write_text(json.dumps({"deviceType": "jetson-orin-nano", "gpuArch": "sm_87",
                                    "gpuVendor": "nvidia", "cpuArchitecture": "arm64", "os": "wendyos",
                                    "jetpackVersion": "7.2", "cudaVersion": "13.2",
                                    "version": "candidate", "osVersion": "fixture-os"}))
        path = root / "inventory.json"
        path.write_text(json.dumps(dict(files, agent_source_commit="a" * 40,
                                      camera_source="camera:usb-fixture", inference_root="/data/fixture/inference",
                                      board={"selector": "fixture.invalid:50051", "serial": "fixture-serial",
                                             "owner": "fixture operator", "device_info": str(info)})))
        return path

    def test_prepares_four_cases_and_only_explicit_bounded_cli_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            out = root / "bundle"
            evidence = self.generator.generate(self.inventory(root), out)
            self.assertEqual(evidence["hardware_execution"], "not_run")
            self.assertEqual([c["id"] for c in evidence["cases"]],
                             ["baseline", "agent-restart", "online-reboot", "offline-reboot"])
            self.assertTrue(all(c["status"] == "not_run" for c in evidence["cases"]))
            commands = json.loads((out / "commands.json").read_text())
            for group in commands.values():
                for command in group.values():
                    self.assertEqual(command[command.index("--device") + 1], "fixture.invalid:50051")
                    self.assertNotIn("--force", command)
                    self.assertNotIn("reboot", command)
                    self.assertNotIn("shell", command)
            self.assertIn("--skip-cloud-registration", commands["campaign"]["deploy"])
            campaign = (out / "campaign.yaml").read_text()
            self.assertIn("accelerator: auto", campaign)
            self.assertIn(self.generator.MODEL_REVISION, campaign)
            self.assertIn(evidence["campaign"], campaign)
            self.assertNotIn("notify:", campaign)
            self.assertIn("when: manual", campaign)
            subprocess.run(["sh", "-n", str(out / "console-capture.sh")], check=True)
            for script in ("run-command.py", "capture-cache.py"):
                compile((out / script).read_text(), script, "exec")
            # The fake executable fails if the generator accidentally invokes it.
            # Here invoke the bounded runner intentionally, after enabling the fixture preview.
            evidence["dirty_preview"] = False
            (out / "evidence.json").write_text(json.dumps(evidence))
            run = subprocess.run([sys.executable, str(out / "run-command.py"), "campaign", "inspect", "fixture"],
                                 capture_output=True, text=True, timeout=5)
            self.assertEqual(run.returncode, 99, run.stderr)
            result = json.loads((out / "campaign-inspect-fixture.result.json").read_text())
            self.assertEqual(result["argv"], commands["campaign"]["inspect"])
            self.assertEqual(result["timeout_seconds"], 30)
            self.assertTrue(all(c["status"] == "not_run" for c in evidence["cases"]))

    def test_changed_model_is_refused_before_running_any_command(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            out = root / "bundle"
            evidence = self.generator.generate(self.inventory(root), out)
            evidence["dirty_preview"] = False
            (out / "evidence.json").write_text(json.dumps(evidence))
            (root / "model_onnx").write_text("different model")
            run = subprocess.run([sys.executable, str(out / "run-command.py"), "board", "info", "changed"],
                                 capture_output=True, text=True, timeout=5)
            self.assertNotEqual(run.returncode, 0)
            self.assertIn("artifact changed", run.stderr)
            self.assertFalse((out / "board-info-changed.result.json").exists())

    def test_rejects_wrong_board_wildcard_camera_or_relative_cache_directory(self):
        for field, value in (("camera_source", "*"), ("inference_root", "../inference"),
                             ("agent_source_commit", "main"), ("deviceType", "jetson-agx-thor")):
            with self.subTest(field=field), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                inventory = self.inventory(root)
                data = json.loads(inventory.read_text())
                if field == "deviceType":
                    path = Path(data["board"]["device_info"])
                    info = json.loads(path.read_text()); info[field] = value
                    path.write_text(json.dumps(info))
                else:
                    data[field] = value
                    inventory.write_text(json.dumps(data))
                with self.assertRaises(ValueError):
                    self.generator.generate(inventory, root / "bundle")
                self.assertFalse((root / "bundle").exists())

    def test_cache_capture_verifies_bytes_key_and_uniqueness_without_modification(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            capture = root / "capture.py"
            capture.write_text(self.generator.CACHE_CAPTURE)
            fields = {"model_sha256": "a" * 64, "cuda": "13.2", "gpu_arch": "sm_87"}
            key = hashlib.sha256(json.dumps(fields, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
            engine = root / "engines" / key[:32]
            engine.mkdir(parents=True)
            plan = engine / "engine.plan"
            plan.write_bytes(b"fixture cached plan")
            record = {"key": key, "fields": fields, "plan_bytes": plan.stat().st_size,
                      "plan_sha256": hashlib.sha256(plan.read_bytes()).hexdigest()}
            (engine / "engine.json").write_text(json.dumps(record))
            before = {p.name: (p.read_bytes(), p.stat().st_mtime_ns) for p in engine.iterdir()}
            argv = [sys.executable, str(capture), "--inference-root", str(root), "--key-prefix", key[:16]]
            result = subprocess.run(argv, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout)["record"], record)
            self.assertEqual(before, {p.name: (p.read_bytes(), p.stat().st_mtime_ns) for p in engine.iterdir()})
            duplicate = root / "engines" / (key[:16] + "f" * 16)
            duplicate.mkdir()
            result = subprocess.run(argv, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("exactly one", result.stderr)
            duplicate.rmdir()
            plan.write_bytes(b"tampered cached plan")
            result = subprocess.run(argv, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("engine bytes", result.stderr)
            record["fields"]["cuda"] = "99"
            (engine / "engine.json").write_text(json.dumps(record))
            result = subprocess.run(argv, capture_output=True, text=True, timeout=5)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("fields do not match", result.stderr)


if __name__ == "__main__":
    unittest.main()
