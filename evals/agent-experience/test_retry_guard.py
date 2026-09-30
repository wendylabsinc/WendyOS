import json
from pathlib import Path
import sys
import tempfile
import unittest

import benchmark as b
from retry_guard import RetryGuard


ERROR = '✗ unknown command "ros2" for "wendy"'
ENVELOPE = json.dumps({"error": {
    "code": "cli_usage", "exit": 2, "message": 'unknown command "ros2" for "wendy"',
    "retryable": False, "next_steps": ["Run 'wendy --help' for usage."]}})


class RetryGuardTests(unittest.TestCase):
    def test_only_returned_errors_count_and_duplicate_chunks_count_once(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events"
            guard = RetryGuard(path)
            def send(event):
                with path.open("a") as out:
                    out.write(json.dumps(event) + "\n")
                return guard()
            self.assertIsNone(send({"type": "assistant", "message": {"content": [
                {"type": "text", "text": ERROR}, {"type": "tool_use", "input": {"command": ERROR}}]}}))
            first = {"type": "user", "message": {"content": [
                {"type": "tool_result", "tool_use_id": "1", "content": ERROR}]}}
            self.assertIsNone(send(first))
            self.assertIsNone(send(first))
            self.assertIsNone(send({"type": "item.completed", "item": {
                "type": "command_execution", "id": "2", "aggregated_output": ERROR}}))
            reason = send({"type": "tool_result", "id": "3", "text": ERROR})
            self.assertEqual(reason["completed_tools"], 3)

    def test_partial_lines_and_transient_network_errors_do_not_trip(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events"
            guard = RetryGuard(path, 1)
            event = json.dumps({"type": "tool_result", "id": "1", "text": ERROR})
            path.write_text(event[:30])
            self.assertIsNone(guard())
            with path.open("a") as out:
                out.write(event[30:] + "\n")
            self.assertIsNotNone(guard())
            path = Path(directory) / "network-events"
            path.write_text("\n".join(json.dumps({"type": "tool_result", "id": str(i),
                "text": "connection refused: waiting for first boot"}) for i in range(10)) + "\n")
            self.assertIsNone(RetryGuard(path, 1)())

    def test_json_error_envelopes_count_like_text_errors(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "events"
            path.write_text("\n".join(json.dumps({"type": "tool_result", "id": str(i),
                "text": "Exit code 2\n" + ENVELOPE}) for i in range(3)) + "\n")
            reason = RetryGuard(path)()
            self.assertIsNotNone(reason)
            self.assertEqual(reason["error"], 'unknown command "ros2" for "wendy"')
            self.assertEqual(reason["completed_tools"], 3)
            # A transient failure reported as an envelope still never trips.
            path = Path(directory) / "network-events"
            network = json.dumps({"error": {"code": "device_unreachable", "exit": 5,
                "message": "Could not connect to device at 127.0.0.1:1.", "retryable": True, "next_steps": []}})
            path.write_text("\n".join(json.dumps({"type": "tool_result", "id": str(i), "text": network})
                for i in range(5)) + "\n")
            self.assertIsNone(RetryGuard(path, 1)())

    def test_runner_terminates_repeated_failures_without_steering(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)
            code = ("import json,time\nfor i in range(3):\n print(json.dumps("
                    "{'type':'tool_result','id':str(i),'text':" + repr(ERROR) + "}),flush=True)\ntime.sleep(10)")
            result = b.run_process([sys.executable, "-c", code], path, path / "agent", 5,
                                   progress_guard=RetryGuard(path / "agent.stdout"))
            self.assertEqual(result["no_progress"]["completed_tools"], 3)
            self.assertFalse(result["timed_out"])
            self.assertLess(result["seconds"], 4)

    def test_effective_time_limit_is_the_smaller_explicit_limit(self):
        self.assertEqual(b.agent_timeout({"max_agent_seconds": 900}, {"timeout_seconds": 1800}), 900)
        self.assertEqual(b.agent_timeout({"max_agent_seconds": 900}, {"timeout_seconds": 60}), 60)
        for value in (0, -1, True, float("inf"), "900"):
            with self.assertRaises(ValueError):
                b.agent_timeout({"max_agent_seconds": value}, {})


if __name__ == "__main__":
    unittest.main()
