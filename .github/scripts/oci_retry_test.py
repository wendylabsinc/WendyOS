#!/usr/bin/env python3
"""Exercise the real CI harness's OCI retry policy with local process fixtures.

No Docker daemon or device is contacted. The fake executables retain state across
CLI invocations so a dead disposable builder stays broken until it is removed.
"""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
OCI_FAILURE = "Error: docker buildx build (OCI export) failed: export stream interrupted"
DAEMON_FAILURES = (
    "failed to receive status: rpc error: code = Unavailable desc = EOF",
    "error reading from server: EOF",
    "/run/buildkit/buildkitd.sock: connect: connection refused",
    "failed to list workers: rpc error: code = Unavailable",
    "ResourceExhausted: cannot allocate memory",
)

FIXTURE = r'''
import json, os, sys
from pathlib import Path
root = Path(os.environ["FIXTURE_ROOT"])
state_file = root / "state.json"
state = json.loads(state_file.read_text())
args = sys.argv[1:]
kind = Path(sys.argv[0]).name
with (root / "calls.jsonl").open("a") as log:
    log.write(json.dumps({"command": kind, "args": args}) + "\n")
if kind == "docker":
    expected = ["buildx", "rm", "ci-regression-disposable-oci", "--force"]
    if args != expected:
        sys.exit("unexpected builder operation: " + repr(args))
    state["builders"].pop("ci-regression-disposable-oci", None)
    state_file.write_text(json.dumps(state))
    sys.exit(0)
if args[:2] == ["device", "info"]:
    print(json.dumps({"deviceType": "fixture", "gpuVendor": "", "version": "fixture"}))
elif args and args[0] == "run":
    name = Path(args[args.index("--prefix") + 1]).name
    count = state["attempts"].get(name, 0) + 1
    state["attempts"][name] = count
    error = ""
    if name == "python-hello":
        mode = state["mode"]
        if mode in ("transient", "permanent", "unrelated"):
            if count == 1 or mode == "permanent":
                error = state["error"]
        elif mode == "daemon":
            if count == 1:
                state["builders"]["ci-regression-disposable-oci"] = "dead"
            if state["builders"].get("ci-regression-disposable-oci") == "dead":
                error = state["error"]
            else:
                # The CLI recreates its disposable builder on its next build.
                state["builders"]["ci-regression-disposable-oci"] = "healthy"
    state_file.write_text(json.dumps(state))
    if error:
        print(error, file=sys.stderr)
        sys.exit(23)
    print("fixture deployment accepted")
elif args[:3] == ["device", "apps", "list"]:
    print(json.dumps([{"name": "sh.wendy.ci." + name, "runningState": "STOPPED",
                       "exitCode": state["app_exit"] if name == "python-hello" else 0}
                      for name in ("python-hello", "python-network")]))
else:
    sys.exit("unexpected CLI operation: " + repr(args))
'''


class OCIRetryTests(unittest.TestCase):
    def run_harness(self, mode, error=OCI_FAILURE, builder="ci-regression-disposable", app_exit=0):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            script = root / "go/scripts/test-ci.sh"
            script.parent.mkdir(parents=True)
            # Execute the full production script, including its test loop and exit status.
            script.write_bytes((ROOT / "go/scripts/test-ci.sh").read_bytes())
            for name in ("python-hello", "python-network"):
                project = root / ".github/ci-tests" / name
                project.mkdir(parents=True)
                (project / "wendy.json").write_text(json.dumps({"appId": "sh.wendy.ci." + name}))
            bin_dir = root / "bin"
            bin_dir.mkdir()
            for command in ("wendy-fixture", "docker"):
                executable = bin_dir / command
                executable.write_text("#!" + sys.executable + "\n" + FIXTURE)
                executable.chmod(0o755)
            state = {"mode": mode, "error": error, "app_exit": app_exit, "attempts": {},
                     "builders": {"ci-regression-disposable-oci": "healthy",
                                  "developer-builder": {"id": "owned-by-user", "keep": True}}}
            (root / "state.json").write_text(json.dumps(state))
            env = dict(os.environ, PATH=str(bin_dir) + os.pathsep + os.environ["PATH"],
                       FIXTURE_ROOT=str(root))
            env.pop("WENDY_BUILDX_BUILDER", None)
            if builder is not None:
                env["WENDY_BUILDX_BUILDER"] = builder
            process = subprocess.run(["bash", str(script), "--wendy", str(bin_dir / "wendy-fixture"),
                                      "--hostname", "selected.invalid", "-t", "python-hello", "-t", "python-network"],
                                     env=env, capture_output=True, text=True, timeout=20)
            calls = [json.loads(line) for line in (root / "calls.jsonl").read_text().splitlines()]
            output = re.sub(r"\x1b\[[0-9;]*m", "", process.stdout + process.stderr)
            after = json.loads((root / "state.json").read_text())
            self.assertEqual(after["builders"]["developer-builder"], state["builders"]["developer-builder"])
            for call in calls:
                if call["command"] == "wendy-fixture":
                    args = call["args"]
                    self.assertEqual(args[args.index("--device") + 1], "selected.invalid")
                    if args[0] == "run":
                        self.assertIn("--no-restart", args)
            return process.returncode, output, calls, after

    def test_transient_export_retries_once_without_removing_a_healthy_builder(self):
        for error in (OCI_FAILURE, "build failed at [compile 3/5]: failed to export result"):
            with self.subTest(error=error):
                code, output, calls, after = self.run_harness("transient", error)
                self.assertEqual(code, 0, output)
                self.assertIn("Results: 2 passed, 0 failed, 0 skipped", output)
                self.assertEqual(after["attempts"], {"python-hello": 2, "python-network": 1})
                self.assertEqual([c for c in calls if c["command"] == "docker"], [])
                self.assertEqual(after["builders"]["ci-regression-disposable-oci"], "healthy")

    def test_dead_daemon_removes_only_the_disposable_builder_before_one_retry(self):
        for error in DAEMON_FAILURES:
            with self.subTest(error=error):
                code, output, calls, after = self.run_harness("daemon", error)
                self.assertEqual(code, 0, output)
                self.assertIn("Results: 2 passed, 0 failed, 0 skipped", output)
                self.assertEqual(after["attempts"], {"python-hello": 2, "python-network": 1})
                removals = [c for c in calls if c["command"] == "docker"]
                self.assertEqual(removals, [{"command": "docker", "args":
                                            ["buildx", "rm", "ci-regression-disposable-oci", "--force"]}])
                retry_calls = [c for c in calls if c["command"] == "docker" or c["args"][0] == "run"]
                self.assertEqual([c["command"] for c in retry_calls],
                                 ["wendy-fixture", "docker", "wendy-fixture", "wendy-fixture"])
                self.assertEqual(after["builders"]["ci-regression-disposable-oci"], "healthy")

    def test_permanent_export_failure_stops_after_one_retry_and_does_not_mask_later_results(self):
        code, output, calls, after = self.run_harness("permanent")
        self.assertEqual(code, 1, output)
        self.assertIn("Results: 1 passed, 1 failed, 0 skipped", output)
        self.assertIn("FAIL (exit 23)", output)
        self.assertEqual(after["attempts"], {"python-hello": 2, "python-network": 1})
        self.assertEqual([c for c in calls if c["command"] == "docker"], [])

    def test_local_builds_are_neither_retried_nor_removed(self):
        for builder in (None, ""):
            for error in (OCI_FAILURE, DAEMON_FAILURES[0]):
                with self.subTest(builder=builder, error=error):
                    code, output, calls, after = self.run_harness("transient", error, builder=builder)
                    self.assertEqual(code, 1, output)
                    self.assertIn("Results: 1 passed, 1 failed, 0 skipped", output)
                    self.assertEqual(after["attempts"], {"python-hello": 1, "python-network": 1})
                    self.assertEqual([c for c in calls if c["command"] == "docker"], [])

    def test_unrelated_deploy_failure_and_container_failure_are_not_retried(self):
        for mode, error, app_exit in (("unrelated", "device transport EOF", 0),
                                      ("success", "", 17)):
            with self.subTest(mode=mode):
                code, output, calls, after = self.run_harness(mode, error, app_exit=app_exit)
                self.assertEqual(code, 1, output)
                self.assertIn("Results: 1 passed, 1 failed, 0 skipped", output)
                self.assertEqual(after["attempts"], {"python-hello": 1, "python-network": 1})
                self.assertEqual([c for c in calls if c["command"] == "docker"], [])
                if app_exit:
                    self.assertIn("exited 17", output)


if __name__ == "__main__":
    unittest.main()
