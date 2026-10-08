import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock
from types import SimpleNamespace

import benchmark as b
import fixtures
import robot_fixtures as robots


class MetricsTests(unittest.TestCase):
    def test_failed_cleanup_keeps_vm_recovery_data_but_removes_credentials(self):
        with tempfile.TemporaryDirectory() as directory:
            state = Path(directory)
            (state / "vms").mkdir()
            (state / "vms/disk").write_text("fixture disk")
            (state / "config.json").write_text("private auth")
            (state / "tls").mkdir()
            (state / "tls/session").write_text("private session")
            b.discard_device_state(state, False)
            self.assertEqual(list(state.iterdir()), [state / "vms"])
            self.assertEqual((state / "vms/disk").read_text(), "fixture disk")

    def test_host_pin_watchdog_stops_attempt_when_shared_trust_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            config = path / "config.json"
            config.write_text(json.dumps({"devicePins": {"existing": {"org": 1}}}))
            guard = b.host_pin_guard(config)
            result = b.run_process([sys.executable, "-c",
                "import pathlib,time; pathlib.Path('config.json').write_text('{}'); time.sleep(10)"],
                path, path / "agent", 5, state_guard=guard)
            self.assertTrue(result["host_state_changed"])
            self.assertLess(result["seconds"], 4)
            self.assertFalse(guard())

    def test_private_device_state_starts_without_host_pins_or_vms(self):
        state = b.isolated_device_state({"kind": "simulator"})
        try:
            value = json.loads((state / "config.json").read_text())
            self.assertNotIn("devicePins", value)
            self.assertNotIn("auth", value)
            self.assertFalse((state / "vms").exists())
            self.assertEqual((state / "config.json").stat().st_mode & 0o777, 0o600)
        finally:
            import shutil
            shutil.rmtree(state)

    def test_robot_verification_cannot_provision_an_unready_runtime(self):
        args = SimpleNamespace(task="create-g1-simulator", vm_name="wendy-eval-test")
        calls = []
        def cli(args, *argv, **kwargs):
            calls.append(argv)
            if argv == ("--json", "vm", "list"):
                return json.dumps([{"name": args.vm_name, "state": "running"}])
            raise AssertionError("device connection would repair the runtime")
        with mock.patch.object(fixtures, "cli", side_effect=cli), mock.patch.object(
                fixtures.robot_fixtures, "verify", side_effect=RuntimeError("not ready")):
            with self.assertRaisesRegex(RuntimeError, "not ready"):
                fixtures.verify(args)
        self.assertEqual(calls, [("--json", "vm", "list")])

    def test_cleanup_does_not_delete_resources_when_setup_rejects_collision(self):
        with tempfile.TemporaryDirectory() as directory:
            args = SimpleNamespace(state=Path(directory)/"absent-state", task="create-simulator")
            with mock.patch.object(fixtures, "cli", side_effect=AssertionError("must not remove anything")):
                fixtures.cleanup(args)

    def test_missing_fixture_is_visible_and_cannot_run(self):
        task = {"id": "flash", "fixture": "external", "destructive": True,
                "prompt": "install", "environments": ["physical"], "requires": ["sd-card"]}
        targets = {"pi": {"kind": "physical", "device": "192.0.2.1", "capabilities": ["sd-card"]}}
        self.assertEqual(b.plan_coverage([task], targets, ["flash"], ["pi"])[0]["status"], "unconfigured")
        self.assertEqual(b.plan_coverage([task], {}, ["flash"], [])[0]["target"], None)
        fixture = {phase: ["trusted-fixture", phase] for phase in ("prepare", "verify", "cleanup")}
        fixture["prompt_context"] = "board identity and drive serial"
        targets["pi"]["fixtures"] = {"flash": fixture}
        self.assertIn("disposable_target", b.task_for_target(task, targets["pi"])[1])
        fixture["disposable_target"] = {"serial": "disposable-test-medium"}
        resolved, reason = b.task_for_target(task, targets["pi"])
        self.assertIsNone(reason)
        self.assertEqual(resolved["verify"], ["trusted-fixture", "verify"])
        self.assertNotIn("verify", task)

    def test_robot_verifier_rejects_stale_wrong_or_fabricated_samples(self):
        def samples(kind):
            count = 12 if kind == "go2" else 29
            return {"odom": [{"stamp": 100+i, "age": .1, "frame": "odom", "child": "base_link",
                               "position": [0,0,.5], "orientation": [0,0,0,1]} for i in range(5)],
                    "joints": [{"stamp": 100+i, "age": .1, "frame": "", "names": [str(n) for n in range(count)],
                                "positions": [0]*count} for i in range(5)]}
        for kind in ("go2", "g1"):
            robots.validate_samples(samples(kind), kind)
        for change in (lambda s: s["odom"][1].update(stamp=100),
                       lambda s: s["odom"][0].update(age=9),
                       lambda s: s["odom"][0].update(orientation=[0,0,0,0]),
                       lambda s: s["joints"][0].update(positions=[float("nan")]*12),
                       lambda s: s["joints"][0].update(names=["one"]*12)):
            value = samples("go2")
            change(value)
            with self.assertRaises(RuntimeError):
                robots.validate_samples(value, "go2")
        with self.assertRaises(RuntimeError):
            robots.validate_samples(samples("go2"), "g1")

    def test_codex_cache_is_subset_and_tool_events_are_deduplicated(self):
        m = b.metrics("codex", [
            {"type": "item.started", "item": {"type": "mcp_tool_call", "id": "a"}},
            {"type": "item.completed", "item": {"type": "mcp_tool_call", "id": "a"}},
            {"type": "turn.completed", "usage": {"input_tokens": 100, "cached_input_tokens": 80, "output_tokens": 20}},
        ])
        self.assertEqual(m["tool_calls"], 1)
        self.assertEqual(m["tokens"]["input_tokens"], 100)
        self.assertAlmostEqual(b.estimate_cost(m, {"input": 10, "cached_input": 1, "output": 50}), .00128)

    def test_claude_uses_final_total_and_adds_disjoint_cache(self):
        m = b.metrics("claude", [
            {"type": "assistant", "message": {"usage": {"input_tokens": 1000, "output_tokens": 100}}},
            {"type": "result", "usage": {"input_tokens": 10, "cache_read_input_tokens": 80,
                                         "cache_creation_input_tokens": 20, "output_tokens": 5},
             "total_cost_usd": .01, "is_error": False},
        ])
        self.assertEqual(m["tokens"]["input_tokens"], 110)
        self.assertEqual(m["tokens"]["output_tokens"], 5)
        self.assertTrue(m["usage_complete"])

    def test_codex_cache_writes_and_reasoning_are_subsets(self):
        u = b.normalize_usage({"input_tokens": 100, "cached_input_tokens": 70,
                               "cache_write_input_tokens": 20, "output_tokens": 30,
                               "reasoning_output_tokens": 15})
        self.assertEqual(u["cache_write_tokens"], 20)
        self.assertEqual(u["reasoning_tokens"], 15)
        self.assertEqual(u["input_tokens"] + u["output_tokens"], 130)

    def test_missing_and_partial_usage_are_not_free(self):
        for events in ([{"type": "done"}], [
            {"type": "usage", "usage": {"input_tokens": 20, "output_tokens": 5, "complete": False}},
            {"type": "done"},
        ]):
            m = b.metrics("wendy", events)
            self.assertFalse(m["usage_complete"])
            self.assertIsNone(b.estimate_cost(m, {"input": 1, "output": 1}))

    def test_malformed_usage_is_unknown(self):
        for value in (None, "10", -1, True):
            self.assertIsNone(b.normalize_usage({"input_tokens": value, "output_tokens": 1}, anthropic=True))

    def test_wendy_counts_all_rounds_and_children(self):
        m = b.metrics("wendy", [
            {"type": "usage", "usage": {"input_tokens": 100, "output_tokens": 10, "complete": True}},
            {"type": "usage", "agent_id": "child", "usage": {"input_tokens": 50, "output_tokens": 5, "complete": True}},
            {"type": "tool_start", "id": "a"},
            {"type": "tool_start", "id": "a", "agent_id": "child"},
            {"type": "done"},
        ])
        self.assertEqual(m["tokens"]["input_tokens"], 150)
        self.assertEqual(m["tool_calls"], 2)

    def test_failed_turn_can_still_have_complete_usage(self):
        m = b.metrics("wendy", [
            {"type": "usage", "usage": {"input_tokens": 100, "output_tokens": 10, "complete": True}},
            {"type": "error", "text": "round limit"},
        ])
        self.assertTrue(m["usage_complete"])
        self.assertEqual(m["agent_errors"], 1)

    def test_failures_count_in_cost_per_success(self):
        def row(success, status="failed", complete=True):
            return {"agent": "a", "target": "sim", "task": "deploy", "status": status, "success": success,
                    "task_seconds": 10, "metrics": {"usage_complete": complete,
                    "tokens": {"input_tokens": 100, "output_tokens": 20}}}
        rows = [row(True, "passed"), row(False), row(False, "setup_error")]
        result = b.summary(rows)[0]
        self.assertEqual(result["unassisted_success_rate"], .5)
        self.assertEqual(result["tokens_per_success_including_failures"], 240)
        self.assertEqual(result["setup_errors"], 1)
        rows.append(row(False, complete=False))
        self.assertIsNone(b.summary(rows)[0]["tokens_per_success_including_failures"])

    def test_timeout_retains_partial_logs(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            result = b.run_process([sys.executable, "-c", "import time; print('partial', flush=True); time.sleep(20)"],
                                   path, path / "agent", .15)
            self.assertTrue(result["timed_out"])
            self.assertIn("partial", (path / "agent.stdout").read_text())

    def test_adapter_fixture_trial_end_to_end(self):
        # A fake agent validates orchestration, without being reported as a model benchmark.
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            command = [sys.executable, "-c", "import pathlib,json; pathlib.Path('answer').write_text('ok'); print(json.dumps({'type':'done'}))"]
            config = {"agents": {"fake": {"adapter": "wendy", "model": "fixture", "command": command}},
                      "targets": {"sim": {"kind": "simulator", "device": "vm:test"}}}
            task = {"id": "fixture", "prompt": "make answer", "prepare": [sys.executable, "-c", "pass"],
                    "verify": [sys.executable, "-c", "from pathlib import Path; assert Path('answer').read_text()=='ok'"],
                    "cleanup": [sys.executable, "-c", "pass"]}
            result = b.trial(config, "fake", "sim", task, path, sys.executable)
            self.assertTrue(result["success"])
            self.assertFalse(result["metrics"]["usage_complete"])
            self.assertTrue(result["cleanup_ok"])


if __name__ == "__main__":
    unittest.main()
