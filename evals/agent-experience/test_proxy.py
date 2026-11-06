import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest import mock

from fake_mcp_server import TOOLS
from mcp_client import StdioMCP
import mcp_proxy

HERE = Path(__file__).resolve().parent
PROXY = str(HERE / "mcp_proxy.py")
FAKE = str(HERE / "fake_mcp_server.py")
CAPTURED = {"content": [{"type": "text", "text": "captured write"}], "isError": False}


class ProxyTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "tools.json").write_text(json.dumps(TOOLS))
        captures = self.root / "canned" / "s"
        captures.mkdir(parents=True)
        (captures / "write_thing.json").write_text(json.dumps(
            {"tool": "write_thing", "captured_with": {}, "replies": [CAPTURED]}))
        self.fake_log = self.root / "fake.jsonl"
        self.proxy_log = self.root / "proxy.jsonl"

    def env(self, real_wendy=FAKE, **overrides):
        scenario = {"real_wendy": real_wendy, "tools": str(self.root / "tools.json"),
                    "canned_dir": str(self.root / "canned"), "canned_set": "s",
                    "canned": [], "allow_real": [], "block": [], "context": {"device": "vm:test"},
                    "proxy_log": str(self.proxy_log), "cli_log": str(self.root / "cli.jsonl"),
                    "cli_safe": str(self.root / "cli_safe.json"), "shim": str(self.root / "wendy"),
                    **overrides}
        path = self.root / "scenario.json"
        path.write_text(json.dumps(scenario))
        return {**os.environ, "WENDY_EVAL_SCENARIO": str(path), "FAKE_MCP_LOG": str(self.fake_log)}

    def proxy(self, **overrides):
        return StdioMCP([sys.executable, PROXY, "mcp", "serve"], env=self.env(**overrides), timeout=5)

    def records(self):
        return [json.loads(line) for line in self.proxy_log.read_text().splitlines()]

    def fake_messages(self):
        if not self.fake_log.exists():
            return []
        return [json.loads(line) for line in self.fake_log.read_text().splitlines()]

    def fake_calls(self):
        return [(m["params"]["name"], m["params"].get("arguments"))
                for m in self.fake_messages() if m.get("method") == "tools/call"]

    def send_raw(self, mcp, data):
        mcp.proc.stdin.write(data + b"\n")
        mcp.proc.stdin.flush()
        return mcp._read_line(time.monotonic() + 5)

    def test_read_only_call_passes_through(self):
        with self.proxy() as mcp:
            mcp.initialize()
            self.assertEqual(len(mcp.list_tools()), len(TOOLS))
            result = mcp.call("read_thing", {"a": 1})
            self.assertIn("read_thing", result["content"][0]["text"])
        self.assertEqual(self.fake_calls(), [("read_thing", {"a": 1})])
        [record] = self.records()
        self.assertEqual({k: record[k] for k in ("seq", "tool", "arguments", "canned", "captured", "is_error")},
                         {"seq": 1, "tool": "read_thing", "arguments": {"a": 1}, "canned": False,
                          "captured": None, "is_error": False})
        self.assertIsInstance(record["t"], float)

    def test_mutating_call_is_canned_and_never_reaches_server(self):
        with self.proxy() as mcp:
            mcp.initialize()
            self.assertEqual(mcp.call("write_thing", {"x": "y"}), CAPTURED)
        self.assertNotIn("write_thing", self.fake_log.read_text())
        [record] = self.records()
        self.assertEqual((record["tool"], record["arguments"], record["canned"], record["captured"], record["is_error"]),
                         ("write_thing", {"x": "y"}, True, True, False))

    def test_canned_read_switches_to_its_after_state_once_the_action_is_canned(self):
        before = {"content": [{"type": "text", "text": "running"}], "isError": False}
        after = {"content": [{"type": "text", "text": "stopped"}], "isError": False}
        (self.root / "canned/s/read_thing.json").write_text(json.dumps(
            {"tool": "read_thing", "replies": [before],
             "after": {"actions": ["tool:write_thing", "cli:thing stop"], "replies": [after]}}))
        with self.proxy(canned=["read_thing"]) as mcp:
            mcp.initialize()
            self.assertEqual(mcp.call("read_thing", {}), before)
            self.assertEqual(mcp.call("read_thing", {}), before)
            self.assertEqual(mcp.call("write_thing", {}), CAPTURED)  # canned: the action happened
            self.assertEqual(mcp.call("read_thing", {}), after)
        self.assertNotIn("read_thing", self.fake_log.read_text() if self.fake_log.exists() else "")

    def test_canned_read_sees_a_cli_canned_action(self):
        after = {"content": [{"type": "text", "text": "stopped"}], "isError": False}
        (self.root / "canned/s/read_thing.json").write_text(json.dumps(
            {"tool": "read_thing", "replies": [CAPTURED],
             "after": {"actions": ["cli:vm stop"], "replies": [after]}}))
        (self.root / "cli.jsonl").write_text(
            json.dumps({"argv": ["vm", "--yes", "stop", "bench-sim"], "canned": True}) + "\n")
        with self.proxy(canned=["read_thing"]) as mcp:
            mcp.initialize()
            self.assertEqual(mcp.call("read_thing", {}), after)

    def test_unknown_tool_is_canned(self):
        with self.proxy() as mcp:
            mcp.initialize()
            self.assertEqual(mcp.call("mystery_tool", {}), mcp_proxy.FALLBACK_RESULT)
        self.assertNotIn("mystery_tool", self.fake_log.read_text())
        [record] = self.records()
        self.assertEqual((record["tool"], record["canned"], record["captured"]), ("mystery_tool", True, False))

    def test_allow_real_and_always_real_pass_through(self):
        with self.proxy(allow_real=["write_thing"]) as mcp:
            mcp.initialize()
            self.assertIn("write_thing", mcp.call("write_thing", {})["content"][0]["text"])
            self.assertIn("wendy_tools", mcp.call("wendy_tools", {"groups": ["setup"]})["content"][0]["text"])
        self.assertEqual(self.fake_calls(), [("write_thing", {}), ("wendy_tools", {"groups": ["setup"]})])
        self.assertEqual([r["canned"] for r in self.records()], [False, False])

    def test_block_beats_allow_real_for_container_exec(self):
        block = [{"tool": "container_exec", "args_regex": "nmcli", "block": True}]
        with self.proxy(allow_real=["container_exec"], block=block) as mcp:
            mcp.initialize()
            self.assertEqual(mcp.call("container_exec", {"command": ["nmcli", "dev"]}), mcp_proxy.FALLBACK_RESULT)
            mcp.call("container_exec", {"command": ["ls"]})
        self.assertEqual(self.fake_calls(), [("container_exec", {"command": ["ls"]})])
        self.assertEqual([(r["arguments"]["command"][0], r["canned"]) for r in self.records()],
                         [("nmcli", True), ("ls", False)])

    def test_canned_reply_preserves_request_id(self):
        with self.proxy() as mcp:
            mcp.initialize()
            for raw_id in (b"9007199254740993", b'"abc"'):
                line = self.send_raw(mcp, b'{"jsonrpc":"2.0","id":' + raw_id
                                     + b',"method":"tools/call","params":{"name":"write_thing","arguments":{}}}')
                self.assertRegex(line, rb'"id"\s*:\s*' + re.escape(raw_id) + rb"\s*[,}]")
                self.assertEqual(json.loads(line)["result"], CAPTURED)
        self.assertNotIn("write_thing", self.fake_log.read_text())

    def test_cancel_for_canned_request_is_dropped(self):
        with self.proxy() as mcp:
            mcp.initialize()
            self.send_raw(mcp, b'{"jsonrpc":"2.0","id":"w1","method":"tools/call",'
                               b'"params":{"name":"write_thing","arguments":{}}}')
            for request_id in ("w1", "r1"):
                mcp._send({"jsonrpc": "2.0", "method": "notifications/cancelled",
                           "params": {"requestId": request_id, "reason": "user"}})
            mcp.call("read_thing", {})  # in-order sync: the fake has seen every forwarded line
        cancelled = [m["params"]["requestId"] for m in self.fake_messages()
                     if m.get("method") == "notifications/cancelled"]
        self.assertEqual(cancelled, ["r1"])

    def test_case_variant_keys_never_reach_server(self):
        # Go's encoding/json matches keys case-insensitively and lets the last duplicate win.
        with self.proxy(allow_real=["container_exec"],
                        block=[{"tool": "container_exec", "args": {"command": "nmcli"}, "block": True}]) as mcp:
            mcp.initialize()
            reply = json.loads(self.send_raw(mcp, b'{"jsonrpc":"2.0","id":7,"Method":"tools/call",'
                                                  b'"params":{"name":"write_thing","arguments":{}}}'))
            self.assertEqual((reply["id"], reply["error"]["code"]), (7, -32600))
            for params in (b'{"name":"read_thing","Name":"write_thing","arguments":{}}',
                           b'{"name":"container_exec","arguments":{"command":["ls"],"Command":["nmcli"]}}',
                           b'{"name":"container_exec","arguments":["nmcli"]}'):
                reply = json.loads(self.send_raw(mcp, b'{"jsonrpc":"2.0","id":8,"method":"tools/call","params":'
                                                      + params + b"}"))
                self.assertEqual(reply["result"], mcp_proxy.FALLBACK_RESULT)
            mcp.call("read_thing", {})
        self.assertEqual(self.fake_calls(), [("read_thing", {})])
        self.assertEqual([r["canned"] for r in self.records()], [True, True, True, False])

    def test_undecodable_call_is_still_intercepted(self):
        with self.proxy() as mcp:
            mcp.initialize()
            line = self.send_raw(mcp, b'{"jsonrpc":"2.0","id":9,"method":"tools/call",'
                                      b'"params":{"name":"write_thing","arguments":{"x":"\xff"}}}')
            self.assertEqual(json.loads(line)["result"], CAPTURED)
        self.assertEqual(self.fake_calls(), [])

    def test_unanswered_forwarded_call_is_logged_at_exit(self):
        env = {**self.env(), "FAKE_MCP_SILENT": "1"}
        call = b'{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_thing","arguments":{}}}\n'
        done = subprocess.run([sys.executable, PROXY, "mcp", "serve"], input=call, env=env,
                              capture_output=True, timeout=10)
        self.assertEqual(done.returncode, 0)
        self.assertEqual([(r["tool"], r["canned"], r["is_error"]) for r in self.records()],
                         [("read_thing", False, None)])

    def test_exits_with_child_code_while_agent_stdin_is_open(self):
        child = self.root / "exit3"
        child.write_text("#!/bin/sh\nexit 3\n")
        child.chmod(0o755)
        proc = subprocess.Popen([sys.executable, PROXY, "mcp", "serve"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=self.env(real_wendy=str(child)))
        try:
            self.assertEqual(proc.wait(timeout=10), 3)
            self.assertEqual(proc.stderr.read(), b"")  # no shutdown crash from the blocked stdin reader
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
            for stream in (proc.stdin, proc.stdout, proc.stderr):
                stream.close()

    def test_lines_split_across_reads_and_keep_a_final_partial_line(self):
        big = b"x" * 200000
        parts = [b"a\nb", b"c\n\n" + big[:70000], big[70000:] + b"\nend"]
        read_fd, write_fd = os.pipe()

        def feed():
            for part in parts:
                os.write(write_fd, part)
            os.close(write_fd)
        writer = threading.Thread(target=feed)
        writer.start()
        with os.fdopen(read_fd, "rb", buffering=0) as stream:
            self.assertEqual(list(mcp_proxy._lines(stream)), [b"a\n", b"bc\n", b"\n", big + b"\n", b"end"])
        writer.join()

    def test_policy_rules_first_match_wins(self):
        policy = mcp_proxy.CannedPolicy(
            TOOLS, canned=["read_thing"], allow_real=["write_thing", "container_exec"],
            block=[{"tool": "container_exec", "args_regex": "nmcli", "block": True, "agents": ["codex"]}])
        self.assertTrue(policy.should_can("container_exec", {"command": ["nmcli"]}))  # block, for every agent
        self.assertFalse(policy.should_can("container_exec", {"command": ["ls"]}))   # allow_real
        self.assertTrue(policy.should_can("read_thing", {}))                         # canned beats read-only
        self.assertFalse(policy.should_can("write_thing", {}))                       # allow_real
        self.assertFalse(policy.should_can("wendy_tools", {}))                       # ALWAYS_REAL
        self.assertFalse(policy.should_can("device_connect", {}))                    # ALWAYS_REAL, not in tools
        self.assertTrue(policy.should_can("mystery_tool", {}))                       # unknown
        bare = mcp_proxy.CannedPolicy(TOOLS, [], [], [])
        self.assertFalse(bare.should_can("read_thing", {}))                          # readOnlyHint true
        self.assertTrue(bare.should_can("write_thing", {}))                          # readOnlyHint false
        self.assertTrue(mcp_proxy.CannedPolicy([{"name": "x", "annotations": {}}], [], [], []).should_can("x", {}))


    def test_passthrough_forwards_state_changing_calls(self):
        with self.proxy(passthrough=True) as mcp:
            mcp.initialize()
            self.assertIn("write_thing", mcp.call("write_thing", {"x": 1})["content"][0]["text"])
            self.assertIn("mystery_tool", mcp.call("mystery_tool", {})["content"][0]["text"])
        self.assertEqual(self.fake_calls(), [("write_thing", {"x": 1}), ("mystery_tool", {})])
        self.assertEqual([r["canned"] for r in self.records()], [False, False])

    def test_passthrough_still_cans_block_matches(self):
        block = [{"tool": "write_thing", "args": {"app_name": "own"}, "block": True}]
        with self.proxy(passthrough=True, block=block) as mcp:
            mcp.initialize()
            self.assertEqual(mcp.call("write_thing", {"app_name": "own"}), CAPTURED)
            mcp.call("write_thing", {"app_name": "other"})
        self.assertEqual(self.fake_calls(), [("write_thing", {"app_name": "other"})])
        self.assertEqual([r["canned"] for r in self.records()], [True, False])
        policy = mcp_proxy.CannedPolicy(TOOLS, [], [], block, passthrough=True)
        self.assertTrue(policy.should_can("write_thing", {"app_name": "own"}))
        self.assertFalse(policy.should_can("write_thing", {"app_name": "other"}))
        self.assertTrue(mcp_proxy.CannedPolicy(TOOLS, [], [], [], passthrough=False).should_can("write_thing", {}))

    def test_record_keeps_bounded_reply_text(self):
        with mock.patch.dict(os.environ, {"FAKE_MCP_ERROR_TOOLS": "read_thing"}):
            with self.proxy() as mcp:
                mcp.initialize()
                mcp.call("read_thing", {"pad": "x" * 5000})
                mcp.call("write_thing", {})
        failed, canned = self.records()
        self.assertIs(failed["is_error"], True)
        self.assertEqual(len(failed["reply_text"]), 2000)
        self.assertTrue(failed["reply_text"].startswith('{"tool": "read_thing"'))
        self.assertIs(canned["canned"], True)
        self.assertIsNone(canned["reply_text"])

    def test_unanswered_call_has_no_reply_text(self):
        with mock.patch.dict(os.environ, {"FAKE_MCP_SILENT": "1"}):
            with self.proxy() as mcp:
                mcp.proc.stdin.write(json.dumps({"jsonrpc": "2.0", "id": 9, "method": "tools/call",
                                                 "params": {"name": "read_thing", "arguments": {}}}).encode() + b"\n")
                mcp.proc.stdin.flush()
                time.sleep(.3)
        [record] = self.records()
        self.assertIsNone(record["reply_text"])


if __name__ == "__main__":
    unittest.main()
