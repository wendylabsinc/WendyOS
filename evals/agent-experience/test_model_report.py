import io
import json
import os
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

import benchmark as b
import model_report as mr
import scenarios as sc
import tool_trace as tt

APP = sc.BENCHMARK_APP
TOOLS = ["run", "container_list", "container_stop", "telemetry_logs", "device_os_logs", "device_info"]
RIGHT_RUN = {"right": [{"tool": "run"}], "wrong": []}


def call(name, args=None, is_error=False, reply="ok", canned=False, source="proxy"):
    return tt.entry("mcp", name, args if args is not None else {}, source, canned, None,
                    is_error, None if canned else reply)


def shell(command):
    return tt.entry("shell", "shell", command)


class Pass:
    """A results directory with saved attempts, built the way benchmark.py lays them out."""

    def __init__(self, test):
        self.tmp = tempfile.TemporaryDirectory()
        test.addCleanup(self.tmp.cleanup)
        self.dir = Path(self.tmp.name)
        self.rows = []

    def attempt(self, trace=(), expect=RIGHT_RUN, outcome=None, task="deploy", kind="task", status="failed",
                verified=None, final_text="", cli=(), events=(), markers=None, cleanup_ok=True, local=True,
                metrics=True, stages=None):
        ident = f"{len(self.rows):012x}"
        row = {"id": ident, "agent": "chat", "target": "thor", "task": task, "kind": kind, "status": status,
               "success": status == "passed", "cleanup_ok": cleanup_ok, "task_seconds": 10,
               "target_config": {"kind": "local" if local else "board"}, "model_requested": "m",
               "stages": stages or {}, "tool_choice": {"outcome": outcome} if outcome else None}
        if metrics:
            row["metrics"] = {"tool_trace": list(trace), "final_text": final_text, "agent_completed": True}
        if verified is not None:
            row["outcome_verified"] = verified
        if markers:
            row["device_markers"] = markers
        adir = self.dir / f"chat-thor-{task}-{ident}"
        adir.mkdir()
        (adir / "task.json").write_text(json.dumps({"id": task, "expect": expect}))
        (adir / "cli.jsonl").write_text("".join(json.dumps(r) + "\n" for r in cli))
        (adir / "agent.stdout").write_text("".join(json.dumps(e) + "\n" for e in events))
        self.rows.append(row)
        self.save()
        return row

    def save(self):
        (self.dir / "results.jsonl").write_text("".join(json.dumps(r) + "\n" for r in self.rows))
        (self.dir / "tools.json").write_text(json.dumps([{"name": n} for n in TOOLS]))

    def classify(self, index=-1, previous=None):
        guards = sc.guard_matchers(APP)
        return mr.classify(self.rows[index], self.dir, TOOLS, guards, previous=previous)


def labels(findings):
    return [(f["section"], f["item"]) for f in findings]


class EachCategory(unittest.TestCase):
    def check(self, want, **kwargs):
        p = Pass(self)
        p.attempt(**kwargs)
        self.assertEqual(labels(p.classify()), [want])
        return p.classify()[0]

    def test_each_category(self):
        run_call = call("run", {"project_path": "/tmp/hello", "start": True})
        cases = {
            "wrong": (("model", "Wrong tool"), dict(outcome="wrong", trace=[shell("ssh root@x ls")])),
            "missed": (("model", "Missing tool"), dict(outcome="missed", trace=[call("device_info")])),
            "went_around": (("model", "Went around"),
                            dict(outcome="went_around", trace=[call("device_os_logs"), shell("dmesg | grep -i usb")])),
            "bad_args": (("model", "Bad arguments"), dict(outcome="right", trace=[
                call("run", {"project_path": 3}, True, "[INVALID_ARGUMENT] project_path must be a string")])),
            "repeated": (("model", "Repeated call"), dict(outcome="right", trace=[run_call] * 3)),
            "asked": (("model", "Asked instead of acting"),
                      dict(outcome="missed", final_text="Which project should I deploy?")),
            "unfinished_never": (("model", "Task not finished"),
                                 dict(outcome="missed", verified=False, trace=[call("device_info")])),
            "touched": (("model", "Touched the benchmark"), dict(outcome="right", trace=[
                call("container_stop", {"app_name": APP}, canned=True), call("run")])),
            "tool_error": (("tool", "Tool error"), dict(outcome="right", trace=[
                call("telemetry_logs", {}, True, "[DEVICE_UNREACHABLE] stream terminated by RST_STREAM")])),
            "hang": (("tool", "Hang"), dict(outcome="right", trace=[call("run", {}, None, None)])),
            "said_success": (("tool", "Said success, state wrong"),
                             dict(outcome="right", verified=False, trace=[call("run", {"a": 1})])),
            "cleanup": (("tool", "Cleanup failed"), dict(outcome="right", cleanup_ok=False, trace=[call("run")])),
            "undetermined": (("undetermined", "Undetermined"),
                             dict(outcome="missed", verified=False, expect={"right": [], "wrong": []})),
        }
        for name, (want, kwargs) in cases.items():
            with self.subTest(name):
                kwargs.setdefault("trace", [])
                self.check(want, **kwargs)

    def test_ignored_error_is_both_task_not_finished_and_a_tool_error(self):
        # The model is blamed for going on; the tool's error is still shown as the tool's.
        p = Pass(self)
        p.attempt(outcome="right", verified=False, trace=[call("run", {"x": 1}, True, "[DEVICE_UNREACHABLE] no")])
        self.assertEqual(sorted(labels(p.classify())), [("model", "Task not finished"), ("tool", "Tool error")])

    def test_restart_when_markers_change(self):
        p = Pass(self)
        first = p.attempt(outcome="right", trace=[call("run")], markers={"uptime": "500.0", "socket_inode": 7})
        p.attempt(outcome="right", trace=[call("run")], markers={"uptime": "12.0", "socket_inode": 7})
        self.assertEqual(labels(p.classify(0)), [])
        self.assertEqual(labels(p.classify(1, previous=first)), [("tool", "Agent or device restart")])
        p.attempt(outcome="right", trace=[call("run")], markers={"uptime": "20.0", "socket_inode": 9})
        self.assertEqual(labels(p.classify(2, previous=p.rows[1])), [("tool", "Agent or device restart")])

    def test_went_around_after_a_wendy_tool_has_no_other_finding(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("device_os_logs"), shell("dmesg | grep -i usb")])
        self.assertEqual(p.classify(), [])

    def test_finding_has_the_documented_fields(self):
        f = self.check(("tool", "Tool error"), outcome="right", task="logs", trace=[
            call("device_info"), call("telemetry_logs", {}, True, "[DEVICE_UNREACHABLE] stream terminated\nmore")])
        self.assertEqual(set(f), {"section", "item", "task", "expected", "detail", "excerpt", "trace_path"})
        self.assertEqual(f["task"], "logs")
        self.assertIn("telemetry_logs", f["detail"])
        self.assertIn("DEVICE_UNREACHABLE", f["detail"])
        self.assertIn("stream terminated", f["excerpt"])
        self.assertNotIn("more", f["excerpt"])
        self.assertLessEqual(len(f["excerpt"].splitlines()), 6)
        self.assertTrue(Path(f["trace_path"]).is_dir())

    def test_cli_nonzero_exit_is_a_tool_error_without_a_message(self):
        argv = ["device", "apps", "list"]
        f = self.check(("tool", "Tool error"), outcome="right",
                       trace=[tt.entry("cli", "device apps list", argv, "wrapper")],
                       cli=[{"argv": argv, "exit_code": 1}, {"argv": ["x"], "exit_code": 0}])
        self.assertIn("device apps list", f["detail"])

    def test_cli_usage_error_is_the_models_bad_arguments(self):
        argv = ["run", "--wait-for-it"]
        f = self.check(("model", "Bad arguments"), outcome="right",
                       trace=[tt.entry("cli", "run", argv, "wrapper")], cli=[{"argv": argv, "exit_code": 2}])
        self.assertIn("wendy run", f["detail"])

    def test_cli_killed_by_the_models_pipeline_is_no_finding(self):
        argv = ["device", "logs"]
        for code in (141, 143, -13):
            p = Pass(self)
            p.attempt(outcome="right", trace=[tt.entry("cli", "device logs", argv, "wrapper")],
                      cli=[{"argv": argv, "exit_code": code}])
            self.assertEqual(p.classify(), [], code)

    def test_a_check_that_reports_failures_is_no_finding(self):
        for argv in (["json", "validate", "/tmp/x/wendy.json"], ["doctor", "--json"]):
            p = Pass(self)
            p.attempt(outcome="right", trace=[tt.entry("cli", " ".join(argv[:2]), argv, "wrapper")],
                      cli=[{"argv": argv, "exit_code": 1}])
            self.assertEqual(p.classify(), [], argv)

    def test_cli_excerpt_stops_at_the_failing_call(self):
        argv = ["run"]
        events = [{"type": "tool_start", "id": "a", "tool": "workspace_exec", "arguments": {"command": "wendy run"}},
                  {"type": "tool_start", "id": "b", "tool": "device_info", "arguments": {}}]
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("device_info")], cli=[{"argv": argv, "exit_code": 6}], events=events)
        f = p.classify()[0]
        self.assertEqual(f["item"], "Tool error")
        self.assertIn("run", f["excerpt"])
        self.assertNotIn("device_info", f["excerpt"])

    def test_cli_excerpt_finds_a_call_hidden_behind_timeout(self):
        argv = ["run", "--detach"]
        events = [{"type": "tool_start", "id": "a", "tool": "workspace_exec", "arguments": {"command": "timeout 90 /x/wendy run --detach"}},
                  {"type": "tool_start", "id": "b", "tool": "device_info", "arguments": {}}]
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("device_info")], cli=[{"argv": argv, "exit_code": 6}], events=events)
        self.assertNotIn("device_info", p.classify()[0]["excerpt"])

    def test_a_failing_command_in_a_container_is_not_the_tools_fault(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[
            call("container_exec", {"command": ["false"]}, True, '[COMMAND_FAILED] command exited with code 1')])
        self.assertEqual(p.classify(), [])

    def test_a_json_error_reply_shows_its_code(self):
        f = self.check(("tool", "Tool error"), outcome="right", trace=[
            call("ros2_topics", {}, True, '{"error_code":"INTERNAL","message":"no running ROS 2 containers found"}')])
        self.assertIn("ros2_topics [INTERNAL]", f["detail"])

    def test_empty_trace_shows_what_the_model_said(self):
        f = self.check(("model", "Missing tool"), outcome="missed", kind="scenario", final_text="I'll check the logs first.")
        self.assertIn("I'll check the logs first.", f["excerpt"])

    def test_ask_only_scenario_that_did_not_ask(self):
        p = Pass(self)
        p.attempt(outcome="missed", kind="scenario", expect={"right": [], "ask": True}, trace=[call("device_info")])
        self.assertEqual(labels(p.classify()), [("model", "Did not ask first")])

    def test_made_up_tool_comes_from_the_transcript(self):
        p = Pass(self)
        events = [{"type": "tool_start", "id": "1", "tool": "make_coffee", "arguments": {}},
                  {"type": "tool_start", "id": "2", "tool": "run", "arguments": {}},
                  {"type": "tool_start", "id": "3", "tool": "memory_save", "arguments": {}}]
        p.attempt(outcome="right", trace=[call("run")], events=events)
        found = p.classify()
        self.assertEqual(labels(found), [("model", "Made-up tool")])
        self.assertIn("make_coffee", found[0]["detail"])

    def test_two_identical_calls_are_not_repeated(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run", {"a": 1})] * 2 + [call("run", {"a": 2})])
        self.assertEqual(p.classify(), [])

    def test_bad_arguments_is_not_also_a_tool_error(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run", {}, True, "[INVALID_ARGUMENT] project_path required")])
        self.assertEqual(labels(p.classify()), [("model", "Bad arguments")])

    def test_not_counted_statuses_are_listed_not_blamed(self):
        for status in ("context_exceeded", "model_server_down", "interrupted", "timeout"):
            with self.subTest(status):
                p = Pass(self)
                p.attempt(status=status, outcome="missed", trace=[call("device_info")], verified=False,
                          final_text="Which one?")
                self.assertEqual([f for f in p.classify() if f["section"] == "model"], [])
                text = mr.render_markdown([mr.load_run(p.dir)])
                section = text.split("Not counted against the model")[1]
                self.assertIn(status, section)
                self.assertIn("deploy", section)

    def test_timeout_on_a_laptop_is_the_models(self):
        p = Pass(self)
        p.attempt(status="timeout", local=False, outcome="missed", trace=[call("device_info")])
        self.assertEqual(labels(p.classify()), [("model", "Missing tool")])


class Rendering(unittest.TestCase):
    def build(self, scored, total, dirpath=None):
        p = Pass(self)
        for i in range(total):
            p.attempt(status="passed" if i < scored else "model_server_down", outcome="right",
                      trace=[call("run")], verified=True)
        return p

    def test_warns_when_little_was_scored(self):
        text = mr.render_markdown([mr.load_run(self.build(1, 2).dir)])
        self.assertIn("Only 50% of attempts were scored", text)
        text = mr.render_markdown([mr.load_run(self.build(8, 10).dir)])
        self.assertNotIn("Only", text)
        self.assertEqual(mr.scored_share([]), 0.0)
        self.assertEqual(mr.scored_share(self.build(8, 10).rows), 0.8)

    def test_halt_reason_in_header(self):
        p = self.build(2, 2)
        (p.dir / "halt.json").write_text(json.dumps({"reason": "agent unreachable"}))
        text = mr.render_markdown([mr.load_run(p.dir)])
        self.assertIn("halted", text)
        self.assertIn("agent unreachable", text)

    def test_header_uses_on_device_json(self):
        p = self.build(2, 2)
        (p.dir / "on_device.json").write_text(json.dumps({
            "model": "qwen-x", "revision": "abc123", "context": 32768, "baseline_prompt_tokens": 9100,
            "server": {"ngl": 99}, "device": "Jetson Thor"}))
        text = mr.render_markdown([mr.load_run(p.dir)])
        for part in ("qwen-x", "abc123", "32768", "9100", "Jetson Thor"):
            self.assertIn(part, text)

    def test_not_run_lists_coverage_reasons(self):
        p = self.build(2, 2)
        (p.dir / "coverage.json").write_text(json.dumps([
            {"task": "camera-capture", "target": "thor", "status": "unconfigured", "reason": "no capture recorded",
             "agents": {}},
            {"task": "deploy", "target": "thor", "status": "ready", "reason": None, "agents": {}}]))
        text = mr.render_markdown([mr.load_run(p.dir)])
        section = text.split("Not run")[1]
        self.assertIn("camera-capture", section)
        self.assertIn("no capture recorded", section)

    def test_not_run_without_a_target_does_not_say_none(self):
        p = self.build(2, 2)
        (p.dir / "coverage.json").write_text(json.dumps([
            {"task": "ros-topics", "target": None, "status": "unconfigured", "reason": "no matching target configured",
             "agents": {}}]))
        text = mr.render_markdown([mr.load_run(p.dir)])
        self.assertIn("- ros-topics: no matching target configured", text)
        self.assertNotIn("on None", text)

    def test_findings_sorted_by_frequency_with_counts(self):
        p = Pass(self)
        for _ in range(3):
            p.attempt(outcome="wrong", trace=[shell("ssh root@x")])
        p.attempt(outcome="missed", trace=[call("device_info")])
        p.attempt(outcome="right", trace=[call("run")])
        run = mr.load_run(p.dir)
        groups = mr.group_findings(run["findings"], len(p.rows))
        self.assertEqual([g["item"] for g in groups], ["Wrong tool", "Missing tool"])
        self.assertEqual((groups[0]["count"], groups[0]["attempts"]), (3, 5))
        text = mr.render_markdown([run])
        self.assertIn("3 of 5 attempts", text)
        self.assertLess(text.index("Wrong tool"), text.index("Missing tool"))

    def test_survives_odd_rows(self):
        p = Pass(self)
        p.attempt(outcome="right", metrics=False)
        p.attempt(outcome="right", trace=[tt.entry("mcp", "run", "not an object", "proxy", False, None, True, "[X] bad"),
                                          tt.entry("mcp", "run", ["a"], "proxy", False, None, True, "[X] bad")])
        with (p.dir / "results.jsonl").open("a") as f:
            f.write('{"id": "torn", "task": "dep')
        text = mr.render_markdown([mr.load_run(p.dir)])
        self.assertIn("Tool error", text)

    def test_write_is_atomic(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run")])
        (p.dir / "report.md").write_text("old")
        mr.write_report([p.dir], p.dir)
        self.assertNotEqual((p.dir / "report.md").read_text(), "old")
        self.assertIn("runs", json.loads((p.dir / "report.json").read_text()))
        self.assertEqual(sorted(x.name for x in p.dir.iterdir() if x.name.startswith(".") or x.suffix == ".tmp"), [])
        with mock.patch("os.replace", side_effect=OSError("disk")):
            with self.assertRaises(OSError):
                mr.write_report([p.dir], p.dir)
        self.assertEqual(sorted(x.name for x in p.dir.iterdir() if x.name.startswith(".")), [])

    def test_two_dirs_give_a_column_per_model(self):
        a, c = Pass(self), Pass(self)
        a.attempt(outcome="wrong", trace=[shell("ssh x")])
        c.attempt(outcome="right", trace=[call("run")])
        (a.dir / "on_device.json").write_text(json.dumps({"model": "alpha-model"}))
        (c.dir / "on_device.json").write_text(json.dumps({"model": "gamma-model"}))
        text = mr.render_markdown([mr.load_run(a.dir), mr.load_run(c.dir)])
        header = next(line for line in text.splitlines() if "alpha-model" in line)
        self.assertIn("gamma-model", header)
        self.assertIn("1 of 1 attempts", text)
        self.assertIn("0 of 1 attempts", text)

    def test_report_failure_does_not_stop_a_pass(self):
        out = Path(tempfile.mkdtemp())
        self.addCleanup(lambda: __import__("shutil").rmtree(out))
        calls = []
        with mock.patch("model_report.write_report", side_effect=RuntimeError("boom")):
            err = io.StringIO()
            with redirect_stderr(err), redirect_stdout(io.StringIO()):
                for _ in range(2):  # the attempt loop keeps going
                    b.write_model_report(out)
                    calls.append(1)
        self.assertEqual(len(calls), 2)
        self.assertIn("boom", err.getvalue())


def start(ident, tool, args=None):
    return {"type": "tool_start", "id": ident, "tool": tool, "arguments": args or {}}


def finish(ident):
    return {"type": "tool_result", "id": ident}


class FixRound1(unittest.TestCase):
    def real_trace(self, events, proxy_records):
        return tt.build_trace("wendy", events, proxy_records=proxy_records, cli_records=[])

    def test_alternating_calls_through_build_trace_are_not_repeated(self):
        p = Pass(self)
        events, records = [], []
        for i in range(3):
            events += [start(f"a{i}", "container_list"), start(f"b{i}", "workspace_exec", {"command": "make"})]
            records.append({"tool": "container_list", "arguments": {}, "is_error": False, "reply_text": "ok"})
        trace = self.real_trace(events, records)
        p.attempt(outcome="right", trace=trace, events=events)
        self.assertEqual(p.classify(), [])

    def test_truly_consecutive_calls_through_build_trace_are_repeated(self):
        p = Pass(self)
        events = [start(f"a{i}", "container_list") for i in range(3)]
        records = [{"tool": "container_list", "arguments": {}, "is_error": False, "reply_text": "ok"}] * 3
        p.attempt(outcome="right", trace=self.real_trace(events, records), events=events)
        self.assertEqual(labels(p.classify()), [("model", "Repeated call")])

    def test_excerpt_follows_transcript_order(self):
        p = Pass(self)
        events = [start("2", "telemetry_logs"), start("1", "workspace_exec", {"command": "make"})]
        records = [{"tool": "telemetry_logs", "arguments": {}, "is_error": True,
                    "reply_text": "[DEVICE_UNREACHABLE] gone"}]
        p.attempt(outcome="right", trace=self.real_trace(events, records), events=events)
        excerpt = p.classify()[0]["excerpt"].splitlines()
        # The workspace_exec ran after the failing call, so it is not part of the excerpt.
        self.assertEqual(len(excerpt), 1)
        self.assertIn("DEVICE_UNREACHABLE", excerpt[0])

    def test_restart_found_across_a_row_without_markers(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run")], markers={"uptime": "5000.0", "socket_inode": 7})
        p.attempt(status="interrupted")
        p.attempt(outcome="right", trace=[call("run")], markers={"uptime": "12.0", "socket_inode": 9})
        found = mr.load_run(p.dir)["findings"]
        self.assertEqual(labels(found), [("tool", "Agent or device restart")])

    def test_halted_pass_lists_ready_entries_not_reached(self):
        p = Pass(self)
        (p.dir / "coverage.json").write_text(json.dumps([
            {"task": "deploy", "target": "thor", "status": "ready", "reason": None,
             "agents": {"chat": {"not_run": 2}}}]))
        text = mr.render_markdown([mr.load_run(p.dir)])
        self.assertNotIn("Everything configured was run", text)
        self.assertIn("not reached", text.split("## Not run")[1])
        self.assertIn("deploy", text.split("## Not run")[1])
        (p.dir / "coverage.json").write_text(json.dumps([
            {"task": "deploy", "target": "thor", "status": "ready", "reason": None,
             "agents": {"chat": {"not_run": 0}}}]))
        self.assertIn("Everything configured was run", mr.render_markdown([mr.load_run(p.dir)]))

    def test_harness_stop_keeps_the_grade_and_is_the_models_finding(self):
        # The harness stopped chat at its call limit. Chat's "context canceled" comes from that stop.
        stop = {"agent": {"exit_code": None, "no_progress": {"kind": "tool_call_cap", "calls": 21, "limit": 20}}}
        events = [{"type": "error", "text": "model response: context canceled"}]
        p = Pass(self)
        p.attempt(outcome="wrong", kind="scenario", status="no_progress", stages=stop, events=events,
                  trace=[call("os_update", canned=True)])
        p.rows[-1]["metrics"]["agent_errors"] = 1
        p.rows[-1]["tool_choice"]["entry"] = 0
        found = labels(p.classify())
        self.assertIn(("model", "Hit the call limit"), found)
        self.assertIn(("model", "Wrong tool"), found)
        self.assertNotIn(("undetermined", "Undetermined"), found)
        self.assertIn("limit of 20", next(f for f in p.classify() if f["item"] == "Hit the call limit")["detail"])

    def test_running_out_of_rounds_keeps_the_grade(self):
        events = [{"type": "error", "text": "stopped after 40 model rounds; send another message to continue"}]
        p = Pass(self)
        p.attempt(outcome="right", verified=True, events=events, trace=[call("run")])
        p.rows[-1]["metrics"]["agent_errors"] = 1
        self.assertEqual(labels(p.classify()), [("model", "Ran out of rounds")])
        self.assertIn("40 model rounds", p.classify()[0]["detail"])

    def test_running_out_of_rounds_before_the_verifier_passed_is_not_the_tools_fault(self):
        events = [{"type": "error", "text": "stopped after 40 model rounds; send another message to continue"}]
        p = Pass(self)
        p.attempt(outcome="right", verified=False, events=events, trace=[call("run")])
        p.rows[-1]["metrics"]["agent_errors"] = 1
        self.assertEqual(sorted(labels(p.classify())), [("model", "Ran out of rounds"), ("model", "Task not finished")])

    def test_another_agent_error_shows_its_text(self):
        p = Pass(self)
        p.attempt(outcome="right", events=[{"type": "error", "text": "provider exploded"}], trace=[call("run")])
        p.rows[-1]["metrics"]["agent_errors"] = 1
        self.assertEqual(labels(p.classify()), [("undetermined", "Undetermined")])
        self.assertIn("provider exploded", p.classify()[0]["detail"])

    def test_unverified_or_failed_agent_goes_to_undetermined(self):
        p = Pass(self)
        p.attempt(status="mcp_unverified", outcome="missed", trace=[])
        self.assertEqual(labels(p.classify()), [("undetermined", "Undetermined")])
        self.assertIn("mcp_unverified", p.classify()[0]["detail"])
        p.attempt(outcome="missed", verified=False, trace=[call("device_info")])
        p.rows[-1]["metrics"]["agent_errors"] = ["HTTP 500"]
        self.assertEqual(labels(p.classify()), [("undetermined", "Undetermined")])
        self.assertIn("HTTP 500", p.classify()[0]["detail"])
        p.attempt(outcome="missed", trace=[call("device_info")])
        p.rows[-1]["metrics"]["agent_completed"] = False
        self.assertEqual(labels(p.classify()), [("undetermined", "Undetermined")])

    def test_timeout_hang_comes_from_unanswered_transcript_calls(self):
        p = Pass(self)
        events = [start("1", "device_info"), finish("1"), start("2", "telemetry_logs")]
        p.attempt(status="timeout", outcome="missed", trace=[], events=events)
        found = p.classify()
        self.assertEqual(labels(found), [("tool", "Hang")])
        self.assertIn("telemetry_logs", found[0]["detail"])
        q = Pass(self)
        q.attempt(status="failed", outcome="missed", trace=[call("device_info")], events=events)
        self.assertNotIn(("tool", "Hang"), labels(q.classify()))

    def test_a_timed_out_builtin_is_not_a_tool_hang(self):
        p = Pass(self)
        events = [start("1", "workspace_exec", {"command": "wendy run"}), start("2", "telemetry_logs")]
        p.attempt(status="timeout", outcome="missed", trace=[], events=events)
        found = p.classify()
        self.assertEqual(labels(found), [("tool", "Hang")])
        self.assertNotIn("workspace_exec", found[0]["detail"])
        self.assertIn("telemetry_logs", found[0]["detail"])

    def test_unsupported_is_bad_arguments_when_the_model_is_blamed(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("device_connect", {"device": "other"}, True,
                                               "[UNSUPPORTED] this device deploys only to itself")])
        found = p.classify()
        self.assertEqual(labels(found), [("model", "Bad arguments")])
        self.assertIn("UNSUPPORTED", found[0]["detail"])
        q = Pass(self)
        q.attempt(status="model_server_down", outcome="right", trace=[call("run", {}, True, "[UNSUPPORTED] no")])
        self.assertNotIn(("model", "Bad arguments"), labels(q.classify()))
        self.assertNotIn(("tool", "Tool error"), labels(q.classify()))

    def test_device_unreachable_stays_a_tool_error(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run", {}, True, "[DEVICE_UNREACHABLE] down")])
        self.assertEqual(labels(p.classify()), [("tool", "Tool error")])

    def test_without_a_tool_list_no_tool_is_made_up(self):
        p = Pass(self)
        events = [start("1", "container_list"), finish("1")]
        p.attempt(outcome="right", trace=[call("container_list")], events=events)
        self.assertIn(("model", "Made-up tool"), labels(mr.classify(p.rows[0], p.dir, ["run"], sc.guard_matchers(APP))))
        self.assertNotIn(("model", "Made-up tool"), labels(mr.classify(p.rows[0], p.dir, None, sc.guard_matchers(APP))))

    def test_an_unreadable_tools_file_is_undetermined_not_made_up_tools(self):
        p = Pass(self)
        events = [start("1", "container_list"), finish("1")]
        p.attempt(outcome="right", trace=[call("container_list")], events=events)
        for text in ('[{"name": "ru', "[]"):
            (p.dir / "tools.json").write_text(text)
            run = mr.load_run(p.dir)
            self.assertNotIn("Made-up tool", [f["item"] for f in run["findings"]])
            notes = [f for f in run["findings"] if f["section"] == "undetermined"]
            self.assertEqual(len(notes), 1)
            self.assertIn("tools.json", notes[0]["detail"])
            self.assertIn("tools.json", mr.render_markdown([run]))

    def test_duration_is_the_wall_time_between_the_manifest_and_the_last_row(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run")])
        (p.dir / "manifest.json").write_text("{}")
        os.utime(p.dir / "manifest.json", (1_000_000, 1_000_000))
        os.utime(p.dir / "results.jsonl", (1_000_000 + 3 * 3600 + 1800, 1_000_000 + 3 * 3600 + 1800))
        text = mr.render_markdown([mr.load_run(p.dir)])
        self.assertIn("| Duration | 210 min |", text)
        self.assertIn("| Agent time | 0 min |", text)  # the one attempt took 10 s
        (p.dir / "manifest.json").unlink()
        self.assertIn("| Duration | - |", mr.render_markdown([mr.load_run(p.dir)]))

    def test_a_lone_surrogate_does_not_block_the_report(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run", {}, True, "[DEVICE_UNREACHABLE] bad \ud800 text")])
        mr.write_report([p.dir], p.dir)
        self.assertIn("\\ud800", (p.dir / "report.md").read_text())

    def test_notes_render_after_the_header(self):
        p = Pass(self)
        p.attempt(outcome="right", trace=[call("run")])
        (p.dir / "on_device.json").write_text(json.dumps({"notes": ["chat sends all 79 tools", "second note"]}))
        text = mr.render_markdown([mr.load_run(p.dir)])
        self.assertIn("## Notes", text)
        self.assertIn("- chat sends all 79 tools", text)
        self.assertLess(text.index("| Scored"), text.index("## Notes"))
        self.assertLess(text.index("## Notes"), text.index("## Model deviations"))
        (p.dir / "on_device.json").write_text(json.dumps({"notes": "just a string"}))
        self.assertNotIn("## Notes", mr.render_markdown([mr.load_run(p.dir)]))
        (p.dir / "on_device.json").write_text(json.dumps({}))
        self.assertNotIn("## Notes", mr.render_markdown([mr.load_run(p.dir)]))


if __name__ == "__main__":
    unittest.main()
