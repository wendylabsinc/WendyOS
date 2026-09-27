#!/usr/bin/env python3
"""Measure one-prompt Wendy tasks. Python standard library only."""

import argparse
import collections
import hashlib
import json
import math
import os
from pathlib import Path
import random
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import uuid

from retry_guard import RetryGuard

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
TOKEN_FIELDS = ("input_tokens", "cached_input_tokens", "cache_write_tokens",
                "output_tokens", "reasoning_tokens")


def dump(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def expand(value, variables):
    if isinstance(value, str):
        for name, replacement in variables.items():
            value = value.replace("{" + name + "}", str(replacement))
        return value
    if isinstance(value, list):
        return [expand(v, variables) for v in value]
    if isinstance(value, dict):
        return {k: expand(v, variables) for k, v in value.items()}
    return value


def run_process(argv, cwd, prefix, timeout, env=None, state_guard=None, progress_guard=None):
    """Keep raw logs, bound wall time, and stop the process group on exit."""
    start = time.monotonic()
    result = {"argv": argv, "exit_code": None, "timed_out": False, "interrupted": False,
              "timeout_seconds": timeout}
    process = None
    with prefix.with_suffix(".stdout").open("wb") as out, prefix.with_suffix(".stderr").open("wb") as err:
        try:
            process = subprocess.Popen(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                                       stdout=out, stderr=err, start_new_session=True)
            try:
                if state_guard is None and progress_guard is None:
                    result["exit_code"] = process.wait(timeout=timeout)
                else:
                    while True:
                        if state_guard is not None and not state_guard():
                            result["host_state_changed"] = True
                            break
                        if progress_guard is not None and (reason := progress_guard()):
                            result["no_progress"] = reason
                            break
                        remaining = timeout - (time.monotonic() - start)
                        if remaining <= 0:
                            raise subprocess.TimeoutExpired(argv, timeout)
                        try:
                            result["exit_code"] = process.wait(timeout=min(.2, remaining))
                            if state_guard is not None and not state_guard():
                                result["host_state_changed"] = True
                            if progress_guard is not None and (reason := progress_guard()):
                                result["no_progress"] = reason
                            break
                        except subprocess.TimeoutExpired:
                            continue
            except subprocess.TimeoutExpired:
                result["timed_out"] = True
            except KeyboardInterrupt:
                result["interrupted"] = True
        except OSError as exc:
            result["error"] = str(exc)
        finally:
            if process is not None:
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    pass
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait()
    result["seconds"] = round(time.monotonic() - start, 3)
    return result


def read_events(path):
    events, invalid = [], 0
    for line in path.read_text(errors="replace").splitlines():
        if not line.strip():
            continue
        try:
            value = json.loads(line)
            if not isinstance(value, dict):
                raise ValueError("event must be an object")
            events.append(value)
        except ValueError:
            invalid += 1
    return events, invalid


def normalize_usage(raw, anthropic=False):
    if not isinstance(raw, dict) or "input_tokens" not in raw or "output_tokens" not in raw:
        return None
    result = {key: raw.get(key, 0) for key in TOKEN_FIELDS}
    result["cache_write_tokens"] = raw.get("cache_write_tokens", raw.get("cache_write_input_tokens", 0))
    if anthropic:
        result["cached_input_tokens"] = raw.get("cache_read_input_tokens", 0)
        result["cache_write_tokens"] = raw.get("cache_creation_input_tokens", 0)
    result["reasoning_tokens"] = raw.get("reasoning_tokens", raw.get("reasoning_output_tokens", 0))
    if any(isinstance(v, bool) or not isinstance(v, int) or v < 0 for v in result.values()):
        return None
    if anthropic:
        result["input_tokens"] += result["cached_input_tokens"] + result["cache_write_tokens"]
    if result["cached_input_tokens"] + result["cache_write_tokens"] > result["input_tokens"]:
        return None
    return result


def metrics(adapter, events):
    usage = []
    complete, ended, errors = True, False, 0
    tools, denied = set(), set()
    reported_cost = None
    models = set()
    text = []
    for index, event in enumerate(events):
        kind = event.get("type")
        if adapter == "codex":
            item = event.get("item", {})
            if kind in ("item.started", "item.completed") and item.get("type") in (
                    "command_execution", "mcp_tool_call", "web_search", "file_change"):
                tools.add(item.get("id", str(index)))
            if kind == "item.completed" and item.get("type") == "agent_message":
                text.append(item.get("text", ""))
            if kind == "turn.completed":
                ended = True
                usage.append(normalize_usage(event.get("usage")))
            errors += kind in ("turn.failed", "error")
        elif adapter == "claude":
            if kind == "assistant":
                message = event.get("message", {})
                if message.get("model"):
                    models.add(message["model"])
                for block in message.get("content", []):
                    if block.get("type") == "tool_use":
                        tools.add(block["id"])
            if kind == "result":
                ended = True
                errors += bool(event.get("is_error"))
                # This is the session total; do not add assistant-message usage.
                usage = [normalize_usage(event.get("usage"), anthropic=True)]
                reported_cost = event.get("total_cost_usd")
                denied.update(d.get("tool_use_id", str(i)) for i, d in enumerate(event.get("permission_denials", [])))
                text = [event.get("result", "")]
        elif adapter == "wendy":
            if kind == "tool_start":
                tools.add((event.get("agent_id", ""), event.get("id", str(index))))
            if kind == "approval" and event.get("approved") is False:
                denied.add((event.get("agent_id", ""), event.get("id", str(index))))
            if kind == "usage":
                raw = event.get("usage", {})
                usage.append(normalize_usage(raw))
                complete = complete and raw.get("complete") is True
                if raw.get("model"):
                    models.add(raw["model"])
            if kind == "done":
                ended = True
                text = [event.get("text", "")]
            if kind == "error":
                ended = True
            errors += kind == "error"
        else:
            raise ValueError(f"unknown adapter: {adapter}")
    known = [u for u in usage if u is not None]
    tokens = {key: sum(u[key] for u in known) for key in TOKEN_FIELDS} if known else None
    return {"tokens": tokens, "usage_complete": bool(usage) and complete and ended and len(known) == len(usage),
            "tool_calls": len(tools), "permission_denials": len(denied) if adapter != "codex" else None,
            "reported_cost_usd": reported_cost, "reported_cost_kind": "client_estimate" if reported_cost is not None else None,
            "models_observed": sorted(models), "agent_completed": ended, "agent_errors": errors,
            "final_text": "\n".join(text)}


def estimate_cost(metric, prices):
    if not prices or not metric["usage_complete"]:
        return None
    if any(isinstance(v, bool) or not isinstance(v, (float, int)) or not math.isfinite(v) or v < 0 for v in prices.values()):
        raise ValueError("prices must be finite nonnegative numbers")
    u = metric["tokens"]
    counts = {"input": u["input_tokens"] - u["cached_input_tokens"] - u["cache_write_tokens"],
              "cached_input": u["cached_input_tokens"], "cache_write": u["cache_write_tokens"],
              "output": u["output_tokens"]}
    if any(count and key not in prices for key, count in counts.items()):
        return None
    return round(sum(count * prices.get(key, 0) for key, count in counts.items()) / 1_000_000, 8)


def summary(rows):
    groups = collections.defaultdict(list)
    for row in rows:
        groups[(row["agent"], row["target"], row["task"])].append(row)
    output = []
    for (agent, target, task), trials in sorted(groups.items()):
        eligible = [r for r in trials if r["status"] != "setup_error"]
        passed = [r for r in eligible if r["success"]]
        times = sorted(r["task_seconds"] for r in passed)
        token_values = [r["metrics"]["tokens"]["input_tokens"] + r["metrics"]["tokens"]["output_tokens"]
                        for r in eligible if r["metrics"]["usage_complete"]]
        all_usage = len(token_values) == len(eligible) and bool(eligible)
        costs = [r["metrics"].get("estimated_cost_usd") if r["metrics"].get("estimated_cost_usd") is not None
                 else r["metrics"].get("reported_cost_usd") for r in eligible]
        all_costs = bool(costs) and all(isinstance(v, (int, float)) and math.isfinite(v) and v >= 0 for v in costs)
        output.append({"agent": agent, "target": target, "task": task,
                       "attempts": len(eligible), "setup_errors": len(trials) - len(eligible),
                       "successes": len(passed), "unassisted_success_rate": len(passed) / len(eligible) if eligible else None,
                       "median_success_seconds": statistics.median(times) if times else None,
                       "p90_success_seconds": times[math.ceil(.9 * len(times)) - 1] if times else None,
                       "mean_tokens_per_attempt": statistics.mean(token_values) if all_usage else None,
                       "tokens_per_success_including_failures": sum(token_values) / len(passed) if passed and all_usage else None,
                       "estimated_usd_per_attempt": statistics.mean(costs) if all_costs else None,
                       "estimated_usd_per_success_including_failures": sum(costs) / len(passed) if passed and all_costs else None,
                       "usage_complete_attempts": len(token_values),
                       "timeouts": sum(r["status"] == "timeout" for r in eligible),
                       "cleanup_errors": sum(not r.get("cleanup_ok", True) for r in trials)})
    return output


def report(rows, output):
    data = summary(rows)
    dump(output / "summary.json", data)
    lines = ["| Agent | Target | Task | Unassisted passes | Median seconds | Tokens / attempt | Tokens / success incl. failures | Estimated $ / success |",
             "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    def fmt(value):
        return "unknown" if value is None else f"{value:,.1f}"
    for row in data:
        lines.append(f"| {row['agent']} | {row['target']} | {row['task']} | {row['successes']}/{row['attempts']} | "
                     f"{fmt(row['median_success_seconds'])} | {fmt(row['mean_tokens_per_attempt'])} | "
                     f"{fmt(row['tokens_per_success_including_failures'])} | "
                     f"{fmt(row['estimated_usd_per_success_including_failures'])} |")
    (output / "summary.md").write_text("\n".join(lines) + "\n")


def stage_context(workspace, adapter, wendy, state_dir=None):
    # The supplied Wendy skills are part of the treatment. The benchmark's task
    # definitions, verifier, previous runs, and parent repository are not copied.
    skills = ROOT / "plugins/wendy-agentic-coding/skills"
    if adapter == "codex":
        shutil.copytree(skills, workspace / ".agents/skills")
    if adapter == "claude":
        server = {"command": wendy, "args": ["mcp", "serve"]}
        if state_dir is not None:
            server["env"] = {"WENDY_CONFIG_DIR": str(state_dir), "WENDY_SECRET_STORE": "file"}
        dump(workspace / "mcp.json", {"mcpServers": {"wendy": server}})


def isolated_device_state(target):
    """Keep credentials outside app build contexts; never copy host device pins."""
    # QEMU's Unix socket path is limited to 104 bytes on macOS. Its default
    # per-user TMPDIR is too long once the VM name and qmp.sock are appended.
    state = Path(tempfile.mkdtemp(prefix="we-state-", dir="/private/tmp" if sys.platform == "darwin" else "/tmp"))
    cfg = {"analytics": {"enabled": False}, "completionInstalled": True, "completionPromptDismissed": True}
    source = Path.home() / ".wendy/config.json"
    if target["kind"] in ("cloud", "physical") and source.exists():
        host = json.loads(source.read_text())
        for name in ("auth", "currentContext", "defaultCloudGRPC", "defaultOrgId", "defaultTenantUUID"):
            if name in host:
                cfg[name] = host[name]
    dump(state / "config.json", cfg)
    (state / "config.json").chmod(0o600)
    return state


def discard_device_state(state, cleanup_ok):
    if cleanup_ok:
        shutil.rmtree(state)
        return
    # Preserve only VM disks/state for recovery. Remove auth, TLS sessions,
    # discovery caches and other host-side state even when VM cleanup fails.
    for child in state.iterdir():
        if child.name == "vms" and child.is_dir() and not child.is_symlink():
            continue
        if child.is_dir() and not child.is_symlink():
            shutil.rmtree(child)
        else:
            child.unlink()


def host_pin_guard(path=None):
    path = path or Path.home() / ".wendy/config.json"
    def pins():
        return json.loads(path.read_text()).get("devicePins", {}) if path.exists() else {}
    before = pins()
    def unchanged():
        try:
            current = pins()
            return all(current.get(key) == value for key, value in before.items())
        except (OSError, ValueError):
            return False
    return unchanged


def require_state_isolation(wendy):
    # Older CLIs silently ignore unknown environment variables. Test that the
    # candidate rejects a relative override before it can touch any host state.
    env = dict(os.environ, WENDY_CONFIG_DIR="relative-isolation-probe", WENDY_ANALYTICS="false")
    probe = subprocess.run([wendy, "--json", "vm", "list"], env=env, capture_output=True, text=True, timeout=20)
    if probe.returncode == 0 or "WENDY_CONFIG_DIR must be an absolute directory" not in probe.stderr:
        raise ValueError("candidate lacks verified WENDY_CONFIG_DIR support; rebuild before using isolated trials")


def needs_fixture_vm(task, target, state_dir):
    return (state_dir is not None and target["kind"] == "simulator" and "simulator" in task.get("environments", [])
            and not task.get("owns_vm"))


def fixture_vm(config, variables, workspace, directory, env):
    """Unscored target preparation for app/telemetry tasks in a private VM store."""
    wendy, name = variables["wendy"], variables["vm_name"]
    stages = {}
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
    commands = [("vm-create", [wendy, "vm", "--yes", "create", name]),
                ("vm-start", [wendy, "vm", "--yes", "start", name, "--detach", "--port", str(port)])]
    if config.get("agent_binary"):
        commands.append(("vm-agent", [wendy, "--device", "vm:" + name, "device", "update", "--binary", config["agent_binary"]]))
    for phase, command in commands:
        stages[phase] = run_process(command, workspace, directory / phase, 900, env)
        if stages[phase]["exit_code"] != 0:
            return stages, False
    return stages, True


def remove_fixture_vm(variables, workspace, directory, env):
    wendy, name = variables["wendy"], variables["vm_name"]
    listing = subprocess.run([wendy, "--json", "vm", "list"], env=env, capture_output=True, text=True, timeout=30)
    if listing.returncode == 0 and not any(vm["name"] == name for vm in json.loads(listing.stdout)):
        return {"exit_code": 0, "seconds": 0}
    return run_process([wendy, "vm", "--yes", "rm", name, "--force"], workspace, directory / "vm-cleanup", 180, env)


def task_for_target(task, target):
    """Require real fixture implementations, never turn absent hardware into a pass."""
    task = dict(task)
    fixture = target.get("fixtures", {}).get(task["id"], {})
    if task.get("fixture") == "external":
        missing = [phase for phase in ("prepare", "verify", "cleanup")
                   if not isinstance(fixture.get(phase), list) or not fixture[phase]
                   or not all(isinstance(arg, str) and arg for arg in fixture[phase])]
        if missing:
            return task, "fixture commands required: " + ", ".join(missing)
        for phase in ("prepare", "verify", "cleanup"):
            task[phase] = fixture[phase]
        if not fixture.get("prompt_context"):
            return task, "fixture prompt_context must identify the workload and permitted resources"
        if task.get("destructive") and not fixture.get("disposable_target"):
            return task, "an explicit disposable_target identity is required"
    if fixture.get("prompt_context"):
        task["prompt"] += "\n" + fixture["prompt_context"]
    required = set(task.get("requires", []))
    missing = required - set(target.get("capabilities", []))
    if missing:
        return task, "target capabilities required: " + ", ".join(sorted(missing))
    return task, None


def plan_coverage(tasks, targets, selected_tasks, selected_targets):
    coverage = []
    for task in tasks:
        if task["id"] not in selected_tasks:
            continue
        matching = [name for name in selected_targets if targets[name]["kind"] in task["environments"]]
        if not matching:
            coverage.append({"task": task["id"], "target": None, "status": "unconfigured",
                             "reason": "no matching target configured"})
        for name in matching:
            _, reason = task_for_target(task, targets[name])
            coverage.append({"task": task["id"], "target": name,
                             "status": "unconfigured" if reason else "ready", "reason": reason})
    return coverage


def coverage_results(coverage, rows, agents, repetitions):
    result = []
    for entry in coverage:
        entry = dict(entry)
        entry["agents"] = {}
        for agent in agents:
            matching = [r for r in rows if (r["agent"], r["target"], r["task"]) ==
                        (agent, entry["target"], entry["task"])]
            attempts = [r for r in matching if r["status"] != "setup_error"]
            entry["agents"][agent] = {"attempts": len(attempts), "passed": sum(r["success"] for r in attempts),
                                      "setup_errors": len(matching) - len(attempts),
                                      "not_run": max(0, repetitions - len(matching))}
        result.append(entry)
    return result


def agent_timeout(config, task):
    values = [task.get("timeout_seconds", 900)]
    if config.get("max_agent_seconds") is not None:
        values.append(config["max_agent_seconds"])
    if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) or v <= 0 for v in values):
        raise ValueError("agent time limits must be finite positive numbers")
    return min(values)


def trial(config, agent_name, target_name, task, output, wendy):
    agent, target = config["agents"][agent_name], config["targets"][target_name]
    ident = uuid.uuid4().hex[:12]
    directory = output / f"{agent_name}-{target_name}-{task['id']}-{ident}"
    directory.mkdir()
    workspace = Path(tempfile.mkdtemp(prefix="wendy-eval-"))
    state_dir = isolated_device_state(target) if config.get("isolate_device_state", True) else None
    variables = {"root": ROOT, "here": HERE, "workspace": workspace, "wendy": wendy,
                 "wendy_home": state_dir or Path.home() / ".wendy",
                 "wendy_build_cache": Path.home() / ".cache/wendy",
                 "docker_buildx_cache": Path(os.environ.get("DOCKER_CONFIG", Path.home() / ".docker")) / "buildx",
                 "wendy_cache": (Path.home() / "Library/Caches/wendy" if sys.platform == "darwin" else
                                 Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "wendy"),
                 "device": target["device"], "run_id": ident, "app_id": f"dev.wendy.eval.{ident}",
                 "vm_name": f"wendy-eval-{ident}", "port": config.get("port", 18765),
                 "model": agent["model"]}
    private_vm = needs_fixture_vm(task, target, state_dir)
    if private_vm or (target["kind"] == "simulator" and task.get("owns_vm")):
        variables["device"] = "vm:" + variables["vm_name"]
    variables.update({"evidence_dir": directory, "agent_events": directory / "agent.stdout",
                      "target_config": directory / "target.json"})
    dump(directory / "target.json", target)
    resolved = expand(task, variables)
    resolved["prompt"] += f"\nThe Wendy executable for this environment is {wendy}. Use that absolute path for Wendy CLI commands."
    if config.get("environment_context"):
        resolved["prompt"] += "\n" + expand(config["environment_context"], variables)
    if state_dir is not None:
        resolved["prompt"] += (f"\nWendy device/cloud configuration and its VM store are isolated at {state_dir} "
                               "through WENDY_CONFIG_DIR. Preserve that environment setting. Do not change the host's ~/.wendy configuration or identity pins.")
    variables["prompt"] = resolved["prompt"]
    row = {"id": ident, "agent": agent_name, "target": target_name, "task": task["id"],
           "target_config": target, "model_requested": agent["model"], "workspace": str(workspace),
           "device_state_isolated": state_dir is not None,
           "device_selector": variables["device"],
           "agent_timeout_seconds": agent_timeout(config, task),
           "human_messages_after_start": 0, "human_approvals": 0, "success": False,
           "status": "setup_error", "task_seconds": 0, "metrics": {}, "stages": {}}
    dump(directory / "task.json", resolved)
    (directory / "prompt.txt").write_text(resolved["prompt"] + "\n")
    env = dict(os.environ, WENDY_ANALYTICS="false")
    if state_dir is not None:
        env["WENDY_CONFIG_DIR"] = str(state_dir)
        env["WENDY_SECRET_STORE"] = "file"
    env["PATH"] = str(Path(wendy).parent) + os.pathsep + env.get("PATH", "")
    guard = host_pin_guard()
    retry_guard = RetryGuard(directory / "agent.stdout", config.get("syntax_error_limit", 3))
    try:
        if private_vm:
            row["stages"], ready = fixture_vm(config, variables, workspace, directory, env)
            if not ready:
                return row
        stage_context(workspace, agent["adapter"], wendy, state_dir)
        for phase in ("prepare", "agent", "verify"):
            if phase == "agent":
                argv = expand(agent["command"], variables)
                timeout = row["agent_timeout_seconds"]
            else:
                argv = resolved[phase]
                timeout = task.get("setup_timeout_seconds", 900) if phase == "prepare" else task.get("verify_timeout_seconds", 90)
            result = run_process(argv, workspace, directory / phase, timeout, env,
                                 guard if phase == "agent" else None,
                                 retry_guard if phase == "agent" else None)
            row["stages"][phase] = result
            if phase == "prepare" and result["exit_code"] != 0:
                return row
            if phase == "agent":
                events, invalid = read_events(directory / "agent.stdout")
                row["metrics"] = metrics(agent["adapter"], events)
                row["metrics"]["invalid_json_lines"] = invalid
                row["metrics"]["usage_complete"] &= not result["timed_out"] and not result["interrupted"] and not result.get("host_state_changed") and not result.get("no_progress") and invalid == 0
                row["metrics"]["estimated_cost_usd"] = estimate_cost(row["metrics"], agent.get("prices_per_million"))
                row["task_seconds"] = result["seconds"]
                row["status"] = ("host_state_changed" if result.get("host_state_changed") else "no_progress"
                                 if result.get("no_progress") else "interrupted"
                                 if result["interrupted"] else "timeout" if result["timed_out"] else "failed")
            if phase == "verify":
                row["task_seconds"] += result["seconds"]
                row["outcome_verified"] = result["exit_code"] == 0
                row["success"] = (row["outcome_verified"] and row["stages"]["agent"]["exit_code"] == 0
                                  and row["metrics"]["agent_completed"] and not row["metrics"]["agent_errors"]
                                  and row["metrics"]["invalid_json_lines"] == 0
                                  and not row["stages"]["agent"].get("no_progress")
                                  and not row["stages"]["agent"].get("host_state_changed"))
                if row["success"]:
                    row["status"] = "passed"
    finally:
        row["stages"]["cleanup"] = run_process(resolved["cleanup"], workspace, directory / "cleanup",
                                               task.get("cleanup_timeout_seconds", 180), env)
        row["cleanup_ok"] = row["stages"]["cleanup"]["exit_code"] == 0
        if private_vm:
            row["stages"]["vm-cleanup"] = remove_fixture_vm(variables, workspace, directory, env)
            row["cleanup_ok"] &= row["stages"]["vm-cleanup"]["exit_code"] == 0
        if state_dir is not None:
            discard_device_state(state_dir, row["cleanup_ok"])
            if not row["cleanup_ok"]:
                row["device_state_retained"] = str(state_dir)
        dump(directory / "result.json", row)
        # Keep the workspace for diagnosis; it contains only this run's fixtures.
    return row


def calibrate(config, tasks, targets, args):
    """Run reference solutions against real fixtures, with no LLM calls."""
    wendy = shutil.which(config.get("wendy", "wendy"))
    if not wendy:
        raise ValueError("Wendy executable not found")
    plan = [(target, task) for task in tasks for target in targets
            if config["targets"][target]["kind"] in task["environments"]]
    if any(task.get("fixture") != "builtin" for _, task in plan):
        raise ValueError("calibration mode currently supports built-in fixtures only")
    if not plan:
        raise ValueError("no calibration tasks match these targets")
    print(json.dumps({"mode": "fixture_calibration", "model_calls": 0,
                      "plan": [{"target": target, "task": task["id"]} for target, task in plan]}, indent=2))
    if not args.execute:
        return 0
    if config.get("isolate_device_state", True):
        require_state_isolation(wendy)
    output = args.output.resolve() / (time.strftime("%Y%m%d-%H%M%S") + "-calibration-" + uuid.uuid4().hex[:6])
    output.mkdir(parents=True)
    rows = []
    for target, task in plan:
        ident = uuid.uuid4().hex[:12]
        workspace = Path(tempfile.mkdtemp(prefix="wendy-eval-calibration-"))
        directory = output / (target + "-" + task["id"])
        directory.mkdir()
        target_config = config["targets"][target]
        state_dir = isolated_device_state(target_config) if config.get("isolate_device_state", True) else None
        variables = {"root": ROOT, "here": HERE, "wendy": wendy, "workspace": workspace,
                     "device": config["targets"][target]["device"], "run_id": ident,
                     "vm_name": "wendy-eval-" + ident,
                     "port": config.get("port", 18765)}
        private_vm = needs_fixture_vm(task, target_config, state_dir)
        if private_vm:
            variables["device"] = "vm:" + variables["vm_name"]
        command = expand(task["prepare"], variables)
        if command[2] != "prepare":
            raise ValueError("unsupported calibration command shape")
        command[2] = "smoke"
        if task["id"] in ("create-go2-simulator", "create-g1-simulator"):
            command += ["--agent-binary", str(ROOT / "go/bin/wendy-agent-linux-arm64")]
        print(f"Calibrating {task['id']} / {target}", flush=True)
        env = dict(os.environ, WENDY_ANALYTICS="false")
        if state_dir is not None:
            env["WENDY_CONFIG_DIR"] = str(state_dir)
            env["WENDY_SECRET_STORE"] = "file"
        stages, ready = fixture_vm(config, variables, workspace, directory, env) if private_vm else ({}, True)
        result = (run_process(command, workspace, directory / "smoke", task.get("setup_timeout_seconds", 900) +
                              task.get("verify_timeout_seconds", 180) + 600, env) if ready else
                  {"exit_code": 1, "seconds": 0, "interrupted": False, "error": "private VM preparation failed"})
        cleanup = run_process(expand(task["cleanup"], variables), workspace, directory / "cleanup", 180, env)
        vm_cleanup = remove_fixture_vm(variables, workspace, directory, env) if private_vm else {"exit_code": 0}
        row = {"mode": "fixture_calibration", "model_calls": 0, "task": task["id"], "target": target,
               "workspace": str(workspace), "result": result, "cleanup": cleanup, "vm_stages": stages,
               "vm_cleanup": vm_cleanup,
               "passed": result["exit_code"] == 0 and cleanup["exit_code"] == 0 and vm_cleanup["exit_code"] == 0}
        if state_dir is not None:
            cleaned = cleanup["exit_code"] == 0 and vm_cleanup["exit_code"] == 0
            discard_device_state(state_dir, cleaned)
            if not cleaned:
                row["device_state_retained"] = str(state_dir)
        rows.append(row)
        dump(directory / "result.json", row)
        dump(output / "calibration.json", rows)
        print(f"  {'passed' if row['passed'] else 'failed'} ({result['seconds']:.1f}s)", flush=True)
        if result["interrupted"] or cleanup["exit_code"] != 0:
            break
    print(f"Calibration evidence: {output}")
    return 0 if len(rows) == len(plan) and all(r["passed"] for r in rows) else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=HERE / "config.example.json")
    parser.add_argument("--tasks", type=Path, default=HERE / "tasks.json")
    parser.add_argument("--agent", action="append")
    parser.add_argument("--target", action="append")
    parser.add_argument("--task", action="append")
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--limit", type=int)
    parser.add_argument("--output", type=Path, default=HERE / "results")
    parser.add_argument("--execute", action="store_true", help="Run the plan. Without this flag, make no model or device calls.")
    parser.add_argument("--calibrate", action="store_true", help="Check built-in fixtures with reference solutions; no model calls.")
    args = parser.parse_args()
    config = json.loads(args.config.read_text())
    tasks = json.loads(args.tasks.read_text())
    if args.repetitions < 1 or (args.limit is not None and args.limit < 1):
        parser.error("repetitions and limit must be positive")
    agents = args.agent or list(config["agents"])
    targets = args.target or list(config["targets"])
    selected_tasks = args.task or [t["id"] for t in tasks]
    for selected, available in ((agents, config["agents"]), (targets, config["targets"]),
                                (selected_tasks, [t["id"] for t in tasks])):
        if any(name not in available for name in selected):
            parser.error("unknown selection: " + str(selected))
    if args.calibrate:
        return calibrate(config, [t for t in tasks if t["id"] in selected_tasks], targets, args)
    # Validate guard settings before creating workspaces or copying credentials.
    RetryGuard(args.output / "validation-only", config.get("syntax_error_limit", 3))
    coverage = plan_coverage(tasks, config["targets"], selected_tasks, targets)
    print(json.dumps({"coverage": coverage}, indent=2))
    plan = [(agent, target, task_for_target(task, config["targets"][target])[0])
            for _ in range(args.repetitions) for task in tasks
            if task["id"] in selected_tasks for target in targets
            if config["targets"][target]["kind"] in task["environments"]
            if task_for_target(task, config["targets"][target])[1] is None for agent in agents]
    random.Random(args.seed).shuffle(plan)
    if args.limit:
        plan = plan[:args.limit]
    print(json.dumps([{"agent": a, "target": t, "task": s["id"],
                       "agent_timeout_seconds": agent_timeout(config, s),
                       "syntax_error_limit": config.get("syntax_error_limit", 3)} for a, t, s in plan], indent=2))
    if not args.execute:
        print(f"Plan only: {len(plan)} trials. Add --execute to run.")
        return 0
    if not plan:
        parser.error("no runnable trials; configure the fixtures reported above")
    for name in agents:
        agent = config["agents"][name]
        if not agent.get("model") or "SET_" in agent["model"]:
            parser.error(f"pin a model for {name} in the config")
        if agent["adapter"] not in ("codex", "claude", "wendy"):
            parser.error(f"unknown adapter for {name}")
    for name in {target for _, target, _ in plan}:
        target = config["targets"][name]
        if target["kind"] not in ("simulator", "cloud", "physical"):
            parser.error(f"unknown environment for {name}")
        expected = {"cloud": "cloud://", "simulator": "vm:", "physical": ""}[target["kind"]]
        if not target.get("device") or not target["device"].startswith(expected) or "SET_" in target["device"]:
            parser.error(f"{name} requires an explicit {expected} selector")
    wendy = shutil.which(config.get("wendy", "wendy"))
    if not wendy:
        parser.error("Wendy executable not found")
    if config.get("isolate_device_state", True):
        require_state_isolation(wendy)
    output = args.output.resolve() / (time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6])
    output.mkdir(parents=True)
    dump(output / "coverage.json", coverage_results(coverage, [], agents, args.repetitions))
    snapshot = {"config": config, "tasks": tasks, "seed": args.seed,
                "config_sha256": hashlib.sha256(args.config.read_bytes()).hexdigest(),
                "task_sha256": hashlib.sha256(args.tasks.read_bytes()).hexdigest()}
    snapshot["runner_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    snapshot["fixtures_sha256"] = hashlib.sha256((HERE / "fixtures.py").read_bytes()).hexdigest()
    snapshot["wendy_binary_sha256"] = hashlib.sha256(Path(wendy).read_bytes()).hexdigest()
    source = output / "source"
    source.mkdir()
    for file in HERE.glob("*.py"):
        shutil.copy2(file, source / file.name)
    if (HERE / "fixture_apps").exists():
        shutil.copytree(HERE / "fixture_apps", source / "fixture_apps", ignore=shutil.ignore_patterns("__pycache__"))
    chat_source = source / "chat"
    chat_source.mkdir()
    for file in (ROOT / "go/internal/cli/chat").glob("*.go"):
        shutil.copy2(file, chat_source / file.name)
    config_source = source / "config"
    config_source.mkdir()
    for file in (ROOT / "go/internal/shared/config").glob("*.go"):
        shutil.copy2(file, config_source / file.name)
    run_process(["git", "diff", "--", "go/internal/cli/chat", "go/internal/shared/config"],
                ROOT, output / "candidate-diff", 15)
    for name, argv in [("wendy", [wendy, "--version"]), ("revision", ["git", "rev-parse", "HEAD"])]:
        snapshot[name] = run_process(argv, ROOT, output / name, 15)
    for name in agents:
        argv = expand(config["agents"][name]["command"], {"wendy": wendy})
        snapshot[name + "_version"] = run_process([argv[0], "--version"], ROOT, output / (name + "-version"), 15)
    dump(output / "manifest.json", snapshot)
    rows = []
    spend = config.get("prior_spend_usd", 0)
    if not isinstance(spend, (int, float)) or not math.isfinite(spend) or spend < 0:
        parser.error("prior_spend_usd must be a finite nonnegative number")
    for i, (agent, target, task) in enumerate(plan, 1):
        if spend + config.get("trial_reserve_usd", 15) > config.get("budget_usd", 100):
            print("Stopping at the configured budget reserve.")
            break
        print(f"[{i}/{len(plan)}] {agent} / {target} / {task['id']} "
              f"(agent limit {agent_timeout(config, task):g}s)", flush=True)
        row = trial(config, agent, target, task, output, wendy)
        rows.append(row)
        with (output / "results.jsonl").open("a") as f:
            f.write(json.dumps(row) + "\n")
        report(rows, output)
        dump(output / "coverage.json", coverage_results(coverage, rows, agents, args.repetitions))
        if any(stage.get("interrupted") for stage in row["stages"].values()):
            print("Experiment interrupted; logs and cleanup result retained.")
            break
        if row["status"] != "setup_error":
            cost = row["metrics"].get("estimated_cost_usd")
            if cost is None:
                cost = row["metrics"].get("reported_cost_usd")
            if cost is None:
                print("Usage cost is unknown. Stopping before spending on another trial.")
                break
            spend += cost
        print(f"  {row['status']} ({row['task_seconds']:.1f}s)", flush=True)
        if not row["cleanup_ok"]:
            print("Cleanup failed. Stopping before another trial can inherit this state.")
            break
    print(f"Results: {output / 'summary.md'}")
    configured = all(entry["status"] == "ready" for entry in coverage)
    return 0 if configured and len(rows) == len(plan) and all(r["success"] and r["cleanup_ok"] for r in rows) else 1


if __name__ == "__main__":
    sys.exit(main())
