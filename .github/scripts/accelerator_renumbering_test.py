#!/usr/bin/env python3
"""Offline qualification and command checks; never vendor hardware evidence."""
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def load_generator():
    spec = importlib.util.spec_from_file_location("accelerator_renumbering", ROOT / ".github/regression-coverage/manual/accelerator-renumbering.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class AcceleratorRenumberingTests(unittest.TestCase):
    def setUp(self):
        self.generator = load_generator()

    def inventory(self, root, npu=False, thor=False):
        for name in ("agent", "old-os", "new-os", "recovery", "qualification-log", "red.png", "blue.png"):
            (root / name).write_text("OFFLINE FIXTURE ONLY: " + name)
        cli = root / "wendy"
        cli.write_text("#!/bin/sh\nexit 93\n"); cli.chmod(0o755)
        info = {"deviceType": "dragonwing-iq-9075" if npu else ("jetson-agx-thor" if thor else "jetson-agx-orin"), "os": "wendyos",
                "cpuArchitecture": "arm64", "version": "candidate", "osVersion": "before",
                "gpuVendor": "nvidia", "gpuArch": "sm_110" if thor else "sm_87", "cudaVersion": "13.0", "jetpackVersion": "7.0"}
        (root / "info.json").write_text(json.dumps(info))
        digest = lambda name: hashlib.sha256((root / name).read_bytes()).hexdigest()
        transition = {"schema": "wendy.accelerator-transition.v1", "method": "wendy-os-update",
            "serial": "reserved-serial", "device_type": info["deviceType"],
            "approved_by": "offline fixture", "observed_utc": "2026-01-01T00:00:00Z",
            "from_os_version": "before", "to_os_version": "after",
            "from_driver_version": "driver-before", "to_driver_version": "driver-after",
            "from_os_sha256": digest("old-os"), "to_os_sha256": digest("new-os"),
            "from_driver_sha256": "a"*64, "to_driver_sha256": "b"*64,
            "before_device": [10, 122], "after_device": [10, 123],
            "host_device_path": "/dev/fastrpc-cdsp" if npu else "/dev/nvidia-uvm",
            "container_device_path": "/dev/fastrpc-cdsp" if npu else "/dev/nvidia-uvm",
            "driver_library": "/run/wendyos/npu/lib/libcdsprpc.so" if npu else "/usr/lib/libcuda.so.1",
            "observation_log": str(root / "qualification-log"), "observation_sha256": digest("qualification-log")}
        (root / "transition.json").write_text(json.dumps(transition))
        raw = {"cli_binary": str(cli), "agent_binary": str(root / "agent"), "agent_source_commit": "a"*40,
            "old_os_artifact": str(root / "old-os"), "new_os_artifact": str(root / "new-os"),
            "artifact_url": "https://reserved.invalid/artifacts/" + digest("new-os") + ".wic",
            "recovery_record": str(root / "recovery"), "transition_record": str(root / "transition.json"),
            "board": {"selector": "reserved.invalid", "serial": "reserved-serial", "owner": "fixture owner",
                      "device_info": str(root / "info.json")},
            "qualcomm_checkout": str(root), "endpoint": "http://reserved.invalid:9110",
            "images": [{"path": str(root / name), "expected": label}
                       for name, label in (("red.png", "red triangle"), ("blue.png", "blue circle"))]}
        inventory = root / "inventory.json"; inventory.write_text(json.dumps(raw))
        return inventory

    def qwen_project(self):
        return {Path("wendy.json"): json.dumps({"appId": "production-qwen", "entitlements": [
                {"type": "npu"}, {"type": "persist", "name": "production-models", "path": "/data"}]}).encode(),
                Path("Dockerfile"): b"FROM offline-fixture\n", Path("model.py"): b"# offline fixture\n"}

    def test_both_vendor_bundles_keep_hardware_unexecuted_and_scope_every_command(self):
        for npu, thor in ((False, False), (False, True), (True, False)):
            with self.subTest(npu=npu, thor=thor), tempfile.TemporaryDirectory() as directory:
                root = Path(directory); inventory = self.inventory(root, npu, thor)
                out = root / "bundle"
                with patch.object(self.generator.npu, "pinned_project", return_value=self.qwen_project()):
                    evidence = self.generator.generate(inventory, out)
                self.assertEqual(evidence["audit_entries"], ["2540", "2554"] if npu else ["1888"])
                self.assertEqual(evidence["hardware_execution"], "not_run")
                self.assertEqual(evidence["transition_execution"]["status"], "not_run")
                self.assertEqual(evidence["qualification_review"], "pending_human_review")
                self.assertEqual([c["status"] for c in evidence["cases"]], ["not_run"]*3)
                self.assertEqual(evidence["mesh_2554"]["status"], "verified_independently")
                commands = json.loads((out / "commands.json").read_text())
                for group in commands.values():
                    for stage, argv in group.items():
                        if stage.startswith("infer-") or stage in ("status", "report", "log"):
                            continue
                        self.assertEqual(argv[argv.index("--device")+1], "reserved.invalid")
                        self.assertEqual("--force" in argv, stage == "remove")
                        self.assertNotIn("--delete-volumes", argv)
                        self.assertNotIn("--cleanup", argv)
                self.assertIn("--no-restart", commands["app"]["deploy"])
                self.assertNotIn("--json", commands["board"]["update"])
                self.assertIn("--artifact-url", commands["board"]["update"])
                self.assertIn("--json", commands["board"]["agent-reapply"])
                self.assertIn("--binary", commands["board"]["agent-reapply"])
                self.assertNotIn("--artifact-url", commands["board"]["agent-reapply"])
                self.assertEqual("--detach" in commands["app"]["start"], npu)
                self.assertNotIn("run", commands["app"]["start"])
                cfg = json.loads((out / "project/wendy.json").read_text())
                self.assertEqual(cfg["appId"], evidence["app_id"])
                self.assertTrue(cfg["appId"].startswith("sh.wendy.renumber."))
                if npu:
                    self.assertNotIn("production-models", json.dumps(cfg))
                    self.assertEqual(evidence["companion_commit"], self.generator.npu.COMPANION_COMMIT)
                    compile((out / "probe.py").read_text(), "probe", "exec")
                else:
                    self.assertIn("Executed Mul providers", (out / "project/main.py").read_text())
                    argv = commands["app"]["deploy"]
                    self.assertEqual(argv[argv.index("--dockerfile")+1], "Dockerfile.cuda13" if thor else "Dockerfile")
                subprocess.run(["sh", "-n", str(out / "console-capture.sh")], check=True)
                console = (out / "console-capture.sh").read_text()
                self.assertNotIn("modprobe", console); self.assertNotIn("mknod", console)
                self.assertIn(evidence["app_id"], console)
                procedure = (out / "procedure.txt").read_text()
                self.assertIn("record not_run", procedure)
                self.assertIn("naturally stale saved spec", procedure)
                self.assertIn("never deploy again", procedure)
                if npu:
                    self.assertIn("old answer", procedure)
                else:
                    self.assertIn("exclusively on CUDAExecutionProvider", procedure)
                # Running one local sentinel command cannot create a hardware pass.
                evidence["dirty_preview"] = False
                (out / "evidence.json").write_text(json.dumps(evidence))
                result = subprocess.run([sys.executable, str(out / "run-command.py"), "app", "stop", "offline"], capture_output=True, timeout=5)
                self.assertEqual(result.returncode, 93, result.stderr)
                self.assertEqual(json.loads((out / "evidence.json").read_text())["hardware_execution"], "not_run")
                original_commands = (out / "commands.json").read_bytes()
                changed = json.loads(original_commands)
                changed["app"]["stop"][-1] = "different.invalid"
                (out / "commands.json").write_text(json.dumps(changed))
                result = subprocess.run([sys.executable, str(out / "run-command.py"), "app", "stop", "wrong-target"], capture_output=True, timeout=5)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(b"artifact changed", result.stderr)
                self.assertFalse((out / "app-stop-wrong-target.result.json").exists())
                (out / "commands.json").write_bytes(original_commands)
                (root / "new-os").write_text("tampered")
                result = subprocess.run([sys.executable, str(out / "run-command.py"), "board", "update", "changed"], capture_output=True, timeout=5)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(b"artifact changed", result.stderr)
                self.assertFalse((out / "board-update-changed.result.json").exists())

    def test_unqualified_or_unchanged_transitions_cannot_prepare_a_bundle(self):
        changes = {"method": "guess-modprobe", "serial": "different-board", "before_device": [10, 123],
                   "after_device": [True, 123], "from_os_sha256": "0"*64,
                   "to_os_sha256": "0"*64, "observation_sha256": "0"*64,
                   "from_os_version": "unknown", "to_os_version": "before",
                   "driver_library": "/dev/zero", "container_device_path": "/etc/passwd", "approved_by": "",
                   "host_device_path": "/dev/fastrpc-cdsp-secure"}
        for key, value in changes.items():
            with self.subTest(key=key), tempfile.TemporaryDirectory() as directory:
                root = Path(directory); inventory = self.inventory(root, True)
                record = root / "transition.json"; raw = json.loads(record.read_text())
                raw[key] = value; record.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    self.generator.generate(inventory, root / "bundle")
                self.assertFalse((root / "bundle").exists())
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); inventory = self.inventory(root)
            (root / "transition.json").unlink()
            with self.assertRaises(FileNotFoundError):
                self.generator.generate(inventory, root / "bundle")
            self.assertFalse((root / "bundle").exists())


if __name__ == "__main__":
    unittest.main()
