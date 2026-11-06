from collections import Counter
import contextlib
import io
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import unittest
from types import SimpleNamespace
from unittest import mock

import benchmark as b
import eval_wendy
from fake_mcp_server import TOOLS
import canned
from grading import grade, matches as grade_matches
import scenarios as sc

HERE = Path(__file__).resolve().parent
FAKE = str(HERE / "fake_mcp_server.py")
CAPTURED = {"content": [{"type": "text", "text": "wrote it"}], "isError": False}
AGENT = """import json, os, sys
sys.path.insert(0, {here!r})
from mcp_client import StdioMCP
with StdioMCP([os.environ.get("WENDY_EVAL_SHIM", "wendy"), "mcp", "serve"], timeout=20) as mcp:
    mcp.initialize()
    for i, (name, args) in enumerate((("write_thing", {{"path": "x"}}), ("read_thing", {{}}))):
        print(json.dumps({{"type": "tool_start", "id": str(i), "tool": name, "arguments": args}}), flush=True)
        mcp.call(name, args)
    if os.environ.get("FAKE_AGENT_HANG"):
        import time; time.sleep(30)
print(json.dumps({{"type": "usage", "usage": {{"input_tokens": 10, "output_tokens": 2, "complete": True}}}}))
print(json.dumps({{"type": "done", "text": "done"}}))
"""
CATALOG = HERE / "scenarios.json"
TOOL_GROUPS_GO = HERE.parents[1] / "go/internal/cli/mcp/tool_groups.go"
MINIMAL = {"id": "probe", "group": "core", "environments": ["simulator"], "prompt": "Do it on {device}.",
           "expect": {"right": [{"tool": "write_thing"}]}}


def write_scenarios(path, items):
    path.write_text(json.dumps(items))
    return sc.load_scenarios(path)


class LoadTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)

    def test_load_scenarios_applies_defaults_and_rejects_bad_group(self):
        scenario = write_scenarios(self.root / "s.json", [MINIMAL])[0]
        self.assertEqual({k: scenario[k] for k in ("timeout_seconds", "max_tool_calls", "canned", "captures",
                                                   "allow_real", "requires", "files", "canned_set", "vm_profile",
                                                   "cli_canned")},
                         {"timeout_seconds": 180, "max_tool_calls": 20, "canned": [], "captures": [],
                          "allow_real": [], "requires": [], "files": {}, "canned_set": "probe",
                          "vm_profile": "generic", "cli_canned": []})
        self.assertEqual(scenario["group"], "core")
        bad = [dict(MINIMAL, group="networking"),
               dict(MINIMAL, environments=["laptop"]),
               dict(MINIMAL, environments=[]),
               dict(MINIMAL, expect={"right": [{"tool": "run", "bogus": 1}]}),
               dict(MINIMAL, prompt=""),
               dict(MINIMAL, vm_profile="go3"),
               dict(MINIMAL, max_tool_calls=0),
               dict(MINIMAL, timeout_seconds=-1),
               dict(MINIMAL, files={"../escape": "x"}),
               dict(MINIMAL, requirse=["wifi"]),
               dict(MINIMAL, cli_canned=["device apps list"]),
               dict(MINIMAL, cli_canned=[[]]),
               dict(MINIMAL, cli_canned=[["device", ""]]),
               dict(MINIMAL, cli_canned=[["device", "--json"]]),
               {k: v for k, v in MINIMAL.items() if k != "expect"}]
        for item in bad:
            with self.subTest(item=item), self.assertRaises(ValueError):
                write_scenarios(self.root / "s.json", [item])
        with self.assertRaisesRegex(ValueError, "duplicate"):
            write_scenarios(self.root / "s.json", [MINIMAL, MINIMAL])
        self.assertEqual(set(sc.GROUPS), {"core", "setup", "simulator", "hardware", "robotics",
                                         "observability", "cloud"})

    def test_cloud_group_scenarios_must_require_cloud(self):
        with self.assertRaisesRegex(ValueError, "cloud-status"):
            write_scenarios(self.root / "s.json", [dict(MINIMAL, id="cloud-status", group="cloud",
                                                        environments=["cloud"], requires=["wifi"])])
        with self.assertRaisesRegex(ValueError, "cloud-ping"):
            write_scenarios(self.root / "s.json", [dict(MINIMAL, id="cloud-ping", group="cloud", environments=["cloud"])])
        loaded = write_scenarios(self.root / "s.json", [dict(MINIMAL, id="cloud-ok", group="cloud",
                                                             environments=["cloud"], requires=["cloud", "wifi"])])
        self.assertEqual(loaded[0]["requires"], ["cloud", "wifi"])

    def test_readiness_reports_missing_captures_and_capabilities(self):
        scenario = write_scenarios(self.root / "s.json", [dict(
            MINIMAL, environments=["physical"], requires=["wifi"], captures=["write_thing", "wifi_connect"])])[0]
        canned = self.root / "canned"
        board = sc.board_target("192.0.2.10:50051")
        self.assertEqual(sc.scenario_readiness(scenario, board, canned), "capabilities not checked (add --execute)")
        board["capabilities"] = ["camera"]
        self.assertEqual(sc.scenario_readiness(scenario, board, canned), "target capabilities required: wifi")
        board["capabilities"] = ["camera", "wifi"]
        self.assertEqual(sc.scenario_readiness(scenario, board, canned),
                         "canned replies not captured: write_thing, wifi_connect")
        (canned / "probe").mkdir(parents=True)
        (canned / "probe/write_thing.json").write_text(json.dumps({"tool": "write_thing", "replies": [CAPTURED]}))
        self.assertEqual(sc.scenario_readiness(scenario, board, canned), "canned replies not captured: wifi_connect")
        (canned / "probe/wifi_connect.json").write_text(json.dumps({"tool": "wifi_connect", "replies": []}))
        self.assertEqual(sc.scenario_readiness(scenario, board, canned), "canned replies not captured: wifi_connect")
        (canned / "probe/wifi_connect.json").write_text(json.dumps({"tool": "wifi_connect", "replies": [CAPTURED]}))
        self.assertIsNone(sc.scenario_readiness(scenario, board, canned))
        # A capture that would fail mid-attempt fails before the run instead.
        (canned / "probe/wifi_connect.json").write_text(json.dumps(
            {"tool": "wifi_connect", "replies": [{"content": [{"type": "text", "text": "{{args.ssid}} {{bogus}}"}]}]}))
        with self.assertRaisesRegex(ValueError, "wifi_connect"):
            sc.scenario_readiness(scenario, board, canned)
        # A CLI read the scenario cans needs its capture too.
        listed = write_scenarios(self.root / "s2.json", [dict(MINIMAL, cli_canned=[["device", "apps", "list"]])])[0]
        self.assertEqual(sc.scenario_readiness(listed, {"kind": "simulator"}, canned),
                         "CLI replies not captured: device apps list")
        (canned / "probe/cli").mkdir(parents=True)
        (canned / "probe/cli/device-apps-list.json").write_text(json.dumps(
            {"argv_prefix": ["device", "apps", "list"], "stdout": "[]", "stderr": "", "exit_code": 0}))
        self.assertIsNone(sc.scenario_readiness(listed, {"kind": "simulator"}, canned))
        # A configured target without a capabilities list has none.
        self.assertEqual(sc.scenario_readiness(scenario, {"kind": "physical"}, canned),
                         "target capabilities required: wifi")
        # Capabilities only matter when a scenario requires some.
        plain = write_scenarios(self.root / "s.json", [MINIMAL])[0]
        self.assertIsNone(sc.scenario_readiness(plain, sc.board_target("192.0.2.10:50051"), canned))

    def test_robot_scenarios_need_the_agent_binary_and_docker_buildx(self):
        robot = write_scenarios(self.root / "s.json", [dict(MINIMAL, id="robot", group="robotics", vm_profile="go2")])[0]
        plain = write_scenarios(self.root / "p.json", [MINIMAL])[0]
        canned, sim = self.root / "canned", {"kind": "simulator", "capabilities": []}
        self.assertIsNone(sc.scenario_readiness(robot, sim, canned, prerequisites=lambda: []))
        self.assertEqual(sc.scenario_readiness(robot, sim, canned, prerequisites=lambda: ["docker buildx"]),
                         "robot VM prerequisites missing: docker buildx")
        never = mock.Mock(side_effect=AssertionError("only a go2 or g1 run VM has prerequisites"))
        self.assertIsNone(sc.scenario_readiness(plain, sim, canned, prerequisites=never))
        # One check per coverage, however many robot scenarios and targets there are.
        check = mock.Mock(return_value=["docker buildx"])
        coverage = sc.scenario_coverage([robot, dict(robot, id="robot-2")], {"a": sim, "b": sim}, ["robot", "robot-2"],
                                        ["a", "b"], canned, check)
        self.assertEqual({c["reason"] for c in coverage}, {"robot VM prerequisites missing: docker buildx"})
        self.assertEqual(check.call_count, 1)
        # The check itself: the agent binary prepare installs, and `docker buildx version`.
        calls = []
        def run(argv, **kwargs):
            calls.append(argv)
            return mock.Mock(returncode=calls_code)
        missing, present = self.root / "no-agent", self.root / "agent"
        present.write_text("elf")
        calls_code = 1
        self.assertEqual(sc.robot_prerequisites({"agent_binary": str(missing)}, run=run),
                         [f"agent binary {missing}", "docker buildx"])
        self.assertEqual(calls, [["docker", "buildx", "version"]])
        calls_code = 0
        self.assertEqual(sc.robot_prerequisites({"agent_binary": str(present)}, run=run), [])
        no_docker = mock.Mock(side_effect=FileNotFoundError("docker"))
        self.assertEqual(sc.robot_prerequisites({"agent_binary": str(present)}, run=no_docker), ["docker buildx"])
        self.assertEqual(sc.robot_agent_binary({}), str(b.ROOT / "go/bin/wendy-agent-linux-arm64"))

    def test_board_target_kind_from_selector(self):
        self.assertEqual(sc.board_target("192.0.2.10:50051"),
                         {"kind": "physical", "device": "192.0.2.10:50051", "capabilities": None, "fixtures": {},
                          "cache_state": "spare board named with --device"})
        self.assertEqual(sc.board_target("cloud://h:443/org/1/asset/2")["kind"], "cloud")
        self.assertEqual(sc.board_target("cloud://h:443/org/1/asset/2")["device"], "cloud://h:443/org/1/asset/2")

    def test_detect_capabilities_maps_sample_output(self):
        # Shape and categories as the agent's hardware discoverer and the MCP tool emit them.
        payload = {"capabilities": [
            {"category": "gpu", "device_path": "/dev/dri/card0", "description": "Broadcom GPU v3d",
             "properties": {"vendor": "Broadcom", "driver": "v3d"}},
            {"category": "usb", "device_path": "/sys/bus/usb/devices/1-1", "description": "USB2.0 Hub (2109:3431)"},
            {"category": "usb", "device_path": "/sys/bus/usb/devices/1-1.3",
             "description": "Bluetooth Radio (0bda:b00c)"},
            {"category": "i2c", "device_path": "/dev/i2c-1", "description": "I2C bus 1"},
            {"category": "camera", "device_path": "/dev/video0", "description": "unicam (video0)",
             "properties": {"transport": "csi"}},
            {"category": "audio", "device_path": "alsa_output.platform-hdmi", "description": "HDMI"},
            {"category": "network", "device_path": "/sys/class/net/eth0", "description": "eth0 (up)"},
            {"category": "network", "device_path": "/sys/class/net/wlan0", "description": "wlan0 (up)"},
            {"category": "storage", "device_path": "/dev/mmcblk0", "description": "mmcblk0"}]}
        result = {"content": [{"type": "text", "text": json.dumps(payload)}], "structuredContent": payload}
        self.assertEqual(sc.detect_capabilities(result), ["bluetooth", "camera", "wifi"])
        self.assertEqual(sc.detect_capabilities({"content": result["content"]}), ["bluetooth", "camera", "wifi"])
        wired = {"capabilities": [c for c in payload["capabilities"]
                                  if "wlan" not in c["device_path"] and "Bluetooth" not in c["description"]
                                  and c["category"] != "camera"]}
        self.assertEqual(sc.detect_capabilities({"structuredContent": wired}), [])
        synonyms = {"capabilities": [{"category": "WiFi"}, {"category": "wlan"}, {"category": "Bluetooth"},
                                     {"category": "cameras", "properties": {"transport": "usb"}},
                                     {"category": "audio", "device_path": "bluez_output.02_00"}]}
        self.assertEqual(sc.detect_capabilities({"structuredContent": synonyms}), ["bluetooth", "camera", "wifi"])
        for junk in ({}, {"isError": True, "content": [{"type": "text", "text": "not connected"}]},
                     {"structuredContent": {"capabilities": None}}, {"structuredContent": {"capabilities": ["x"]}}):
            self.assertEqual(sc.detect_capabilities(junk), [])


    def test_detect_capabilities_ignores_codec_and_isp_nodes(self):
        # A camera-less Pi 4 still has V4L2 m2m nodes in the camera category (discoverer.go discoverCamera).
        def node(video, driver, transport):
            return {"category": "camera", "device_path": f"/dev/{video}", "description": f"{driver} ({video})",
                    "properties": {"transport": transport, "driver": driver}}
        pi = [node("video10", "bcm2835-codec", "unknown"), node("video13", "bcm2835-isp", "unknown"),
              node("video14", "bcm2835-isp", "csi"), node("video31", "BCM2835-ISP", "csi"),
              node("video20", "pispbe", "csi"), node("video19", "rpi-hevc-dec", "csi"),
              node("video0", "some-bridge", "unknown"),
              {"category": "camera", "device_path": "/dev/video1", "description": "no properties"}]
        self.assertEqual(sc.detect_capabilities({"structuredContent": {"capabilities": pi}}), [])
        for camera in (node("video0", "uvcvideo", "usb"), node("video0", "unicam", "csi"),
                       {"category": "camera", "properties": {"transport": "csi"}}):
            with self.subTest(camera=camera):
                self.assertEqual(sc.detect_capabilities({"structuredContent": {"capabilities": pi + [camera]}}),
                                 ["camera"])

    def test_cloud_capability_comes_only_from_a_cloud_selector(self):
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(os.environ, {"HOME": directory}):
            cloud, _ = sc.detect_board(FAKE, sc.board_target("cloud://h:443/org/1/asset/2"), timeout=10)
            lan, _ = sc.detect_board(FAKE, sc.board_target("192.0.2.10:50051"), timeout=10)
        self.assertEqual((cloud, lan), (["cloud"], []))
        cloudy = write_scenarios(self.root / "s.json", [dict(
            MINIMAL, group="cloud", environments=["simulator", "physical", "cloud"], requires=["cloud"])])[0]
        canned = self.root / "canned"
        board = dict(sc.board_target("cloud://h:443/org/1/asset/2"), capabilities=cloud)
        self.assertIsNone(sc.scenario_readiness(cloudy, board, canned))
        self.assertIsNone(sc.scenario_readiness(
            cloudy, {"kind": "cloud", "device": "cloud://h:443/org/1/asset/2", "capabilities": []}, canned))
        for target in ({"kind": "simulator", "device": "vm:x", "capabilities": ["cloud"]},
                       {"kind": "physical", "device": "192.0.2.1:50051", "capabilities": ["cloud"]},
                       dict(sc.board_target("192.0.2.10:50051"), capabilities=lan)):
            with self.subTest(target=target):
                self.assertEqual(sc.scenario_readiness(cloudy, target, canned), "target capabilities required: cloud")


class GuardTests(unittest.TestCase):
    def test_tool_cap_guard_counts_both_logs_and_tolerates_partial_line(self):
        with tempfile.TemporaryDirectory() as directory:
            proxy, cli = Path(directory, "proxy.jsonl"), Path(directory, "cli.jsonl")
            self.assertIsNone(sc.ToolCapGuard(proxy, cli, 1)())
            proxy.write_text('{"tool": "a"}\n{"tool": "b"}\n{"tool": "c", "argu')
            cli.write_text('{"argv": ["device", "list"]}\n')
            self.assertEqual(sc.ToolCapGuard(proxy, cli, 2)(), {"kind": "tool_call_cap", "calls": 3, "limit": 2})
            self.assertIsNone(sc.ToolCapGuard(proxy, cli, 3)())

    def test_first_reason_returns_the_first_guard_that_fires(self):
        calls = []
        def guard(value):
            def check():
                calls.append(value)
                return value
            return check
        self.assertEqual(sc.first_reason(guard(None), guard({"kind": "a"}), guard({"kind": "b"}))(), {"kind": "a"})
        self.assertEqual(calls, [None, {"kind": "a"}])
        self.assertIsNone(sc.first_reason(guard(None))())


class StateTests(unittest.TestCase):
    def test_attempt_state_links_shared_vm_store_and_discard_keeps_it(self):
        run_state = b.isolated_device_state({"kind": "simulator"})
        self.addCleanup(shutil.rmtree, run_state, True)
        (run_state / "vms/wendy-eval-run").mkdir(parents=True)
        (run_state / "vms/wendy-eval-run/disk.img").write_text("shared disk")
        run_vm = {"state": run_state, "name": "wendy-eval-run", "device": "vm:wendy-eval-run"}
        for cleanup_ok in (True, False):
            with self.subTest(cleanup_ok=cleanup_ok):
                state = sc.attempt_state({"kind": "simulator"}, run_vm)
                self.addCleanup(shutil.rmtree, state, True)
                self.assertNotEqual(state, run_state)
                self.assertTrue((state / "vms").is_symlink())
                self.assertEqual((state / "vms").resolve(), (run_state / "vms").resolve())
                self.assertNotIn("devicePins", json.loads((state / "config.json").read_text()))
                b.discard_device_state(state, cleanup_ok)
                self.assertEqual(state.exists(), not cleanup_ok)
                if not cleanup_ok:
                    self.assertEqual(list(state.iterdir()), [])
                self.assertEqual((run_state / "vms/wendy-eval-run/disk.img").read_text(), "shared disk")
        with self.assertRaises(ValueError):
            sc.scenario_trial({"agents": {"a": {"adapter": "wendy"}}, "targets": {"sim": {"kind": "simulator"}}},
                              "a", "sim", {}, Path("/nonexistent"), FAKE, FAKE, None, None, None)
        board = sc.attempt_state(sc.board_target("192.0.2.10:50051"), None)
        self.addCleanup(shutil.rmtree, board, True)
        self.assertFalse((board / "vms").exists())

    def test_vm_snapshot_is_sorted_name_version_state(self):
        apps = [{"name": "b", "version": "2", "runningState": "STOPPED", "httpPort": 80},
                {"name": "a", "version": "1", "runningState": "RUNNING", "failureCount": 3}]
        done = mock.Mock(returncode=0, stdout=json.dumps(apps), stderr="")
        with mock.patch.object(sc.subprocess, "run", return_value=done) as run:
            snapshot = sc.vm_snapshot("/x/wendy", "vm:run", {"WENDY_CONFIG_DIR": "/tmp/s"})
        self.assertEqual(snapshot, [{"name": "a", "version": "1", "state": "RUNNING"},
                                    {"name": "b", "version": "2", "state": "STOPPED"}])
        self.assertEqual(run.call_args.args[0], ["/x/wendy", "--json", "--device", "vm:run", "device", "apps", "list"])
        self.assertEqual(run.call_args.kwargs["env"], {"WENDY_CONFIG_DIR": "/tmp/s"})
        failed = mock.Mock(returncode=1, stdout="", stderr="unreachable")
        with mock.patch.object(sc.subprocess, "run", return_value=failed), self.assertRaises(RuntimeError):
            sc.vm_snapshot("/x/wendy", "vm:run", {})

    def test_run_vm_lives_in_private_state_never_host_wendy(self):
        seen = {}
        def fixture_vm(config, variables, workspace, directory, env):
            seen.update(variables=variables, env=env, directory=directory)
            (Path(env["WENDY_CONFIG_DIR"]) / "vms" / variables["vm_name"]).mkdir(parents=True)
            return {"vm-create": {"exit_code": 0}}, True
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)
            with mock.patch.object(b, "fixture_vm", side_effect=fixture_vm):
                run_vm = sc.prepare_run_vm({"agent_binary": "/x/agent"}, "/x/wendy", output, "generic")
            self.addCleanup(shutil.rmtree, run_vm["state"], True)
            self.assertTrue(run_vm["ready"])
            self.assertRegex(run_vm["name"], r"^wendy-eval-[0-9a-f]{12}$")
            self.assertEqual(run_vm["device"], "vm:" + run_vm["name"])
            self.assertEqual(seen["env"]["WENDY_CONFIG_DIR"], str(run_vm["state"]))
            self.assertEqual(seen["env"]["WENDY_SECRET_STORE"], "file")
            self.assertNotEqual(Path(run_vm["state"]).resolve(), (Path.home() / ".wendy").resolve())
            self.assertEqual(seen["variables"], {"wendy": "/x/wendy", "vm_name": run_vm["name"]})
            self.assertEqual(seen["directory"].parent, output)
            removed = {}
            def remove(variables, workspace, directory, env):
                removed.update(variables=variables, env=env)
                return {"exit_code": 0, "seconds": 0}
            with mock.patch.object(b, "remove_fixture_vm", side_effect=remove):
                result = sc.remove_run_vm(run_vm, "/x/wendy", output)
            self.assertEqual(result["exit_code"], 0)
            self.assertEqual(removed["env"]["WENDY_CONFIG_DIR"], str(run_vm["state"]))
            self.assertFalse(Path(run_vm["state"]).exists())
            with self.assertRaises(ValueError):
                sc.prepare_run_vm({}, "/x/wendy", output, "go3")

    def test_prepare_run_vm_removes_a_vm_whose_creation_raises(self):
        created, removed, registered = [], [], []
        def fixture_vm(config, variables, workspace, directory, env):
            created.append(variables["vm_name"])
            raise RuntimeError("start failed after create")
        def remove(variables, workspace, directory, env):
            removed.append(variables["vm_name"])
            return {"exit_code": 0, "seconds": 0}
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(b, "fixture_vm", side_effect=fixture_vm), \
                mock.patch.object(b, "remove_fixture_vm", side_effect=remove):
            with self.assertRaises(RuntimeError):
                sc.prepare_run_vm({}, "/x/wendy", Path(directory), "generic")
            self.assertEqual(removed, created)
            with self.assertRaises(RuntimeError):
                sc.prepare_run_vm({}, "/x/wendy", Path(directory), "generic", register=registered.append)
            self.assertEqual(len(removed), 1)  # an owner that registered the VM removes it
            self.assertEqual(registered[0]["name"], created[1])
            self.assertIs(registered[0]["ready"], False)
            sc.remove_run_vm(registered[0], "/x/wendy", Path(directory))
            self.assertFalse(Path(registered[0]["state"]).exists())

    def test_detect_board_reads_capabilities_in_a_throwaway_state(self):
        with tempfile.TemporaryDirectory() as directory:
            log, states = Path(directory, "fake.jsonl"), []
            real_state = b.isolated_device_state
            def isolated(target):
                states.append(real_state(target))
                return states[-1]
            with mock.patch.dict(os.environ, {"HOME": directory, "FAKE_MCP_LOG": str(log)}), \
                    mock.patch.object(b, "isolated_device_state", side_effect=isolated):
                capabilities, evidence = sc.detect_board(FAKE, sc.board_target("192.0.2.10:50051"), timeout=10)
            self.assertEqual(capabilities, [])  # the fake has no hardware
            calls = [json.loads(line)["params"] for line in log.read_text().splitlines()
                     if json.loads(line).get("method") == "tools/call"]
            self.assertEqual(calls, [{"name": "device_connect", "arguments": {"device": "192.0.2.10:50051"}},
                                     {"name": "hardware_capabilities", "arguments": {}}])
            self.assertEqual(evidence["capabilities"], [])
            self.assertFalse(states[0].exists())
            missing = sc.detect_board(str(Path(directory, "no-such-wendy")), sc.board_target("192.0.2.10:50051"))
            self.assertIsNone(missing[0])
            self.assertIn("error", missing[1])

    def test_robot_run_vm_reuses_robot_diagnostics_commands(self):
        calls = []
        def run_process(argv, cwd, prefix, timeout, env=None, *rest):
            calls.append((argv, timeout, env["WENDY_CONFIG_DIR"]))
            return {"exit_code": 0, "seconds": 0}
        verified = []
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(b, "run_process", side_effect=run_process), \
                mock.patch.object(sc.robot_fixtures, "verify", side_effect=lambda args, cli: verified.append(args)):
            run_vm = sc.prepare_run_vm({}, "/x/wendy", Path(directory), "go2")
            self.addCleanup(shutil.rmtree, run_vm["state"], True)
        self.assertTrue(run_vm["ready"])
        import robot_diagnostics
        expected = robot_diagnostics.robot_vm_commands("/x/wendy", run_vm["name"], "go2",
                                                       str(b.ROOT / "go/bin/wendy-agent-linux-arm64"), 1)
        self.assertEqual([argv[:3] for argv, _, _ in calls], [argv[:3] for _, argv, _ in expected])
        self.assertIn("--profile", calls[0][0])
        self.assertEqual({state for _, _, state in calls}, {str(run_vm["state"])})
        self.assertEqual(verified[0].task, "create-go2-simulator")
        self.assertEqual(verified[0].vm_name, run_vm["name"])
        calls.clear()
        with tempfile.TemporaryDirectory() as directory, \
                mock.patch.object(b, "run_process", side_effect=run_process), \
                mock.patch.object(sc.robot_fixtures, "verify"):
            run_vm = sc.prepare_run_vm({"agent_binary": "/x/agent-arm64"}, "/x/wendy", Path(directory), "g1")
            self.addCleanup(shutil.rmtree, run_vm["state"], True)
        self.assertIn(["--binary", "/x/agent-arm64"], [argv[-2:] for argv, _, _ in calls])


class TrialTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "tools.json").write_text(json.dumps(TOOLS))
        self.canned = self.root / "canned"
        (self.canned / "probe").mkdir(parents=True)
        (self.canned / "probe/write_thing.json").write_text(json.dumps(
            {"tool": "write_thing", "captured_with": {}, "replies": [CAPTURED]}))
        (self.root / "bin").mkdir()
        self.shim = eval_wendy.write_shim(self.root / "bin")
        self.agent = self.root / "agent.py"
        self.agent.write_text(AGENT.format(here=str(HERE)))
        self.run_state = b.isolated_device_state({"kind": "simulator"})
        self.addCleanup(shutil.rmtree, self.run_state, True)
        (self.run_state / "vms/wendy-eval-run").mkdir(parents=True)
        self.run_vm = {"state": self.run_state, "name": "wendy-eval-run", "device": "vm:wendy-eval-run",
                       "stages": {}, "ready": True}
        self.output = self.root / "out"
        self.output.mkdir()

    def config(self, command=None, adapter="wendy"):
        return {"agents": {"fake": {"adapter": adapter, "model": "fixture",
                                    "command": command or [sys.executable, str(self.agent)]}},
                "targets": {"sim": {"kind": "simulator", "device": "vm:pilot", "capabilities": []}}}

    def scenario(self, **extra):
        item = dict(MINIMAL, captures=["write_thing"], files={"notes/device.txt": "device {device}"},
                    timeout_seconds=60, **extra)
        item["expect"] = {"right": [{"tool": "write_thing"}],
                          "wrong": [{"tool": "container_exec", "args_regex": "nmcli", "block": True},
                                    {"cli": "wifi connect"}]}
        return write_scenarios(self.root / "scenarios.json", [item])[0]

    def run_trial(self, config, scenario, snapshots=([],), env=None):
        side_effect = [list(s) for s in snapshots] if len(snapshots) > 1 else None
        patch = mock.patch.object(sc, "vm_snapshot", side_effect=side_effect,
                                  return_value=list(snapshots[0]) if side_effect is None else None)
        with patch as snapshot, mock.patch.dict(os.environ, {"WENDY_EVAL_SHIM": str(self.shim), **(env or {})}):
            row = sc.scenario_trial(config, "fake", "sim", scenario, self.output, FAKE, self.shim, self.run_vm,
                                    self.root / "tools.json", self.canned)
        self.forget(row)
        return row, snapshot

    def forget(self, row):
        # Workspaces and states kept after a failed reset are evidence in a real run, litter here.
        for key in ("workspace", "device_state_retained"):
            if row.get(key):
                self.addCleanup(shutil.rmtree, row[key], True)

    def attempt_dir(self, row):
        return next(self.output.glob(f"*-{row['id']}"))

    def test_local_scenario_attempts_also_block_the_guarded_actions(self):
        config = self.config()
        config["targets"] = {"sim": dict(LOCAL_TARGET, capabilities=[])}
        run_vm = None
        with mock.patch.object(sc, "vm_snapshot", return_value=[]), \
                mock.patch.dict(os.environ, {"WENDY_EVAL_SHIM": str(self.shim), "WENDY_AGENT_SOCKET": "/run/wendy.sock"}):
            row = sc.scenario_trial(config, "fake", "sim", self.scenario(), self.output, FAKE, self.shim, run_vm,
                                    self.root / "tools.json", self.canned)
        self.forget(row)
        written = json.loads((self.attempt_dir(row) / "scenario.json").read_text())
        self.assertEqual(written["block"][-len(sc.guard_matchers(sc.BENCHMARK_APP)):], sc.guard_matchers(sc.BENCHMARK_APP))
        self.assertEqual(written["block"][0], {"tool": "container_exec", "args_regex": "nmcli", "block": True})
        self.assertIs(written["passthrough"], False)

    def test_scenario_trial_with_fake_agent_grades_from_proxy_log(self):
        apps = [{"name": "dev.wendy.example", "version": "1", "state": "RUNNING"}]
        row, snapshot = self.run_trial(self.config(), self.scenario(), snapshots=(apps,))
        self.assertEqual(row["tool_choice"]["outcome"], "right", row)
        trace = row["metrics"]["tool_trace"]
        write = next(e for e in trace if e["name"] == "write_thing")
        self.assertIs(write["canned"], True)
        self.assertIs(write["captured"], True)
        self.assertEqual(write["source"], "proxy")
        self.assertIs(next(e for e in trace if e["name"] == "read_thing")["canned"], False)
        self.assertIs(row["success"], True)
        self.assertEqual(row["status"], "passed")
        self.assertIs(row["cleanup_ok"], True)
        self.assertEqual((row["kind"], row["group"], row["task"]), ("scenario", "core", "probe"))
        self.assertIs(row["metrics"]["usage_complete"], True)
        self.assertEqual((row["metrics"]["transcript_mcp_calls"], row["metrics"]["proxy_calls"]), (2, 2))
        self.assertEqual(row["device_selector"], "vm:wendy-eval-run")
        directory = self.attempt_dir(row)
        self.assertEqual(json.loads((directory / "result.json").read_text())["status"], "passed")
        config = json.loads((directory / "scenario.json").read_text())
        self.assertEqual(config["block"], [{"tool": "container_exec", "args_regex": "nmcli", "block": True}])
        self.assertEqual(config["context"], {"device": "vm:wendy-eval-run", "vm_name": "wendy-eval-run"})
        self.assertEqual(config["cli_canned"], [])
        self.assertEqual((config["real_wendy"], config["shim"]), (FAKE, str(self.shim)))
        self.assertEqual(config["cli_safe"], str(HERE / "cli_safe.json"))
        self.assertEqual(config["canned_set"], "probe")
        records = [json.loads(line) for line in (directory / "proxy.jsonl").read_text().splitlines()]
        self.assertEqual([r["tool"] for r in records], ["write_thing", "read_thing"])
        self.assertTrue((directory / "cli.jsonl").exists())
        prompt = (directory / "prompt.txt").read_text()
        self.assertIn("Do it on vm:wendy-eval-run.", prompt)
        self.assertIn(str(self.shim), prompt)
        self.assertEqual((Path(row["workspace"]) / "notes/device.txt").read_text(), "device vm:wendy-eval-run")
        # The reset check reads the shared VM through the runner's own state with the real binary.
        self.assertEqual(snapshot.call_count, 2)
        wendy, device, env = snapshot.call_args.args
        self.assertEqual((wendy, device, env["WENDY_CONFIG_DIR"]), (FAKE, "vm:wendy-eval-run", str(self.run_state)))
        self.assertTrue((self.run_state / "vms/wendy-eval-run").is_dir())
        self.assertEqual(rescored_outcome(self.output, self.config(), [self.scenario()]), "right")

    def test_agent_environment_drops_device_overrides(self):
        # An allowlisted read without --device would otherwise reach the developer's default device.
        seen = self.root / "env.json"
        command = [sys.executable, "-c",
                   "import json, os, sys; json.dump({k: os.environ.get(k) for k in "
                   "('WENDY_DEVICE', 'WENDY_AGENT_SOCKET', 'WENDY_CONFIG_DIR')}, open(sys.argv[1], 'w')); "
                   "print(json.dumps({'type': 'done', 'text': 'ok'}))", str(seen)]
        self.run_trial(self.config(command), self.scenario(),
                       env={"WENDY_DEVICE": "10.0.0.9:50051", "WENDY_AGENT_SOCKET": "/run/wendy/agent.sock"})
        values = json.loads(seen.read_text())
        self.assertEqual((values["WENDY_DEVICE"], values["WENDY_AGENT_SOCKET"]), (None, None))
        self.assertIn("/we-state-", values["WENDY_CONFIG_DIR"])

    def test_reset_mismatch_fails_cleanup_and_keeps_shared_vm(self):
        command = [sys.executable, "-c", "import json; print(json.dumps({'type': 'done', 'text': 'ok'}))"]
        row, _ = self.run_trial(self.config(command), self.scenario(),
                                snapshots=([], [{"name": "stray", "version": "1", "state": "RUNNING"}]))
        self.assertEqual(row["tool_choice"]["outcome"], "missed")
        self.assertIs(row["success"], False)
        self.assertEqual(row["status"], "failed")
        self.assertIs(row["cleanup_ok"], False)
        self.assertTrue((self.run_state / "vms/wendy-eval-run").is_dir())

    def test_unreachable_target_before_the_attempt_is_a_setup_error(self):
        command = [sys.executable, "-c", "raise SystemExit('agent must not run')"]
        with mock.patch.dict(os.environ, {"WENDY_EVAL_SHIM": str(self.shim)}), \
                mock.patch.object(sc, "vm_snapshot", side_effect=RuntimeError("unreachable")):
            row = sc.scenario_trial(self.config(command), "fake", "sim", self.scenario(), self.output, FAKE,
                                    self.shim, self.run_vm, self.root / "tools.json", self.canned)
        self.forget(row)
        self.assertEqual(row["status"], "setup_error")
        self.assertNotIn("agent", row["stages"])
        self.assertIs(row["cleanup_ok"], False)

    def test_tool_call_cap_stops_attempt_with_unknown_usage(self):
        row, _ = self.run_trial(self.config(), self.scenario(max_tool_calls=1), env={"FAKE_AGENT_HANG": "1"})
        self.assertEqual(row["stages"]["agent"]["no_progress"], {"kind": "tool_call_cap", "calls": 2, "limit": 1})
        self.assertLess(row["stages"]["agent"]["seconds"], 20)
        self.assertIs(row["metrics"]["usage_complete"], False)
        self.assertEqual(row["tool_choice"]["outcome"], "right")
        self.assertIs(row["cleanup_ok"], True)

    def test_exactly_max_tool_calls_finishes_naturally(self):
        row, _ = self.run_trial(self.config(), self.scenario(max_tool_calls=2))
        self.assertNotIn("no_progress", row["stages"]["agent"])
        self.assertIs(row["metrics"]["usage_complete"], True)
        self.assertEqual(row["status"], "passed")

    def test_exception_mid_attempt_still_discards_state_and_moves_logs(self):
        config = dict(self.config(), max_agent_seconds=0)  # agent_timeout raises after the state exists
        before = temp_dirs()
        with mock.patch.dict(os.environ, {"WENDY_EVAL_SHIM": str(self.shim)}), \
                mock.patch.object(sc, "vm_snapshot", return_value=[]), self.assertRaises(ValueError):
            sc.scenario_trial(config, "fake", "sim", self.scenario(), self.output, FAKE, self.shim, self.run_vm,
                              self.root / "tools.json", self.canned)
        self.assertEqual(temp_dirs() - before, set())
        directory = next(self.output.glob("fake-sim-probe-*"))
        self.assertEqual(json.loads((directory / "result.json").read_text())["status"], "setup_error")
        self.assertTrue((directory / "proxy.jsonl").exists() and (directory / "cli.jsonl").exists())
        shutil.rmtree(json.loads((directory / "result.json").read_text())["workspace"], True)

    def test_claude_attempts_get_the_same_mcp_server_checks(self):
        init = {"type": "system", "subtype": "init", "mcp_servers": [
            {"name": "wendy"}, {"name": "plugin:wendy-agentic-coding:wendy"}]}
        for events, status in (([{"type": "result", "result": "ok"}], "mcp_unverified"),
                               ([init, {"type": "result", "result": "ok"}], "mcp_contaminated")):
            with self.subTest(status=status):
                code = "import json\n" + "".join(f"print(json.dumps({e!r}))\n" for e in events)
                command = [sys.executable, "-c", code, "--plugin-dir", "{plugin_dir}"]
                row, _ = self.run_trial(self.config(command, adapter="claude"), self.scenario())
                self.assertEqual(row["status"], status)
                self.assertIs(row["success"], False)
                directory = self.attempt_dir(row)
                self.assertNotIn("mcpServers", json.loads((directory / "plugin/.claude-plugin/plugin.json").read_text()))
                server = json.loads((Path(row["workspace"]) / "mcp.json").read_text())["mcpServers"]["wendy"]
                self.assertEqual(server["command"], str(self.shim))
                self.assertEqual(server["env"]["WENDY_EVAL_SCENARIO"], str(directory / "scenario.json"))


    def test_mcp_calls_the_proxy_never_saw_are_unverified(self):
        # A transcript with Wendy MCP calls and an empty proxy log means another server answered them.
        call = {"id": "1", "name": "mcp__wendy__write_thing", "input": {"path": "x"}}
        init = {"type": "system", "subtype": "init", "mcp_servers": [{"name": "wendy"}]}
        transcripts = {
            "wendy": [{"type": "tool_start", "id": "1", "tool": "write_thing", "arguments": {"path": "x"}},
                      {"type": "done", "text": "done"}],
            "claude": [init, {"type": "assistant", "message": {"content": [{"type": "tool_use", **call}]}},
                       {"type": "result", "result": "done"}],
            "codex": [{"type": "item.completed", "item": {"id": "1", "type": "mcp_tool_call", "server": "wendy",
                                                           "tool": "write_thing", "arguments": {"path": "x"}}},
                      {"type": "turn.completed", "usage": {"input_tokens": 1, "output_tokens": 1}}]}
        for adapter, events in transcripts.items():
            with self.subTest(adapter=adapter):
                code = "import json\n" + "".join(f"print(json.dumps({e!r}))\n" for e in events)
                row, _ = self.run_trial(self.config([sys.executable, "-c", code], adapter=adapter), self.scenario())
                self.assertEqual((row["status"], row["success"]), ("mcp_unverified", False))
                self.assertEqual((row["metrics"]["transcript_mcp_calls"], row["metrics"]["proxy_calls"]), (1, 0))
        # No MCP call in the transcript, no proxy record: nothing to cross-check.
        code = "import json; print(json.dumps({'type': 'done', 'text': 'ok'}))"
        row, _ = self.run_trial(self.config([sys.executable, "-c", code]), self.scenario())
        self.assertEqual(row["status"], "failed")

    def test_shared_command_templates_expand_in_scenarios_and_tasks(self):
        code = "import json,sys; print(json.dumps({'type': 'done', 'text': json.dumps(sys.argv[1:])}))"
        command = [sys.executable, "-c", code, "{wendy_vm_home}", "{scenario_config}", "{wendy_home}", "{wendy}"]
        row, _ = self.run_trial(self.config(command), self.scenario())
        directory = self.attempt_dir(row)
        vm_home, scenario_config, home, wendy = json.loads(row["metrics"]["final_text"])
        self.assertEqual((vm_home, scenario_config, wendy),
                         (str(self.run_state), str(directory / "scenario.json"), str(self.shim)))
        self.assertNotEqual(home, str(self.run_state))
        task = {"id": "t", "prompt": "p", "prepare": [sys.executable, "-c", "pass"],
                "verify": [sys.executable, "-c", "pass"], "cleanup": [sys.executable, "-c", "pass"]}
        row = b.trial(self.config(command), "fake", "sim", task, self.output, sys.executable)
        self.forget(row)
        vm_home, scenario_config, home, wendy = json.loads(row["metrics"]["final_text"])
        self.assertEqual((vm_home, scenario_config, wendy), (home, "", sys.executable))


def temp_dirs():
    return {p for pattern in ("we-state-*", "we-log-*") for p in Path("/tmp").glob(pattern)}


def rescored_outcome(output, config, scenarios):
    b.dump(output / "manifest.json", {"config": config})
    out = b.rescore(output, [], scenarios)
    return json.loads((out / "results.jsonl").read_text().splitlines()[0])["tool_choice"]["outcome"]


LOCAL_TARGET = {"kind": "local", "device": "local", "prompt_device": "this device", "canned_dir": "canned-local",
                "capabilities": None, "fixtures": {}}
LOCAL_SCENARIOS = {
    "apps-overview", "device-versions", "hardware-inventory", "deploy-project", "app-stop", "app-start",
    "app-remove-free-space", "app-exec-command", "app-resource-usage", "recent-errors", "erase-device",
    "project-validate", "os-upgrade-status", "os-upgrade", "wifi-scan", "wifi-saved-networks", "wifi-connect",
    "bt-scan", "bt-connect", "bt-disconnect", "camera-list", "camera-controls", "camera-exposure", "camera-snapshot",
    "kernel-log", "device-metrics", "slow-traces", "app-health", "app-readiness"}
LOCAL_TASKS = {
    "deploy-http", "repair-startup", "update-http", "stop-app", "start-app", "compose-app", "debug-otel-logs",
    "debug-otel-metrics", "debug-otel-traces", "debug-ros2", "camera-capture", "camera-repair", "audio-loopback",
    "peripheral-io"}


class LocalTargetTests(unittest.TestCase):
    def test_private_env_removes_the_socket_by_default(self):
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock", "WENDY_DEVICE": "x"}):
            env = sc.private_env(Path("/tmp/state"))
        self.assertNotIn("WENDY_AGENT_SOCKET", env)
        self.assertNotIn("WENDY_DEVICE", env)

    def test_private_env_keeps_the_socket_only_when_asked(self):
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock", "WENDY_DEVICE": "x"}):
            env = sc.private_env(Path("/tmp/state"), keep_socket=True)
        self.assertEqual(env["WENDY_AGENT_SOCKET"], "/run/wendy.sock")
        self.assertNotIn("WENDY_DEVICE", env)

    def test_is_local_and_canned_dir_for(self):
        self.assertTrue(sc.is_local(LOCAL_TARGET))
        self.assertFalse(sc.is_local({"kind": "simulator"}))
        self.assertEqual(sc.canned_dir_for(LOCAL_TARGET), HERE / "canned-local")
        self.assertEqual(sc.canned_dir_for({"kind": "simulator"}), sc.CANNED)

    def test_local_scenarios_are_exactly_these(self):
        tagged = {s["id"] for s in sc.load_scenarios(CATALOG) if "local" in s["environments"]}
        self.assertEqual(tagged, LOCAL_SCENARIOS)
        self.assertEqual(len(tagged), 29)

    def test_local_tasks_are_exactly_these(self):
        tasks = json.loads((HERE / "tasks.json").read_text())
        self.assertEqual({t["id"] for t in tasks if "local" in t["environments"]}, LOCAL_TASKS)

    def test_existing_environments_unchanged(self):
        scenarios = {s["id"]: s for s in sc.load_scenarios(CATALOG)}
        tasks = {t["id"]: t for t in json.loads((HERE / "tasks.json").read_text())}
        self.assertIn("simulator", scenarios["app-stop"]["environments"])
        self.assertEqual(set(tasks["deploy-http"]["environments"]), {"simulator", "cloud", "physical", "local"})

    def test_local_prompt_says_this_device(self):
        scenario = {s["id"]: s for s in sc.load_scenarios(CATALOG)}["erase-device"]
        variables = {"device": "local"}
        self.assertEqual(b.expand(scenario["prompt"], b.prompt_variables(variables, LOCAL_TARGET)),
                         "Erase this device so I can start over from a clean slate.")
        self.assertEqual(b.expand(["verify", "--device", "{device}"], variables), ["verify", "--device", "local"])
        self.assertEqual(b.prompt_variables(variables, {"kind": "simulator"}), variables)

    def test_readiness_reads_the_targets_canned_dir(self):
        scenario = dict(sc.DEFAULTS, id="probe", canned_set="probe", captures=["container_list"])
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            target = dict(LOCAL_TARGET, canned_dir=os.path.relpath(root / "canned-local", HERE))
            (root / "canned-local" / "probe").mkdir(parents=True)
            (root / "canned-local" / "probe" / "container_list.json").write_text(json.dumps({"replies": [CAPTURED]}))
            self.assertIsNone(sc.scenario_readiness(scenario, target, sc.canned_dir_for(target)))
            self.assertIn("not captured", sc.scenario_readiness(scenario, {"kind": "simulator"},
                                                                sc.canned_dir_for({"kind": "simulator"}, root / "empty")))


class LocalTaskTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        (self.root / "tools.json").write_text(json.dumps(TOOLS))
        (self.root / "bin").mkdir()
        self.shim = eval_wendy.write_shim(self.root / "bin")
        self.out = self.root / "out"
        self.out.mkdir()

    def argv(self, name):
        return [sys.executable, "-c", "pass", name]

    def test_guard_matchers_hit_the_benchmarks_app_only(self):
        block = sc.guard_matchers(sc.BENCHMARK_APP)
        self.assertTrue(all(m.get("block") is True for m in block))

        def hit(entry):
            return any(grade_matches(m, entry) for m in block)

        def mcp(name, **args):
            return {"channel": "mcp", "name": name, "arguments": args}

        def cli(*argv):
            return {"channel": "cli", "name": " ".join(argv[:3]), "arguments": list(argv)}
        for tool in ("container_stop", "container_delete", "container_start"):
            self.assertTrue(hit(mcp(tool, app_name=sc.BENCHMARK_APP)), tool)
            self.assertFalse(hit(mcp(tool, app_name="other")), tool)
        for tool in ("os_update", "device_update_agent", "wifi_connect", "wifi_disconnect", "provisioning_start",
                     "cloud_enroll_device", "os_install_start"):
            self.assertTrue(hit(mcp(tool)), tool)
        self.assertFalse(hit(mcp("container_list")))
        for argv in (["device", "apps", "stop", sc.BENCHMARK_APP], ["device", "apps", "remove", sc.BENCHMARK_APP, "--force"],
                     ["device", "update"], ["device", "wifi", "connect", "Lab"], ["device", "wifi", "disconnect"]):
            self.assertTrue(hit(cli(*argv)), argv)
        self.assertFalse(hit(cli("device", "apps", "stop", "other")))
        self.assertFalse(hit(cli("device", "apps", "remove", "other")))
        self.assertFalse(hit(cli("device", "apps", "list")))

    def test_guard_matchers_cover_every_os_agent_network_and_enrolment_verb(self):
        block = sc.guard_matchers(sc.BENCHMARK_APP)
        verbs = ["os update", "os install", "install", "cloud enroll-device", "net configure"]
        verbs += [f"{prefix} {v}" for prefix in ("device", "cloud device") for v in (
            "update", "push-agent", "setup", "enroll", "unenroll", "wifi connect", "wifi disconnect", "wifi forget",
            "wifi rank", "wifi regulatory set-country")]
        for verb in verbs:
            argv = [*verb.split(), "x", "--device", "local"] if verb.endswith("setup") else [*verb.split(), "x"]
            self.assertTrue(eval_wendy._blocked(argv, block), verb)
        for prefix in ("device", "cloud device"):
            for verb in ("start", "stop", "remove"):
                own = [*prefix.split(), "apps", verb, sc.BENCHMARK_APP]
                self.assertTrue(eval_wendy._blocked(own, block), own)
                self.assertFalse(eval_wendy._blocked([*own[:-1], "other"], block), own)
        # reads stay open
        for argv in (["device", "wifi", "status"], ["device", "wifi", "list"], ["device", "wifi", "regulatory"],
                     ["device", "enrollment-status"], ["os", "update-status"], ["os", "list-drives"],
                     ["device", "apps", "list"], ["cloud", "device", "wifi", "list"], ["device", "info"]):
            self.assertFalse(eval_wendy._blocked(argv, block), argv)

    def test_guard_matchers_can_every_added_tool_in_the_proxy(self):
        import mcp_proxy
        tools = [{"name": n, "annotations": {"readOnlyHint": False}} for n in sc.GUARDED_TOOLS]
        policy = mcp_proxy.CannedPolicy(tools, [], [], sc.guard_matchers(sc.BENCHMARK_APP), passthrough=True)
        for name in ("os_update", "device_update_agent", "wifi_connect", "wifi_disconnect", "provisioning_start",
                     "cloud_enroll_device", "os_install_start", "os_install_resume"):
            self.assertTrue(policy.should_can(name, {}), name)
        for name in ("wifi_status", "wifi_list", "wifi_known_networks", "os_update_status", "os_install_status"):
            self.assertFalse(policy.should_can(name, {}), name)

    def test_guard_matchers_escape_the_app_id(self):
        block = sc.guard_matchers("a.b")
        entry = {"channel": "cli", "name": "device apps stop", "arguments": ["device", "apps", "stop", "axb"]}
        self.assertFalse(any(grade_matches(m, entry) for m in block))

    def trial(self, local=True, prompt="Use {device}.", context=None, **extra):
        task = {"id": "probe-task", "prompt": prompt,
                "prepare": [FAKE, "--version", "{wendy}"], "verify": [FAKE, "--version", "{wendy}"],
                "cleanup": [FAKE, "--version", "{wendy}"]}
        target = dict(LOCAL_TARGET, capabilities=[]) if local else {"kind": "simulator", "device": "vm:x"}
        config = {"agents": {"fake": {"adapter": "wendy", "model": "fixture",
                                      "command": ["{wendy}", "chat", "--json", "--prompt", "{prompt}"]}},
                  "targets": {"t": target}, "disposable_device": True, "isolate_device_state": False}
        if context:
            config["environment_context"] = context
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock", **extra.pop("env", {})}):
            row = b.trial(config, "fake", "t", task, self.out, FAKE, **extra)
        self.addCleanup(shutil.rmtree, row["workspace"], True)
        return row

    def test_local_task_routes_agent_through_shim_and_fixtures_through_real_wendy(self):
        row = self.trial(shim=str(self.shim), tools_path=self.root / "tools.json")
        directory = next(self.out.glob(f"*-{row['id']}"))
        self.assertTrue((directory / "proxy.jsonl").exists())
        self.assertEqual(len((directory / "proxy.jsonl").read_text().splitlines()), 2)
        for phase in ("prepare", "verify", "cleanup"):
            self.assertIn(FAKE, row["stages"][phase]["argv"], phase)
            self.assertNotIn(str(self.shim), row["stages"][phase]["argv"], phase)
        self.assertEqual(row["stages"]["agent"]["argv"][0], str(self.shim))
        prompt = (directory / "prompt.txt").read_text()
        self.assertIn(f"The Wendy executable for this environment is {self.shim}.", prompt)
        self.assertNotIn(f"is {FAKE}.", prompt)
        # the rebuilt trace carries the proxy's records before grading
        trace = row["metrics"]["tool_trace"]
        self.assertEqual([(e["name"], e["source"]) for e in trace if e["channel"] == "mcp"],
                         [("write_thing", "proxy"), ("read_thing", "proxy")])
        self.assertEqual(row["metrics"]["proxy_calls"], 2)
        self.assertIsNotNone(row["metrics"]["tool_trace"][0]["reply_text"] or trace[1]["reply_text"])
        scenario = json.loads((directory / "scenario.json").read_text())
        self.assertIs(scenario["passthrough"], True)
        self.assertEqual(scenario["real_wendy"], FAKE)
        self.assertEqual(scenario["block"], sc.guard_matchers(sc.BENCHMARK_APP))
        self.assertEqual(row["status"], "passed" if row["success"] else row["status"])

    def test_wendy_in_prompt_and_context_templates_names_the_shim(self):
        row = self.trial(shim=str(self.shim), tools_path=self.root / "tools.json",
                         prompt="Run {wendy} device list on {device}.", context="Also try {wendy} info.")
        directory = next(self.out.glob(f"*-{row['id']}"))
        prompt = (directory / "prompt.txt").read_text()
        self.assertIn(f"Run {self.shim} device list", prompt)
        self.assertIn(f"Also try {self.shim} info.", prompt)
        self.assertNotIn(FAKE, prompt)

    def test_attempt_log_helpers_share_one_layout(self):
        logs, proxy_log, cli_log = b.start_attempt_logs()
        self.addCleanup(shutil.rmtree, logs, True)
        self.assertEqual((proxy_log.name, cli_log.name, proxy_log.read_text(), cli_log.read_text()),
                         ("proxy.jsonl", "cli.jsonl", "", ""))
        self.assertTrue(logs.name.startswith("we-log-"))
        proxy_log.write_text(json.dumps({"tool": "read_thing", "arguments": {}, "canned": False}) + "\n")
        metrics = {"tool_trace": [{"channel": "mcp", "name": "x"}]}
        trace = b.rebuild_trace(metrics, "wendy", [], proxy_log, cli_log)
        self.assertIs(metrics["tool_trace"], trace)
        self.assertEqual((metrics["transcript_mcp_calls"], metrics["proxy_calls"]), (1, 1))
        self.assertEqual([(e["channel"], e["source"]) for e in trace], [("mcp", "proxy")])
        directory = self.root / "attempt"
        directory.mkdir()
        b.keep_attempt_logs(logs, directory)
        self.assertEqual(sorted(p.name for p in directory.iterdir()), ["cli.jsonl", "proxy.jsonl"])
        self.assertFalse(logs.exists())

    def test_guard_app_id_comes_from_the_environment(self):
        self.trial(shim=str(self.shim), tools_path=self.root / "tools.json", env={"WENDY_APP_ID": "sh.example.app"})
        directory = next(self.out.iterdir())
        self.assertEqual(json.loads((directory / "scenario.json").read_text())["block"], sc.guard_matchers("sh.example.app"))

    def test_laptop_trial_has_no_proxy_and_no_shim(self):
        row = self.trial(local=False)
        directory = next(self.out.glob(f"*-{row['id']}"))
        self.assertFalse((directory / "proxy.jsonl").exists())
        self.assertFalse((directory / "scenario.json").exists())
        self.assertEqual(row["stages"]["agent"]["argv"][0], FAKE)

    def test_write_scenario_config_records_passthrough(self):
        path = self.root / "s.json"
        sc.write_scenario_config(path, real_wendy="/r", shim="/s", tools_path="/t", canned_dir="/c", canned_set="x",
                                 canned=[], allow_real=[], block=[{"cli": "a"}], cli_canned=[], device="d",
                                 vm_name="v", proxy_log="/p", cli_log="/l", passthrough=True)
        config = json.loads(path.read_text())
        self.assertEqual((config["passthrough"], config["real_wendy"], config["context"]),
                         (True, "/r", {"device": "d", "vm_name": "v"}))
        self.assertEqual(config["cli_safe"], str(sc.CLI_SAFE))
        sc.write_scenario_config(path, real_wendy="/r", shim="/s", tools_path="/t", canned_dir="/c", canned_set="x",
                                 canned=[], allow_real=[], block=[], cli_canned=[], device="d",
                                 vm_name="v", proxy_log="/p", cli_log="/l")
        self.assertIs(json.loads(path.read_text())["passthrough"], False)


class SweepTests(unittest.TestCase):
    PROTECTED = "sh.wendy.evals.model-tool-report"
    APPS = [PROTECTED, "old", "dev.wendy.eval.a", "made-up-by-model"]

    def fake_run(self, apps=None, list_code=0, fail=()):
        self.calls = []
        def run(argv, **kwargs):
            self.calls.append(argv)
            if argv[-3:] == ["device", "apps", "list"]:
                return SimpleNamespace(returncode=list_code, stdout=json.dumps([{"name": a} for a in apps or self.APPS]),
                                       stderr="no route" if list_code else "")
            return SimpleNamespace(returncode=1 if argv[argv.index("remove") + 1] in fail else 0, stdout="", stderr="nope")
        return run

    def test_sweep_removes_only_new_apps(self):
        result = sc.sweep_apps("wendy", ["old"], self.PROTECTED, {}, run=self.fake_run())
        removes = [c for c in self.calls if "remove" in c]
        self.assertEqual(removes, [["wendy", "--device", "local", "device", "apps", "remove", name,
                                    "--force", "--cleanup", "--delete-volumes"]
                                   for name in ("dev.wendy.eval.a", "made-up-by-model")])
        self.assertEqual(result, {"removed": ["dev.wendy.eval.a", "made-up-by-model"], "failed": [], "list_error": None})

    def test_sweep_reports_a_failed_removal(self):
        result = sc.sweep_apps("wendy", ["old"], [self.PROTECTED], {}, run=self.fake_run(fail=("made-up-by-model",)))
        self.assertEqual((result["removed"], result["failed"]), (["dev.wendy.eval.a"], ["made-up-by-model"]))
        self.assertIsNone(result["list_error"])

    def test_sweep_reports_a_failed_list(self):
        result = sc.sweep_apps("wendy", ["old"], self.PROTECTED, {}, run=self.fake_run(list_code=1))
        self.assertIn("no route", result["list_error"])
        self.assertEqual((result["removed"], result["failed"]), ([], []))
        self.assertFalse([c for c in self.calls if "remove" in c])

    def test_sweep_survives_a_command_that_cannot_run(self):
        def run(argv, **kwargs):
            raise subprocess.TimeoutExpired(argv, 120)
        self.assertIn("timed out", sc.sweep_apps("wendy", [], self.PROTECTED, {}, run=run)["list_error"])

    def test_baseline_survives_resume(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "baseline_apps.json"
            self.assertEqual(sc.baseline_apps(path, "wendy", {}, run=self.fake_run(apps=["old"])), ["old"])
            self.assertEqual(json.loads(path.read_text()), ["old"])
            # a restart after a leftover appeared must not adopt the leftover as baseline
            self.assertEqual(sc.baseline_apps(path, "wendy", {}, run=self.fake_run(apps=["old", "leftover"])), ["old"])
            self.assertEqual(json.loads(path.read_text()), ["old"])


class MainTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        config = json.loads((HERE / "config.example.json").read_text())
        self.config = self.root / "config.json"
        self.config.write_text(json.dumps(config))
        self.scenarios = self.root / "scenarios.json"
        self.scenarios.write_text(json.dumps([
            MINIMAL, dict(MINIMAL, id="wifi", group="setup", environments=["physical", "cloud"], requires=["wifi"])]))

    def main(self, *argv):
        out, self.err = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, "argv", ["benchmark.py", "--config", str(self.config),
                                             "--scenarios", str(self.scenarios), *argv]), \
                mock.patch("subprocess.Popen", side_effect=AssertionError("plan-only must not run anything")), \
                mock.patch("subprocess.run", side_effect=AssertionError("plan-only must not run anything")), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(self.err):
            code = b.main()
        return code, out.getvalue()

    def test_plan_only_lists_scenarios_on_simulators_without_calls(self):
        code, text = self.main("--scenario", "all", "--repetitions", "1")
        self.assertEqual(code, 0)
        coverage = json.loads(text[:text.index("\n}\n") + 2])["coverage"]
        self.assertEqual({(c["task"], c["target"], c["status"]) for c in coverage},
                         {("probe", "simulator", "ready"), ("wifi", None, "unconfigured")})
        self.assertTrue(all(c["kind"] == "scenario" for c in coverage))
        self.assertIn('"kind": "scenario"', text)
        self.assertIn("Plan only: 3 trials", text)

    def test_plan_only_reports_robot_scenarios_without_their_prerequisites(self):
        self.scenarios.write_text(json.dumps([MINIMAL, dict(MINIMAL, id="robot", group="robotics", vm_profile="g1")]))
        with mock.patch.object(sc, "robot_prerequisites", return_value=["docker buildx"]) as check:
            code, text = self.main("--scenario", "all", "--repetitions", "1")
        self.assertEqual(code, 0)
        coverage = json.loads(text[:text.index("\n}\n") + 2])["coverage"]
        self.assertEqual({(c["task"], c["status"], c["reason"]) for c in coverage},
                         {("probe", "ready", None), ("robot", "unconfigured", "robot VM prerequisites missing: docker buildx")})
        self.assertIn("Plan only: 3 trials", text)  # probe for each of three agents; robot waits
        self.assertEqual(check.call_args.args[0]["targets"]["simulator"]["kind"], "simulator")  # the run's config

    def test_device_adds_board_target_whose_capabilities_wait_for_execute(self):
        code, text = self.main("--scenario", "wifi", "--device", "192.0.2.10:50051", "--repetitions", "1")
        self.assertEqual(code, 0)
        coverage = json.loads(text[:text.index("\n}\n") + 2])["coverage"]
        self.assertEqual(coverage, [{"task": "wifi", "kind": "scenario", "target": "board", "status": "unconfigured",
                                     "reason": "capabilities not checked (add --execute)"}])
        self.assertIn("Plan only: 0 trials", text)

    def test_plan_only_lists_local_scenarios_not_run_reasons(self):
        config = json.loads(self.config.read_text())
        config["targets"]["local"] = dict(LOCAL_TARGET)
        self.config.write_text(json.dumps(config))
        self.scenarios.write_text(json.dumps([
            MINIMAL, dict(MINIMAL, id="sim-list", group="simulator"),
            dict(MINIMAL, id="on-local", environments=["simulator", "local"])]))
        code, text = self.main("--scenario", "all", "--target", "local", "--repetitions", "1")
        self.assertEqual(code, 0)
        coverage = {c["task"]: c for c in json.loads(text[:text.index("\n}\n") + 2])["coverage"]}
        self.assertEqual(coverage["sim-list"]["reason"], "no matching target configured")
        self.assertEqual(coverage["on-local"]["target"], "local")

    def plan_order(self, disposable):
        config = json.loads(self.config.read_text())
        config["targets"]["local"] = dict(LOCAL_TARGET, capabilities=[])
        if disposable:
            config["disposable_device"] = True
        self.config.write_text(json.dumps(config))
        self.scenarios.write_text(json.dumps([dict(MINIMAL, id=name, environments=["local"]) for name in ("s1", "s2")]))
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock"}):
            code, text = self.main("--scenario", "all", "--task", "stop-app", "--task", "start-app", "--target", "local",
                                   "--agent", "codex", "--repetitions", "2")
        self.assertEqual(code, 0)
        decoder = json.JSONDecoder()
        _, end = decoder.raw_decode(text)
        plan, _ = decoder.raw_decode(text[end:].lstrip())
        return [item["task"] for item in plan]

    def test_laptop_plan_keeps_its_shuffled_order(self):
        self.assertEqual(self.plan_order(False),
                         ["start-app", "s1", "start-app", "s2", "s2", "stop-app", "s1", "stop-app"])

    def test_disposable_pass_runs_scenarios_first(self):
        self.assertEqual(self.plan_order(True),
                         ["s1", "s2", "s2", "s1", "start-app", "start-app", "stop-app", "stop-app"])

    def test_a_flag_without_the_socket_keeps_the_laptop_order(self):
        with mock.patch.dict(os.environ):
            os.environ.pop("WENDY_AGENT_SOCKET", None)
            config = json.loads(self.config.read_text())
            config["targets"]["local"] = dict(LOCAL_TARGET, capabilities=[])
            config["disposable_device"] = True
            self.config.write_text(json.dumps(config))
            self.scenarios.write_text(json.dumps([dict(MINIMAL, id=name, environments=["local"]) for name in ("s1", "s2")]))
            code, text = self.main("--scenario", "all", "--task", "stop-app", "--task", "start-app", "--target", "local",
                                   "--agent", "codex", "--repetitions", "2")
        decoder = json.JSONDecoder()
        _, end = decoder.raw_decode(text)
        plan, _ = decoder.raw_decode(text[end:].lstrip())
        self.assertEqual([item["task"] for item in plan],
                         ["start-app", "s1", "start-app", "s2", "s2", "stop-app", "s1", "stop-app"])

    def test_device_requires_scenario(self):
        with self.assertRaises(SystemExit):
            self.main("--device", "192.0.2.10:50051")
        self.assertIn("--device needs --scenario", self.err.getvalue())

    def test_device_conflicts_with_a_configured_board(self):
        config = json.loads(self.config.read_text())
        config["targets"]["board"] = {"kind": "physical", "device": "192.0.2.1:50051"}
        self.config.write_text(json.dumps(config))
        with self.assertRaises(SystemExit):
            self.main("--scenario", "all", "--device", "192.0.2.10:50051")
        self.assertIn('already has a target named "board"', self.err.getvalue())
        del config["targets"]["board"]
        self.config.write_text(json.dumps(config))
        with self.assertRaises(SystemExit):
            self.main("--scenario", "all", "--device", "vm:pretend-board")
        self.assertIn("a vm: selector is a simulator", self.err.getvalue())


class ExecuteTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        # Wendy chat as configured for real: through the shim, which hands chat the hook (fake_mcp_server chat).
        self.config = {"wendy": FAKE, "budget_usd": 10, "scenario_reserve_usd": .5,
                       "agents": {"fake": {"adapter": "wendy", "model": "fixture",
                                           "command": ["{wendy}", "chat", "--json", "--prompt", "{prompt}"]}},
                       "targets": {"simulator": {"kind": "simulator", "device": "vm:pilot", "capabilities": []}}}
        self.scenarios = self.root / "scenarios.json"
        self.scenarios.write_text(json.dumps([
            MINIMAL, dict(MINIMAL, id="wifi", group="setup", environments=["physical"], requires=["wifi"])]))
        self.run_state = b.isolated_device_state({"kind": "simulator"})
        self.addCleanup(shutil.rmtree, self.run_state, True)
        (self.run_state / "vms").mkdir()

    def remove(self, run_vm, *rest):
        self.calls["remove"].append(run_vm["name"])
        if Path(run_vm["state"]) != self.run_state:
            shutil.rmtree(run_vm["state"], True)
        return {"exit_code": 0}

    def main(self, *argv, failures=(), capabilities=("wifi",), real_prepare=False, refuse=False, prepare=None, snapshots=None):
        """snapshots: successive app lists the scenario reset check reads. refuse: the run must stop before running anything, so a missed check can't start a real agent."""
        path = self.root / "config.json"
        path.write_text(json.dumps(self.config))
        run_vm = {"state": self.run_state, "name": "wendy-eval-run", "device": "vm:wendy-eval-run",
                  "profile": "generic", "stages": {}, "ready": True}
        out, err = io.StringIO(), io.StringIO()
        self.calls = {"prepare": [], "remove": []}
        prepare = prepare or (sc.prepare_run_vm if real_prepare else
                              lambda *a, **k: self.calls["prepare"].append(a[3]) or run_vm)
        patches = [mock.patch.object(sys, "argv", ["benchmark.py", "--config", str(path), "--scenarios", str(self.scenarios),
                                                   "--output", str(self.root / "results"), "--wendy", FAKE, *argv]),
                   mock.patch.object(b, "require_state_isolation"),
                   mock.patch("preflight.check_host", return_value=list(failures)),
                   mock.patch.object(sc, "detect_board", return_value=((list(capabilities) if capabilities is not None else None), {"capabilities": capabilities})),
                   mock.patch.object(sc, "prepare_run_vm", side_effect=prepare),
                   mock.patch.object(sc, "remove_run_vm", side_effect=self.remove),
                   mock.patch.object(sc, "vm_snapshot", **({"side_effect": snapshots} if snapshots else {"return_value": []})),
                   mock.patch.object(sc, "robot_prerequisites", return_value=[]),
                   contextlib.redirect_stdout(out), contextlib.redirect_stderr(err)]
        if refuse:
            nothing = AssertionError("the run must be refused before anything runs")
            patches += [mock.patch.object(b, name, side_effect=nothing) for name in ("run_process", "list_all_tools", "trial")]
            patches.append(mock.patch.object(sc, "scenario_trial", side_effect=nothing))
        with contextlib.ExitStack() as stack:
            for patch in patches:
                stack.enter_context(patch)
            try:
                code = b.main()
            except SystemExit:
                code = ("exit", err.getvalue())
        return code, out.getvalue()

    def test_execute_runs_scenarios_on_shared_vm_and_board_then_removes_the_vm(self):
        code, text = self.main("--scenario", "all", "--device", "192.0.2.10:50051", "--repetitions", "1", "--execute")
        self.assertEqual(code, 0, text)
        self.assertEqual(self.calls, {"prepare": ["generic"], "remove": ["wendy-eval-run"]})
        output = next((self.root / "results").iterdir())
        rows = [json.loads(line) for line in (output / "results.jsonl").read_text().splitlines()]
        for row in rows:
            self.addCleanup(shutil.rmtree, row["workspace"], True)
        self.assertEqual(sorted((r["task"], r["target"], r["device_selector"]) for r in rows),
                         [("probe", "simulator", "vm:wendy-eval-run"), ("wifi", "board", "192.0.2.10:50051")])
        self.assertTrue(all(r["kind"] == "scenario" and r["success"] for r in rows))
        # chat -> WENDY_CHAT_MCP_COMMAND (the shim) -> recording proxy -> the real binary's mcp serve
        self.assertEqual([(r["metrics"]["transcript_mcp_calls"], r["metrics"]["proxy_calls"]) for r in rows], [(2, 2)] * 2)
        self.assertEqual(text.count("Usage unknown; charged the scenario reserve of $0.50."), 2)
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertEqual(manifest["scenarios_sha256"], b.hashlib.sha256(self.scenarios.read_bytes()).hexdigest())
        self.assertEqual(manifest["harness_sha256"], b.harness_sha256())
        self.assertEqual(manifest["config"]["targets"]["board"]["capabilities"], ["wifi"])
        self.assertEqual(manifest["run_vms"], {"generic": {"name": "wendy-eval-run", "device": "vm:wendy-eval-run"}})
        self.assertTrue((output / "bin/wendy").exists() and (output / "tools.json").exists())
        coverage = json.loads((output / "coverage.json").read_text())
        self.assertEqual({(c["task"], c["status"]) for c in coverage}, {("probe", "ready"), ("wifi", "ready")})

    def disposable_main(self, *argv, flag=True, sweeps=None, baseline=("old",), list_error=None, **kwargs):
        """A scenario pass on the local target with the agent socket set; sweeps are the sweep_apps results in order."""
        # Without the flag this is a laptop run, and a laptop never has the local target (main refuses it).
        board = dict(LOCAL_TARGET, capabilities=[]) if flag else dict(
            LOCAL_TARGET, kind="physical", device="192.0.2.10:50051", capabilities=[])
        self.config["targets"].setdefault("local", board)
        if flag:
            self.config["disposable_device"] = True
        self.scenarios.write_text(json.dumps([dict(MINIMAL, environments=["local" if flag else "physical"])]))
        ok = {"removed": [], "failed": [], "list_error": None}
        results = iter(sweeps or [])
        self.sweep_calls = []
        def sweep(wendy, base, protected, env, run=None):
            self.sweep_calls.append((base, protected))
            return next(results, ok)
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock"}), \
                mock.patch.object(sc, "sweep_apps", side_effect=sweep), \
                mock.patch.object(sc, "list_apps", **({"side_effect": list_error} if list_error else {"return_value": list(baseline)})):
            code, text = self.main("--scenario", "probe", "--target", "local", "--execute", *argv, **kwargs)
        outputs = list((self.root / "results").iterdir()) if (self.root / "results").exists() else []
        output = outputs[0] if outputs else None
        if output and (output / "results.jsonl").exists():
            for line in (output / "results.jsonl").read_text().splitlines():
                self.addCleanup(shutil.rmtree, json.loads(line)["workspace"], True)
        return code, text, output

    def test_preflight_still_refuses_without_the_flag(self):
        code, text, output = self.disposable_main("--repetitions", "1", flag=False, failures=["another wendy"])
        self.assertEqual(code, 2)
        self.assertIn("Host check failed: another wendy", text)
        self.assertFalse((output / "results.jsonl").exists())

    def test_a_disposable_device_skips_preflight_and_says_so(self):
        code, text, output = self.disposable_main("--repetitions", "1", failures=["another wendy"])
        self.assertEqual(code, 0, text)
        self.assertNotIn("Host check failed", text)
        self.assertEqual(json.loads((output / "manifest.json").read_text())["preflight"], "skipped: disposable_device")
        self.assertEqual(self.sweep_calls, [(["old"], sc.BENCHMARK_APP)])
        self.assertEqual(json.loads((output / "baseline_apps.json").read_text()), ["old"])
        self.assertNotIn("Usage unknown; charged", text)

    def local_task_main(self, *, flag=True, socket=True, execute=True, agent_command=None, adapter="wendy"):
        self.config["targets"]["local"] = dict(LOCAL_TARGET, capabilities=[])
        self.config["budget_usd"] = 100
        if flag:
            self.config["disposable_device"] = True
        if agent_command is not None:
            self.config["agents"]["fake"] = {"adapter": adapter, "model": "fixture", "command": agent_command}
        path = self.root / "config.json"
        path.write_text(json.dumps(self.config))
        self.scenarios.write_text(json.dumps([MINIMAL]))
        trials, out, err = [], io.StringIO(), io.StringIO()
        env = {"WENDY_AGENT_SOCKET": "/run/wendy.sock"} if socket else {}
        argv = ["benchmark.py", "--config", str(path), "--scenarios", str(self.scenarios),
                "--output", str(self.root / "results"), "--wendy", FAKE, "--task", "deploy-http",
                "--target", "local", "--repetitions", "1", *(["--execute"] if execute else [])]
        with mock.patch.dict(os.environ, env), \
                mock.patch.dict(os.environ, {} if socket else {"WENDY_AGENT_SOCKET": ""}), \
                mock.patch.object(b, "trial", side_effect=lambda *a, **k: trials.append(a) or {
                    "id": "x", "agent": "fake", "target": "local", "task": "deploy-http", "kind": "task",
                    "status": "setup_error", "success": False, "cleanup_ok": True,
                    "task_seconds": 0, "workspace": str(self.root), "metrics": {}, "stages": {}}), \
                mock.patch.object(b, "require_state_isolation"), \
                mock.patch.object(sc, "list_apps", return_value=[]), \
                mock.patch.object(sc, "sweep_apps", return_value={"removed": [], "failed": [], "list_error": None}), \
                mock.patch.object(sys, "argv", argv), \
                contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            try:
                code = b.main()
            except SystemExit as exit_:
                code = ("exit", exit_.code)
        return code, err.getvalue(), trials

    def test_a_local_target_needs_both_the_flag_and_the_socket_to_execute(self):
        for flag, socket in ((False, True), (True, False), (False, False)):
            code, err, trials = self.local_task_main(flag=flag, socket=socket)
            self.assertEqual(code, ("exit", 2), (flag, socket))
            self.assertIn("disposable_device", err)
            self.assertIn("WENDY_AGENT_SOCKET", err)
            self.assertEqual(trials, [])
        code, err, trials = self.local_task_main()
        self.assertEqual((code, len(trials)), (1, 1), err)

    def test_plan_only_still_allows_a_local_target_without_either(self):
        code, err, trials = self.local_task_main(flag=False, socket=False, execute=False)
        self.assertEqual((code, trials), (0, []), err)

    def test_local_only_runs_get_the_shim_command_checks(self):
        bad = {
            "wendy": ["/usr/bin/wendy", "chat", "--prompt", "{prompt}"],
            "claude": ["claude", "-p", "{prompt}", "--plugin-dir", "{plugin_dir}"],
            "codex": ["codex", "exec", "-c", 'mcp_servers.wendy.command="/usr/bin/wendy"'],
        }
        for adapter, command in bad.items():
            code, err, trials = self.local_task_main(agent_command=command, adapter=adapter)
            self.assertEqual(code, ("exit", 2), adapter)
            self.assertEqual(trials, [], adapter)
            self.assertIn("shim" if adapter != "claude" else "MCP config", err, adapter)

    def test_shim_stays_off_the_results_volume_when_disposable(self):
        made = []
        real = eval_wendy.write_shim

        def write_shim(directory, *a, **k):
            made.append(Path(directory))
            return real(directory, *a, **k)
        with mock.patch.object(eval_wendy, "write_shim", side_effect=write_shim):
            code, text, output = self.disposable_main("--repetitions", "1")
        self.assertEqual(code, 0, text)
        [directory] = made
        self.addCleanup(shutil.rmtree, directory, True)
        self.assertTrue(directory.name.startswith("we-bin-"))
        self.assertNotIn(output.resolve(), [directory.resolve(), *directory.resolve().parents])
        self.assertFalse((output / "bin").exists())

    def test_local_tasks_get_the_shim_and_the_tools_list(self):
        self.scenarios.write_text(json.dumps([MINIMAL]))
        seen = []

        def trial(config, agent, target, task, output, wendy, treatment_root=None, shim=None, tools_path=None):
            seen.append((target, task["id"], wendy, shim, tools_path))
            return {"id": "x", "agent": agent, "target": target, "task": task["id"], "kind": "task", "status": "setup_error",
                    "success": False, "cleanup_ok": True, "task_seconds": 0, "workspace": str(self.root),
                    "metrics": {"estimated_cost_usd": 0}, "stages": {}}
        self.config["targets"]["local"] = dict(LOCAL_TARGET, capabilities=[])
        self.config["disposable_device"] = True
        self.config["budget_usd"] = 100
        path = self.root / "config.json"
        path.write_text(json.dumps(self.config))
        out = io.StringIO()
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock"}), \
                mock.patch.object(b, "trial", side_effect=trial), \
                mock.patch.object(b, "require_state_isolation"), \
                mock.patch.object(sc, "list_apps", return_value=[]), \
                mock.patch.object(sc, "sweep_apps", return_value={"removed": [], "failed": [], "list_error": None}), \
                mock.patch.object(sys, "argv", ["benchmark.py", "--config", str(path), "--scenarios", str(self.scenarios),
                                                "--output", str(self.root / "results"), "--wendy", FAKE, "--task", "deploy-http",
                                                "--target", "local", "--repetitions", "1", "--execute"]), \
                contextlib.redirect_stdout(out):
            code = b.main()
        self.assertEqual(code, 1, out.getvalue()[-600:])
        [(target, task, wendy, shim, tools)] = seen
        self.addCleanup(shutil.rmtree, Path(shim).parent, True)
        self.assertEqual((target, task, wendy), ("local", "deploy-http", FAKE))
        self.assertTrue(Path(shim).is_file())
        self.assertTrue(Path(tools).name == "tools.json")

    def test_chat_hook_is_checked_when_only_local_tasks_are_planned(self):
        nohook = self.root / "nohook"
        nohook.write_text("#!/bin/sh\n")
        nohook.chmod(0o755)
        self.config["targets"]["local"] = dict(LOCAL_TARGET, capabilities=[])
        self.config["disposable_device"] = True
        path = self.root / "config.json"
        path.write_text(json.dumps(self.config))
        err = io.StringIO()
        with mock.patch.dict(os.environ, {"WENDY_AGENT_SOCKET": "/run/wendy.sock"}), \
                mock.patch.object(sys, "argv", ["benchmark.py", "--config", str(path), "--scenarios", str(self.scenarios),
                                                "--output", str(self.root / "results"), "--wendy", str(nohook),
                                                "--task", "deploy-http", "--target", "local", "--repetitions", "1", "--execute"]), \
                contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(err), self.assertRaises(SystemExit):
            b.main()
        self.assertIn("predates the WENDY_CHAT_MCP_COMMAND hook", err.getvalue())

    def test_one_failed_sweep_does_not_halt(self):
        bad = {"removed": [], "failed": ["stuck"], "list_error": None}
        code, text, output = self.disposable_main("--repetitions", "3", sweeps=[bad])
        rows = [json.loads(line) for line in (output / "results.jsonl").read_text().splitlines()]
        self.assertEqual(len(rows), 3)
        self.assertEqual([r["cleanup_ok"] for r in rows], [False, True, True])
        self.assertFalse((output / "halt.json").exists())
        self.assertEqual(code, 1)

    def test_two_failed_sweeps_halt_and_write_the_reason(self):
        bad = {"removed": [], "failed": [], "list_error": "no route"}
        code, text, output = self.disposable_main("--repetitions", "4", sweeps=[bad, bad])
        self.assertEqual(code, 3)
        rows = (output / "results.jsonl").read_text().splitlines()
        self.assertEqual(len(rows), 2)
        reason = json.loads((output / "halt.json").read_text())["reason"]
        self.assertIn("sweep failed twice", reason)
        self.assertIn(reason, text)

    def test_a_failed_sweep_then_a_clean_one_resets_the_count(self):
        bad = {"removed": [], "failed": ["stuck"], "list_error": None}
        ok = {"removed": [], "failed": [], "list_error": None}
        code, text, output = self.disposable_main("--repetitions", "4", sweeps=[bad, ok, bad])
        self.assertEqual(len((output / "results.jsonl").read_text().splitlines()), 4)
        self.assertFalse((output / "halt.json").exists())

    def test_an_unreadable_app_list_at_start_halts_with_a_reason(self):
        code, text, output = self.disposable_main("--repetitions", "1", list_error=RuntimeError("agent unreachable"))
        self.assertEqual(code, 3)
        reason = json.loads((output / "halt.json").read_text())["reason"]
        self.assertIn("agent unreachable", reason)
        self.assertFalse((output / "results.jsonl").exists())

    def test_unreadable_capabilities_halt_a_disposable_pass_with_a_reason(self):
        self.config["targets"]["local"] = dict(LOCAL_TARGET)  # capabilities None: read under --execute
        code, text, output = self.disposable_main("--repetitions", "1", capabilities=None)
        self.assertEqual(code, 3, text)
        self.assertIn("Could not read the capabilities of local", json.loads((output / "halt.json").read_text())["reason"])

    def test_unreadable_capabilities_on_a_laptop_still_return_2_without_halt_json(self):
        self.config["targets"]["local"] = dict(LOCAL_TARGET, kind="physical", device="192.0.2.10:50051")
        code, text, output = self.disposable_main("--repetitions", "1", flag=False, capabilities=None)
        self.assertEqual(code, 2, text)
        self.assertFalse((output / "halt.json").exists())

    def test_a_laptop_still_stops_on_failed_cleanup(self):
        code, text, output = self.disposable_main("--repetitions", "3", flag=False,
                                                  snapshots=[[], [{"name": "new"}], [], []])
        # without the flag this is a laptop run: preflight ran, nothing swept, and the changed app list stopped it
        self.assertEqual(self.sweep_calls, [])
        self.assertIn("Cleanup failed. Stopping before another trial can inherit this state.", text)
        self.assertEqual(len((output / "results.jsonl").read_text().splitlines()), 1)
        self.assertFalse((output / "halt.json").exists())

    def test_a_changed_app_list_is_recorded_but_does_not_stop_a_disposable_pass(self):
        code, text, output = self.disposable_main("--repetitions", "3", snapshots=[[], [{"name": "new"}], [], [], [], []])
        rows = [json.loads(line) for line in (output / "results.jsonl").read_text().splitlines()]
        self.assertEqual(len(rows), 3)
        self.assertEqual(rows[0]["stages"]["reset"]["exit_code"], 1)
        self.assertTrue(rows[0]["cleanup_ok"])
        self.assertNotIn("Cleanup failed", text)

    def test_interrupted_run_vm_preparation_still_removes_the_vm(self):
        def fixture_vm(config, variables, workspace, directory, env):
            self.calls["prepare"].append(variables["vm_name"])  # the VM exists from here on
            raise KeyboardInterrupt
        with mock.patch.object(b, "fixture_vm", side_effect=fixture_vm), self.assertRaises(KeyboardInterrupt):
            self.main("--scenario", "probe", "--repetitions", "1", "--execute", real_prepare=True)
        self.assertEqual(len(self.calls["prepare"]), 1)
        self.assertEqual(self.calls["remove"], self.calls["prepare"])

    def test_a_failed_robot_vm_turns_its_scenarios_into_setup_errors_and_the_run_goes_on(self):
        self.scenarios.write_text(json.dumps([MINIMAL, dict(MINIMAL, id="dog", group="robotics", vm_profile="go2"),
                                              dict(MINIMAL, id="humanoid", group="robotics", vm_profile="g1")]))
        generic = {"state": self.run_state, "name": "wendy-eval-run", "device": "vm:wendy-eval-run",
                   "profile": "generic", "stages": {}, "ready": True}
        def prepare(config, wendy, output, profile, register=None):
            self.calls["prepare"].append(profile)
            if profile == "generic":
                return generic
            vm = dict(generic, name=f"wendy-eval-{profile}", device=f"vm:wendy-eval-{profile}", profile=profile,
                      stages={"vm-create": {"exit_code": 0}, "vm-robot": {"exit_code": 1}}, ready=False)
            register(vm)  # created before it failed, so it must still be removed
            if profile == "g1":
                raise RuntimeError("qemu exploded")
            return vm
        code, text = self.main("--scenario", "all", "--repetitions", "1", "--execute", prepare=prepare)
        self.assertEqual(code, 1)
        output = next((self.root / "results").iterdir())
        rows = {r["task"]: r for r in map(json.loads, (output / "results.jsonl").read_text().splitlines())}
        for row in rows.values():
            if row.get("workspace"):
                self.addCleanup(shutil.rmtree, row["workspace"], True)
        self.assertEqual({t: r["status"] for t, r in rows.items()},
                         {"probe": "passed", "dog": "setup_error", "humanoid": "setup_error"})
        self.assertIn("vm-robot failed (exit 1)", rows["dog"]["stages"]["run-vm"]["error"])
        self.assertIn("RuntimeError('qemu exploded')", rows["humanoid"]["stages"]["run-vm"]["error"])
        self.assertIn("Preparing the shared go2 VM failed (vm-robot failed (exit 1); see", text)
        self.assertIn("its scenario attempts are recorded as setup errors", text)
        self.assertEqual(self.calls["prepare"], ["g1", "generic", "go2"])
        self.assertEqual(self.calls["remove"], ["wendy-eval-g1", "wendy-eval-go2", "wendy-eval-run"])
        summary = {r["task"]: r for r in json.loads((output / "summary.json").read_text())}
        self.assertEqual((summary["dog"]["attempts"], summary["dog"]["setup_errors"]), (0, 1))
        self.assertEqual(summary["probe"]["successes"], 1)
        manifest = json.loads((output / "manifest.json").read_text())
        self.assertEqual(set(manifest["run_vms"]), {"generic"})
        self.assertEqual(set(manifest["unprepared_run_vms"]), {"go2", "g1"})
        self.assertEqual(set(json.loads((output / "run_vms.json").read_text())), {"generic", "go2", "g1"})

    def test_unsafe_host_refuses_before_any_vm_or_board_call(self):
        code, text = self.main("--scenario", "all", "--device", "192.0.2.10:50051", "--execute",
                               failures=["another wendy is reachable at /usr/bin/wendy"])
        self.assertEqual(code, 2)
        self.assertIn("Host check failed: another wendy is reachable at /usr/bin/wendy", text)
        self.assertEqual(self.calls, {"prepare": [], "remove": []})

    def test_budget_reserve_stops_scenarios(self):
        self.config["budget_usd"] = .7
        code, text = self.main("--scenario", "probe", "--repetitions", "3", "--execute")
        output = next((self.root / "results").iterdir())
        rows = (output / "results.jsonl").read_text().splitlines()
        self.addCleanup(shutil.rmtree, json.loads(rows[0])["workspace"], True)
        self.assertEqual(len(rows), 1)
        self.assertIn("Stopping at the configured budget reserve.", text)
        self.assertEqual(code, 1)
        self.assertEqual(self.calls["remove"], ["wendy-eval-run"])

    def test_codex_template_must_carry_the_scenario_and_vm_store(self):
        codex = json.loads((HERE / "config.example.json").read_text())["agents"]["codex"]
        self.config["agents"] = {"codex": dict(codex, model="pinned")}
        code, _ = self.main("--scenario", "probe", "--execute", "--wendy", str(self.root / "missing"), refuse=True)
        self.assertIn("Wendy executable not found", code[1])
        for dropped in ('mcp_servers.wendy.env.WENDY_EVAL_SCENARIO="{scenario_config}"', "{wendy_vm_home}",
                        'mcp_servers.wendy.command="{wendy}"'):
            with self.subTest(dropped=dropped):
                self.config["agents"]["codex"]["command"] = [a for a in codex["command"] if a != dropped]
                code, _ = self.main("--scenario", "probe", "--execute", refuse=True)
                self.assertEqual(code[0], "exit")
                self.assertIn("codex must", code[1])

    def test_scenario_templates_must_route_wendy_through_the_shim(self):
        example = json.loads((HERE / "config.example.json").read_text())["agents"]
        def without(name, *dropped, **replaced):
            command = [replaced.get(a, a) for a in example[name]["command"] if a not in dropped]
            return {name: dict(example[name], model="pinned", command=command)}
        cases = [(without("claude", "--plugin-dir", "{plugin_dir}"), "Claude must load the staged plugin"),
                 (without("claude", "--strict-mcp-config"), "claude must load only the runner's MCP config"),
                 (without("claude", **{"{workspace}/mcp.json": "/elsewhere/mcp.json"}),
                  "claude must load only the runner's MCP config"),
                 (without("wendy", "{wendy}"), "wendy must run chat through the shim")]
        for agents, message in cases:
            with self.subTest(message=message, command=next(iter(agents.values()))["command"]):
                self.config["agents"] = agents
                code, _ = self.main("--scenario", "probe", "--execute", refuse=True)
                self.assertEqual(code[0], "exit")
                self.assertIn(message, code[1])
        self.assertFalse((self.root / "results").exists())
        # The staged plugin is required for end-to-end runs too.
        self.config["agents"] = without("claude", "--plugin-dir", "{plugin_dir}")
        code, _ = self.main("--task", "deploy-http", "--execute", refuse=True)
        self.assertIn("Claude must load the staged plugin", code[1])

    def test_wendy_chat_needs_a_binary_with_the_mcp_hook(self):
        old = self.root / "old-wendy"
        old.write_text("#!/bin/sh\nexit 0\n")  # a chat built before WENDY_CHAT_MCP_COMMAND
        old.chmod(0o755)
        code, _ = self.main("--scenario", "probe", "--execute", "--wendy", str(old), refuse=True)
        self.assertEqual(code[0], "exit")
        self.assertIn(f"{old} predates the WENDY_CHAT_MCP_COMMAND hook", code[1])
        self.assertFalse((self.root / "results").exists())
        self.assertEqual(self.calls["prepare"], [])

    def test_a_chat_that_bypasses_the_proxy_stops_the_run(self):
        with mock.patch.dict(os.environ, {"FAKE_CHAT_IGNORES_HOOK": "1"}):
            code, text = self.main("--scenario", "probe", "--repetitions", "2", "--execute")
        output = next((self.root / "results").iterdir())
        rows = [json.loads(line) for line in (output / "results.jsonl").read_text().splitlines()]
        for row in rows:
            self.addCleanup(shutil.rmtree, row["workspace"], True)
        self.assertEqual([(r["status"], r["success"]) for r in rows], [("mcp_unverified", False)])
        self.assertEqual((rows[0]["metrics"]["transcript_mcp_calls"], rows[0]["metrics"]["proxy_calls"]), (2, 0))
        self.assertIn("The agent's Wendy MCP calls never reached the recording proxy; stopping.", text)
        self.assertEqual(code, 1)
        self.assertEqual(self.calls["remove"], ["wendy-eval-run"])


def go_tool_names():
    text = TOOL_GROUPS_GO.read_text()
    start = text.index("var toolGroups = map[string][]string{")
    block = text[start:text.index("\n}\n", start)]
    return set(re.findall(r'"([a-z0-9_]+)"', block))


def matchers(scenario):
    return [m for side in ("right", "wrong") for m in scenario["expect"].get(side, [])]


class CatalogTests(unittest.TestCase):
    """The shipped scenario set (scenarios.json) and its simulator captures."""

    def setUp(self):
        self.catalog = sc.load_scenarios(CATALOG)

    def test_catalog_covers_every_group(self):
        self.assertGreaterEqual(len(self.catalog), 40)
        counts = Counter(s["group"] for s in self.catalog)
        minimum = {"core": 8, "setup": 8, "simulator": 4, "hardware": 6, "robotics": 4, "observability": 5, "cloud": 4}
        for group, least in minimum.items():
            with self.subTest(group=group):
                self.assertGreaterEqual(counts[group], least)
        # Ask-only: asking is the one good outcome (no right path).
        self.assertGreaterEqual(sum(1 for s in self.catalog
                                    if s["expect"].get("ask") is True and not s["expect"].get("right")), 3)

    def test_prompts_never_name_the_expected_tool(self):
        for scenario in self.catalog:
            prompt = scenario["prompt"].lower()
            for name in {m["tool"] for m in matchers(scenario) if "tool" in m}:
                with self.subTest(scenario=scenario["id"], tool=name):
                    self.assertNotIn(name, prompt)
                    self.assertNotIn(name.replace("_", " "), prompt)

    def test_catalog_names_only_real_tools(self):
        real = go_tool_names()
        self.assertIn("wifi_connect", real)  # the parse found the block
        for scenario in self.catalog:
            names = {m["tool"] for m in matchers(scenario) if "tool" in m}
            names |= set(scenario["canned"]) | set(scenario["captures"]) | set(scenario["allow_real"])
            with self.subTest(scenario=scenario["id"]):
                self.assertEqual(names - real, set())

    def test_ask_scenarios_have_a_wrong_path(self):
        # With a right path too, both doing it and asking pass; the wrong path is what the ask guards.
        for scenario in self.catalog:
            if scenario["expect"].get("ask"):
                with self.subTest(scenario=scenario["id"]):
                    self.assertTrue(scenario["expect"].get("wrong"))

    def test_skill_ask_before_actions_accept_asking(self):
        # SKILL.md: "Ask before calls that interrupt or erase: container_stop, container_delete,
        # wifi_connect, os_update, device_update_agent and simulator_delete"; camera_snapshot's
        # description says it requires approval. A scenario whose right path is one of them accepts asking.
        ask_before = {"container_stop", "container_delete", "wifi_connect", "os_update", "device_update_agent",
                      "simulator_delete", "camera_snapshot"}
        for scenario in self.catalog:
            if {m.get("tool") for m in scenario["expect"].get("right", [])} & ask_before:
                with self.subTest(scenario=scenario["id"]):
                    self.assertIs(scenario["expect"].get("ask"), True)

    def test_canned_reads_are_canned_on_the_cli_too(self):
        # A canned MCP read and a real CLI read of the same state would contradict each other.
        equivalents = {"container_list": [["device", "apps", "list"], ["device", "ps"]],
                       "telemetry_logs": [["device", "logs"]], "container_stats": [["device", "top"]],
                       "simulator_list": [["vm", "list"]],
                       "device_list": [["device", "list"], ["device", "ls"], ["discover"]],
                       "wifi_status": [["device", "wifi", "status"]], "wifi_list": [["device", "wifi", "list"]],
                       "camera_controls": [["device", "camera", "controls"]],
                       "camera_list": [["device", "camera", "list"]],
                       "bluetooth_scan": [["device", "bluetooth", "list"], ["device", "bt", "list"]],
                       "hardware_capabilities": [["device", "hardware", "list"]], "device_info": [["device", "info"]],
                       "os_list_drives": [["os", "list-drives"]],
                       "os_install_status": [["os", "install", "jobs", "status"]]}
        device_scoped = {"wifi_status", "wifi_list", "camera_controls", "camera_list", "bluetooth_scan",
                         "hardware_capabilities", "device_info", "container_list", "container_stats", "telemetry_logs"}
        for scenario in self.catalog:
            for tool in scenario["canned"]:
                wanted = list(equivalents.get(tool, []))
                if "cloud" in scenario["environments"] and tool in device_scoped:
                    wanted += [["cloud", *words] for words in wanted]
                for words in wanted:
                    with self.subTest(scenario=scenario["id"], tool=tool, words=words):
                        self.assertIn(words, scenario["cli_canned"])

    def test_container_commands_are_matched_on_the_command_only(self):
        # grading searches the whole argument JSON; app_name or a device attach APP must not match.
        wifi = next(s for s in self.catalog if s["id"] == "wifi-connect")["expect"]
        for scenario in self.catalog:
            for matcher in matchers(scenario):
                if matcher.get("tool") == "container_exec" and "args_regex" in matcher:
                    with self.subTest(scenario=scenario["id"]):
                        self.assertTrue(matcher["args_regex"].startswith('(?i)"command":'))
        exec_entry = lambda **args: {"channel": "mcp", "name": "container_exec", "arguments": args}
        attach = lambda *argv: {"channel": "cli", "name": "device attach " + argv[2], "arguments": list(argv)}
        self.assertEqual(grade([exec_entry(app_name="nmcli-tools", command=["cat", "/etc/hostname"])],
                               wifi, "", True)["outcome"], "missed")
        self.assertEqual(grade([exec_entry(app_name="a", command=["sh", "-c", "nmcli dev wifi connect Lab5G"])],
                               wifi, "", True)["outcome"], "wrong")
        self.assertEqual(grade([exec_entry(app_name="a", command=["ip", "link", "set", "wlan0", "up"])],
                               wifi, "", True)["outcome"], "wrong")
        self.assertEqual(grade([attach("device", "attach", "nmcli-app", "--", "ls")], wifi, "", True)["outcome"],
                         "missed")
        self.assertEqual(grade([attach("device", "attach", "app", "--", "nmcli", "dev")], wifi, "", True)["outcome"],
                         "wrong")

    def test_wifi_scenarios_treat_host_networksetup_as_wrong(self):
        # macOS changes the host's own network with networksetup; at command position it is wrong.
        by_id = {s["id"]: s for s in self.catalog}
        shell = lambda command: {"channel": "shell", "name": "shell", "arguments": command}
        for sid in ("wifi-connect", "wifi-scan", "wifi-saved-networks"):
            expect = by_id[sid]["expect"]
            for command in ("networksetup -setairportnetwork en0 Lab5G secret", "sudo networksetup -setairportpower en0 off",
                            "cd /tmp && networksetup -listpreferredwirelessnetworks en0"):
                with self.subTest(scenario=sid, command=command):
                    self.assertEqual(grade([shell(command)], expect, "", True)["outcome"], "wrong")
            for command in ("echo networksetup", "man networksetup | head"):
                with self.subTest(scenario=sid, command=command):
                    self.assertEqual(grade([shell(command)], expect, "", True)["outcome"], "missed")
        # Only connecting would change the network, so only there is the matcher a block.
        blocks = {sid: [m.get("block", False) for m in by_id[sid]["expect"]["wrong"] if "networksetup" in m.get("shell", "")]
                  for sid in ("wifi-connect", "wifi-scan", "wifi-saved-networks")}
        self.assertEqual(blocks, {"wifi-connect": [True], "wifi-scan": [False], "wifi-saved-networks": [False]})

    def test_block_matchers_use_args_regex_not_exact_args(self):
        # Exact args are bypassed by key casing or `sh -c`; a block must match on a regex.
        for scenario in self.catalog:
            for matcher in matchers(scenario):
                if matcher.get("block"):
                    with self.subTest(scenario=scenario["id"], matcher=matcher):
                        self.assertNotIn("args", matcher)

    def test_hardware_scenarios_run_only_on_a_board(self):
        # The simulator has no wifi, bluetooth, camera or Cloud access (target_capabilities).
        for scenario in self.catalog:
            if scenario["group"] in ("hardware", "cloud") or scenario["requires"]:
                with self.subTest(scenario=scenario["id"]):
                    self.assertNotIn("simulator", scenario["environments"])

    def test_every_simulator_scenario_is_captured(self):
        # Except two whose streaming CLI right path needs a go2 VM, which needs docker buildx to build.
        waiting = {"ros-rate": "CLI replies not captured: device ros2 hz",
                   "ros-sample": "CLI replies not captured: device ros2 echo"}
        simulator = {"kind": "simulator", "capabilities": []}
        for scenario in self.catalog:
            if "simulator" in scenario["environments"]:
                with self.subTest(scenario=scenario["id"]):
                    self.assertEqual(sc.scenario_readiness(scenario, simulator, sc.CANNED, prerequisites=lambda: []),
                                     waiting.get(scenario["id"]))

    def test_reads_after_the_canned_action_show_the_after_state(self):
        # An agent that acts first and then checks must see the new state, or a correct action looks
        # failed and a recovery move (kill qemu, rm the VM dir) is graded wrong.
        stories = {"deploy-project": ["tool:run", "cli:run"],
                   "app-stop": ["tool:container_stop", "cli:device apps stop"],
                   "app-start": ["tool:container_start", "cli:device apps start"],
                   "app-remove-free-space": ["tool:container_delete", "cli:device apps remove"],
                   "sim-create-go2": ["tool:simulator_create", "cli:vm create"],
                   "sim-stop": ["tool:simulator_stop", "cli:vm stop"],
                   "sim-delete": ["tool:simulator_delete", "cli:vm rm", "cli:vm remove"]}
        reads = {"container_list", "telemetry_logs", "simulator_list", "device_list"}
        by_id = {s["id"]: s for s in self.catalog}
        for name, actions in stories.items():
            scenario = by_id[name]
            for tool in reads & set(scenario["canned"]):
                with self.subTest(scenario=name, tool=tool):
                    capture = canned.load_capture(sc.CANNED, scenario["canned_set"], tool)
                    self.assertEqual(sorted(capture["after"]["actions"]), sorted(actions))
            for words in scenario["cli_canned"]:
                with self.subTest(scenario=name, words=words):
                    capture = canned.load_cli_capture(sc.CANNED, scenario["canned_set"], words)
                    self.assertEqual(sorted(capture["after"]["actions"]), sorted(actions))


if __name__ == "__main__":
    unittest.main()
