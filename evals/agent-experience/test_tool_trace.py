import json
from pathlib import Path
import tempfile
import unittest

import benchmark as b
import tool_trace as t


def pairs(trace):
    return [(e["channel"], e["name"]) for e in trace]


class ToolTraceTests(unittest.TestCase):
    def test_claude_mcp_bash_and_builtin_are_classified(self):
        def use(name, input):
            return {"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": name, "name": name, "input": input}]}}
        events = [use("mcp__wendy__run", {}),
                  use("Bash", {"command": "wendy --device vm:x wifi connect Lab5G"}),
                  use("Read", {"file_path": "/x"})]
        self.assertEqual(pairs(t.build_trace("claude", events)),
                         [("mcp", "run"), ("shell", "shell"), ("cli", "wifi connect Lab5G"), ("builtin", "Read")])

    def test_codex_items_deduplicate_and_use_server_tool_names(self):
        call = {"type": "mcp_tool_call", "id": "a", "server": "wendy", "tool": "container_list", "arguments": {"x": 1}}
        events = [{"type": "item.started", "item": dict(call)},
                  {"type": "item.completed", "item": dict(call)},
                  {"type": "item.completed", "item": {"type": "mcp_tool_call", "id": "b", "server": "other", "tool": "ping"}},
                  {"type": "item.completed", "item": {"type": "command_execution", "id": "c", "command": "ls"}}]
        trace = t.build_trace("codex", events)
        self.assertEqual(pairs(trace), [("mcp", "container_list"), ("builtin", "other.ping"), ("shell", "shell")])
        self.assertEqual(trace[0]["arguments"], {"x": 1})

    def test_wendy_chat_separates_builtins_shell_and_mcp(self):
        events = [{"type": "tool_start", "id": "1", "tool": "workspace_exec", "arguments": {"command": "wendy device list"}},
                  {"type": "tool_start", "id": "2", "tool": "wendy_docs", "arguments": {}},
                  {"type": "tool_start", "id": "3", "tool": "wifi_status", "arguments": {}},
                  {"type": "tool_start", "id": "3", "tool": "wifi_status", "arguments": {}},
                  {"type": "tool_start", "id": "3", "agent_id": "child", "tool": "wifi_status", "arguments": {}}]
        self.assertEqual(pairs(t.build_trace("wendy", events)),
                         [("shell", "shell"), ("cli", "device list"), ("builtin", "wendy_docs"),
                          ("mcp", "wifi_status"), ("mcp", "wifi_status")])

    def test_shell_parsing_handles_env_prefix_pipes_and_nested_shells(self):
        p = t.parse_wendy_invocations
        self.assertEqual(p('WENDY_DEVICE=x /opt/bin/wendy --json run --detach | tee log'), [["--json", "run", "--detach"]])
        self.assertEqual(p('sudo sh -c "wendy os install --yes"'), [["os", "install", "--yes"]])
        self.assertEqual(p('echo wendy'), [])
        self.assertEqual(p('wendy "unbalanced'), [])
        self.assertEqual(p('wendy a && wendy b; echo hi'), [["a"], ["b"]])
        self.assertEqual(t.cli_path(["--json", "--device=x", "run", "--device", "y", "app", "--detach", "z"]), ["run", "app"])

    def test_redirections_never_contribute_arguments(self):
        p = t.parse_wendy_invocations
        self.assertEqual(p('wendy run 2>&1 | tee x'), [["run"]])
        self.assertEqual(p('wendy run 2>/dev/null'), [["run"]])
        self.assertEqual(p('wendy run &>log'), [["run"]])
        self.assertEqual(p('wendy run --detach 2>/dev/null'), [["run", "--detach"]])
        self.assertEqual(p('wendy run > out --detach'), [["run", "--detach"]])
        self.assertEqual(p('wendy run >> out 2>> err < in'), [["run"]])
        self.assertEqual(p('wendy run >&log'), [["run"]])

    def test_wrapper_flags_are_skipped(self):
        p = t.parse_wendy_invocations
        self.assertEqual(p('sudo -u me wendy run'), [["run"]])
        self.assertEqual(p('env -i PATH=/x wendy device list'), [["device", "list"]])
        self.assertEqual(p('env -u FOO -S x wendy a'), [["a"]])
        self.assertEqual(p('sudo -E wendy b'), [["b"]])

    def test_newlines_separate_commands_outside_quotes(self):
        p = t.parse_wendy_invocations
        self.assertEqual(p('wendy a\nwendy b'), [["a"], ["b"]])
        self.assertEqual(p('wendy a\\\n  --x\nwendy b'), [["a", "--x"], ["b"]])
        self.assertEqual(p('echo "x\nwendy y"'), [])

    def test_proxy_and_wrapper_records_replace_transcript_channels(self):
        events = [{"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "1", "name": "mcp__wendy__x", "input": {}},
            {"type": "tool_use", "id": "2", "name": "Bash", "input": {"command": "wendy run"}}]}}]
        trace = t.build_trace("claude", events,
                              proxy_records=[{"tool": "run", "arguments": {}, "canned": True, "captured": True}],
                              cli_records=[{"argv": ["run"], "canned": True, "captured": False}])
        self.assertEqual(sorted(pairs(trace)), [("cli", "run"), ("mcp", "run"), ("shell", "shell")])
        by = {e["channel"]: e for e in trace}
        self.assertEqual((by["mcp"]["source"], by["cli"]["source"], by["shell"]["source"]), ("proxy", "wrapper", "transcript"))
        self.assertIs(by["mcp"]["canned"], True)
        self.assertIs(by["cli"]["canned"], True)
        self.assertIs(by["cli"]["captured"], False)

    def test_trace_carries_error_and_reply_text(self):
        trace = t.build_trace("claude", [], proxy_records=[
            {"tool": "run", "arguments": {}, "canned": False, "captured": None, "is_error": True, "reply_text": "boom"},
            {"tool": "x", "arguments": {}, "canned": True, "captured": False, "is_error": False, "reply_text": None}],
            cli_records=[])
        self.assertEqual([(e["is_error"], e["reply_text"]) for e in trace], [(True, "boom"), (False, None)])
        self.assertEqual(t.entry("mcp", "run", {}), {"channel": "mcp", "name": "run", "arguments": {}, "source": "transcript",
                                                     "canned": False, "captured": None, "is_error": None, "reply_text": None})

    def test_profile_skill_is_a_chat_builtin(self):
        events = [{"type": "tool_start", "id": "1", "tool": "profile_skill", "arguments": {}}]
        self.assertEqual(pairs(t.build_trace("wendy", events)), [("builtin", "profile_skill")])

    def test_read_jsonl_skips_partial_last_line(self):
        with tempfile.TemporaryDirectory() as d:
            path = Path(d) / "x.jsonl"
            path.write_text('junk\n{"x": 1}\n{"y":')
            self.assertEqual(t.read_jsonl(path), [{"x": 1}])

    def test_metrics_includes_tool_trace(self):
        events = [{"type": "assistant", "message": {"content": [
            {"type": "tool_use", "id": "1", "name": "Read", "input": {}}]}}]
        self.assertEqual(b.metrics("claude", events)["tool_trace"], t.build_trace("claude", events))


if __name__ == "__main__":
    unittest.main()
