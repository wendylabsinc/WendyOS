"""Exercise the real controller/replay paths with a stalled CLI transport."""

import importlib.util
import json
import pathlib
import signal
import sys
import tempfile
import threading
import time
import types
import unittest
from unittest.mock import patch

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1] / "control"))
import probes
from functional import CleanupError
from test_functional import Fixture

spec = importlib.util.spec_from_file_location(
    "scout_controller_test", pathlib.Path(probes.__file__).with_name("main.py")
)
controller = importlib.util.module_from_spec(spec)
# Generated Agent clients exist in the image. No live RPC belongs in this test.
with patch.dict(sys.modules, {"local": types.SimpleNamespace(Local=object)}), patch.object(signal, "signal"):
    spec.loader.exec_module(controller)


class SessionCleanupTests(unittest.TestCase):
    def scout(self, root):
        s = controller.Scout()
        s.local = Fixture()
        s.probes = probes.Probes.__new__(probes.Probes)
        s.probes.local = s.local
        s.probes.scratch = "sh.wendy.scout.scratch.0123456789"
        s.probes.socket = "/run/wendy/agent/agent.sock"
        s.local.generation = "fixture"
        s.probes.agent = "test-agent"
        s.probes.deadline = None
        s.probes.cancelled = lambda: False
        s.versions = s.probes.versions = {
            k: {"tag": k, "binary": "/fixture/wendy", "published_at": "2026-11-01"}
            for k in ["stable", "past", "nightly"]
        }
        s.sessions["test"] = {
            "run": root / "runs" / "test", "finished": False,
            "started": time.monotonic(), "calls": 0,
            "experiments": [{"id": "experiment-prior", "kind": "restart_persistence",
                             "version": "nightly", "outcome": "failed",
                             "completed": True, "cleanup_verified": True}],
        }
        return s

    def test_finish_waits_for_timed_out_replay_and_prevents_later_workflows(self):
        entered, release, finished = threading.Event(), threading.Event(), threading.Event()
        errors, finish_errors, mutations = [], [], []
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            s = self.scout(root)
            original_cleanup = s.local.cleanup_scratch

            def cleanup():
                if s.local.state != "ABSENT":
                    mutations.append("delete")
                original_cleanup()

            s.local.cleanup_scratch = cleanup

            def stalled_command(*args, **kwargs):
                s.local.state, s.local.starts = "RUNNING", 1
                entered.set()
                if not release.wait(2):
                    raise AssertionError("test did not release stalled command")
                raise TimeoutError("in-flight CLI transport deadline")

            def replay():
                try:
                    s.tool({"session": "test", "operation": "reproduce",
                            "kind": "restart_persistence", "version": "stable"})
                except Exception as e:
                    errors.append(e)

            def finish():
                try:
                    s.finish({"session": "test", "exit": -9, "proof": False})
                except Exception as e:
                    finish_errors.append(e)
                finally:
                    finished.set()

            with (
                patch.object(probes, "command", side_effect=stalled_command) as command,
                patch.object(controller, "STATE", root),
                patch.object(controller, "event"),
            ):
                a = threading.Thread(target=replay)
                b = threading.Thread(target=finish)
                a.start()
                try:
                    self.assertTrue(entered.wait(1))
                    b.start()
                    until = time.monotonic() + 1
                    while not s.sessions["test"]["finished"] and time.monotonic() < until:
                        time.sleep(0.001)
                    self.assertTrue(s.sessions["test"]["finished"])
                    self.assertFalse(finished.is_set())
                    self.assertEqual(mutations, [])  # no concurrent delete
                finally:
                    release.set()
                    a.join(2)
                    if b.ident is not None:
                        b.join(2)
                self.assertFalse(a.is_alive() or b.is_alive())
                self.assertEqual(command.call_count, 1)  # no next replay/workflow
            self.assertIsInstance(errors[0], TimeoutError)
            self.assertEqual(finish_errors, [])
            self.assertEqual(mutations, ["delete"])
            self.assertEqual(s.local.state, "ABSENT")
            self.assertEqual(s.sessions, {})
            self.assertFalse(s.probes.cancelled())  # next session is not poisoned
            partial = json.loads((root / "runs/test/replay.partial.json").read_text())
            self.assertFalse(partial["verified"])
            self.assertEqual(len(partial["results"]), 1)
            self.assertFalse(partial["results"][0]["completed"])

    def test_finish_cleanup_failure_still_holds_and_preserves_detail(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            s = self.scout(root)
            s.local.cleanup_bad = True
            with self.assertRaises(CleanupError):
                s.finish({"session": "test", "exit": -9, "proof": False})
            self.assertEqual(s.error, "Owned scratch cleanup failed; testing held")
            detail = json.loads((root / "runs/test/finish-cleanup.private.json").read_text())
            self.assertIn("fixture cleanup failure", detail["traceback"])
