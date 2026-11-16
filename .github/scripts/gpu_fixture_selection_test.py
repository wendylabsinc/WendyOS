#!/usr/bin/env python3
"""Run the real hardware-test harness against a local CLI protocol fixture."""

import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
GPU_TESTS = ("python-gpu", "python-onnx-gpu")


class GPUFixtureSelectionTests(unittest.TestCase):
    def run_harness(self, info, exit_code=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            cli = root / "wendy-fixture"
            calls = root / "calls.jsonl"
            cli.write_text('''#!/usr/bin/env python3
import json, os, sys
from pathlib import Path
args = sys.argv[1:]
with open(os.environ["CALLS"], "a") as log:
    log.write(json.dumps(args) + "\\n")
if args[:2] == ["device", "info"]:
    print(os.environ["DEVICE_INFO"])
elif args and args[0] == "run":
    print("fixture deployment accepted")
elif args[:3] == ["device", "apps", "list"]:
    print(json.dumps([{"name": "sh.wendy.ci." + name, "runningState": "STOPPED",
                       "exitCode": int(os.environ["APP_EXIT"])}
                      for name in ["python-gpu", "python-onnx-gpu"]]))
else:
    sys.exit("unexpected CLI operation: " + repr(args))
''')
            cli.chmod(0o755)
            env = dict(os.environ, DEVICE_INFO=json.dumps(info), CALLS=str(calls),
                       APP_EXIT=str(exit_code), WENDY_BUILDX_BUILDER="")
            result = subprocess.run(
                ["bash", str(ROOT / "go/scripts/test-ci.sh"), "--wendy", str(cli),
                 "--hostname", "selected.invalid", "-t", GPU_TESTS[0], "-t", GPU_TESTS[1]],
                env=env, capture_output=True, text=True, timeout=20,
            )
            output = re.sub(r"\x1b\[[0-9;]*m", "", result.stdout + result.stderr)
            invocations = [json.loads(line) for line in calls.read_text().splitlines()]
            for args in invocations:
                self.assertEqual(args[args.index("--device") + 1], "selected.invalid")
            return result.returncode, output, invocations

    def test_supported_and_legacy_nvidia_devices_select_exact_images(self):
        cases = [
            ("jetson-orin-nano", "arm64", "sm_87", "Dockerfile"),
            ("jetson-agx-orin", "arm64", "sm_87", "Dockerfile"),
            ("jetson-agx-thor", "arm64", "sm_110", "Dockerfile.cuda13"),
            ("nvidia-spark", "arm64", "sm_121", "Dockerfile.cuda13"),
            ("jetson-agx-thor", "arm64", None, "Dockerfile.cuda13"),
            ("jetson-orin-nano", "arm64", None, "Dockerfile"),
            ("generic-nvidia", "amd64", None, "Dockerfile"),
        ]
        for kind, cpu, arch, dockerfile in cases:
            with self.subTest(kind=kind, cpu=cpu, arch=arch):
                info = {"deviceType": kind, "cpuArchitecture": cpu, "gpuVendor": "nvidia", "hasGpu": True}
                if arch is not None:
                    info["gpuArch"] = arch
                code, output, calls = self.run_harness(info)
                self.assertEqual(code, 0, output)
                self.assertIn("Results: 2 passed, 0 failed, 0 skipped", output)
                runs = [args for args in calls if args[0] == "run"]
                self.assertEqual(len(runs), 2, calls)
                for args, fixture in zip(runs, GPU_TESTS):
                    self.assertEqual(Path(args[args.index("--prefix") + 1]), ROOT / ".github/ci-tests" / fixture)
                    self.assertEqual(args[args.index("--dockerfile") + 1], dockerfile)
                    self.assertIn("--no-restart", args)
                    self.assertTrue((ROOT / ".github/ci-tests" / fixture / dockerfile).is_file())

    def test_unsupported_gpu_cpu_combinations_skip_without_deploying(self):
        cases = [
            ("pc", "amd64", "amd", "", "no NVIDIA GPU"),
            ("pc", "amd64", "intel", "", "no NVIDIA GPU"),
            ("dragonwing-iq-8275", "arm64", "qualcomm", "", "no NVIDIA GPU"),
            ("raspberry-pi-5", "arm64", "", "", "no NVIDIA GPU"),
            ("pc", "amd64", "nvidia", "sm_89", "CI GPU fixtures support"),
            ("jetson-agx-thor", "arm64", "nvidia", "sm_unknown", "CI GPU fixtures support"),
        ]
        for kind, cpu, vendor, arch, reason in cases:
            with self.subTest(kind=kind, vendor=vendor, arch=arch):
                code, output, calls = self.run_harness({
                    "deviceType": kind, "cpuArchitecture": cpu,
                    "gpuVendor": vendor, "gpuArch": arch, "hasGpu": True,
                })
                self.assertEqual(code, 0, output)
                self.assertIn("Results: 0 passed, 0 failed, 2 skipped", output)
                self.assertEqual(output.count("SKIP ("), 2, output)
                self.assertEqual(output.count(reason), 2, output)
                self.assertEqual([args[:2] for args in calls], [["device", "info"]])

    def test_container_failure_is_not_a_deployment_pass_or_skip(self):
        code, output, calls = self.run_harness({
            "deviceType": "jetson-agx-thor", "gpuVendor": "nvidia", "gpuArch": "sm_110",
        }, exit_code=17)
        self.assertEqual(code, 1, output)
        self.assertIn("Results: 0 passed, 2 failed, 0 skipped", output)
        self.assertEqual(output.count("exited 17"), 2, output)
        self.assertEqual(len([args for args in calls if args[0] == "run"]), 2)


if __name__ == "__main__":
    unittest.main()
