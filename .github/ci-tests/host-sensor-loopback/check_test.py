"""Exercise host checks and harness verdicts without loading local kernel modules."""

import json
import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


FIXTURE = Path(__file__).resolve().parent
REPO = FIXTURE.parents[2]


class HostLoopbackTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.release = "7.2.0-ci"
        self.env = dict(os.environ, PATH=f"{self.bin}:{os.environ['PATH']}",
                        FIXTURE_ROOT=str(self.root), FIXTURE_RELEASE=self.release)
        (self.root / "sys/module/snd_aloop").mkdir(parents=True)
        for module in ("v4l2loopback", "snd-aloop"):
            path = self.root / "lib/modules" / self.release / f"{module}.ko"
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(b"fixture module")
        for name, content in (("sys/module/v4l2loopback/version", "0.15.4"),
                              ("sys/class/sound/card2/id", "Loopback")):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content + "\n")
        for name in ("dev/v4l2loopback", "dev/snd/pcmC2D0p", "dev/snd/pcmC2D1c"):
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.symlink_to("/dev/null")
        self.write_tool("fake-tool", '''
import os, pathlib, sys
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
root = pathlib.Path(os.environ["FIXTURE_ROOT"])
release = os.environ["FIXTURE_RELEASE"]
if name == "uname":
    print("Linux" if args == ["-s"] else release)
elif name == "id":
    print(0)
elif name == "sleep":
    pass
elif name == "timeout":
    os.execvp(args[1], args[1:])
elif name == "modinfo":
    module = args[-1]
    if module == os.environ.get("MISSING_MODULE"):
        print("modinfo: module not found", file=sys.stderr)
        sys.exit(1)
    field = args[args.index("-F") + 1]
    if field == "version":
        print(os.environ.get("MODULE_VERSION", "0.15.4"))
    elif field == "vermagic":
        print(os.environ.get("MODULE_VERMAGIC", release + " SMP preempt mod_unload aarch64"))
    else:
        if os.environ.get("WRONG_KERNEL"):
            release = "wrong-kernel"
        print(root / "lib/modules" / release / (module + ".ko"))
elif name == "modprobe":
    with (root / "modprobe.log").open("a") as output:
        output.write(" ".join(args) + "\\n")
    if args[0] == os.environ.get("UNLOADABLE_MODULE"):
        print("modprobe: invalid module format", file=sys.stderr)
        sys.exit(1)
else:
    raise AssertionError(name)
''')
        for tool in ("uname", "id", "sleep", "timeout", "modinfo", "modprobe"):
            (self.bin / tool).symlink_to("fake-tool")

    def write_tool(self, name, body):
        path = self.bin / name
        path.write_text(f"#!{sys.executable}\n" + body)
        path.chmod(0o755)
        return path

    def run_check(self, scope="video-audio", **env):
        return subprocess.run(["sh", str(FIXTURE / "check.sh"), scope, str(self.root)],
                              env=dict(self.env, **env), text=True, capture_output=True,
                              timeout=15)

    def assert_failed(self, result, text):
        self.assertNotEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn(text, result.stdout + result.stderr)
        self.assertNotIn("PASS:", result.stdout)

    def test_complete_host_support_passes(self):
        result = self.run_check()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("PASS: host sensor loopback support (video-audio)", result.stdout)
        self.assertEqual((self.root / "modprobe.log").read_text().splitlines(),
                         ["v4l2loopback devices=0 exclusive_caps=1", "snd-aloop"])

    def test_tegra_video_scope_does_not_require_audio(self):
        result = self.run_check("video", MISSING_MODULE="snd-aloop")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("snd-aloop", (self.root / "modprobe.log").read_text())

    def test_each_missing_module_fails(self):
        for module in ("v4l2loopback", "snd-aloop"):
            with self.subTest(module=module):
                self.assert_failed(self.run_check(MISSING_MODULE=module), f"{module} is missing")

    def test_module_must_belong_to_running_kernel(self):
        self.assert_failed(self.run_check(WRONG_KERNEL="1"), "outside the running kernel")

    def test_old_control_abi_fails(self):
        self.assert_failed(self.run_check(MODULE_VERSION="0.14.0"), "0.15.x control ABI")

    def test_installed_module_vermagic_must_match_running_kernel(self):
        self.assert_failed(self.run_check(MODULE_VERMAGIC="6.8.0 SMP"), "vermagic '6.8.0 SMP'")

    def test_stale_loaded_module_fails(self):
        (self.root / "sys/module/v4l2loopback/version").write_text("0.14.0\n")
        self.assert_failed(self.run_check(), "differs from installed")

    def test_each_missing_device_endpoint_fails(self):
        for name in ("dev/v4l2loopback", "dev/snd/pcmC2D0p", "dev/snd/pcmC2D1c"):
            with self.subTest(node=name):
                path = self.root / name
                path.unlink()
                self.assert_failed(self.run_check(), f"missing host character device {path}")
                path.symlink_to("/dev/null")

    def test_regular_file_is_not_a_device(self):
        path = self.root / "dev/v4l2loopback"
        path.unlink()
        path.write_text("not a character device")
        self.assert_failed(self.run_check(), "missing host character device")

    def test_missing_loopback_card_fails(self):
        (self.root / "sys/class/sound/card2/id").write_text("PhysicalCard\n")
        self.assert_failed(self.run_check(), "no ALSA Loopback card")

    def test_successful_modprobe_must_leave_audio_module_loaded(self):
        (self.root / "sys/module/snd_aloop").rmdir()
        self.assert_failed(self.run_check(), "snd-aloop is not loaded")

    def test_each_unloadable_module_fails(self):
        for module in ("v4l2loopback", "snd-aloop"):
            with self.subTest(module=module):
                self.assert_failed(self.run_check(UNLOADABLE_MODULE=module), f"{module} cannot load")


class HarnessVerdictTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.harness = self.root / "go/scripts/test-ci.sh"
        self.harness.parent.mkdir(parents=True)
        shutil.copyfile(REPO / "go/scripts/test-ci.sh", self.harness)
        fixture = self.root / ".github/ci-tests/host-sensor-loopback"
        fixture.mkdir(parents=True)
        shutil.copyfile(FIXTURE / "check.sh", fixture / "check.sh")
        shutil.copyfile(FIXTURE / "run_host_check.py", fixture / "run_host_check.py")
        self.wendy = self.root / "wendy"
        self.wendy.write_text(f"#!{sys.executable}\n" + '''
import json, os, pathlib, sys
args = sys.argv[1:]
if args[:2] == ["device", "info"]:
    print(json.dumps({"os": os.environ.get("DEVICE_OS", "wendyos"),
                      "deviceType": os.environ.get("DEVICE_TYPE", "dragonwing-iq-9075")}))
elif args[:2] == ["device", "shell"]:
    pathlib.Path(os.environ["SHELL_CALL"]).write_text(json.dumps(args))
    scope = args[-1]
    if os.environ.get("SHELL_VERDICT", "pass") == "pass":
        print("PASS: host sensor loopback support (" + scope + ")\\r")
    sys.exit(int(os.environ.get("SHELL_EXIT", "0")))
else:
    raise AssertionError(args)
''')
        self.wendy.chmod(0o755)
        self.shell_call = self.root / "shell-call.json"

    def run_harness(self, **env):
        return subprocess.run(["bash", str(self.harness), "-w", str(self.wendy),
                               "-h", "fixture.local", "-t", "host-sensor-loopback"],
                              env=dict(os.environ, SHELL_CALL=str(self.shell_call), **env),
                              text=True, capture_output=True, timeout=15)

    def test_iq9_requires_video_and_audio_on_host(self):
        result = self.run_harness()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        command = json.loads(self.shell_call.read_text())
        self.assertEqual(command[:7], ["device", "shell", "--device", "fixture.local", "--", "sh", "-c"])
        self.assertEqual(command[-2:], ["--", "video-audio"])

    def test_nonzero_shell_exit_cannot_pass_despite_pass_text(self):
        result = self.run_harness(SHELL_EXIT="7")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("1 failed", result.stdout)

    def test_missing_verdict_cannot_pass_despite_cli_success(self):
        result = self.run_harness(SHELL_VERDICT="none")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("without a successful loopback verdict", result.stdout)

    def test_unknown_wendyos_identity_fails_without_shell(self):
        for device_type in ("", "future-board"):
            with self.subTest(device_type=device_type):
                result = self.run_harness(DEVICE_TYPE=device_type)
                self.assertNotEqual(result.returncode, 0, result.stdout)
                self.assertFalse(self.shell_call.exists())

    def test_missing_os_identity_fails_without_shell(self):
        result = self.run_harness(DEVICE_OS="")
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertIn("Cannot identify device OS", result.stdout)
        self.assertFalse(self.shell_call.exists())

    def test_declared_scope_for_other_supported_boards(self):
        for device_type, scope in (("dragonwing-iq-8275", "video-audio"),
                                   ("arduino-uno-q", "video-audio"),
                                   ("jetson-agx-thor", "video"),
                                   ("jetson-agx-orin", "video")):
            with self.subTest(device_type=device_type):
                result = self.run_harness(DEVICE_TYPE=device_type)
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                self.assertEqual(json.loads(self.shell_call.read_text())[-1], scope)

    def test_known_unsupported_platforms_skip_without_shell(self):
        for env in ({"DEVICE_OS": "linux"}, {"DEVICE_OS": "darwin"}, {"DEVICE_OS": "ubuntu"},
                    {"DEVICE_TYPE": "raspberry-pi-5"}):
            with self.subTest(env=env):
                result = self.run_harness(**env)
                self.assertEqual(result.returncode, 0, result.stdout)
                self.assertIn("1 skipped", result.stdout)
                self.assertFalse(self.shell_call.exists())


class HostShellDeadlineTests(unittest.TestCase):
    def test_hung_device_shell_fails_after_bounded_deadline(self):
        spec = importlib.util.spec_from_file_location("run_host_check", FIXTURE / "run_host_check.py")
        runner = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(runner)
        with mock.patch.object(runner.subprocess, "run", side_effect=subprocess.TimeoutExpired("wendy", 60)) as run:
            with mock.patch("builtins.print"):
                self.assertEqual(runner.main(["wendy", "fixture.local", "video-audio"]), 1)
        self.assertEqual(run.call_args.kwargs["timeout"], 60)


if __name__ == "__main__":
    unittest.main()
