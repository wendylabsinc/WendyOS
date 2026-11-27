"""Runner regressions with no Docker daemon, credentials, or device access."""
import contextlib
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("agentvm_verify", Path(__file__).with_name("verify.py"))
verify = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verify)


class FakeNode:
    """Model remote process-check results without running generated commands."""

    def __init__(self, runner):
        self.runner = runner
        self.now = 0
        self.pid = 101
        self.exe = "/var/lib/agentvm/bin/wendy-agent"
        self.rpc_attempts = []
        self.rpc_ready = True
        self.exit_after_rpc = False

    def sleep(self, seconds):
        self.now += seconds
        if self.now >= 1:
            self.pid = 202

    def call(self, argv, timeout=60, check=True):
        if argv[:3] == ["docker", "inspect", "--format"]:
            return subprocess.CompletedProcess(argv, 0, b"true\n", b"")
        return subprocess.CompletedProcess(argv, 0, b"", b"")

    def python(self, code, *args, check=True):
        identity_matches = self.exe == "/var/lib/agentvm/bin/wendy-agent"
        if args:
            ready = identity_matches and self.pid == int(args[0])
        else:
            ready = identity_matches and self.pid > 1 and self.pid != self.runner.agent_pid
        rc = 0 if ready else 1
        result = subprocess.CompletedProcess(["python3", "-c", code], rc,
                                             (str(self.pid) + "\n").encode() if ready else b"", b"")
        if rc and check:
            raise RuntimeError("remote process check failed")
        return result

    def cli(self, *args, timeout=300, check=True):
        self.rpc_attempts.append((self.now, self.pid, args, timeout))
        ready = self.rpc_ready and self.now >= 2
        if ready and self.exit_after_rpc:
            self.exe = "/bin/unrelated-process"
        return subprocess.CompletedProcess(["wendy", *args], 0 if ready else 1, b"[]\n", b"")

    def patches(self):
        stack = contextlib.ExitStack()
        stack.enter_context(patch.object(self.runner, "call", side_effect=self.call))
        stack.enter_context(patch.object(self.runner, "python", side_effect=self.python))
        stack.enter_context(patch.object(self.runner, "cli", side_effect=self.cli))
        stack.enter_context(patch.object(verify.time, "monotonic", side_effect=lambda: self.now))
        stack.enter_context(patch.object(verify.time, "sleep", side_effect=self.sleep))
        return stack


class RunnerTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="world-agentvm-runner-test-")
        self.addCleanup(self.temp.cleanup)
        self.runner = verify.Runner(SimpleNamespace(output=Path(self.temp.name) / "evidence", keep=False))

    def test_restart_requires_new_process_and_successful_bounded_admin_rpc(self):
        self.runner.agent_pid = 101
        node = FakeNode(self.runner)
        with node.patches():
            self.runner.agent("restart")
        self.assertEqual(self.runner.agent_pid, 202)
        self.assertEqual([attempt[0] for attempt in node.rpc_attempts], [1, 2])
        self.assertTrue(all(attempt[1] == 202 for attempt in node.rpc_attempts))
        self.assertTrue(all(attempt[2] == ("device", "apps", "list") for attempt in node.rpc_attempts))
        self.assertTrue(all(attempt[3] <= 8 for attempt in node.rpc_attempts))

    def test_live_replacement_without_serving_rpc_is_never_ready(self):
        self.runner.agent_pid = 101
        node = FakeNode(self.runner)
        node.rpc_ready = False
        with node.patches(), self.assertRaisesRegex(RuntimeError, "bounded admin RPC"):
            self.runner.agent("restart")
        self.assertEqual(self.runner.agent_pid, 101)
        self.assertGreater(len(node.rpc_attempts), 0)
        self.assertLessEqual(node.now, 61)

    def test_process_identity_must_still_match_after_successful_rpc(self):
        self.runner.agent_pid = 101
        node = FakeNode(self.runner)
        node.exit_after_rpc = True
        with node.patches(), self.assertRaisesRegex(RuntimeError, "process check failed"):
            self.runner.agent("restart")
        self.assertEqual(self.runner.agent_pid, 101)

    def test_command_timeout_retains_partial_output_and_failure_receipt(self):
        command = ["docker", "exec", "owned", "bounded-command"]
        failure = subprocess.TimeoutExpired(command, 3, output=b"partial output", stderr=b"partial error")
        with patch.object(verify.subprocess, "run", side_effect=failure):
            result = self.runner.call(command, timeout=3, check=False)
        self.assertEqual(result.returncode, 124)
        self.assertEqual((self.runner.out / "001.stdout").read_bytes(), b"partial output")
        self.assertIn(b"partial error", (self.runner.out / "001.stderr").read_bytes())
        self.assertIn(b"timed out", (self.runner.out / "001.stderr").read_bytes())
        receipt = json.loads((self.runner.out / "001.json").read_text())
        self.assertEqual(receipt, {"argv": command, "exit_code": 124})

    def cleanup_commands(self, labels_match):
        commands = []
        self.runner.created = ["network", "volume", "container"]

        def run(argv, **kwargs):
            commands.append(argv)
            if argv[:2] in (["docker", "logs"], ["docker", "cp"]):
                raise subprocess.TimeoutExpired(argv, kwargs["timeout"], output=b"partial cleanup output")
            if "inspect" in argv:
                owner = self.runner.name if labels_match else "another-run"
                item = {"Labels": {"wendy.world.agentvm": owner}, "Config": {"Labels": {"wendy.world.agentvm": owner}}}
                return subprocess.CompletedProcess(argv, 0, json.dumps([item]).encode(), b"")
            return subprocess.CompletedProcess(argv, 0, b"", b"")

        with patch.object(verify.subprocess, "run", side_effect=run):
            self.runner.finish()
        return commands

    def test_log_timeouts_do_not_prevent_all_owned_resource_cleanup(self):
        commands = self.cleanup_commands(True)
        self.assertIn(["docker", "rm", "-f", self.runner.name], commands)
        self.assertIn(["docker", "volume", "rm", self.runner.volume], commands)
        self.assertIn(["docker", "network", "rm", self.runner.network], commands)

    def test_cleanup_refuses_resources_with_another_runs_ownership_label(self):
        commands = self.cleanup_commands(False)
        self.assertFalse(any("rm" in command for command in commands))
        self.assertFalse(any(command[:2] in (["docker", "logs"], ["docker", "cp"])
                             for command in commands))


if __name__ == "__main__":
    unittest.main()
