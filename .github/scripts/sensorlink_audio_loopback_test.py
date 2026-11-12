#!/usr/bin/env python3
"""Offline checks using private ordinary processes; no host audio/account I/O."""

import importlib.util
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("test-sensorlink-audio-loopback.sh")
LAUNCHER = SCRIPT.with_name("run-owned-media-test.py")
spec = importlib.util.spec_from_file_location("owned_media", LAUNCHER)
owned_media = importlib.util.module_from_spec(spec)
spec.loader.exec_module(owned_media)


class KernelFixtureLauncherTests(unittest.TestCase):
    def test_private_runtime_is_outside_artifacts_and_launcher_death_is_reported(self):
        source = SCRIPT.read_text()
        allocation = source.split('# only its runtime/config under /tmp;', 1)[1].split('# A real ALSA source', 1)[0]
        # Keep the executable allocation lines, excluding the preceding comment.
        allocation = allocation[allocation.index('daemon_directory='):]
        startup = source.split('# The runner owns the log file;', 1)[1].split('# These inventory files', 1)[0]
        startup = startup[startup.index('# shellcheck'):]
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            fake = root / 'pipewire'
            fake.write_text('#!/bin/sh\nprintf \"fixture daemon startup detail\\n\"\nexit 17\n')
            fake.chmod(0o700)
            output = root / 'private-ancestor' / 'artifacts'
            output.mkdir(parents=True)
            output.parent.chmod(0o700)
            output.chmod(0o700)
            shell = '''set -euo pipefail
output="$1"
wendy_uid="$(id -u)"
wendy_gid="$(id -g)"
daemon_directory=
trap '[ -z "$daemon_directory" ] || rm -rf -- "$daemon_directory"' EXIT
sudo() { if [[ "${1:-}" == -u ]]; then shift 2; fi; "$@"; }
''' + allocation + '''
printf 'private-runtime=%s\\n' "$daemon_directory"
''' + startup
            result = subprocess.run(['bash', '-c', shell, 'private-runtime-test', str(output)],
                                    env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH']),
                                    capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 1, result.stderr)
            runtime = Path(result.stdout.strip().split('private-runtime=', 1)[1])
            self.assertEqual(runtime.parent, Path('/tmp'))
            self.assertTrue(runtime.name.startswith('wendy-sensorlink-pipewire.'))
            self.assertFalse(runtime.exists(), 'owned runtime was not removed')
            self.assertEqual(output.parent.stat().st_mode & 0o777, 0o700)
            self.assertEqual(output.stat().st_mode & 0o777, 0o700)
            self.assertIn('fixture daemon startup detail', result.stderr)
            self.assertIn('Owned PipeWire launcher exited during startup (status 17).', result.stderr)
            self.assertIn('fixture daemon startup detail', (output / 'pipewire.log').read_text())

    def test_non_hosted_invocation_refuses_before_setup(self):
        env = dict(os.environ, GITHUB_ACTIONS="", RUNNER_ENVIRONMENT="")
        result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertIn("requires the disposable GitHub-hosted Linux runner", result.stderr)

    def test_existing_group_refuses_before_build_or_host_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / "unexpected-operation"
            tools = {"uname": 'echo Linux', "getent": '[ "$1" = group ]',
                     **{name: f'touch "{marker}"; exit 91' for name in
                        ("go", "sudo", "modprobe", "pipewire", "pw-dump", "aplay", "arecord")}}
            for name, body in tools.items():
                path = root / name
                path.write_text("#!/bin/sh\n" + body + "\n")
                path.chmod(0o700)
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                       GITHUB_ACTIONS="true", RUNNER_ENVIRONMENT="github-hosted")
            result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 2, result.stderr)
            self.assertIn("existing wendy account/group", result.stderr)
            self.assertFalse(marker.exists())

    def test_cleanup_removes_only_the_group_it_created(self):
        cleanup = SCRIPT.read_text().split("cleanup() {\n", 1)[1].split("\n}\ntrap cleanup EXIT", 1)[0]
        for gid in (12345, 54321):
            with self.subTest(current_gid=gid), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                commands = root / "commands.log"
                sudo = root / "sudo"
                sudo.write_text(f'#!/bin/sh\nprintf "%s\\n" "$*" >> "{commands}"\n[ "$1" != pgrep ]\n')
                sudo.chmod(0o700)
                getent = root / "getent"
                getent.write_text(f'#!/bin/sh\necho "wendy:x:{gid}:"\n')
                getent.chmod(0o700)
                shell = ('created_user=1; loaded_module=0; daemon_launcher=; daemon_directory=; '
                         'wendy_uid=12345; wendy_gid=12345; output="$1"\n'
                         + "cleanup() {\n" + cleanup + "\n}\ntrue\ncleanup\n")
                result = subprocess.run(["bash", "-c", shell, "cleanup-test", str(root)],
                                        env=dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"]),
                                        capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0 if gid == 12345 else 1, result.stderr)
                recorded = commands.read_text().splitlines()
                self.assertIn("userdel wendy", recorded)
                self.assertEqual("groupdel wendy" in recorded, gid == 12345)

    def fake_test(self, root, code, *, resist=False, hang=False):
        child_pid = root / "child.pid"
        fake_test = root / "test-binary"
        # Eight seconds is a final bound even if the launcher under test is
        # broken. The test never signals a reparented/reusable numeric child PID.
        child_source = "import signal,time; "
        if resist:
            child_source += "signal.signal(signal.SIGTERM, signal.SIG_IGN); "
        child_source += "time.sleep(8)"
        fake_test.write_text(
            f"#!{sys.executable}\n"
            "import os, subprocess, sys, time\n"
            f"child = subprocess.Popen([sys.executable, '-c', {child_source!r}])\n"
            # Let the child install its TERM handler before reporting its PID.
            "time.sleep(0.1)\n"
            f"with open({str(child_pid)!r}, 'w') as out: out.write(str(child.pid))\n"
            + ("time.sleep(8)\n" if hang else "")
            + f"sys.exit({code})\n"
        )
        fake_test.chmod(0o700)
        return fake_test, child_pid

    def assert_not_running(self, child_pid):
        state = subprocess.run(["ps", "-o", "stat=", "-p", str(child_pid)], capture_output=True, text=True, timeout=2)
        self.assertTrue(not state.stdout.strip() or state.stdout.strip().startswith("Z"), state.stdout)

    def test_completed_failed_and_term_resistant_descendants_drain(self):
        for code, resist in ((0, False), (17, False), (0, True)):
            with self.subTest(code=code, resist=resist), tempfile.TemporaryDirectory() as directory:
                fake_test, child_pid = self.fake_test(Path(directory), code, resist=resist)
                # An unrelated private child must survive every group signal.
                unrelated = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(15)"])
                try:
                    result = subprocess.run([sys.executable, str(LAUNCHER), "--timeout", "5", "--", str(fake_test)],
                                            capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, code, result.stderr)
                    self.assert_not_running(int(child_pid.read_text()))
                    self.assertIsNone(unrelated.poll())
                finally:
                    unrelated.terminate()
                    unrelated.wait(timeout=2)

    def test_group_leader_stays_live_until_final_signal(self):
        with tempfile.TemporaryDirectory() as directory:
            fake_test, child_pid = self.fake_test(Path(directory), 0, resist=True)
            real_killpg = os.killpg
            signals = []

            def observe(pid, sig):
                signals.append(sig)
                if sig == signal.SIGKILL:
                    state = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True, timeout=2)
                    self.assertTrue(state.stdout.strip(), "guardian disappeared before final group signal")
                    self.assertFalse(state.stdout.strip().startswith("Z"), "TERM killed the ownership guardian")
                real_killpg(pid, sig)

            with patch.object(owned_media.os, "killpg", side_effect=observe):
                self.assertEqual(owned_media.run([str(fake_test)], 5), 0)
            self.assertEqual(signals, [signal.SIGTERM, signal.SIGKILL])
            self.assert_not_running(int(child_pid.read_text()))

    def test_timeout_drains_running_command(self):
        with tempfile.TemporaryDirectory() as directory:
            fake_test, child_pid = self.fake_test(Path(directory), 0, resist=True, hang=True)
            started = time.monotonic()
            result = subprocess.run([sys.executable, str(LAUNCHER), "--timeout", "0.5", "--", str(fake_test)],
                                    capture_output=True, text=True, timeout=8)
            self.assertEqual(result.returncode, 124, result.stderr)
            self.assertLess(time.monotonic() - started, 7)
            self.assert_not_running(int(child_pid.read_text()))

    def test_helper_term_drains_running_command(self):
        with tempfile.TemporaryDirectory() as directory:
            fake_test, child_pid = self.fake_test(Path(directory), 0, resist=True, hang=True)
            process = subprocess.Popen([sys.executable, str(LAUNCHER), "--timeout", "5", "--", str(fake_test)],
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                deadline = time.monotonic() + 3
                while not child_pid.exists():
                    if time.monotonic() >= deadline:
                        self.fail("fixture child did not start")
                    time.sleep(0.01)
                process.terminate()  # Popen-owned, unreaped direct child.
                _, stderr = process.communicate(timeout=7)
                self.assertEqual(process.returncode, 143, stderr)
                self.assert_not_running(int(child_pid.read_text()))
            finally:
                if process.poll() is None:
                    process.terminate()
                    process.communicate(timeout=7)


if __name__ == "__main__":
    unittest.main()
