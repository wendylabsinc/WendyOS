import json
import os
import signal
import time
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest

from fake_mcp_server import TOOLS
from mcp_client import StdioMCP
import eval_wendy

HERE = Path(__file__).resolve().parent
FAKE = str(HERE / "fake_mcp_server.py")
SAFE = [["device", "list"], ["docs"]]
FAKE_REAL = """#!/bin/sh
echo run > "$FAKE_MARKER"
if [ -n "$FAKE_STDIN" ]; then
  if [ -t 0 ]; then tty=tty; else tty=notty; fi
  echo "$tty:$(cat)" > "$FAKE_STDIN"
fi
if [ -n "$FAKE_SLEEP" ]; then echo $$ > "$FAKE_SLEEP"; exec sleep 30; fi
printf '{"argv":['
sep=
for a in "$@"; do printf '%s"%s"' "$sep" "$a"; sep=,; done
printf '],"chat":"%s"}\\n' "$WENDY_CHAT_MCP_COMMAND"
"""


class WrapperTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "tools.json").write_text(json.dumps(TOOLS))
        (self.root / "cli_safe.json").write_text(json.dumps({"read_only": SAFE}))
        self.marker = self.root / "marker"
        self.cli_log = self.root / "cli.jsonl"
        self.proxy_log = self.root / "proxy.jsonl"
        self.shim = eval_wendy.write_shim(self.root)
        fake = self.root / "fake-wendy"
        fake.write_text(FAKE_REAL)
        fake.chmod(0o755)
        self.fake = str(fake)

    def env(self, real=None, **overrides):
        scenario = {"real_wendy": real or self.fake, "tools": str(self.root / "tools.json"),
                    "canned_dir": str(self.root / "canned"), "canned_set": "s", "canned": [],
                    "allow_real": [], "block": [], "context": {}, "proxy_log": str(self.proxy_log),
                    "cli_log": str(self.cli_log), "cli_safe": str(self.root / "cli_safe.json"),
                    "shim": str(self.shim), **overrides}
        path = self.root / "scenario.json"
        path.write_text(json.dumps(scenario))
        return {**os.environ, "WENDY_EVAL_SCENARIO": str(path), "FAKE_MARKER": str(self.marker), "FAKE_SLEEP": "",
                "PYTHONPATH": "/nonexistent"}

    def run_shim(self, *argv, **overrides):
        return subprocess.run([str(self.shim), *argv], env=self.env(**overrides), cwd="/",
                              capture_output=True, text=True, timeout=20)

    def records(self):
        if not self.cli_log.exists():
            return []
        return [json.loads(line) for line in self.cli_log.read_text().splitlines()]

    def capture(self, words, stdout, code):
        folder = self.root / "canned" / "s" / "cli"
        folder.mkdir(parents=True, exist_ok=True)
        (folder / "c.json").write_text(json.dumps(
            {"argv_prefix": words, "stdout": stdout, "stderr": "", "exit_code": code}))

    def test_shim_file(self):
        text = self.shim.read_text()
        self.assertTrue(text.startswith("#!/bin/sh\nexec "))
        self.assertIn(" -E ", text)
        self.assertTrue(self.shim.stat().st_mode & stat.S_IXUSR)

    def test_shim_routes_mcp_serve_to_proxy(self):
        with StdioMCP([str(self.shim), "mcp", "serve"], env=self.env(real=FAKE), timeout=10) as client:
            client.initialize()
            self.assertTrue(client.list_tools())
            client.call("read_thing", {})
        self.assertTrue(self.proxy_log.exists())
        self.assertEqual(json.loads(self.proxy_log.read_text().splitlines()[0])["tool"], "read_thing")

    def test_safe_cli_passes_through_and_is_recorded(self):
        result = self.run_shim("--json", "device", "list")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["argv"], ["--json", "device", "list"])
        record = self.records()[0]
        self.assertEqual(record["argv"], ["--json", "device", "list"])
        self.assertIs(record["canned"], False)
        self.assertEqual(record["exit_code"], 0)
        self.assertIsInstance(record["t"], float)

    def test_unsafe_cli_is_canned_by_default(self):
        result = self.run_shim("wifi", "connect", "Lab5G")
        self.assertEqual((result.returncode, result.stdout), (0, ""))
        self.assertFalse(self.marker.exists())
        record = self.records()[0]
        self.assertIs(record["canned"], True)
        self.assertIs(record["captured"], False)
        self.capture(["wifi", "connect"], "connected\n", 3)
        result = self.run_shim("wifi", "connect", "Lab5G")
        self.assertEqual((result.returncode, result.stdout), (3, "connected\n"))
        self.assertFalse(self.marker.exists())
        self.assertIs(self.records()[1]["captured"], True)

    def test_whole_word_allowlist(self):
        for argv in (["device", "listx"], ["device", "list-all"], ["device"]):
            self.assertFalse(eval_wendy.cli_allowed(argv, SAFE), argv)
        self.assertTrue(eval_wendy.cli_allowed(["--json", "device", "list", "-x"], SAFE))
        self.assertTrue(eval_wendy.cli_allowed(["--version"], SAFE))
        self.assertTrue(eval_wendy.cli_allowed(["--help"], SAFE))
        self.assertFalse(eval_wendy.cli_allowed(["--version", "x"], SAFE))
        self.assertFalse(eval_wendy.cli_allowed([], SAFE))

    def test_block_matchers_can_a_safe_command(self):
        for matcher in ({"cli": "device list"}, {"shell": r"wendy .*device list"}):
            self.marker.unlink(missing_ok=True)
            result = self.run_shim("device", "list", block=[{**matcher, "agents": ["x"]}])
            self.assertEqual(result.returncode, 0)
            self.assertFalse(self.marker.exists(), matcher)
            self.assertIs(self.records()[-1]["canned"], True)

    def test_passthrough_cli_runs_for_real(self):
        result = self.run_shim("wifi", "connect", "Lab5G", passthrough=True, cli_canned=[["wifi", "connect"]])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["argv"], ["wifi", "connect", "Lab5G"])
        self.assertTrue(self.marker.exists())
        self.assertIs(self.records()[0]["canned"], False)

    def test_passthrough_cli_blocks_guarded_commands(self):
        block = [{"cli": "device update", "block": True},
                 {"cli": "device apps stop", "args_regex": "sh\\.wendy\\.own", "block": True}]
        for argv in (["device", "update"], ["device", "apps", "stop", "sh.wendy.own"]):
            result = self.run_shim(*argv, passthrough=True, block=block)
            self.assertEqual((result.returncode, result.stdout), (0, ""), argv)
            self.assertFalse(self.marker.exists(), argv)
            self.assertIs(self.records()[-1]["canned"], True)
        result = self.run_shim("device", "apps", "stop", "other", passthrough=True, block=block)
        self.assertTrue(self.marker.exists())
        self.assertIs(self.records()[-1]["canned"], False)

    def test_help_runs_the_real_help_command(self):
        # Every help lookup becomes `wendy help WORDS`: cobra's help command only resolves and prints.
        # Rebuilding as `WORDS --help` was not enough: `device ros2 exec` stops flag parsing at its
        # first word and would forward --help to the device, and a value flag can swallow --help
        # (`os update --artifact-url --help` runs the update).
        cases = ((["os", "install", "--device-type", "rpi", "--force", "--help"], ["help", "os", "install"]),
                 (["--device", "X", "os", "update", "--artifact-url", "--help"], ["help", "os", "update"]),
                 (["--json", "vm", "rm", "box", "--help"], ["help", "vm", "rm", "box"]),
                 (["device", "ros2", "exec", "run", "demo_nodes_cpp", "talker", "--help"],
                  ["help", "device", "ros2", "exec", "run", "demo_nodes_cpp", "talker"]),
                 (["os", "update", "-h"], ["help", "os", "update"]),
                 (["wifi", "connect", "-h"], ["help", "wifi", "connect"]))
        for argv, real in cases:
            self.marker.unlink(missing_ok=True)
            with self.subTest(argv=argv):
                result = self.run_shim(*argv, cli_canned=[["vm"]], block=[{"cli": "os update", "block": True}])
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout)["argv"], real)
                record = self.records()[-1]
                self.assertEqual((record["argv"], record["canned"], record["help"]), (argv, False, True))

    def test_other_help_shapes_stay_canned(self):
        for argv in (["os", "install", "--help=false"], ["os", "install", "-help"],
                     ["device", "shell", "--", "nmcli", "--help"], ["__t234-write", "--help"]):
            with self.subTest(argv=argv):
                result = self.run_shim(*argv)
                self.assertEqual(result.returncode, 0)
                self.assertFalse(self.marker.exists())
                self.assertIs(self.records()[-1]["canned"], True)

    def capture_after(self, words, before, after, actions):
        folder = self.root / "canned" / "s" / "cli"
        folder.mkdir(parents=True, exist_ok=True)
        (folder / ("-".join(words) + ".json")).write_text(json.dumps(
            {"argv_prefix": words, "stdout": before, "stderr": "", "exit_code": 0,
             "after": {"actions": actions, "stdout": after, "stderr": "", "exit_code": 0}}))

    def test_cli_read_after_an_mcp_canned_action_shows_the_after_state(self):
        self.capture_after(["device", "apps", "list"], "RUNNING\n", "STOPPED\n", ["tool:container_stop"])
        listed = dict(cli_canned=[["device", "apps", "list"]])
        self.assertEqual(self.run_shim("device", "apps", "list", **listed).stdout, "RUNNING\n")
        # A real (not canned) call to the action's tool changes nothing.
        self.proxy_log.write_text(json.dumps({"seq": 1, "tool": "container_stop", "canned": False}) + "\n")
        self.assertEqual(self.run_shim("device", "apps", "list", **listed).stdout, "RUNNING\n")
        with self.proxy_log.open("a") as log:
            log.write(json.dumps({"seq": 2, "tool": "container_stop", "canned": True}) + "\n")
        self.assertEqual(self.run_shim("device", "apps", "list", **listed).stdout, "STOPPED\n")

    def test_cli_read_after_a_cli_canned_action_shows_the_after_state(self):
        self.capture_after(["vm", "list"], "running\n", "stopped\n", ["tool:simulator_stop", "cli:vm stop"])
        listed = dict(cli_canned=[["vm", "list"]])
        self.assertEqual(self.run_shim("vm", "list", **listed).stdout, "running\n")
        self.run_shim("vm", "--yes", "stop", "bench-sim", **listed)  # canned; flags don't hide the words
        self.assertIs(self.records()[-1]["canned"], True)
        self.assertEqual(self.run_shim("--json", "vm", "list", **listed).stdout, "stopped\n")

    def test_cli_capture_lookup_skips_flags(self):
        self.capture(["vm", "stop"], "Stopped VM.\n", 0)
        for argv in (["vm", "--yes", "stop", "bench-sim"], ["--device", "X", "vm", "stop", "--force", "bench-sim"]):
            with self.subTest(argv=argv):
                self.assertEqual(self.run_shim(*argv).stdout, "Stopped VM.\n")

    def test_cli_canned_cans_an_allowlisted_command_and_renders_placeholders(self):
        self.capture(["device", "list"], "Connecting to simulator {{vm_name}}...\n[\"{{device}}\"]\n", 0)
        result = self.run_shim("device", "list", "--json", cli_canned=[["device", "list"]],
                               context={"device": "vm:wendy-eval-1", "vm_name": "wendy-eval-1"})
        self.assertEqual(result.returncode, 0)
        self.assertFalse(self.marker.exists())
        self.assertEqual(result.stdout, 'Connecting to simulator wendy-eval-1...\n["vm:wendy-eval-1"]\n')
        self.assertEqual((self.records()[-1]["canned"], self.records()[-1]["captured"]), (True, True))
        # Other allowlisted commands still run for real.
        self.assertEqual(self.run_shim("docs", cli_canned=[["device", "list"]]).returncode, 0)
        self.assertTrue(self.marker.exists())

    def test_chat_is_exec_with_mcp_override(self):
        result = self.run_shim("chat")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["chat"], str(self.shim))
        self.assertEqual(self.records(), [])

    def test_chat_refuses_a_binary_without_the_mcp_hook(self):
        # A chat that predates WENDY_CHAT_MCP_COMMAND starts its own `mcp serve`, past the proxy.
        old = self.root / "old-wendy"
        old.write_text(FAKE_REAL.replace('"$WENDY_CHAT_MCP_COMMAND"', '"none"'))
        old.chmod(0o755)
        self.assertNotIn(b"WENDY_CHAT_MCP_COMMAND", old.read_bytes())
        result = self.run_shim("chat", "--prompt", "hi", real_wendy=str(old))
        self.assertEqual((result.returncode, result.stdout), (2, ""))
        self.assertEqual(len(result.stderr.strip().splitlines()), 1, result.stderr)
        self.assertIn(str(old), result.stderr)
        self.assertIn("predates WENDY_CHAT_MCP_COMMAND", result.stderr)
        self.assertFalse(self.marker.exists())
        self.assertEqual(self.records(), [])
        self.assertTrue(eval_wendy.has_chat_hook(self.fake))
        empty = self.root / "empty"
        empty.write_bytes(b"")
        self.assertFalse(eval_wendy.has_chat_hook(empty))
        self.assertFalse(eval_wendy.has_chat_hook(self.root / "missing"))
        # The same binary still serves every other route.
        self.assertEqual(self.run_shim("device", "list", real_wendy=str(old)).returncode, 0)
        self.assertTrue(self.marker.exists())

    def test_missing_scenario_env_refuses(self):
        env = {k: v for k, v in os.environ.items() if k != "WENDY_EVAL_SCENARIO"}
        result = subprocess.run([str(self.shim), "device", "list"], env=env, capture_output=True,
                                text=True, timeout=20)
        self.assertEqual(result.returncode, 2)
        self.assertIn("eval wrapper used outside a scenario", result.stderr)

    def test_real_runs_get_no_stdin(self):
        env = {**self.env(), "FAKE_STDIN": str(self.root / "stdin")}
        result = subprocess.run([str(self.shim), "device", "list"], env=env, cwd="/",
                                input="typed\n", capture_output=True, text=True, timeout=20)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.root / "stdin").read_text().strip(), "notty:")

    def test_canned_call_is_logged_even_if_stdout_is_closed(self):
        self.capture(["wifi", "connect"], "connected\n", 3)
        r, w = os.pipe()
        os.close(r)
        proc = subprocess.run([str(self.shim), "wifi", "connect", "X"], env=self.env(), cwd="/",
                              stdout=w, stderr=subprocess.PIPE, text=True, timeout=20)
        os.close(w)
        self.assertEqual(proc.returncode, 3)
        self.assertNotIn("Traceback", proc.stderr)
        self.assertEqual(self.records()[0]["exit_code"], 3)

    def test_interrupted_real_run_is_logged(self):
        sleeper = self.root / "pid"
        env = self.env()
        env["FAKE_SLEEP"] = str(sleeper)
        proc = subprocess.Popen([str(self.shim), "device", "list"], env=env, cwd="/",
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(proc.kill)
        deadline = time.time() + 10
        while not (sleeper.exists() and sleeper.read_text().strip()) and time.time() < deadline:
            time.sleep(0.05)
        pid = int(sleeper.read_text())
        proc.send_signal(signal.SIGTERM)
        proc.communicate(timeout=10)
        self.assertEqual(self.records()[0]["exit_code"], 143)
        with self.assertRaises(OSError):
            for _ in range(50):
                os.kill(pid, 0)
                time.sleep(0.1)
            os.kill(pid, 0)

    def test_internal_errors_stay_out_of_agent_output(self):
        (self.root / "cli_safe.json").write_text("{not json")
        result = self.run_shim("device", "list")
        self.assertEqual((result.returncode, result.stdout, result.stderr), (0, "", ""))
        self.assertFalse(self.marker.exists())
        self.assertIn("error", self.records()[0])

    def test_bad_config_fails_closed(self):
        scenario = self.root / "scenario.json"
        env = self.env()
        scenario.write_text("{corrupt")
        result = subprocess.run([str(self.shim), "device", "list"], env=env, capture_output=True,
                                text=True, timeout=20)
        self.assertEqual(result.returncode, 2)
        self.assertEqual(len(result.stderr.strip().splitlines()), 1)
        result = self.run_shim("device", "list", real_wendy="fake-wendy")
        self.assertEqual(result.returncode, 2)
        link = self.root / "link"
        link.symlink_to(self.shim)
        self.assertEqual(self.run_shim("device", "list", real_wendy=str(link)).returncode, 2)
        self.assertEqual(self.run_shim("device", "list", shim="").returncode, 2)
        self.assertEqual(self.run_shim("device", "list", real_wendy=str(self.root / "missing")).returncode, 2)
        plain = self.root / "plain"
        plain.write_text("x")
        plain.chmod(0o644)
        self.assertEqual(self.run_shim("device", "list", real_wendy=str(plain)).returncode, 2)
        self.assertEqual(self.run_shim("device", "list", real_wendy=str(self.root)).returncode, 2)
        self.assertFalse(self.marker.exists())

    def test_risky_shapes_never_run_real(self):
        for argv in (["--device", "X", "wifi", "connect", "Lab5G"], ["wifi", "connect", "--help=false"],
                     ["mcp", "setup"]):
            result = self.run_shim(*argv)
            self.assertEqual(result.returncode, 0, argv)
            self.assertFalse(self.marker.exists(), argv)
            self.assertIs(self.records()[-1]["canned"], True)

    def test_closed_stdout_and_stderr_keep_canned_exit_code(self):
        self.capture(["wifi", "connect"], "connected\n", 3)
        script = 'exec "$0" wifi connect X >&- 2>&-'
        proc = subprocess.run(["sh", "-c", script, str(self.shim)], env=self.env(), cwd="/", timeout=20)
        self.assertEqual(proc.returncode, 3)
        log = self.root / "errlog"
        proc = subprocess.run(["sh", "-c", f'exec "$0" wifi connect X >&- 2>"{log}"', str(self.shim)],
                              env=self.env(), cwd="/", timeout=20)
        self.assertEqual(proc.returncode, 3)
        self.assertNotIn("Traceback", log.read_text())

    def test_ignored_sigterm_stays_ignored(self):
        sleeper = self.root / "pid"
        env = self.env()
        env["FAKE_SLEEP"] = str(sleeper)
        proc = subprocess.Popen(["sh", "-c", 'trap "" TERM; exec "$0" device list', str(self.shim)],
                                env=env, cwd="/", stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(proc.kill)
        deadline = time.time() + 10
        while not (sleeper.exists() and sleeper.read_text().strip()) and time.time() < deadline:
            time.sleep(0.05)
        pid = int(sleeper.read_text())
        proc.send_signal(signal.SIGTERM)
        time.sleep(0.5)
        self.assertIsNone(proc.poll())  # the shim did not take over a signal that was ignored
        os.kill(pid, signal.SIGKILL)
        proc.communicate(timeout=10)
        self.assertEqual(self.records()[0]["exit_code"], 137)


if __name__ == "__main__":
    unittest.main()
