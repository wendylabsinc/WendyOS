"""Short tool-choice scenarios: loading, readiness, the shared run VM or spare board, and one attempt.

An attempt runs the agent against the `wendy` shim (eval_wendy.py): MCP calls go through the recording
proxy and shell `wendy` calls through the CLI wrapper, so risky calls are canned and every call is
logged. Python standard library only.
"""
import contextlib
import copy
import functools
import json
import math
import os
from pathlib import Path
import re
import shutil
import socket
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import uuid

import benchmark as b
from canned import load_capture, load_cli_capture
from grading import grade, validate_expect
from mcp_client import StdioMCP
from retry_guard import RetryGuard
import robot_diagnostics
import robot_fixtures
from tool_trace import build_trace, read_jsonl

HERE = Path(__file__).resolve().parent
CANNED = HERE / "canned"
CLI_SAFE = HERE / "cli_safe.json"
GROUPS = ("core", "setup", "simulator", "hardware", "robotics", "observability", "cloud")
ENVIRONMENTS = ("simulator", "physical", "cloud", "local")
VM_PROFILES = ("generic", "go2", "g1")
REQUIRED = ("id", "group", "environments", "prompt", "expect")
DEFAULTS = {"timeout_seconds": 180, "max_tool_calls": 20, "canned": [], "captures": [], "allow_real": [],
            "requires": [], "files": {}, "vm_profile": "generic", "cli_canned": []}
OPTIONAL = frozenset({"canned_set", "notes"})
_NAME = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*$")

# The agent's hardware discoverer (go/internal/agent/hardware/discoverer.go, ListHardwareCapabilities)
# emits the categories gpu, usb, i2c, spi, gpio, camera, audio, network and storage. Only camera maps
# directly; the other names here are explicit synonyms in case a category is added under them.
CATEGORY_TAGS = {"camera": "camera", "cameras": "camera",
                 "wifi": "wifi", "wi-fi": "wifi", "wlan": "wifi", "wireless": "wifi",
                 "bluetooth": "bluetooth", "bt": "bluetooth"}
# discoverCamera lists every /dev/video* node, V4L2 memory-to-memory codecs and ISPs included. These
# driver prefixes are never a camera: go/internal/agent/camera/transport.go nonCameraDriverPrefixes
# (bcm2835-isp, bcm2835-codec), plus the Pi 5 ISP back end and HEVC decoder, which that list lacks and
# whose device-tree node would make Classify call them CSI.
NON_CAMERA_DRIVERS = ("bcm2835-isp", "bcm2835-codec", "pispbe", "rpi-hevc-dec")
CAMERA_TRANSPORTS = frozenset({"usb", "csi"})


def _check(condition, message):
    if not condition:
        raise ValueError(message)


def _names(value, key):
    _check(isinstance(value, list) and all(isinstance(v, str) and v for v in value),
           f"{key} must be a list of names")


def _cli_words(value):
    # Each entry is a subcommand path, as tool_trace.cli_path reads it: plain words, no flags.
    _check(isinstance(value, list) and all(
        isinstance(words, list) and words and all(isinstance(w, str) and w and not w.startswith("-") for w in words)
        for words in value), "cli_canned must be a list of subcommand word lists")


def _relative(name):
    parts = name.split("/") if isinstance(name, str) else [""]
    _check(not name.startswith("/") and all(p not in ("", ".", "..") for p in parts),
           f"files names must be plain relative paths: {name!r}")


def _scenario(item):
    _check(isinstance(item, dict), "a scenario must be an object")
    unknown = set(item) - set(REQUIRED) - set(DEFAULTS) - OPTIONAL
    _check(not unknown, f"unknown keys {sorted(unknown)}")
    missing = [key for key in REQUIRED if key not in item]
    _check(not missing, "missing " + ", ".join(missing))
    scenario = copy.deepcopy(DEFAULTS)
    scenario.update(copy.deepcopy(item))
    scenario.setdefault("canned_set", scenario["id"])
    for key in ("id", "canned_set"):
        _check(isinstance(scenario[key], str) and _NAME.match(scenario[key]), f"{key} must be a plain name")
    _check(scenario["group"] in GROUPS, "group must be one of " + ", ".join(GROUPS))
    environments = scenario["environments"]
    _check(isinstance(environments, list) and environments and all(e in ENVIRONMENTS for e in environments),
           "environments must be a non-empty subset of " + ", ".join(ENVIRONMENTS))
    _check(isinstance(scenario["prompt"], str) and scenario["prompt"].strip(), "prompt must be text")
    validate_expect(scenario["expect"])
    timeout = scenario["timeout_seconds"]
    _check(not isinstance(timeout, bool) and isinstance(timeout, (int, float)) and math.isfinite(timeout)
           and timeout > 0, "timeout_seconds must be a positive number")
    cap = scenario["max_tool_calls"]
    _check(not isinstance(cap, bool) and isinstance(cap, int) and cap > 0, "max_tool_calls must be a positive integer")
    for key in ("canned", "captures", "allow_real", "requires"):
        _names(scenario[key], key)
    _cli_words(scenario["cli_canned"])
    # Cloud scenarios need Cloud access, which only a cloud:// target has (target_capabilities).
    _check(scenario["group"] != "cloud" or "cloud" in scenario["requires"],
           'a cloud-group scenario must list "cloud" in requires')
    _check(isinstance(scenario["files"], dict) and all(isinstance(v, str) for v in scenario["files"].values()),
           "files must map names to text")
    for name in scenario["files"]:
        _relative(name)
    _check(scenario["vm_profile"] in VM_PROFILES, "vm_profile must be one of " + ", ".join(VM_PROFILES))
    return scenario


def load_scenarios(path):
    data = json.loads(Path(path).read_text())
    _check(isinstance(data, list), "the scenario file must hold a list of scenarios")
    scenarios, seen = [], set()
    for index, item in enumerate(data):
        label = item.get("id", f"#{index}") if isinstance(item, dict) else f"#{index}"
        try:
            scenario = _scenario(item)
        except ValueError as error:
            raise ValueError(f"scenario {label}: {error}") from None
        _check(scenario["id"] not in seen, f"duplicate scenario id {scenario['id']!r}")
        seen.add(scenario["id"])
        scenarios.append(scenario)
    return scenarios


def robot_agent_binary(config):
    """The ARM64 agent a go2 or g1 run VM installs: config agent_binary, else this checkout's build."""
    return config.get("agent_binary") or str(b.ROOT / "go/bin/wendy-agent-linux-arm64")


def robot_prerequisites(config, run=subprocess.run):
    """What a go2 or g1 run VM needs that this host lacks ([] when nothing). Read-only and fast."""
    missing = []
    binary = robot_agent_binary(config)
    if not os.path.isfile(binary):
        missing.append(f"agent binary {binary}")
    try:  # `vm robot start` builds the robot runtime image with buildx
        buildx = run(["docker", "buildx", "version"], stdin=subprocess.DEVNULL, capture_output=True,
                     timeout=20).returncode == 0
    except (OSError, subprocess.SubprocessError):
        buildx = False
    if not buildx:
        missing.append("docker buildx")
    return missing


def scenario_readiness(scenario, target, canned_dir, prerequisites=None):
    """None when the scenario can run on target, else the reason it is unconfigured.

    prerequisites() lists what a go2 or g1 run VM lacks on this host (default: robot_prerequisites with
    no config). An invalid capture raises ValueError here, before a run, rather than failing an attempt.
    """
    required = set(scenario["requires"])
    if required:
        capabilities = target_capabilities(target)
        if capabilities is None:
            return "capabilities not checked (add --execute)"
        missing = sorted(required - set(capabilities))
        if missing:
            return "target capabilities required: " + ", ".join(missing)
    reasons = []
    if scenario["vm_profile"] != "generic" and target.get("kind") == "simulator":
        missing = (prerequisites or (lambda: robot_prerequisites({})))()
        if missing:
            reasons.append("robot VM prerequisites missing: " + ", ".join(missing))
    uncaptured = []
    for tool in scenario["captures"]:
        try:
            capture = load_capture(canned_dir, scenario["canned_set"], tool)
            _check(capture is None or isinstance(capture, dict), "a capture must be an object")
            replies = None if capture is None else capture.get("replies")
            _check(replies is None or (isinstance(replies, list) and all(isinstance(r, dict) for r in replies)),
                   "replies must be a list of objects")
        except ValueError as error:
            raise ValueError(f"scenario {scenario['id']}: invalid capture for {tool}: {error!r}") from None
        if not replies:
            uncaptured.append(tool)
    if uncaptured:
        reasons.append("canned replies not captured: " + ", ".join(uncaptured))
    try:
        cli_missing = [" ".join(words) for words in scenario["cli_canned"]
                       if load_cli_capture(canned_dir, scenario["canned_set"], words) is None]
    except ValueError as error:
        raise ValueError(f"scenario {scenario['id']}: invalid CLI capture: {error!r}") from None
    if cli_missing:
        reasons.append("CLI replies not captured: " + ", ".join(cli_missing))
    return "; ".join(reasons) or None


def scenario_coverage(scenarios, targets, selected, selected_targets, canned_dir, prerequisites=None):
    coverage = []
    prerequisites = functools.cache(prerequisites or (lambda: robot_prerequisites({})))  # one check per plan
    for scenario in scenarios:
        if scenario["id"] not in selected:
            continue
        matching = [name for name in selected_targets if targets[name]["kind"] in scenario["environments"]]
        if not matching:
            coverage.append({"task": scenario["id"], "kind": "scenario", "target": None,
                             "status": "unconfigured", "reason": "no matching target configured"})
        for name in matching:
            reason = scenario_readiness(scenario, targets[name], canned_dir_for(targets[name], canned_dir), prerequisites)
            coverage.append({"task": scenario["id"], "kind": "scenario", "target": name,
                             "status": "unconfigured" if reason else "ready", "reason": reason})
    return coverage


def target_capabilities(target):
    """The target's capability tags, or None when a board's are still unread.

    "cloud" (Cloud access) comes only from a cloud:// selector, never from a list: a LAN board or the
    simulator never has it.
    """
    capabilities = target.get("capabilities", [])
    if capabilities is None:
        return None
    tags = set(capabilities) - {"cloud"}
    if target.get("kind") == "cloud" and str(target.get("device") or "").startswith("cloud://"):
        tags.add("cloud")
    return tags


def board_target(selector):
    return {"kind": "cloud" if selector.startswith("cloud://") else "physical", "device": selector,
            "capabilities": None, "fixtures": {}, "cache_state": "spare board named with --device"}


def _capability_payload(result):
    if not isinstance(result, dict):
        return {}
    if isinstance(result.get("structuredContent"), dict):
        return result["structuredContent"]
    if "capabilities" in result:
        return result
    for item in result.get("content") or []:
        if isinstance(item, dict) and item.get("type") == "text":
            try:
                parsed = json.loads(item.get("text") or "")
            except (TypeError, ValueError):
                continue
            if isinstance(parsed, dict):
                return parsed
    return {}


def detect_capabilities(result):
    """Map a hardware_capabilities result to the tags scenarios require: wifi, bluetooth, camera.

    A camera counts only on a usb or csi transport and not on a codec/ISP driver. The agent reports
    no wifi or bluetooth category, so those come from evidence inside other categories: a network interface named wl* (the kernel's wlanN and systemd's wlp*/wlx* names
    for WLAN), a USB device whose product says Bluetooth, or a PipeWire bluez_* audio node.
    """
    if not isinstance(result, dict) or result.get("isError") is True:
        return []
    capabilities = _capability_payload(result).get("capabilities")
    tags = set()
    for cap in capabilities if isinstance(capabilities, list) else []:
        if not isinstance(cap, dict):
            continue
        category = str(cap.get("category") or "").strip().lower()
        path, description = str(cap.get("device_path") or ""), str(cap.get("description") or "")
        interface = os.path.basename(path) or (description.split() or [""])[0]
        if CATEGORY_TAGS.get(category) == "camera":
            if _is_camera(cap):
                tags.add("camera")
        elif category in CATEGORY_TAGS:
            tags.add(CATEGORY_TAGS[category])
        elif category == "network" and interface.startswith("wl"):
            tags.add("wifi")
        elif category == "usb" and "bluetooth" in description.lower():
            tags.add("bluetooth")
        elif category == "audio" and path.startswith("bluez_"):
            tags.add("bluetooth")
    return sorted(tags)


def _is_camera(cap):
    properties = cap.get("properties") if isinstance(cap.get("properties"), dict) else {}
    driver = str(properties.get("driver") or "").lower()
    return (str(properties.get("transport") or "").lower() in CAMERA_TRANSPORTS
            and not driver.startswith(NON_CAMERA_DRIVERS))


def is_local(target):
    """True for the target where the agent runs on the device it manages, over WENDY_AGENT_SOCKET."""
    return target.get("kind") == "local"


def canned_dir_for(target, default=CANNED):
    """The target's own canned replies (relative to this directory), else default."""
    return HERE / target["canned_dir"] if target.get("canned_dir") else default


def private_env(state, keep_socket=False):
    env = dict(os.environ, WENDY_ANALYTICS="false", WENDY_CONFIG_DIR=str(state), WENDY_SECRET_STORE="file")
    # A read without --device must not fall back to the developer's device, nor to an agent socket
    # unless the target is the device this process runs on.
    env.pop("WENDY_DEVICE", None)
    if not keep_socket:
        env.pop("WENDY_AGENT_SOCKET", None)
    return env


def detect_board(wendy, target, timeout=120):
    """Read a board's capabilities in a throwaway private state. Returns (tags or None, evidence)."""
    state = b.isolated_device_state(target)
    evidence = {"device": target["device"]}
    try:
        with StdioMCP([wendy, "mcp", "serve"], env=private_env(state, keep_socket=is_local(target)), timeout=timeout) as mcp:
            mcp.initialize()
            evidence["device_connect"] = mcp.call("device_connect", {"device": target["device"]})
            if evidence["device_connect"].get("isError"):
                return None, evidence
            evidence["hardware_capabilities"] = mcp.call("hardware_capabilities", {})
        if evidence["hardware_capabilities"].get("isError"):
            return None, evidence
        tags = detect_capabilities(evidence["hardware_capabilities"])
        if target["device"].startswith("cloud://"):
            tags = sorted(set(tags) | {"cloud"})  # connected through Wendy Cloud, so Cloud access works
        evidence["capabilities"] = tags
        return tags, evidence
    except (OSError, RuntimeError, TimeoutError, ValueError) as error:
        evidence["error"] = repr(error)
        return None, evidence
    finally:
        shutil.rmtree(state, ignore_errors=True)


class ToolCapGuard:
    """A run_process progress guard: stop once the proxy and wrapper logs hold more than `limit` calls."""

    def __init__(self, proxy_log, cli_log, limit):
        if isinstance(limit, bool) or not isinstance(limit, int) or limit < 1:
            raise ValueError("max_tool_calls must be a positive integer")
        self.paths, self.limit = (Path(proxy_log), Path(cli_log)), limit

    def __call__(self):
        calls = 0
        for path in self.paths:
            try:
                calls += len(read_jsonl(path))
            except OSError:
                pass  # not written yet
        return {"kind": "tool_call_cap", "calls": calls, "limit": self.limit} if calls > self.limit else None


def first_reason(*guards):
    def check():
        for guard in guards:
            reason = guard()
            if reason:
                return reason
        return None
    return check


def _run_vm_dir(output, profile):
    directory = Path(output) / f"run-vm-{profile}"
    directory.mkdir(parents=True, exist_ok=True)
    return directory


def _robot_vm(wendy, name, profile, directory, env, agent_binary):
    # The same commands and readiness check as robot_diagnostics.prepare, with the ARM64 agent the
    # robot tasks in tasks.json install (robot_agent_binary).
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    stages = {}
    for phase, argv, timeout in robot_diagnostics.robot_vm_commands(wendy, name, profile, agent_binary, port):
        stages[phase] = b.run_process(argv, directory, directory / phase, timeout, env)
        if stages[phase]["exit_code"] != 0:
            return stages, False

    def cli(args, *argv, timeout=60):
        done = subprocess.run([args.wendy, *argv], env=env, stdin=subprocess.DEVNULL,
                              capture_output=True, text=True, timeout=timeout)
        if done.returncode:
            raise RuntimeError(done.stderr + done.stdout)
        return done.stdout

    args = SimpleNamespace(wendy=wendy, vm_name=name, task=f"create-{profile}-simulator")
    try:
        with (directory / "vm-robot-verify.stdout").open("w") as out, contextlib.redirect_stdout(out):
            robot_fixtures.verify(args, cli)
    except (RuntimeError, ValueError, KeyError, OSError, subprocess.SubprocessError) as error:
        stages["vm-robot-verify"] = {"exit_code": 1, "error": str(error)}
        return stages, False
    stages["vm-robot-verify"] = {"exit_code": 0}
    return stages, True


def prepare_run_vm(config, wendy, output, profile, register=None):
    """Create the run's shared VM for one profile, in its own private state (never the host's ~/.wendy).

    `register(run_vm)` is called before anything is created, so its caller can remove the VM even if
    preparation is interrupted; without it, a failure here removes the VM before re-raising.
    """
    if profile not in VM_PROFILES:
        raise ValueError(f"unknown vm_profile {profile!r}")
    directory = _run_vm_dir(output, profile)
    state = b.isolated_device_state({"kind": "simulator"})
    name = "wendy-eval-" + uuid.uuid4().hex[:12]
    run_vm = {"state": state, "name": name, "device": "vm:" + name, "profile": profile, "stages": {}, "ready": False}
    if register is not None:
        register(run_vm)
    try:
        env = private_env(state)
        if profile == "generic":
            stages, ready = b.fixture_vm(config, {"wendy": wendy, "vm_name": name}, directory, directory, env)
        else:
            stages, ready = _robot_vm(wendy, name, profile, directory, env, robot_agent_binary(config))
    except BaseException:
        if register is None:
            remove_run_vm(run_vm, wendy, output)
        raise
    run_vm.update(stages=stages, ready=ready)
    b.dump(directory / "prepare.json", {**run_vm, "state": str(state)})
    return run_vm


def remove_run_vm(run_vm, wendy, output):
    directory, state = _run_vm_dir(output, run_vm.get("profile", "generic")), Path(run_vm["state"])
    try:
        result = b.remove_fixture_vm({"wendy": wendy, "vm_name": run_vm["name"]}, directory, directory,
                                     private_env(state))
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        result = {"exit_code": None, "error": repr(error)}
    ok = result.get("exit_code") == 0
    if state.exists():
        b.discard_device_state(state, ok)
    if not ok:
        result["device_state_retained"] = str(state)
    return result


def unready_reason(run_vm):
    """Why a prepared run VM is not ready: its first failed stage."""
    for phase, stage in (run_vm.get("stages") or {}).items():
        if isinstance(stage, dict) and stage.get("exit_code") != 0:
            return f"{phase} failed" + (f": {stage['error']}" if stage.get("error") else f" (exit {stage.get('exit_code')})")
    return "not ready"


def attempt_state(target, run_vm):
    """A fresh private Wendy state; with a run VM, its VM store is the shared one, by symlink."""
    state = b.isolated_device_state(target)
    if run_vm is not None:
        (state / "vms").symlink_to(Path(run_vm["state"]) / "vms", target_is_directory=True)
    return state


def vm_snapshot(wendy, device, env):
    done = subprocess.run([wendy, "--json", "--device", device, "device", "apps", "list"], env=env,
                          stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=120)
    if done.returncode != 0:
        raise RuntimeError(f"listing apps on {device} failed: {(done.stderr or done.stdout).strip()}")
    apps = json.loads(done.stdout)
    if not isinstance(apps, list):
        raise RuntimeError(f"unexpected app list from {device}")
    entries = [{"name": app.get("name"), "version": app.get("version"), "state": app.get("runningState")}
               for app in apps if isinstance(app, dict)]
    return sorted(entries, key=lambda entry: json.dumps(entry, sort_keys=True))


# The benchmark's own app on a disposable device; a sweep never removes it.
BENCHMARK_APP = "sh.wendy.evals.model-tool-report"
SWEEP_ERRORS = (OSError, RuntimeError, ValueError, subprocess.SubprocessError)


def list_apps(wendy, env, run=subprocess.run):
    """Names of the apps on the local device. Raises RuntimeError when the list can't be read."""
    done = run([wendy, "--json", "--device", "local", "device", "apps", "list"], env=env,
               stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=120)
    if done.returncode != 0:
        raise RuntimeError(f"listing apps failed: {(done.stderr or done.stdout).strip()}")
    apps = json.loads(done.stdout)
    if not isinstance(apps, list):
        raise RuntimeError("unexpected app list")
    return [app["name"] for app in apps if isinstance(app, dict) and app.get("name")]


def baseline_apps(path, wendy, env, run=subprocess.run):
    """The apps present when the pass first started. A later start keeps the saved list, so an app
    left over from before the restart is still swept."""
    path = Path(path)
    if path.exists():
        try:
            saved = json.loads(path.read_text())
        except (OSError, ValueError):  # a torn write: no sweep has trusted it yet, so list the apps again
            saved = None
        if isinstance(saved, list) and all(isinstance(name, str) for name in saved):
            return saved
    names = list_apps(wendy, env, run)
    b.dump_atomic(path, names)
    return names


def sweep_apps(wendy, baseline, protected, env, run=subprocess.run):
    """Remove every app that was not there at the start, except the protected one(s). A model can pick its own app id."""
    keep = set(baseline) | ({protected} if isinstance(protected, str) else set(protected))
    result = {"removed": [], "failed": [], "list_error": None}
    try:
        names = list_apps(wendy, env, run)
    except SWEEP_ERRORS as error:
        result["list_error"] = str(error)
        return result
    for name in names:
        if name in keep:
            continue
        try:
            done = run([wendy, "--device", "local", "device", "apps", "remove", name, "--force", "--cleanup",
                        "--delete-volumes"], env=env, stdin=subprocess.DEVNULL, capture_output=True, text=True,
                       timeout=300)
            ok = done.returncode == 0
        except SWEEP_ERRORS:
            ok = False
        result["removed" if ok else "failed"].append(name)
    return result


def _target_snapshot(wendy, device, target, run_vm):
    # The runner's own state, never the attempt's, so the check leaves nothing for the agent to find.
    if run_vm is not None:
        return vm_snapshot(wendy, device, private_env(run_vm["state"]))
    state = b.isolated_device_state(target)
    try:
        return vm_snapshot(wendy, device, private_env(state, keep_socket=is_local(target)))
    finally:
        shutil.rmtree(state, ignore_errors=True)


GUARDED_TOOLS = ("os_update", "device_update_agent", "wifi_connect", "wifi_disconnect", "provisioning_start",
                 "cloud_enroll_device", "os_install_start", "os_install_resume")
# Every CLI verb that changes the OS, the agent, the network connection or enrolment. `wendy cloud device`
# mirrors `wendy device`, so each device verb is guarded under both. Reads (status, list, scan) stay open.
GUARDED_DEVICE_CLI = ("update", "push-agent", "setup", "enroll", "unenroll", "wifi connect", "wifi disconnect",
                      "wifi forget", "wifi rank", "wifi regulatory set-country")
GUARDED_OTHER_CLI = ("os update", "update", "cloud enroll-device", "net configure")
GUARDED_APP_VERBS = ("start", "stop", "remove")
GUARDED_CLI = tuple(f"{prefix}{verb}" for prefix in ("device ", "cloud device ") for verb in GUARDED_DEVICE_CLI
                    ) + GUARDED_OTHER_CLI
# `install` and its `os install` twin flash a drive, but two of their subcommands only read, like MCP's
# os_install_plan and os_install_status, which stay open: `plan` and `jobs status`.
GUARDED_INSTALL_CLI = ("os install", "install")
INSTALL_READS = r"(?:plan|jobs\s+status)"


def guard_matchers(app_id):
    """Block matchers for actions that would end a pass on the device it runs on: starting, stopping or
    removing the benchmark's own app, and changing the OS, agent, network or enrolment."""
    own = {"args": {"app_name": app_id}, "block": True}
    matchers = [{"tool": tool, **own} for tool in ("container_stop", "container_delete", "container_start")]
    matchers += [{"tool": tool, "block": True} for tool in GUARDED_TOOLS]
    matchers += [{"cli": f"{prefix}apps {verb}", "args_regex": re.escape(app_id), "block": True}
                 for prefix in ("device ", "cloud device ") for verb in GUARDED_APP_VERBS]
    matchers += [{"cli": cli, "block": True} for cli in GUARDED_CLI]
    matchers += [{"cli": cli, "args_regex": rf"^(?!.*(?:^|\s){re.escape(cli)}\s+{INSTALL_READS}(?:\s|$))",
                  "block": True} for cli in GUARDED_INSTALL_CLI]
    return matchers


def write_scenario_config(path, *, real_wendy, shim, tools_path, canned_dir, canned_set, canned, allow_real, block,
                          cli_canned, device, vm_name, proxy_log, cli_log, passthrough=False):
    """The file the shim and the recording proxy read through WENDY_EVAL_SCENARIO."""
    b.dump(path, {
        "real_wendy": real_wendy, "tools": str(tools_path), "canned_dir": str(canned_dir),
        "canned_set": canned_set, "canned": canned, "allow_real": allow_real, "block": block,
        "cli_canned": cli_canned, "context": {"device": device, "vm_name": vm_name},
        "proxy_log": str(proxy_log), "cli_log": str(cli_log), "cli_safe": str(CLI_SAFE), "shim": shim,
        "passthrough": passthrough})


def _tmp_root():
    return "/private/tmp" if sys.platform == "darwin" else "/tmp"


def _row(config, agent_name, target_name, scenario, ident):
    return {"id": ident, "kind": "scenario", "agent": agent_name, "target": target_name, "task": scenario["id"],
            "group": scenario["group"], "target_config": config["targets"][target_name],
            "model_requested": config["agents"][agent_name]["model"], "workspace": None,
            "device_state_isolated": True, "device_selector": None, "agent_timeout_seconds": None,
            "max_tool_calls": scenario["max_tool_calls"], "human_messages_after_start": 0, "human_approvals": 0,
            "success": False, "status": "setup_error", "task_seconds": 0, "metrics": {}, "stages": {},
            "tool_choice": None}


def unprepared_attempt(config, agent_name, target_name, scenario, output, reason):
    """A setup_error attempt that runs nothing, for a scenario whose shared run VM failed to prepare."""
    row = _row(config, agent_name, target_name, scenario, uuid.uuid4().hex[:12])
    row.update(stages={"run-vm": {"exit_code": 1, "error": reason}}, cleanup_ok=True)
    directory = Path(output) / f"{agent_name}-{target_name}-{scenario['id']}-{row['id']}"
    directory.mkdir()
    b.dump(directory / "result.json", row)
    return row


def scenario_trial(config, agent_name, target_name, scenario, output, real_wendy, shim, run_vm, tools_path,
                   canned_dir, treatment_root=None):
    treatment_root = treatment_root or b.ROOT
    agent, target = config["agents"][agent_name], config["targets"][target_name]
    if (target["kind"] == "simulator") != (run_vm is not None):
        raise ValueError("simulator scenarios run on the shared run VM, and board scenarios never do")
    adapter = agent["adapter"]
    real_wendy, shim = os.path.abspath(real_wendy), os.path.abspath(shim)
    ident = uuid.uuid4().hex[:12]
    directory = Path(output) / f"{agent_name}-{target_name}-{scenario['id']}-{ident}"
    directory.mkdir()
    workspace = Path(tempfile.mkdtemp(prefix="wendy-eval-"))
    device = run_vm["device"] if run_vm is not None else target["device"]
    scenario_config = directory / "scenario.json"
    row = dict(_row(config, agent_name, target_name, scenario, ident), workspace=str(workspace), device_selector=device)
    if b.disposable(config):
        row["device_markers"] = b.device_markers()
    state = logs = before = None
    try:
        # Everything that holds credentials or evidence is created in here, so the finally always
        # discards the state (and any copied Cloud auth) and keeps the logs.
        # The logs start outside the results tree: Codex's sandboxed shell may write only to its
        # workspace, its added dirs and /tmp, and the wrapper logs from inside that shell.
        logs, proxy_log, cli_log = b.start_attempt_logs()
        state = attempt_state(target, run_vm)
        row["agent_timeout_seconds"] = b.agent_timeout(config, scenario)
        variables = {"root": b.ROOT, "here": HERE, "treatment_root": treatment_root,
                     "plugin_dir": directory / "plugin", "workspace": workspace, "wendy": shim, "wendy_home": state,
                     "wendy_vm_home": run_vm["state"] if run_vm is not None else state,
                     "scenario_config": scenario_config,
                     "wendy_build_cache": Path.home() / ".cache/wendy",
                     "docker_buildx_cache": Path(os.environ.get("DOCKER_CONFIG", Path.home() / ".docker")) / "buildx",
                     "wendy_cache": (Path.home() / "Library/Caches/wendy" if sys.platform == "darwin" else
                                     Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "wendy"),
                     "device": device, "run_id": ident, "app_id": f"dev.wendy.eval.{ident}",
                     "vm_name": run_vm["name"] if run_vm is not None else f"wendy-eval-{ident}",
                     "port": config.get("port", 18765), "model": agent["model"],
                     "evidence_dir": directory, "agent_events": directory / "agent.stdout",
                     "target_config": directory / "target.json"}
        b.dump(directory / "target.json", target)
        prompt = b.expand(scenario["prompt"], b.prompt_variables(variables, target))
        prompt += f"\nThe Wendy executable for this environment is {shim}. Use that absolute path for Wendy CLI commands."
        if config.get("environment_context"):
            prompt += "\n" + b.expand(config["environment_context"], b.prompt_variables(variables, target))
        prompt += (f"\nWendy device/cloud configuration and its VM store are isolated at {state} "
                   "through WENDY_CONFIG_DIR. Preserve that environment setting. Do not change the host's ~/.wendy configuration or identity pins.")
        variables["prompt"] = prompt
        b.dump(directory / "task.json", {**scenario, "prompt": prompt, "agent": agent_name, "target": target_name})
        (directory / "prompt.txt").write_text(prompt + "\n")
        expect = scenario["expect"]
        block = [m for m in expect.get("right", []) + expect.get("wrong", []) if m.get("block") is True]
        if is_local(target):
            block += guard_matchers(os.environ.get("WENDY_APP_ID", BENCHMARK_APP))
        write_scenario_config(
            scenario_config, real_wendy=real_wendy, shim=shim, tools_path=tools_path, canned_dir=canned_dir,
            canned_set=scenario["canned_set"], canned=scenario["canned"], allow_real=scenario["allow_real"],
            block=block, cli_canned=scenario["cli_canned"], device=device,
            vm_name=run_vm["name"] if run_vm is not None else "", proxy_log=proxy_log, cli_log=cli_log)
        env = private_env(state, keep_socket=is_local(target))
        env["WENDY_EVAL_SCENARIO"] = str(scenario_config)
        env["PATH"] = str(Path(shim).parent) + os.pathsep + env.get("PATH", "")
        try:
            before = _target_snapshot(real_wendy, device, target, run_vm)
        except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
            row["stages"]["reset"] = {"exit_code": 1, "error": f"reading the target before the attempt: {error}"}
            return row
        for name, content in scenario["files"].items():
            path = workspace / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(b.expand(content, variables))
        b.stage_context(workspace, adapter, shim, state, treatment_root,
                        server_env={"WENDY_EVAL_SCENARIO": str(scenario_config)})
        if adapter == "claude":
            b.stage_plugin(treatment_root, directory / "plugin")
        guard = first_reason(RetryGuard(directory / "agent.stdout", config.get("syntax_error_limit", 3)),
                             ToolCapGuard(proxy_log, cli_log, scenario["max_tool_calls"]))
        result = b.run_process(b.expand(agent["command"], variables), workspace, directory / "agent",
                               row["agent_timeout_seconds"], env, b.host_pin_guard(), guard)
        row["stages"]["agent"] = result
        row["metrics"], events = b.agent_metrics(adapter, directory / "agent.stdout", result,
                                                 agent.get("prices_per_million"))
        trace = b.rebuild_trace(row["metrics"], adapter, events, proxy_log, cli_log)
        row["task_seconds"] = result["seconds"]
        row["tool_choice"] = grade(trace, expect, row["metrics"]["final_text"], row["metrics"]["agent_completed"],
                                   group=scenario["group"], adapter=adapter, went_around=is_local(target))
        row["status"], row["success"] = b.scenario_verdict(adapter, row["metrics"], result, row["tool_choice"])
        infra = b.infra_status(events, config) if b.disposable(config) else None
        if infra:
            row["status"], row["success"] = infra, False
    finally:
        if before is not None:
            # The shared VM or board must be back in its prepared state; anything else stops the run.
            try:
                after = _target_snapshot(real_wendy, device, target, run_vm)
                row["stages"]["reset"] = {"exit_code": 0 if after == before else 1, "before": before, "after": after}
            except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
                row["stages"]["reset"] = {"exit_code": 1, "before": before,
                                          "error": f"reading the target after the attempt: {error}"}
        # On a disposable device the sweep owns leftover apps; the reset stage still records the change.
        row["cleanup_ok"] = (row["stages"].get("reset", {}).get("exit_code") == 0
                             or (b.disposable(config) and "after" in row["stages"].get("reset", {})))
        if state is not None:
            b.discard_device_state(state, row["cleanup_ok"])  # unlinks the shared store; the VM stays
            if state.exists() and not any(state.iterdir()):
                state.rmdir()  # nothing of this attempt's own is worth keeping
            if state.exists():
                row["device_state_retained"] = str(state)
        if logs is not None:
            b.keep_attempt_logs(logs, directory)
        b.dump(directory / "result.json", row)
        # Keep the workspace for diagnosis; it contains only this attempt's files.
    return row
