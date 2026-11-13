import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import compare as c


def row(task, outcome, agent="claude", target="sim", kind="scenario", tokens=None, seconds=None, calls=(), group="core"):
    usage = tokens is not None
    return {"agent": agent, "target": target, "task": task, "kind": kind, "group": group,
            "status": "completed", "success": outcome in ("right", "asked"),
            "tool_choice": {"outcome": outcome} if outcome else None,
            "metrics": {"tokens": {"input_tokens": tokens - 10, "output_tokens": 10} if usage else None,
                        "usage_complete": usage, "tool_calls": len(calls),
                        "tool_trace": [{"channel": ch, "name": n} for ch, n in calls]},
            "stages": {"agent": {"seconds": seconds}} if seconds is not None else {}}


SCENARIO = {"id": "s1", "group": "core", "captures": ["device_info"], "canned": ["container_list"]}
TOOLS = [{"name": "device_info", "description": "d", "inputSchema": {}},
         {"name": "container_list", "description": "c", "inputSchema": {}}]


def make(root, name, rows, model="m1", tools=TOOLS, scenarios=(SCENARIO,), version="1.0", argv=None, kind="simulator",
         harness="h"):
    d = Path(root) / name
    d.mkdir(parents=True, exist_ok=True)
    (d / "results.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))
    (d / "claude-version.stdout").write_text(version)
    manifest = {"config": {"agents": {"claude": {"model": model}}, "targets": {"sim": {"kind": kind, "device": "vm:x"}}},
                "runner_sha256": "r", "task_sha256": "t", "scenarios_sha256": "s", "harness_sha256": harness,
                "scenarios": list(scenarios),
                "claude_version": {"argv": ["claude", "--version"], "exit_code": 0},
                "argv": argv or ["benchmark.py", "--scenario", "all", "--treatment-root", "/t", "--wendy", "/w", "--execute"],
                "cwd": "/cwd", "tools_sha256": name, "treatment": {"revision": name}}
    (d / "manifest.json").write_text(json.dumps(manifest))
    (d / "tools.json").write_text(json.dumps(tools))
    return d


class CompareTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)

    def run_compare(self, base_rows, cand_rows, **kw):
        b = c.load_run(make(self.tmp.name, "b", base_rows))
        n = c.load_run(make(self.tmp.name, "n", cand_rows, **kw))
        return c.compare(b, n, [SCENARIO])

    def test_flags_tool_choice_only_when_every_candidate_run_is_worse(self):
        r = self.run_compare([row("s1", "right")] * 3, [row("s1", "wrong")] * 3)
        self.assertIn("tool_choice", r["rows"][0]["flags"])
        r = self.run_compare([row("s1", "right")] * 3, [row("s1", "right"), row("s1", "wrong"), row("s1", "wrong")])
        self.assertNotIn("tool_choice", r["rows"][0]["flags"])

    def test_good_follows_the_scenario_verdict(self):
        # A right or asked attempt that ended host_state_changed, interrupted or mcp_* is not a success.
        import benchmark
        self.assertTrue({"host_state_changed", "interrupted", "mcp_unverified", "mcp_contaminated"}
                        <= benchmark.SCENARIO_NOT_SUCCESS)
        for status in sorted(benchmark.SCENARIO_NOT_SUCCESS):
            with self.subTest(status=status):
                bad = [dict(row("s1", outcome), status=status, success=False) for outcome in ("right", "asked", "right")]
                self.assertEqual(c.side_stats(bad)["good"], 0)
                self.assertIn("tool_choice", self.run_compare([row("s1", "right")] * 3, bad)["rows"][0]["flags"])
                self.assertNotIn("tool_choice", self.run_compare(bad, [row("s1", "wrong")] * 3)["rows"][0]["flags"])
        self.assertEqual(c.side_stats([dict(row("s1", "right"), status="passed"), row("s1", "asked")])["good"], 2)

    def test_flags_tokens_beyond_margin_over_baseline_max(self):
        base = [row("s1", "right", tokens=t) for t in (100, 110, 120)]
        r = self.run_compare(base, [row("s1", "right", tokens=t) for t in (160, 170, 180)])
        self.assertIn("tokens", r["rows"][0]["flags"])
        r = self.run_compare(base, [row("s1", "right", tokens=t) for t in (140, 145, 150)])
        self.assertNotIn("tokens", r["rows"][0]["flags"])

    def test_flags_time_beyond_margin(self):
        base = [row("s1", "right", seconds=s) for s in (10, 11, 12)]
        r = self.run_compare(base, [row("s1", "right", seconds=s) for s in (20, 21, 22)])
        self.assertIn("time", r["rows"][0]["flags"])

    def test_unknown_usage_excluded_and_counted(self):
        stats = c.side_stats([row("s1", "right", tokens=100), row("s1", "right", tokens=200), row("s1", "right")])
        self.assertEqual((stats["known_usage"], stats["unknown_usage"], stats["median_tokens"]), (2, 1, 150))
        r = self.run_compare([row("s1", "right", tokens=100), row("s1", "right")],
                             [row("s1", "right", tokens=900), row("s1", "right", tokens=900)])
        self.assertEqual(r["rows"][0]["flags"], [])
        self.assertIn("tokens", r["rows"][0]["insufficient"])

    def test_mismatched_models_are_reported(self):
        r = self.run_compare([row("s1", "right")], [row("s1", "right")], model="m2")
        self.assertIn("config.agents.claude.model", r["mismatches"])
        b = make(self.tmp.name, "b2", [row("s1", "right")])
        n = make(self.tmp.name, "n2", [row("s1", "right")], model="m2")
        out = Path(self.tmp.name) / "out"
        self.assertEqual(c.main(["--baseline", str(b), "--candidate", str(n), "--output", str(out)]), 2)
        self.assertEqual(c.main(["--baseline", str(b), "--candidate", str(n), "--output", str(out), "--allow-mismatch"]), 0)
        self.assertTrue((out / "compare.json").exists() and (out / "compare.md").exists())

    def test_harness_mismatch_is_reported(self):
        # A changed shim, proxy, allowlist or canned reply changes what the agents saw.
        r = self.run_compare([row("s1", "right")], [row("s1", "right")], harness="other")
        self.assertIn("harness_sha256", r["mismatches"])
        self.assertEqual(self.run_compare([row("s1", "right")], [row("s1", "right")])["mismatches"], [])

    def test_version_and_target_mismatches(self):
        r = self.run_compare([row("s1", "right")], [row("s1", "right")], version="2.0", kind="physical")
        self.assertIn("claude_version.stdout", r["mismatches"])
        self.assertIn("config.targets.sim.kind", r["mismatches"])

    def test_rows_only_on_one_side_are_listed_not_flagged(self):
        r = self.run_compare([row("s1", "right"), row("old", "right")], [row("s1", "right"), row("new", "wrong")])
        by = {x["task"]: x for x in r["rows"]}
        self.assertTrue(by["old"]["one_sided"] and by["new"]["one_sided"])
        self.assertEqual(by["old"]["flags"] + by["new"]["flags"], [])

    def test_canned_tool_change_is_noted(self):
        changed = [dict(TOOLS[0], description="new"), TOOLS[1]]
        b = c.load_run(make(self.tmp.name, "b", [row("s1", "right")]))
        n = c.load_run(make(self.tmp.name, "n", [row("s1", "right")], tools=changed))
        result = c.compare(b, n, [SCENARIO])
        self.assertEqual(result["rows"][0]["canned_tools_changed"], ["device_info"])
        md = c.render_markdown(result)
        self.assertIn("Not measured", md)
        self.assertIn("s1: reply text of device_info is shared between runs", md)

    def test_tools_added_and_removed(self):
        r = self.run_compare([row("s1", "right", calls=[("mcp", "a")])], [row("s1", "right", calls=[("cli", "b")])])
        self.assertEqual((r["rows"][0]["tools_added"], r["rows"][0]["tools_removed"]), (["b"], ["a"]))

    def confirm(self, rows, fake, extra_target=False, base_rows=None):
        b = make(self.tmp.name, "b", base_rows or [row("s1", "right")] * 3)
        n = make(self.tmp.name, "n", rows)
        out = Path(self.tmp.name) / "out"
        with mock.patch("compare.subprocess.run", side_effect=fake) as run:
            code = c.main(["--baseline", str(b), "--candidate", str(n), "--output", str(out), "--confirm", "2"])
        return code, run, json.loads((out / "compare.json").read_text()), (out / "compare.md").read_text(), n

    @staticmethod
    def writer(rows, code=0):
        def fake(argv, **kw):
            if rows is not None:
                run = Path(argv[argv.index("--output") + 1]) / "20260101-000000-abc"
                run.mkdir(parents=True)
                (run / "results.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))
            return mock.Mock(returncode=code)
        return fake

    def test_confirm_reruns_flagged_pairs_on_candidate(self):
        code, run, result, md, n = self.confirm([row("s1", "wrong")] * 3, self.writer([row("s1", "wrong")]))
        self.assertEqual(code, 1)
        argv = run.call_args.args[0]
        joined = " ".join(argv)
        self.assertIn("--scenario s1 --agent claude --repetitions 2 --execute", joined)
        self.assertNotIn("--scenario all", joined)
        self.assertIn("--treatment-root /t", joined)
        self.assertIn("--wendy /w", joined)
        self.assertEqual(run.call_args.kwargs["cwd"], "/cwd")
        self.assertEqual(result["rows"][0]["candidate"]["attempts"], 4)
        self.assertIn("tool_choice", result["rows"][0]["confirmed"])
        self.assertIn("tool_choice (confirmed)", md)

    def test_rerun_exit_1_with_scored_rows_confirms(self):
        # benchmark.py exits 1 when any attempt is unsuccessful, which a reproduced regression is.
        _, _, result, md, _ = self.confirm([row("s1", "wrong")] * 3, self.writer([row("s1", "wrong")], code=1))
        r = result["rows"][0]
        self.assertEqual((r["confirmed"], r["unconfirmed"], r["rerun_exit"]), (["tool_choice"], {}, 1))
        self.assertIn("tool_choice (confirmed)", md)

    def test_crashed_rerun_without_rows_reports_exit(self):
        _, _, result, md, _ = self.confirm([row("s1", "wrong")] * 3, self.writer(None, code=2))
        self.assertEqual(result["rows"][0]["unconfirmed"]["tool_choice"], "rerun failed: exit 2")
        self.assertIn("unconfirmed: rerun failed: exit 2", md)

    def test_rerun_without_rows_is_unconfirmed(self):
        _, _, result, _, _ = self.confirm([row("s1", "wrong")] * 3, self.writer(None))
        self.assertEqual(result["rows"][0]["unconfirmed"]["tool_choice"], "rerun added no attempts")
        self.assertEqual(result["rows"][0]["candidate"]["attempts"], 3)

    def test_stale_confirm_dir_is_not_merged(self):
        n = Path(self.tmp.name) / "n"
        make(self.tmp.name, "n", [row("s1", "wrong")] * 3)
        stale = n / "confirm" / "old" / "run"
        stale.mkdir(parents=True)
        (stale / "results.jsonl").write_text(json.dumps(row("s1", "wrong")) + "\n")
        _, _, result, _, _ = self.confirm([row("s1", "wrong")] * 3, self.writer(None))
        self.assertEqual(result["rows"][0]["candidate"]["attempts"], 3)
        self.assertIn("tool_choice", result["rows"][0]["unconfirmed"])

    def test_confirm_is_keyed_by_target(self):
        rows = [row("s1", "wrong", target="sim")] * 3 + [row("s1", "wrong", target="board")] * 3
        base = [row("s1", "right", target="sim")] * 3 + [row("s1", "right", target="board")] * 3
        # the rerun only produces a row for target sim
        _, _, result, _, _ = self.confirm(rows, self.writer([row("s1", "wrong", target="sim")]), base_rows=base)
        by = {r["target"]: r for r in result["rows"]}
        self.assertEqual(by["sim"]["confirmed"], ["tool_choice"])
        self.assertEqual(by["board"]["unconfirmed"]["tool_choice"], "rerun added no attempts")

    def test_confirm_leaves_unflagged_rows_alone(self):
        # s1 regresses and is rerun; s2 is unchanged, so it has no rerun to look up.
        base = [row("s1", "right")] * 3 + [row("s2", "right")] * 3
        cand = [row("s1", "wrong")] * 3 + [row("s2", "right")] * 3
        code, run, result, md, _ = self.confirm(cand, self.writer([row("s1", "wrong")]), base_rows=base)
        self.assertEqual(code, 1)
        self.assertEqual(run.call_count, 1)
        by = {r["task"]: r for r in result["rows"]}
        self.assertEqual((by["s1"]["confirmed"], by["s1"]["unconfirmed"]), (["tool_choice"], {}))
        self.assertEqual((by["s2"]["flags"], by["s2"]["confirmed"], by["s2"]["unconfirmed"]), ([], [], {}))
        self.assertNotIn("rerun_exit", by["s2"])
        self.assertEqual(by["s2"]["candidate"]["attempts"], 3)
        self.assertIn("tool_choice (confirmed)", md)

    def test_setup_error_only_side_is_visible_and_never_flagged(self):
        bad = dict(row("s1", None), status="setup_error")
        r = self.run_compare([row("s1", "right")] * 3, [bad] * 3)
        self.assertEqual(r["rows"][0]["flags"], [])
        self.assertEqual(r["rows"][0]["candidate"]["setup_errors"], 3)
        md = c.render_markdown(r)
        self.assertIn("no scored attempts (3 setup errors)", md)

    def test_markdown_counts_and_escapes(self):
        rows = [row("s|1", "right")] * 3 + [dict(row("s|1", None), status="setup_error")]
        r = self.run_compare([row("s|1", "right")] * 3, rows)
        md = c.render_markdown(r)
        self.assertIn("3R/0W/0M/0A of 3 (1 setup error)", md)
        self.assertIn("s\\|1", md)


if __name__ == "__main__":
    unittest.main()
