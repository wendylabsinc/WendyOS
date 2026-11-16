#!/usr/bin/env python3
"""Offline checks of hardware-fixture verdicts; these never execute CUDA."""
import contextlib
import io
import importlib.util
import json
from pathlib import Path
import runpy
import subprocess
import sys
import tempfile
from types import SimpleNamespace as NS
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


class Array:
    def __init__(self, value):
        self.value = value
        self.shape = (256, 256)
        self.device = "cuda:0"

    def __mul__(self, other):
        return Array(self.value * other)

    def cpu(self):
        return self

    def flatten(self):
        return [self.value] * 10


class GPUHardwareVerdictTests(unittest.TestCase):
    def execute(self, fixture, modules):
        output = io.StringIO()
        code = 0
        with patch.dict(sys.modules, modules), contextlib.redirect_stdout(output):
            try:
                runpy.run_path(str(ROOT / ".github/ci-tests" / fixture / "main.py"), run_name="__main__")
            except SystemExit as error:
                code = error.code
            except (RuntimeError, AssertionError):
                code = 1
        return code, output.getvalue()

    def test_pytorch_requires_completed_correct_computation(self):
        for mode in ("correct", "async-failure", "wrong-result"):
            with self.subTest(mode=mode):
                calls = []

                def synchronize():
                    calls.append("synchronize")
                    if mode == "async-failure":
                        raise RuntimeError("asynchronous CUDA error")

                def check(actual, expected, **kwargs):
                    calls.append("check")
                    self.assertEqual(kwargs, {"rtol": 0, "atol": 0})
                    if actual.value != expected.value:
                        raise AssertionError("incorrect matrix result")

                torch = NS(__version__="fixture", version=NS(cuda="fixture"),
                           cuda=NS(is_available=lambda: True, device_count=lambda: 1,
                                   get_device_name=lambda _: "fixture GPU", synchronize=synchronize),
                           ones=lambda *args, **kw: Array(1), full=lambda shape, value: Array(value),
                           matmul=lambda x, y: Array(255 if mode == "wrong-result" else 256),
                           testing=NS(assert_close=check))
                code, output = self.execute("python-gpu", {"torch": torch})
                self.assertEqual(code, 0 if mode == "correct" else 1, output)
                self.assertEqual("PASS:" in output, mode == "correct", output)
                self.assertEqual(calls, ["synchronize"] if mode == "async-failure" else ["synchronize", "check"])

    def test_onnx_requires_cuda_execution_evidence_even_when_cuda_is_preferred(self):
        for mode in ("cuda", "cpu", "mixed", "missing", "wrong-result", "runtime-error"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                fallback_disabled = []
                options = []

                def session_options():
                    option = NS()
                    options.append(option)
                    return option

                def run(*args):
                    self.assertEqual(fallback_disabled, [True])
                    if mode == "runtime-error":
                        raise RuntimeError("CUDA driver error")
                    return [Array(7 if mode == "wrong-result" else 2)]

                def end_profile():
                    providers = {"cpu": ["CPUExecutionProvider"],
                                 "mixed": ["CUDAExecutionProvider", "CPUExecutionProvider"],
                                 "missing": []}.get(mode, ["CUDAExecutionProvider"])
                    profile = Path(directory) / "profile.json"
                    profile.write_text(json.dumps([{"cat": "Node", "name": "gpu_multiply_kernel_time",
                                                     "args": {"op_name": "Mul", "provider": provider}}
                                                    for provider in providers]))
                    return str(profile)

                def session(*args, **kwargs):
                    self.assertTrue(options[0].enable_profiling)
                    self.assertEqual(options[0].graph_optimization_level, "disabled")
                    return NS(get_providers=lambda: ["CUDAExecutionProvider", "CPUExecutionProvider"],
                              disable_fallback=lambda: fallback_disabled.append(True), run=run,
                              end_profiling=end_profile)

                onnx = NS(helper=NS(make_tensor_value_info=lambda *a, **k: None,
                                    make_node=lambda *a, **k: None, make_graph=lambda *a, **k: None,
                                    make_opsetid=lambda *a: None,
                                    make_model=lambda *a, **k: NS(SerializeToString=lambda: b"fixture")),
                          TensorProto=NS(FLOAT=1), numpy_helper=NS(from_array=lambda *a, **k: None))
                numpy = NS(float32="float32", full=lambda shape, value, **kw: Array(value),
                           ones=lambda *a, **kw: Array(1), allclose=lambda a, b, **kw: a.value == b.value)
                ort = NS(__version__="fixture", set_default_logger_severity=lambda _: None,
                         get_available_providers=lambda: ["CUDAExecutionProvider", "CPUExecutionProvider"],
                         SessionOptions=session_options, InferenceSession=session,
                         GraphOptimizationLevel=NS(ORT_DISABLE_ALL="disabled"))
                code, output = self.execute("python-onnx-gpu", {"onnx": onnx, "numpy": numpy, "onnxruntime": ort})
                self.assertEqual(code, 0 if mode == "cuda" else 1, output)
                self.assertEqual("PASS:" in output, mode == "cuda", output)


class GPURehearsalTests(unittest.TestCase):
    def setUp(self):
        path = ROOT / ".github/regression-coverage/manual/jetson-gpu-rehearsal.py"
        spec = importlib.util.spec_from_file_location("gpu_rehearsal", path)
        self.generator = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(self.generator)

    def inventory(self, root):
        cli = root / "wendy"
        cli.write_text("#!/bin/sh\nexit 99\n")
        cli.chmod(0o755)
        boards = []
        for i, (kind, (arch, _)) in enumerate(self.generator.BOARDS.items()):
            info = root / f"{kind}.json"
            info.write_text(json.dumps({"deviceType": kind, "gpuArch": arch, "gpuVendor": "nvidia",
                                        "cpuArchitecture": "arm64", "os": "wendyos", "osVersion": "fixture",
                                        "version": "fixture", "jetpackVersion": "6.2" if kind == "jetson-orin-nx" else "7.2",
                                        "cudaVersion": "12.6" if kind == "jetson-orin-nx" else "13.2"}))
            boards.append({"selector": f"board-{i}.invalid", "serial": f"reserved-{i}",
                           "owner": "fixture operator", "device_info": str(info)})
        path = root / "inventory.json"
        path.write_text(json.dumps({"cli_binary": str(cli), "agent_source_commit": "a" * 40, "boards": boards}))
        return path

    def test_generates_all_board_workload_controls_without_contacting_devices(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory = self.inventory(root)
            out = root / "bundle"
            evidence = self.generator.generate(inventory, out)
            self.assertEqual(evidence["hardware_execution"], "not_run")
            self.assertEqual(len(evidence["cases"]), 24)
            self.assertTrue(all(c["status"] == "not_run" for c in evidence["cases"]))
            self.assertEqual(len({c["app_id"] for c in evidence["cases"]}), 24)
            commands = json.loads((out / "commands.json").read_text())
            for case in evidence["cases"]:
                command = commands[case["id"]]["deploy"]
                self.assertEqual(command[command.index("--device") + 1], case["selector"])
                self.assertEqual(command[command.index("--dockerfile") + 1], case["dockerfile"])
                self.assertIn("--no-restart", command)
                project = Path(command[command.index("--prefix") + 1])
                config = json.loads((project / "wendy.json").read_text())
                self.assertEqual(config["appId"], case["app_id"])
                self.assertEqual(config["entitlements"], [] if case["expect"] == "denied" else [{"type": "gpu"}])
            compile((out / "run-command.py").read_text(), str(out / "run-command.py"), "exec")
            # Even a prepared candidate may not execute if its CLI changed.
            evidence["dirty_preview"] = False
            (out / "evidence.json").write_text(json.dumps(evidence))
            case = evidence["cases"][0]["id"]
            executed = subprocess.run([sys.executable, str(out / "run-command.py"), case, "info", "probe"],
                                      capture_output=True, text=True, timeout=5)
            self.assertEqual(executed.returncode, 99)
            record = json.loads((out / f"{case}-info-probe.result.json").read_text())
            self.assertEqual(record["argv"], commands[case]["info"])
            self.assertEqual(record["exit_code"], 99)
            self.assertEqual(record["timeout_seconds"], 30)
            self.assertTrue(all(c["status"] == "not_run" for c in json.loads((out / "evidence.json").read_text())["cases"]))
            (root / "wendy").write_text("#!/bin/sh\nexit 98\n")
            refused = subprocess.run([sys.executable, str(out / "run-command.py"), case, "info", "first"],
                                     capture_output=True, text=True, timeout=5)
            self.assertNotEqual(refused.returncode, 0)
            self.assertIn("artifact changed", refused.stderr)
            self.assertFalse(list(out.glob("*-first.result.json")))

    def test_refuses_missing_duplicate_or_incompatible_hardware(self):
        for defect in ("missing", "duplicate", "wrong-arch", "wrong-jetpack", "wrong-cuda", "wrong-orin-host"):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory() as directory:
                root = Path(directory); inventory = self.inventory(root)
                raw = json.loads(inventory.read_text())
                if defect == "missing":
                    raw["boards"].pop()
                elif defect == "duplicate":
                    raw["boards"][1]["serial"] = raw["boards"][0]["serial"]
                else:
                    board = raw["boards"][{"wrong-cuda": 2, "wrong-jetpack": 0}.get(defect, 1)]
                    path = Path(board["device_info"]); info = json.loads(path.read_text())
                    key, value = {"wrong-arch": ("gpuArch", "sm_89"), "wrong-jetpack": ("jetpackVersion", "7.2"),
                                  "wrong-cuda": ("cudaVersion", "12.6"), "wrong-orin-host": ("cudaVersion", "12.6")}[defect]
                    info[key] = value; path.write_text(json.dumps(info))
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    self.generator.generate(inventory, root / "bundle")
                self.assertFalse((root / "bundle").exists())


class DisplayRehearsalTests(unittest.TestCase):
    setUp = GPURehearsalTests.setUp
    inventory = GPURehearsalTests.inventory

    def test_generates_display_only_and_denied_cases(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            raw = json.loads(self.inventory(root).read_text())
            inventory = root / "display-inventory.json"
            raw["board"] = raw.pop("boards")[0]
            inventory.write_text(json.dumps(raw))
            path = ROOT / ".github/regression-coverage/manual/jetson-display-rehearsal.py"
            spec = importlib.util.spec_from_file_location("display_rehearsal", path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            out = root / "display-bundle"
            evidence = module.generate(inventory, out)
            self.assertEqual(evidence["audit_entries"], ["1666"])
            self.assertEqual(evidence["hardware_execution"], "not_run")
            self.assertEqual(len(evidence["cases"]), 4)
            commands = json.loads((out / "commands.json").read_text())
            self.assertEqual(len({c["app_id"] for c in evidence["cases"]}), 4)
            for case in evidence["cases"]:
                self.assertEqual(case["status"], "not_run")
                command = commands[case["id"]]["deploy"]
                self.assertEqual(command[command.index("--device") + 1], raw["board"]["selector"])
                project = Path(command[command.index("--prefix") + 1])
                cfg = json.loads((project / "wendy.json").read_text())
                self.assertEqual(cfg["entitlements"], [] if case["expect"] == "denied" else [{"type": "display"}])
                self.assertEqual(cfg["appId"], case["app_id"])
            self.assertIn("compute-only OS CDI list is insufficient", (out / "procedure.txt").read_text())

    def test_refuses_non_nvidia_hardware(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            raw = json.loads(self.inventory(root).read_text())
            raw["board"] = raw.pop("boards")[0]
            info = Path(raw["board"]["device_info"])
            data = json.loads(info.read_text()); data["gpuVendor"] = "qualcomm"
            info.write_text(json.dumps(data))
            inventory = root / "display-inventory.json"
            inventory.write_text(json.dumps(raw))
            path = ROOT / ".github/regression-coverage/manual/jetson-display-rehearsal.py"
            spec = importlib.util.spec_from_file_location("display_rehearsal", path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            with self.assertRaisesRegex(ValueError, "NVIDIA Jetson"):
                module.generate(inventory, root / "bundle")
            self.assertFalse((root / "bundle").exists())


class DriverUpdateRehearsalTests(unittest.TestCase):
    setUp = GPURehearsalTests.setUp
    inventory = GPURehearsalTests.inventory

    def driver_inventory(self, root):
        raw = json.loads(self.inventory(root).read_text())
        raw["board"] = raw.pop("boards")[1]
        for name in ("agent_binary", "os_artifact", "recovery_record"):
            path = root / name
            path.write_text("local fixture for " + name)
            raw[name] = str(path)
        raw.update(artifact_url="https://artifacts.invalid/os-0.21.0.mender", target_os_version="0.21.0",
                   old_driver_path="/usr/lib/libcuda.so.100", new_driver_path="/usr/lib/libcuda.so.101")
        path = root / "driver-inventory.json"
        path.write_text(json.dumps(raw))
        spec = importlib.util.spec_from_file_location("driver_update", ROOT / ".github/regression-coverage/manual/jetson-driver-update.py")
        module = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(module)
        return path, module

    def test_keeps_existing_apps_across_update_and_bounds_explicit_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            inventory, generator = self.driver_inventory(root)
            out = root / "bundle"
            evidence = generator.generate(inventory, out)
            self.assertEqual(evidence["hardware_execution"], "not_run")
            self.assertEqual(len(evidence["cases"]), 6)
            self.assertEqual(len({c["app_id"] for c in evidence["cases"]}), 2)
            self.assertTrue(all(c["status"] == "not_run" for c in evidence["cases"]))
            commands = json.loads((out / "commands.json").read_text())
            for group, stages in commands.items():
                for argv in stages.values():
                    self.assertEqual(argv[argv.index("--device") + 1], evidence["board"]["selector"])
                if group != "board":
                    self.assertNotIn("--detach", stages["restart"])
                    self.assertIn("--no-restart", stages["deploy"])
                    apps = {c["app_id"] for c in evidence["cases"] if c["command_group"] == group}
                    self.assertEqual(apps, {stages["restart"][5]})
            for phase in ("before", "after"):
                console = out / f"console-{phase}.sh"
                subprocess.run(["sh", "-n", str(console)], check=True, timeout=5)
                self.assertEqual(console.read_text().count("containers info"), 2)
                self.assertNotIn("containers delete", console.read_text())
            update = commands["board"]["os-update"]
            self.assertIn("--binary", update)
            self.assertIn("--artifact-url", update)
            evidence["dirty_preview"] = False
            (out / "evidence.json").write_text(json.dumps(evidence))
            result = subprocess.run([sys.executable, str(out / "run-command.py"), "board", "os-update", "fixture"],
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 99)
            record = json.loads((out / "board-os-update-fixture.result.json").read_text())
            self.assertEqual(record["timeout_seconds"], 1800)
            self.assertEqual(record["argv"], update)
            raw = json.loads(inventory.read_text())
            Path(raw["os_artifact"]).write_text("changed artifact")
            refused = subprocess.run([sys.executable, str(out / "run-command.py"), "board", "os-update", "changed"],
                                     capture_output=True, text=True, timeout=5)
            self.assertIn("artifact changed", refused.stderr)
            self.assertFalse((out / "board-os-update-changed.result.json").exists())

    def test_rejects_updates_that_do_not_exercise_the_target(self):
        for defect in ("same-version", "same-path", "latest-url", "unsupported-os"):
            with self.subTest(defect=defect), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                inventory, generator = self.driver_inventory(root)
                raw = json.loads(inventory.read_text())
                if defect == "same-version": raw["target_os_version"] = "fixture"
                if defect == "same-path": raw["new_driver_path"] = raw["old_driver_path"]
                if defect == "latest-url": raw["artifact_url"] = "https://artifacts.invalid/latest.mender"
                if defect == "unsupported-os":
                    path = Path(raw["board"]["device_info"])
                    info = json.loads(path.read_text()); info["os"] = "linux"; path.write_text(json.dumps(info))
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    generator.generate(inventory, root / "bundle")
                self.assertFalse((root / "bundle").exists())


if __name__ == "__main__":
    unittest.main()
