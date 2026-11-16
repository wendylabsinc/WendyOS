import unittest

import grading as g
from tool_trace import build_trace


def mcp(name, **arguments):
    return {"channel": "mcp", "name": name, "arguments": arguments}


def cli(*argv):
    words = [w for w in argv if not w.startswith("-")]
    return {"channel": "cli", "name": " ".join(words), "arguments": list(argv)}


def shell(command):
    return {"channel": "shell", "name": "shell", "arguments": command}


WIFI = {"right": [{"tool": "wifi_connect", "args": {"ssid": "Lab5G"}}],
        "wrong": [{"tool": "container_exec", "args_regex": "nmcli"}]}


THOR_TRACE = [shell("dmesg | grep -i usb"), shell("lsusb"), shell("cat /proc/bus/usb/devices")]
USB_LOGS = {"right": [{"tool": "device_os_logs"}], "wrong": [{"tool": "container_exec"}], "ask": True, "ask_regex": ["which"]}


class WentAroundTests(unittest.TestCase):
    def test_plain_shell_is_went_around_when_enabled(self):
        trace = [shell("dmesg | tail")]
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "went_around")
        self.assertEqual(g.grade(trace, USB_LOGS, "", True)["outcome"], "missed")
        self.assertIn("went_around", g.OUTCOMES)

    def test_right_and_wrong_beat_went_around(self):
        # The Thor attempt ran plain commands and also called the right tool: right wins.
        right = THOR_TRACE + [mcp("device_os_logs")]
        self.assertEqual(g.grade(right, USB_LOGS, "", True, went_around=True)["outcome"], "right")
        wrong = THOR_TRACE + [mcp("container_exec", command=["lsusb"])]
        self.assertEqual(g.grade(wrong, USB_LOGS, "", True, went_around=True)["outcome"], "wrong")
        self.assertEqual(g.grade(THOR_TRACE, USB_LOGS, "", True, went_around=True)["outcome"], "went_around")

    def test_wendy_work_with_a_few_plain_commands_is_not_went_around(self):
        # The Thor's erase-device attempt looked around with ls, then used install tools and the CLI.
        trace = [shell("ls /tmp"), mcp("os_install_plan"), mcp("os_install_start"), cli("install", "plan")]
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "missed")

    def test_connecting_and_reading_help_do_not_outweigh_plain_commands(self):
        trace = [mcp("wendy_status"), mcp("device_connect"), cli("device", "logs", "--help"), cli("--help"),
                 cli("version"), shell("dmesg | tail")]
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "went_around")

    def test_odd_cli_arguments_do_not_break_grading(self):
        trace = [{"channel": "cli", "name": "run", "arguments": [["nested"], 3, "run"]},
                 {"channel": "cli", "name": "x", "arguments": "run"}, shell("dmesg")]
        # One real call (the string word in the first list) against one plain command: a tie is went around.
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "went_around")

    def test_as_many_plain_commands_as_wendy_calls_is_went_around(self):
        trace = [mcp("device_info"), shell("dmesg | tail")]
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "went_around")

    def test_went_around_beats_asked(self):
        r = g.grade(THOR_TRACE, USB_LOGS, "Which one do you want?", True, went_around=True)
        self.assertEqual(r["outcome"], "went_around")
        self.assertEqual(g.grade([], USB_LOGS, "Which one?", True, went_around=True)["outcome"], "asked")

    def test_shell_running_only_wendy_is_not_went_around(self):
        trace = [shell("wendy device os-logs"), shell("sudo wendy --device local device info | head")]
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "missed")
        mixed = [shell("wendy device info; lsusb")]
        self.assertEqual(g.grade(mixed, USB_LOGS, "", True, went_around=True)["outcome"], "missed")

    def test_chat_workspace_browsing_is_builtin_and_workspace_exec_is_shell(self):
        events = [{"type": "tool_start", "id": "1", "tool": "workspace_list", "arguments": {"path": "."}},
                  {"type": "tool_start", "id": "2", "tool": "workspace_read", "arguments": {"path": "a"}}]
        trace = build_trace("wendy", events)
        self.assertEqual([e["channel"] for e in trace], ["builtin", "builtin"])
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "missed")
        events.append({"type": "tool_start", "id": "3", "tool": "workspace_exec", "arguments": {"command": "lsusb"}})
        trace = build_trace("wendy", events)
        self.assertEqual([e["channel"] for e in trace], ["builtin", "builtin", "shell"])
        self.assertEqual(g.grade(trace, USB_LOGS, "", True, went_around=True)["outcome"], "went_around")


class GradeTests(unittest.TestCase):
    def test_wrong_beats_right(self):
        trace = [mcp("wifi_connect", ssid="Lab5G"), mcp("container_exec", command=["nmcli", "dev"])]
        r = g.grade(trace, WIFI, "", True)
        self.assertEqual((r["outcome"], r["rule"], r["entry"]), ("wrong", "wrong[0]", 1))

    def test_required_argument_must_match(self):
        self.assertEqual(g.grade([mcp("wifi_connect", ssid="Other")], WIFI, "", True)["outcome"], "missed")

    def test_cli_prefix_matches_by_word(self):
        m = {"cli": "wifi connect"}
        self.assertTrue(g.matches(m, cli("wifi", "connect", "Lab5G")))
        self.assertFalse(g.matches({"cli": "wifi connect"}, cli("wifi", "connected")))
        self.assertFalse(g.matches({"cli": "os"}, cli("osx")))
        self.assertTrue(g.matches({"cli": "wifi", "args_regex": "Lab5G"}, cli("wifi", "connect", "Lab5G")))

    def test_cli_matcher_skips_flags_like_capture_lookup(self):
        # The shim cans and looks up captures by command_words (flags skipped), so grading must too.
        import tool_trace
        def traced(command):
            events = [{"type": "assistant", "message": {"content": [
                {"type": "tool_use", "id": "1", "name": "Bash", "input": {"command": command}}]}}]
            return tool_trace.build_trace("claude", events)
        sim = {"right": [{"tool": "simulator_create"}, {"cli": "vm create"}]}
        stop = {"right": [{"tool": "container_stop"}, {"cli": "device apps stop"}]}
        for command, expect in (("wendy vm create dog --profile go2", sim),
                                ("wendy vm --yes create dog --profile go2", sim),
                                ("wendy vm -y create dog", sim),
                                ("wendy device --yes apps stop sensor-dashboard", stop),
                                ("wendy --json device apps stop sensor-dashboard", stop)):
            with self.subTest(command=command):
                self.assertEqual(g.grade(traced(command), expect, "", True)["outcome"], "right")
        # Still whole words from the start, and help is still not graded.
        self.assertEqual(g.grade(traced("wendy device --yes apps list"), stop, "", True)["outcome"], "missed")
        self.assertEqual(g.grade(traced("wendy vm --yes create --help"), sim, "", True)["outcome"], "missed")
        self.assertFalse(g.matches({"cli": "apps stop"}, {"channel": "cli", "name": "device",
                                                          "arguments": ["device", "--yes", "apps", "stop"]}))

    def test_help_lookups_never_match(self):
        # Reading help is informational: it is neither the job done nor the wrong way to do it.
        expect = {"right": [{"cli": "os update-status"}], "wrong": [{"cli": "os update", "block": True}]}
        for argv in (["os", "update", "--help"], ["os", "update", "-h"], ["--device", "x", "os", "update", "--help"]):
            entry = {"channel": "cli", "name": "os update", "arguments": argv}
            with self.subTest(argv=argv):
                self.assertFalse(g.matches({"cli": "os update"}, entry))
                self.assertEqual(g.grade([entry], expect, "", True)["outcome"], "missed")
        self.assertFalse(g.matches({"cli": "os update-status"}, cli("os", "update-status", "--help")))
        # After "--" the words belong to the device-side command, not to wendy.
        self.assertTrue(g.matches({"cli": "device shell"}, cli("device", "shell", "--", "nmcli", "--help")))
        self.assertTrue(g.matches({"cli": "os update"}, cli("os", "update", "--help=false")))
        self.assertEqual(g.grade([cli("os", "update")], expect, "", True)["outcome"], "wrong")

    def test_same_job_via_mcp_and_cli_is_right_once(self):
        expect = {"right": [{"tool": "run"}, {"cli": "run"}], "wrong": [{"shell": "\\b(docker|ssh)\\b"}]}
        trace = [mcp("run"), cli("run")]
        r = g.grade(trace, expect, "", True)
        self.assertEqual((r["outcome"], r["rule"]), ("right", "right[0]"))
        self.assertEqual(g.grade(trace + [shell("docker ps")], expect, "", True)["outcome"], "wrong")

    def test_asked_only_when_expected_and_completed(self):
        text = "Which drive should I erase?"
        self.assertEqual(g.grade([], {"ask": True, "ask_regex": ["which"]}, text, True)["outcome"], "asked")
        self.assertEqual(g.grade([], {"ask": False}, text, True)["outcome"], "missed")
        self.assertEqual(g.grade([], {"ask": True, "ask_regex": ["which"]}, text, False)["outcome"], "missed")

    def test_hidden_group_only_for_group_discovering_clients(self):
        e = {"right": [{"tool": "wifi_connect"}]}
        self.assertTrue(g.grade([], e, "", True, group="setup", adapter="claude")["hidden_group"])
        self.assertFalse(g.grade([mcp("wendy_tools", groups=["setup"])], e, "", True, group="setup", adapter="claude")["hidden_group"])
        self.assertFalse(g.grade([mcp("wendy_tools", groups=["all"])], e, "", True, group="setup", adapter="codex")["hidden_group"])
        self.assertFalse(g.grade([], e, "", True, group="setup", adapter="wendy")["hidden_group"])
        self.assertFalse(g.grade([], e, "", True, group="core", adapter="claude")["hidden_group"])

    def test_agents_filter_limits_matcher(self):
        m = {"tool": "run", "agents": ["codex"]}
        self.assertTrue(g.matches(m, mcp("run"), "codex"))
        self.assertFalse(g.matches(m, mcp("run"), "claude"))

    def test_list_argument_contains(self):
        self.assertTrue(g.matches({"tool": "x", "args": {"c": "a"}}, mcp("x", c=["a", "b"])))

    def test_validate_expect_rejects_unknown_keys_and_bad_regex(self):
        g.validate_expect(WIFI)
        for bad in ({"nope": 1}, {"right": [{"tool": "a", "cli": "b"}]}, {"right": [{}]},
                    {"wrong": [{"shell": "("}]}, {"right": [{"tool": "a", "args_regex": "["}]},
                    {"right": [{"tool": "a", "extra": 1}]}):
            with self.assertRaises(ValueError):
                g.validate_expect(bad)

    def test_validate_expect_rejects_silent_typos(self):
        for bad in ({"right": [{"tool": 1}]}, {"right": [{"cli": ["run"]}]}, {"wrong": [{"shell": 5}]},
                    {"right": [{"tool": "a", "agents": "claude"}]}, {"right": [{"tool": "a", "agents": [1]}]},
                    {"right": [{"tool": "a", "block": "yes"}]},
                    {"right": [{"cli": "run", "args": {"a": 1}}]}, {"wrong": [{"shell": "x", "args": {}}]},
                    {"right": [{"tool": "a", "args": ["x"]}]},
                    {"wrong": [{"shell": "x", "args_regex": "y"}]}, {"right": [{"tool": "a", "args_regex": 3}]}):
            with self.assertRaises(ValueError, msg=bad):
                g.validate_expect(bad)
        g.validate_expect({"right": [{"tool": "a", "agents": ["claude"], "block": True}]})

    def test_compose_app_shell_wrong_rules(self):
        import json
        from pathlib import Path
        tasks = json.loads((Path(__file__).parent / "tasks.json").read_text())
        wrong = next(t for t in tasks if t["id"] == "compose-app")["expect"]["wrong"]
        hit = lambda c: any(g.matches(m, shell(c)) for m in wrong)
        for ok in ("cat docker-compose.yml", "docker compose up -d", "grep -r docker ."):
            self.assertFalse(hit(ok), ok)
        for bad in ("ssh root@10.0.0.5 'docker ps'", "cd x && scp a b:", "docker -H tcp://10.0.0.5:2375 ps"):
            self.assertTrue(hit(bad), bad)

    def test_stop_and_start_have_cli_matchers(self):
        import json
        from pathlib import Path
        tasks = {t["id"]: t for t in json.loads((Path(__file__).parent / "tasks.json").read_text())}
        self.assertIn({"cli": "device apps stop"}, tasks["stop-app"]["expect"]["right"])
        self.assertIn({"cli": "device apps start"}, tasks["start-app"]["expect"]["right"])

    def test_diagnosis_tasks_accept_the_cli_equivalents(self):
        # The same CLI paths the scenarios accept for these tools (checked with `wendy WORDS --help`).
        import json
        from pathlib import Path
        tasks = {t["id"]: t for t in json.loads((Path(__file__).parent / "tasks.json").read_text())}
        cases = {"debug-otel-logs": [["device", "logs", "--app", "worker"]],
                 "debug-otel-metrics": [["device", "telemetry-stream", "--metrics", "--app", "worker"]],
                 "debug-otel-traces": [["device", "telemetry-stream", "--traces"]],
                 "debug-go2-ros2": [["device", "ros2", "topics"], ["device", "ros2", "topic", "info", "/odom"]],
                 "debug-g1-ros2": [["device", "ros2", "topics", "--all"], ["device", "ros2", "topic", "info", "/odom"]],
                 "repair-go2-camera": [["device", "camera", "list"], ["device", "camera", "view", "--stdout"]],
                 "repair-g1-camera": [["device", "camera", "list", "--json"], ["--json", "device", "camera", "view"]]}
        for task, argvs in cases.items():
            for argv in argvs:
                with self.subTest(task=task, argv=argv):
                    self.assertEqual(g.grade([cli(*argv)], tasks[task]["expect"], "", True)["outcome"], "right")
        # Metrics and traces are told apart by their stream flag, as in the scenarios.
        self.assertEqual(g.grade([cli("device", "telemetry-stream", "--logs")], tasks["debug-otel-metrics"]["expect"],
                                 "", True)["outcome"], "missed")
        self.assertEqual(g.grade([cli("device", "telemetry-stream", "--metrics")], tasks["debug-otel-traces"]["expect"],
                                 "", True)["outcome"], "missed")


if __name__ == "__main__":
    unittest.main()
