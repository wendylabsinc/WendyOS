"""Exercise capability gating and failure propagation without device access."""

import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]
SCRIPTS = ROOT / "go/scripts"
spec = importlib.util.spec_from_file_location("probe", SCRIPTS / "probe-memory-controller.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)

FAKE_WENDY = '''#!/usr/bin/env python3
import json, os, sys
args = sys.argv[1:]
if args[:2] == ["device", "info"]:
    print(json.dumps({"version": "test", "os": "wendyos"}))
elif args[:2] == ["device", "shell"]:
    with open(os.environ["CALLS"], "a") as log:
        log.write("probe\\n")
    print(os.environ["PROBE_OUTPUT"])
    sys.exit(int(os.environ.get("PROBE_RC", "0")))
elif args[:3] == ["device", "apps", "list"]:
    print(json.dumps([{"name": "sh.wendy.ci.python-resources", "runningState": "STOPPED", "exitCode": 0}]))
elif args and args[0] == "run":
    with open(os.environ["CALLS"], "a") as log:
        log.write("deploy\\n")
    print(os.environ.get("DEPLOY_OUTPUT", "db: PASS\\napi: PASS"))
    sys.exit(int(os.environ.get("DEPLOY_RC", "0")))
elif args[:3] != ["device", "apps", "remove"]:
    sys.exit("unexpected invocation: " + repr(args))
'''


def record(controllers):
    return f"{probe.PREFIX}{controllers}\n{probe.VERDICT}"


class ResourceGateTests(unittest.TestCase):
    def run_harness(self, output, harness=SCRIPTS / "test-ci.sh", **overrides):
        with tempfile.TemporaryDirectory() as directory:
            binary = Path(directory) / "wendy"
            binary.write_text(FAKE_WENDY)
            binary.chmod(0o755)
            calls = Path(directory) / "calls"
            env = dict(os.environ, CALLS=str(calls), PROBE_OUTPUT=output)
            # Ensure a user's CI builder cannot trigger a real docker retry.
            env.pop("WENDY_BUILDX_BUILDER", None)
            env.update(overrides)
            result = subprocess.run(
                ["bash", str(harness), "-w", str(binary),
                 "-h", "fixture.invalid", "-t", "python-resources",
                 "-t", "python-multiservice-resources"],
                env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                timeout=20,
            )
            return result, calls.read_text().splitlines() if calls.exists() else []

    def test_absent_skips_only_after_successful_probe(self):
        result, calls = self.run_harness(record("cpuset cpu io pids"))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls, ["probe"])
        self.assertEqual(result.stdout.count("SKIP"), 2)
        self.assertIn("assertions not exercised", result.stdout)

    def test_capable_deploys_both_original_fixtures(self):
        result, calls = self.run_harness(record("cpuset cpu io memory pids"))
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls, ["probe", "deploy", "deploy"])
        self.assertNotIn("SKIP", result.stdout)

    def test_probe_errors_never_skip_or_deploy(self):
        for output, rc in [(record("cpu pids"), "1"), ("", "0"),
                           (record(""), "0"), (record("cpu ???"), "0"),
                           (record("cpu cpu"), "0"),
                           (record("cpu") + "\n" + record("memory"), "0"),
                           (probe.PREFIX + "cpu pids", "0")]:
            with self.subTest(output=output, rc=rc):
                result, calls = self.run_harness(output, PROBE_RC=rc)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertEqual(calls, ["probe"])
                self.assertNotIn("SKIP", result.stdout)

    def test_deploy_failure_keeps_first_error_and_rejects_stale_pass(self):
        output = "primary startup error\n" + "detail\n" * 15 + "db: PASS\napi: PASS"
        result, calls = self.run_harness(record("cpu memory pids"),
                                         DEPLOY_RC="1", DEPLOY_OUTPUT=output)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls.count("deploy"), 2)
        self.assertIn("2 failed", result.stdout)
        self.assertEqual(result.stdout.count("primary startup error"), 2)

    def test_missing_probe_script_fails_without_skip_or_device_calls(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scripts = root / "go/scripts"
            scripts.mkdir(parents=True)
            (root / ".github/ci-tests").mkdir(parents=True)
            harness = scripts / "test-ci.sh"
            harness.write_bytes((SCRIPTS / "test-ci.sh").read_bytes())
            result, calls = self.run_harness(record("cpu memory pids"), harness=harness)
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertNotIn("SKIP", result.stdout)
            self.assertIn("2 failed", result.stdout)
            self.assertEqual(calls, [])

    def test_multiline_controller_record_fails_without_skip_or_deploy(self):
        result, calls = self.run_harness(record("cpu\nmemory"))
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertNotIn("SKIP", result.stdout)
        self.assertEqual(calls, ["probe"])

    @unittest.skipIf(os.geteuid() == 0, "root can read mode-000 files")
    def test_unreadable_probe_script_fails_without_skip_or_device_calls(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            scripts = root / "go/scripts"
            scripts.mkdir(parents=True)
            (root / ".github/ci-tests").mkdir(parents=True)
            harness = scripts / "test-ci.sh"
            harness.write_bytes((SCRIPTS / "test-ci.sh").read_bytes())
            helper = scripts / "probe-memory-controller.py"
            helper.write_bytes((SCRIPTS / helper.name).read_bytes())
            helper.chmod(0)
            result, calls = self.run_harness(record("cpu memory pids"), harness=harness)
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertNotIn("SKIP", result.stdout)
            self.assertIn("2 failed", result.stdout)
            self.assertEqual(calls, [])

    def run_real_probe(self, files, directories=(), unreadable=()):
        """Run the real Python -> CLI argv -> sh producer/parser boundary."""
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory) / "cgroup"
            root.mkdir()
            for name in directories:
                (root / name).mkdir(parents=True, exist_ok=True)
            for name, content in files.items():
                path = root / name
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(content)
            cli = Path(directory) / "shell-cli"
            cli.write_text('''#!/usr/bin/env python3
import os, sys
args = sys.argv[sys.argv.index("--") + 1:]
assert args[:2] == ["sh", "-c"]
args[2] = args[2].replace("/sys/fs/cgroup", os.environ["CGROUP_FIXTURE"])
os.execvp(args[0], args)
''')
            cli.chmod(0o755)
            modes = []
            try:
                for name in unreadable:
                    path = root / name
                    modes.append((path, path.stat().st_mode & 0o777))
                    path.chmod(0)
                return subprocess.run(
                    [sys.executable, str(SCRIPTS / "probe-memory-controller.py"),
                     str(cli), "fixture.invalid"],
                    stdin=subprocess.DEVNULL, text=True, capture_output=True,
                    env=dict(os.environ, CGROUP_FIXTURE=str(root)), timeout=10,
                )
            finally:
                for path, mode in sorted(modes, key=lambda item: len(item[0].parts)):
                    path.chmod(mode)

    def test_real_shell_v1_v2_and_hybrid(self):
        for files, expected in [
            ({"memory/memory.limit_in_bytes": "9223372036854771712\n"}, "available"),
            ({"cgroup.controllers": "cpuset cpu io memory pids\n"}, "available"),
            ({"cgroup.controllers": "cpuset cpu io pids\n"}, "absent"),
            ({"unified/cgroup.controllers": "cpu pids\n",
              "memory/memory.limit_in_bytes": "123456\n"}, "available"),
            ({"cgroup.controllers": "cpu pids\n",
              "memory/memory.limit_in_bytes": "123456\n"}, "available"),
        ]:
            with self.subTest(files=files):
                result = self.run_real_probe(files)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(result.stdout.splitlines()[-1], f"WENDY_MEMORY_CONTROLLER={expected}")

    def test_real_shell_malformed_and_unknown_are_failures(self):
        for files, directories in [
            ({}, ()),
            ({"cgroup.controllers": "cpu\nmemory\n"}, ()),
            ({"cgroup.controllers": ""}, ()),
            ({"cgroup.controllers": "cpu cpu"}, ()),
            ({"cgroup.controllers": "cpu ???"}, ()),
            ({"memory/memory.limit_in_bytes": "max"}, ()),
            ({"memory/memory.limit_in_bytes": ""}, ()),
            ({"memory/memory.limit_in_bytes": "123\n456"}, ()),
            ({"cgroup.controllers": "cpu memory pids"}, ("memory",)),
            ({"memory": "not a hierarchy", "cgroup.controllers": "cpu pids"}, ()),
        ]:
            with self.subTest(files=files):
                result = self.run_real_probe(files, directories)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("WENDY_MEMORY_CONTROLLER=", result.stdout)

    @unittest.skipIf(os.geteuid() == 0, "root can read mode-000 files")
    def test_real_shell_unreadable_is_unknown_not_absence(self):
        for files, unreadable in [
            ({"cgroup.controllers": "cpu pids"}, ("cgroup.controllers",)),
            ({"cgroup.controllers": "cpu pids"}, (".",)),
            ({"memory/memory.limit_in_bytes": "123456"}, ("memory",)),
            ({"memory/memory.limit_in_bytes": "123456"}, ("memory/memory.limit_in_bytes",)),
            ({"cgroup.controllers": "cpu pids",
              "memory/memory.limit_in_bytes": "123456"}, ("memory",)),
            ({"cgroup.controllers": "cpu memory pids",
              "memory/memory.limit_in_bytes": "123456"}, ("memory/memory.limit_in_bytes",)),
        ]:
            with self.subTest(unreadable=unreadable):
                result = self.run_real_probe(files, unreadable=unreadable)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertNotIn("WENDY_MEMORY_CONTROLLER=", result.stdout)

    def test_v1_result_deploys_original_fixtures(self):
        result, calls = self.run_harness(f"{probe.V1_PREFIX}123456\n{probe.VERDICT}")
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertEqual(calls, ["probe", "deploy", "deploy"])

    def test_failure_output_is_bounded_without_short_output_overlap(self):
        for count in [1, 20, 21, 30, 31, 60]:
            with self.subTest(count=count):
                lines = [f"fixture-line-{i:02d}" for i in range(1, count + 1)]
                result, _ = self.run_harness(record("cpu memory pids"),
                                              DEPLOY_RC="1", DEPLOY_OUTPUT="\n".join(lines))
                self.assertNotEqual(result.returncode, 0)
                for i, line in enumerate(lines, 1):
                    expected = 2 if count <= 30 or i <= 20 or i > count - 10 else 0
                    self.assertEqual(result.stdout.splitlines().count(line), expected, result.stdout)
                marker = f"... [{count - 30} lines omitted; first 20 and last 10] ..."
                self.assertEqual(result.stdout.count(marker), 2 if count > 30 else 0)

    def test_timeout_and_launch_error_are_failures(self):
        for error in [subprocess.TimeoutExpired("wendy", 30), OSError("cannot launch")]:
            with self.subTest(error=error), patch.object(probe.subprocess, "run", side_effect=error):
                self.assertEqual(probe.main(["wendy", "fixture.invalid"]), 1)


if __name__ == "__main__":
    unittest.main()
