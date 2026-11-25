import json
from pathlib import Path
import shutil
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

    def test_summary_counts_tool_choice_and_medians(self):
        def row(outcome, tokens, seconds):
            return {"agent": "a", "target": "sim", "task": "deploy", "status": "failed", "success": False,
                    "task_seconds": 10, "tool_choice": {"outcome": outcome} if outcome else None,
                    "stages": {"agent": {"seconds": seconds}},
                    "metrics": {"usage_complete": True, "tokens": {"input_tokens": tokens, "output_tokens": 10}}}
        result = b.summary([row("right", 90, 5), row("wrong", 190, 9), row(None, 290, 7)])[0]
        self.assertEqual(result["tool_choice"], {"right": 1, "wrong": 1, "missed": 0, "asked": 0, "went_around": 0, "ungraded": 1})
        self.assertEqual(result["median_tokens_per_attempt"], 200)
        self.assertEqual(result["median_agent_seconds"], 7)

    def test_every_builtin_task_has_valid_expect(self):
        import grading
        tasks = json.loads((Path(__file__).parent / "tasks.json").read_text())
        for task in tasks:
            if task.get("fixture") == "builtin":
                with self.subTest(task=task["id"]):
                    self.assertIn("expect", task)
                    grading.validate_expect(task["expect"])

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
            self.addCleanup(shutil.rmtree, result["workspace"], True)
            self.assertTrue(result["success"])
            self.assertFalse(result["metrics"]["usage_complete"])
            self.assertTrue(result["cleanup_ok"])

    def test_staged_plugin_has_no_mcp_server_and_same_skills(self):
        with tempfile.TemporaryDirectory() as directory:
            dest = Path(directory) / "plugin"
            b.stage_plugin(b.ROOT, dest)
            self.assertNotIn("mcpServers", json.loads((dest / ".claude-plugin/plugin.json").read_text()))
            self.assertEqual(b.tree_sha256(dest / "skills"),
                             b.tree_sha256(b.ROOT / "plugins/wendy-agentic-coding/skills"))

    def test_tree_hash_changes_with_content_and_name(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "a").mkdir()
            (root / "a/f").write_text("x")
            first = b.tree_sha256(root / "a")
            (root / "a/f").write_text("y")
            second = b.tree_sha256(root / "a")
            (root / "a/f").rename(root / "a/g")
            self.assertEqual(len({first, second, b.tree_sha256(root / "a")}), 3)

    def test_harness_hash_covers_python_cli_safe_and_canned(self):
        with tempfile.TemporaryDirectory() as directory:
            here = Path(directory)
            for name, text in (("a.py", "x"), ("cli_safe.json", "{}"), ("canned/s/tool.json", "{}"),
                               ("README.md", "doc"), ("__pycache__/a.cpython.pyc", "c"), ("results/r/result.json", "r")):
                (here / name).parent.mkdir(parents=True, exist_ok=True)
                (here / name).write_text(text)
            first = b.harness_sha256(here)
            for name in ("README.md", "__pycache__/a.cpython.pyc", "results/r/result.json"):
                (here / name).write_text("changed")
            self.assertEqual(b.harness_sha256(here), first)
            seen = {first}
            for name in ("a.py", "cli_safe.json", "canned/s/tool.json", "canned/s/cli/new.json", "b.py"):
                with self.subTest(changed=name):
                    (here / name).parent.mkdir(parents=True, exist_ok=True)
                    (here / name).write_text("changed " + name)
                    value = b.harness_sha256(here)
                    self.assertNotIn(value, seen)
                    seen.add(value)
        self.assertEqual(b.harness_sha256(), b.harness_sha256(b.HERE))

    def test_skills_come_from_treatment_root(self):
        with tempfile.TemporaryDirectory() as directory:
            treatment, workspace = Path(directory, "t"), Path(directory, "w")
            skill = treatment / "plugins/wendy-agentic-coding/skills/x"
            skill.mkdir(parents=True)
            (skill / "SKILL.md").write_text("hi")
            workspace.mkdir()
            b.stage_context(workspace, "codex", "wendy", treatment_root=treatment)
            self.assertEqual((workspace / ".agents/skills/x/SKILL.md").read_text(), "hi")

    def test_extra_wendy_server_is_detected(self):
        init = {"type": "system", "subtype": "init", "mcp_servers": [
            {"name": "wendy", "status": "connected"},
            {"name": "plugin:wendy-agentic-coding:wendy", "status": "connected"}]}
        m = b.metrics("claude", [init, {"type": "result", "result": "x"}])
        self.assertEqual(b.extra_wendy_servers(m), ["plugin:wendy-agentic-coding:wendy"])
        self.assertIsNone(b.metrics("claude", [])["mcp_servers_observed"])

    def test_missing_init_event_is_unverified_not_clean(self):
        self.assertIsNone(b.metrics("claude", [{"type": "result", "result": "x"}])["mcp_servers_observed"])
        init = {"type": "system", "subtype": "init", "mcp_servers": []}
        self.assertEqual(b.metrics("claude", [init])["mcp_servers_observed"], [])
        with tempfile.TemporaryDirectory() as directory:
            command = [sys.executable, "-c", "import json; print(json.dumps({'type':'result','result':'ok'}))"]
            config = {"agents": {"fake": {"adapter": "claude", "model": "fixture", "command": command}},
                      "targets": {"sim": {"kind": "simulator", "device": "vm:test"}}}
            task = {"id": "fixture", "prompt": "p", "prepare": [sys.executable, "-c", "pass"],
                    "verify": [sys.executable, "-c", "pass"], "cleanup": [sys.executable, "-c", "pass"]}
            result = b.trial(config, "fake", "sim", task, Path(directory), sys.executable)
            self.addCleanup(shutil.rmtree, result["workspace"], True)
            self.assertEqual(result["status"], "mcp_unverified")
            self.assertFalse(result["success"])

    def test_trial_stages_a_plugin_per_attempt_and_flags_a_second_wendy_server(self):
        # Each Claude attempt loads its own plugin copy without mcpServers; a second Wendy server in the
        # init event still makes the attempt mcp_contaminated, and it never passes.
        init = {"type": "system", "subtype": "init", "mcp_servers": [
            {"name": "wendy"}, {"name": "plugin:wendy-agentic-coding:wendy"}]}
        code = ("import json, sys; print(json.dumps(%r)); "
                "print(json.dumps({'type': 'result', 'result': sys.argv[2], 'is_error': False}))" % init)
        command = [sys.executable, "-c", code, "--plugin-dir", "{plugin_dir}"]
        config = {"agents": {"fake": {"adapter": "claude", "model": "fixture", "command": command}},
                  "targets": {"sim": {"kind": "simulator", "device": "vm:test"}}}
        task = {"id": "fixture", "prompt": "p", "prepare": [sys.executable, "-c", "pass"],
                "verify": [sys.executable, "-c", "pass"], "cleanup": [sys.executable, "-c", "pass"]}
        with tempfile.TemporaryDirectory() as directory:
            rows = [b.trial(config, "fake", "sim", task, Path(directory), sys.executable) for _ in range(2)]
            for row in rows:
                self.addCleanup(shutil.rmtree, row["workspace"], True)
            plugins = []
            for row in rows:
                self.assertEqual((row["status"], row["success"]), ("mcp_contaminated", False))
                plugin = Path(row["metrics"]["final_text"])  # the plugin dir the agent was given
                self.assertEqual(plugin, Path(directory) / f"fake-sim-fixture-{row['id']}" / "plugin")
                self.assertNotIn("mcpServers", json.loads((plugin / ".claude-plugin/plugin.json").read_text()))
                self.assertEqual(b.tree_sha256(plugin / "skills"),
                                 b.tree_sha256(b.ROOT / "plugins/wendy-agentic-coding/skills"))
                plugins.append(plugin)
            self.assertNotEqual(*plugins)

    def test_treatment_git_records_none_outside_a_checkout(self):
        with tempfile.TemporaryDirectory() as directory:
            self.assertEqual(b.treatment_git(Path(directory)), (None, None))

    def test_rescore_rebuilds_grades_without_touching_originals(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            dump = lambda f, v: f.write_text(json.dumps(v))
            dump(path / "manifest.json", {"config": {"agents": {"c": {"adapter": "claude"}}}})
            attempt = path / "c-sim-deploy-http-1"
            attempt.mkdir()
            events = [{"type": "assistant", "message": {"content": [
                          {"type": "tool_use", "id": "1", "name": "mcp__wendy__run", "input": {}}]}},
                      {"type": "result", "result": "done"}]
            (attempt / "agent.stdout").write_text("\n".join(json.dumps(e) for e in events) + "\n")
            dump(attempt / "result.json", {"task": "deploy-http", "agent": "c", "target": "sim",
                                           "metrics": {"tokens": None, "usage_complete": False}, "status": "failed", "success": True, "task_seconds": 1.0, "tool_choice": None})
            before = (attempt / "result.json").read_bytes()
            tasks = [{"id": "deploy-http", "expect": {"right": [{"tool": "run"}], "wrong": []}}]
            out = b.rescore(path, tasks, [])
            self.assertEqual(out.parent, path)
            rows = [json.loads(line) for line in (out / "results.jsonl").read_text().splitlines()]
            self.assertEqual(rows[0]["tool_choice"]["outcome"], "right")
            self.assertEqual((attempt / "result.json").read_bytes(), before)
            self.assertTrue((out / "summary.json").exists() and (out / "summary.md").exists())

    def _rescore_dir(self, path):
        dump = lambda f, v: f.write_text(json.dumps(v))
        dump(path / "manifest.json", {"config": {"agents": {"c": {"adapter": "claude"}}}})
        def attempt(name, **extra):
            d = path / name
            d.mkdir()
            dump(d / "result.json", {"id": name, "task": "t", "agent": "c", "target": "sim", "metrics": {"tokens": None, "usage_complete": False},
                                     "status": "failed", "success": True, "task_seconds": 1.0, "tool_choice": None, **extra})
            return d
        return attempt

    @staticmethod
    def _stdout(directory):
        events = [{"type": "assistant", "message": {"content": [
                      {"type": "tool_use", "id": "1", "name": "mcp__wendy__run", "input": {}}]}},
                  {"type": "result", "result": "done", "usage": {"input_tokens": 5, "output_tokens": 7}}]
        (directory / "agent.stdout").write_text("\n".join(json.dumps(e) for e in events) + "\n")

    def test_rescore_keeps_saved_accounting(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            d = self._rescore_dir(path)("a")
            self._stdout(d)
            out = b.rescore(path, [{"id": "t", "expect": {"right": [{"tool": "run"}], "wrong": []}}], [])
            row = json.loads((out / "results.jsonl").read_text().splitlines()[0])
            self.assertIs(row["metrics"]["usage_complete"], False)
            self.assertIsNone(row["metrics"]["tokens"])
            self.assertEqual(row["tool_choice"]["outcome"], "right")

    def test_rescore_carries_unreadable_attempts_through(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            make = self._rescore_dir(path)
            self._stdout(make("a"))
            make("b", tool_choice={"outcome": "wrong"})
            make("c", agent="ghost")
            out = b.rescore(path, [{"id": "t", "expect": {"right": [{"tool": "run"}], "wrong": []}}], [])
            rows = {json.loads(l)["tool_choice"] and json.loads(l)["agent"] + json.loads(l)["tool_choice"]["outcome"]
                    for l in (out / "results.jsonl").read_text().splitlines()}
            self.assertEqual(rows, {"cright", "cwrong", None})

    def test_unknown_usage_charges_scenario_reserve_and_continues(self):
        import contextlib
        import io
        unknown = {"estimated_cost_usd": None, "reported_cost_usd": None}
        scenario_row_unknown = {"kind": "scenario", "metrics": dict(unknown)}
        task_row_unknown = {"metrics": dict(unknown)}
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(b.charge(1.0, scenario_row_unknown, {"scenario_reserve_usd": .5}), (1.5, True))
            self.assertEqual(b.charge(1.0, scenario_row_unknown, {}), (1.5, True))
            self.assertEqual(b.charge(1.0, task_row_unknown, {}), (1.0, False))
        self.assertIn("Usage unknown; charged the scenario reserve of $0.50.", out.getvalue())
        self.assertEqual(b.charge(1.0, {"kind": "scenario", "metrics": {"estimated_cost_usd": .25}}, {}), (1.25, True))
        self.assertEqual(b.charge(1.0, {"metrics": {"estimated_cost_usd": None, "reported_cost_usd": .5}}, {}),
                         (1.5, True))

    def test_rescore_scenario_rows_follow_the_live_success_rule(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            make = self._rescore_dir(path)
            init = {"type": "system", "subtype": "init", "mcp_servers": [{"name": "wendy"}]}
            agent = {"exit_code": 0, "timed_out": False, "interrupted": False, "seconds": 1.0}
            def attempt(name, events, proxy, **saved):
                d = make(name, kind="scenario", group="core", task="s", stages={"agent": agent}, **saved)
                (d / "agent.stdout").write_text("".join(json.dumps(e) + "\n" for e in events))
                (d / "proxy.jsonl").write_text("".join(json.dumps(r) + "\n" for r in proxy))
                (d / "cli.jsonl").write_text("")
            done = {"type": "result", "result": "done"}
            attempt("unverified", [done], [{"tool": "run"}], status="mcp_unverified", success=False)
            attempt("regraded-wrong", [init, done], [{"tool": "container_exec", "arguments": {"command": ["nmcli"]}}],
                    status="passed", success=True)
            attempt("regraded-right", [init, done], [{"tool": "run"}], status="failed", success=False)
            scenarios = [{"id": "s", "expect": {"right": [{"tool": "run"}], "wrong": [{"tool": "container_exec"}]}}]
            out = b.rescore(path, [], scenarios)
            rows = [json.loads(line) for line in (out / "results.jsonl").read_text().splitlines()]
            got = sorted((r["tool_choice"]["outcome"], r["success"], r["status"]) for r in rows)
            self.assertEqual(got, [("right", False, "mcp_unverified"), ("right", True, "passed"),
                                   ("wrong", False, "failed")])

    def test_rescore_flags_mcp_calls_the_proxy_never_saw(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            make = self._rescore_dir(path)
            agent = {"exit_code": 0, "timed_out": False, "interrupted": False, "seconds": 1.0}
            init = {"type": "system", "subtype": "init", "mcp_servers": [{"name": "wendy"}]}
            call = {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": "1", "name": "mcp__wendy__run", "input": {}}]}}
            for name, proxy in (("unrouted", []), ("routed", [{"tool": "run"}])):
                d = make(name, kind="scenario", group="core", task="s", stages={"agent": agent},
                         status="passed", success=True)
                (d / "agent.stdout").write_text("".join(json.dumps(e) + "\n" for e in (init, call, {"type": "result"})))
                (d / "proxy.jsonl").write_text("".join(json.dumps(r) + "\n" for r in proxy))
                (d / "cli.jsonl").write_text("")
            out = b.rescore(path, [], [{"id": "s", "expect": {"right": [{"tool": "run"}], "wrong": []}}])
            rows = {r["id"]: r for r in (json.loads(l) for l in (out / "results.jsonl").read_text().splitlines())}
            self.assertEqual((rows["unrouted"]["status"], rows["unrouted"]["success"]), ("mcp_unverified", False))
            self.assertEqual((rows["routed"]["status"], rows["routed"]["success"]), ("passed", True))
            self.assertEqual(rows["unrouted"]["metrics"]["proxy_calls"], 0)

    def test_rescore_keeps_a_scenario_row_that_aborted_before_its_verdict(self):
        # scenario_trial raised after run_process (e.g. bad prices): stages.agent exists, metrics is {}.
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            make = self._rescore_dir(path)
            agent = {"exit_code": 0, "timed_out": False, "interrupted": False, "seconds": 1.0}
            init = {"type": "system", "subtype": "init", "mcp_servers": [{"name": "wendy"}]}
            for name, saved in (("normal", {"status": "failed", "success": False}),
                                ("aborted", {"status": "setup_error", "success": False, "metrics": {}})):
                d = make(name, kind="scenario", group="core", task="s", stages={"agent": agent}, **saved)
                (d / "agent.stdout").write_text(json.dumps(init) + "\n" + json.dumps({"type": "result", "result": "x"}) + "\n")
                (d / "proxy.jsonl").write_text(json.dumps({"tool": "run"}) + "\n")
            out = b.rescore(path, [], [{"id": "s", "expect": {"right": [{"tool": "run"}], "wrong": []}}])
            rows = {r["id"]: r for r in (json.loads(l) for l in (out / "results.jsonl").read_text().splitlines())}
            self.assertEqual((rows["aborted"]["status"], rows["aborted"]["success"]), ("setup_error", False))
            self.assertEqual((rows["normal"]["status"], rows["normal"]["success"]), ("passed", True))
            self.assertTrue((out / "summary.json").exists() and (out / "summary.md").exists())
            summary = json.loads((out / "summary.json").read_text())[0]
            self.assertEqual((summary["attempts"], summary["setup_errors"]), (1, 1))

    def test_rescore_looks_up_expect_by_kind(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            self._stdout(self._rescore_dir(path)("a"))
            tasks = [{"id": "t", "expect": {"right": [{"tool": "run"}], "wrong": []}}]
            scenarios = [{"id": "t", "expect": {"right": [{"tool": "other"}], "wrong": []}}]
            out = b.rescore(path, tasks, scenarios)
            row = json.loads((out / "results.jsonl").read_text().splitlines()[0])
            self.assertEqual(row["tool_choice"]["outcome"], "right")


class DisposableDeviceTests(unittest.TestCase):
    SOCKET = {"WENDY_AGENT_SOCKET": "/run/wendy.sock"}

    def test_wait_for_model_returns_once_the_server_recovers(self):
        answers, sleeps = iter([False, False, True]), []
        self.assertTrue(b.wait_for_model({"model_health_url": "u"}, timeout=900, poll=5,
                                         health=lambda: next(answers), sleep=sleeps.append, clock=lambda: 0))
        self.assertEqual(sleeps, [5, 5])

    def test_wait_for_model_gives_up_after_the_timeout(self):
        now, sleeps = [0], []

        def sleep(seconds):
            sleeps.append(seconds)
            now[0] += seconds

        self.assertFalse(b.wait_for_model({"model_health_url": "u"}, timeout=20, poll=5,
                                          health=lambda: False, sleep=sleep, clock=lambda: now[0]))
        self.assertEqual(sum(sleeps), 20)

    def test_wait_for_model_does_not_wait_when_health_is_unknown(self):
        self.assertTrue(b.wait_for_model({}, health=lambda: None, sleep=lambda s: self.fail("slept")))

    def test_a_garbled_health_reply_is_not_healthy_and_does_not_raise(self):
        import http.client
        for error in (http.client.BadStatusLine("garbage"), http.client.IncompleteRead(b"x")):
            with mock.patch.object(b.urllib.request, "urlopen", side_effect=error):
                self.assertIs(b.model_server_healthy({"model_health_url": "http://127.0.0.1:9/health"}), False)

    def test_disposable_needs_both_the_flag_and_the_socket(self):
        for flag in (True, False, None):
            for socket in (True, False):
                config = {} if flag is None else {"disposable_device": flag}
                env = dict(self.SOCKET) if socket else {}
                with self.subTest(flag=flag, socket=socket):
                    self.assertIs(b.disposable(config, env), flag is True and socket)
        self.assertFalse(b.disposable({"disposable_device": "yes"}, self.SOCKET))
        self.assertFalse(b.disposable({"disposable_device": True}, {"WENDY_AGENT_SOCKET": ""}))

    def test_agent_seconds_override(self):
        config = {"agent_seconds": {"scenario": 900, "task": 3600}}
        scenario = {"id": "s", "group": "core", "timeout_seconds": 120}
        task = {"id": "t", "timeout_seconds": 600}
        self.assertEqual(b.agent_timeout(config, scenario), 900)
        self.assertEqual(b.agent_timeout(config, task), 3600)
        # max_agent_seconds still caps the override
        self.assertEqual(b.agent_timeout(dict(config, max_agent_seconds=300), task), 300)
        # with no override the minimum rule is unchanged
        self.assertEqual(b.agent_timeout({}, scenario), 120)
        self.assertEqual(b.agent_timeout({"max_agent_seconds": 60}, task), 60)
        self.assertEqual(b.agent_timeout({}, {"id": "t"}), 900)

    def row(self, **changes):
        return dict({"status": "passed", "kind": "scenario", "cleanup_ok": True,
                     "metrics": {"estimated_cost_usd": .1}, "stages": {"agent": {"interrupted": False}}}, **changes)

    def test_stop_reason_on_a_laptop_is_unchanged(self):
        config = {}
        self.assertIsNone(b.stop_reason(self.row(), config, {}))
        self.assertIn("Cleanup failed", b.stop_reason(self.row(cleanup_ok=False), config, {}))
        unknown = self.row(kind="task", metrics={"estimated_cost_usd": None, "reported_cost_usd": None})
        self.assertIn("Usage cost is unknown", b.stop_reason(unknown, config, {}))
        self.assertIn("Claude reported no MCP server list",
                      b.stop_reason(self.row(status="mcp_unverified", metrics={}), config, {}))
        unrouted = {"transcript_mcp_calls": 2, "proxy_calls": 0}
        self.assertIn("never reached the recording proxy",
                      b.stop_reason(self.row(status="mcp_unverified", metrics=unrouted), config, {}))
        self.assertIn("Another Wendy MCP server", b.stop_reason(self.row(status="mcp_contaminated"), config, {}))
        self.assertIn("interrupted", b.stop_reason(self.row(stages={"agent": {"interrupted": True}}), config, {}))
        # a flag without the socket is still a laptop
        self.assertIn("Cleanup failed", b.stop_reason(self.row(cleanup_ok=False), {"disposable_device": True}, {}))

    def test_stop_reason_on_a_disposable_device(self):
        config, env = {"disposable_device": True}, self.SOCKET
        self.assertIsNone(b.stop_reason(self.row(cleanup_ok=False), config, env))
        unknown = self.row(kind="task", metrics={"estimated_cost_usd": None, "reported_cost_usd": None})
        self.assertIsNone(b.stop_reason(unknown, config, env))
        self.assertIsNotNone(b.stop_reason(self.row(status="mcp_unverified", metrics={}), config, env))
        self.assertIsNotNone(b.stop_reason(self.row(status="mcp_contaminated"), config, env))
        self.assertIsNotNone(b.stop_reason(self.row(stages={"agent": {"interrupted": True}}), config, env))


class UnscoredAndResumeTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)

    def test_went_around_is_not_success(self):
        result = {"interrupted": False, "timed_out": False}
        choice = {"outcome": "went_around"}
        self.assertEqual(b.scenario_verdict("wendy", {"mcp_servers_observed": []}, result, choice), ("failed", False))
        self.assertEqual(b.scenario_verdict("wendy", {"mcp_servers_observed": []}, result, {"outcome": "right"}),
                         ("passed", True))

    def test_unscored_statuses(self):
        self.assertEqual(b.UNSCORED_STATUSES, frozenset(
            {"context_exceeded", "model_server_down", "interrupted", "timeout", "setup_error"}))

    def test_context_overflow_is_unscored(self):
        for text in ("request (9000 tokens) exceed_context_size_error", "the prompt exceeds the available context size"):
            events = [{"type": "usage"}, {"type": "error", "text": text}]
            self.assertEqual(b.infra_status(events, {}), "context_exceeded", text)
        self.assertIsNone(b.infra_status([{"type": "done", "text": "exceed_context_size_error"}], {}))
        self.assertIsNone(b.infra_status([{"type": "error", "text": "round limit"}], {}))

    def test_dead_model_server_is_unscored(self):
        events = [{"type": "error", "text": "connection refused"}]
        self.assertEqual(b.infra_status(events, {}, health=lambda: False), "model_server_down")
        self.assertIsNone(b.infra_status(events, {}, health=lambda: True))
        calls = []
        self.assertIsNone(b.infra_status([{"type": "done"}], {}, health=lambda: calls.append(1) or False))
        self.assertEqual(calls, [])  # no error, so the server isn't asked
        # context overflow wins over a dead server
        overflow = [{"type": "error", "text": "exceed_context_size_error"}]
        self.assertEqual(b.infra_status(overflow, {}, health=lambda: False), "context_exceeded")

    def test_load_rows_ignores_a_truncated_last_line(self):
        path = self.root / "results.jsonl"
        self.assertEqual(b.load_rows(path), [])
        path.write_text(json.dumps({"a": 1}) + "\n" + json.dumps({"a": 2}) + "\n" + '{"a": 3, "tru')
        self.assertEqual(b.load_rows(path), [{"a": 1}, {"a": 2}])

    @staticmethod
    def item(task="deploy-http", kind="task", agent="a", target="local"):
        return (kind, agent, target, {"id": task})

    @staticmethod
    def saved(task="deploy-http", status="passed", kind="task", agent="a", target="local"):
        return {"kind": kind, "agent": agent, "target": target, "task": task, "status": status}

    def test_remaining_plan_counts_completed_repetitions(self):
        plan = [self.item(), self.item(), self.item("stop-app"), self.item("deploy-http", kind="scenario")]
        left = b.remaining_plan(plan, [self.saved()])
        self.assertEqual([(k, t["id"]) for k, _, _, t in left],
                         [("task", "deploy-http"), ("task", "stop-app"), ("scenario", "deploy-http")])
        self.assertEqual(b.remaining_plan(plan, []), plan)
        # an unscored row still counts as done
        self.assertEqual(len(b.remaining_plan(plan, [self.saved(status="context_exceeded")] * 2)), 2)

    def test_interrupted_attempt_is_retried_once(self):
        attempt = self.root / "a-local-deploy-http-0123456789ab"
        attempt.mkdir()
        (attempt / "task.json").write_text(json.dumps({"id": "deploy-http", "agent": "a", "target": "local"}))
        done = self.root / "a-local-deploy-http-ba9876543210"
        done.mkdir()
        (done / "task.json").write_text("{}")
        (done / "result.json").write_text("{}")
        (self.root / "bin").mkdir()  # not an attempt
        made = b.interrupted_rows(self.root, [])
        self.assertEqual([(r["agent"], r["target"], r["task"], r["kind"], r["status"], r["success"], r["id"])
                          for r in made],
                         [("a", "local", "deploy-http", "task", "interrupted", False, "0123456789ab")])
        plan = [self.item(), self.item()]
        self.assertEqual(len(b.remaining_plan(plan, made)), 2)  # the first interruption is retried
        self.assertEqual(b.interrupted_rows(self.root, made), [])  # a row for it exists already
        second = dict(made[0], id="x")
        self.assertEqual(len(b.remaining_plan(plan, made + [second])), 1)  # the second removes it
        self.assertEqual(len(b.remaining_plan([self.item()], made + [second])), 0)

    def test_interrupted_scenario_attempt_keeps_its_kind(self):
        attempt = self.root / "a-local-probe-0123456789ab"
        attempt.mkdir()
        (attempt / "task.json").write_text(json.dumps({"id": "probe", "group": "core", "agent": "a", "target": "local"}))
        self.assertEqual(b.interrupted_rows(self.root, [])[0]["kind"], "scenario")

    @staticmethod
    def summary_row(status, success=False, outcome=None, kind="local"):
        return {"agent": "a", "target": "t", "task": "t", "status": status, "success": success,
                "task_seconds": 1, "tool_choice": {"outcome": outcome} if outcome else None,
                "target_config": {"kind": kind}, "metrics": {"usage_complete": False, "tokens": None}}

    def test_scored_follows_the_target(self):
        row = self.summary_row
        for status in ("context_exceeded", "model_server_down", "setup_error"):
            self.assertFalse(b.scored(row(status, kind="simulator")), status)
            self.assertFalse(b.scored(row(status)), status)
        for status in ("timeout", "interrupted"):
            self.assertFalse(b.scored(row(status)), status)
            self.assertTrue(b.scored(row(status, kind="simulator")), status)
            self.assertTrue(b.scored({"status": status}), status)  # no target config: a laptop row
        self.assertTrue(b.scored(row("failed")))

    def test_summary_keeps_unscored_attempts_out_of_the_denominator(self):
        row = self.summary_row
        rows = [row("passed", True, "right"), row("failed", outcome="went_around"), row("context_exceeded"),
                row("model_server_down"), row("interrupted"), row("timeout"), row("setup_error")]
        result = b.summary(rows)[0]
        self.assertEqual((result["attempts"], result["successes"], result["setup_errors"], result["unscored"]),
                         (2, 1, 1, 5))
        self.assertEqual(result["tool_choice"]["went_around"], 1)
        self.assertEqual(result["timeouts"], 1)
        self.assertEqual(b.choice_cell(result["tool_choice"]), "1/0/0/0/1")

    def test_laptop_timeouts_still_count_against_the_pass_rate(self):
        row = self.summary_row
        rows = [row("passed", True, kind="simulator"), row("timeout", kind="simulator"),
                row("timeout", kind="simulator")]
        result = b.summary(rows)[0]
        self.assertEqual((result["successes"], result["attempts"], result["timeouts"]), (1, 3, 2))
        coverage = b.coverage_results([{"task": "t", "target": "t"}],
                                      [dict(r, kind="task") for r in rows], ["a"], 3)
        self.assertEqual(coverage[0]["agents"]["a"]["attempts"], 3)

    def test_rescore_keeps_infra_statuses_on_scenario_rows(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            make = MetricsTests._rescore_dir(self, path)
            agent = {"exit_code": 0, "timed_out": False, "interrupted": False, "seconds": 1.0}
            for status in ("context_exceeded", "model_server_down", "interrupted"):
                d = make(status, kind="scenario", group="core", task="s", stages={"agent": agent}, status=status,
                         success=False, target_config={"kind": "local"})
                (d / "agent.stdout").write_text(json.dumps({"type": "tool_start", "id": "1", "tool": "run", "arguments": {}}) + "\n")
                (d / "proxy.jsonl").write_text(json.dumps({"tool": "run"}) + "\n")
                (d / "cli.jsonl").write_text("")
            manifest = json.loads((path / "manifest.json").read_text())
            manifest["config"]["agents"]["c"]["adapter"] = "wendy"
            (path / "manifest.json").write_text(json.dumps(manifest))
            out = b.rescore(path, [], [{"id": "s", "expect": {"right": [{"tool": "run"}]}}])
            rows = [json.loads(line) for line in (out / "results.jsonl").read_text().splitlines()]
            self.assertEqual(sorted((r["status"], r["success"]) for r in rows),
                             [("context_exceeded", False), ("interrupted", False), ("model_server_down", False)])

    def test_attempt_records_device_markers(self):
        uptime = self.root / "uptime"
        uptime.write_text("12345.67 8901.23\n")
        sock = self.root / "agent.sock"
        sock.write_text("")
        env = {"WENDY_AGENT_SOCKET": str(sock)}
        self.assertEqual(b.device_markers(env, uptime), {"uptime": "12345.67", "socket_inode": sock.stat().st_ino})
        self.assertEqual(b.device_markers({}, self.root / "missing"), {"uptime": None, "socket_inode": None})

    def test_rescore_reads_went_around_from_the_rows_target_config(self):
        path = self.root / "pass"
        path.mkdir()
        (path / "manifest.json").write_text(json.dumps({"config": {"agents": {"w": {"adapter": "wendy"}}}}))
        tasks = [{"id": "usb", "expect": {"right": [{"tool": "device_os_logs"}]}}]
        events = [{"type": "tool_start", "id": "1", "tool": "workspace_exec", "arguments": {"command": "lsusb"}},
                  {"type": "done", "text": "ok"}]
        for name, kind in (("on-device", "local"), ("laptop", "simulator")):
            d = path / name
            d.mkdir()
            (d / "agent.stdout").write_text("\n".join(json.dumps(e) for e in events) + "\n")
            (d / "result.json").write_text(json.dumps({
                "id": name, "task": "usb", "agent": "w", "target": "t", "target_config": {"kind": kind},
                "metrics": {"tokens": None, "usage_complete": False}, "status": "failed", "success": False,
                "task_seconds": 1.0, "tool_choice": None}))
        out = b.rescore(path, tasks, [])
        rows = {r["id"]: r for r in map(json.loads, (out / "results.jsonl").read_text().splitlines())}
        self.assertEqual(rows["on-device"]["tool_choice"]["outcome"], "went_around")
        self.assertEqual(rows["laptop"]["tool_choice"]["outcome"], "missed")


if __name__ == "__main__":
    unittest.main()
