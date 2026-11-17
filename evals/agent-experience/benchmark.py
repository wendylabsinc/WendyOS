#!/usr/bin/env python3
"""Measure one-prompt Wendy tasks. Python standard library only."""

import argparse
import atexit
import collections
import hashlib
import http.client
import json
import math
import os
from pathlib import Path
import random
import re
import shutil
import signal
import socket
import statistics
import subprocess
import sys
import tempfile
import time
import urllib.request
import uuid

from retry_guard import RetryGuard
from token_diagnostics import diagnostics, tool_payloads, render as render_diagnostics, inference_summary
from statistics_quality import wilson
from grading import OUTCOMES, grade
from mcp_client import StdioMCP
from tool_trace import build_trace, read_jsonl

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]
TOKEN_FIELDS = ("input_tokens", "cached_input_tokens", "cache_write_tokens",
                "output_tokens", "reasoning_tokens")


def dump(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n")


def dump_atomic(path, value):
    """dump(), but a kill mid-write leaves the old file, not half of a new one."""
    path = Path(path)
    temporary = path.with_name(f".{path.name}.tmp")
    dump(temporary, value)
    os.replace(temporary, path)


def prompt_variables(variables, target):
    """The variables a prompt expands with: a target may name its device differently to the model."""
    if target.get("prompt_device"):
        return {**variables, "device": target["prompt_device"]}
    return variables


def graded_target(item, device):
    """The selector an attempt's calls must name, when the task or scenario prompt names a device.
    The live run and --rescore both use this."""
    prompt = item.get("prompt") or ""
    return device if device and ("{device}" in prompt or "{vm_name}" in prompt) else None


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
    result = {"started_at": time.time(), "argv": argv, "exit_code": None, "timed_out": False, "interrupted": False,
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
    result["finished_at"] = time.time()
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
    servers = None
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
            if kind == "system" and event.get("subtype") == "init" and servers is None:
                servers = [s.get("name", "") for s in event.get("mcp_servers") or [] if isinstance(s, dict)]
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
    trace = build_trace(adapter, events)
    return {"token_diagnostics": diagnostics(adapter, events, trace, normalize_usage), "tokens": tokens, "usage_complete": bool(usage) and complete and ended and len(known) == len(usage),
            "tool_calls": len(tools), "permission_denials": len(denied) if adapter != "codex" else None,
            "reported_cost_usd": reported_cost, "reported_cost_kind": "client_estimate" if reported_cost is not None else None,
            "models_observed": sorted(models), "agent_completed": ended, "agent_errors": errors,
            "final_text": "\n".join(text), "mcp_servers_observed": servers if adapter == "claude" else [], "tool_trace": trace}


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
        # A scenario may share an id with an end-to-end task; keep them apart.
        groups[(row["agent"], row["target"], row["task"], row.get("kind", "task"))].append(row)
    output = []
    for (agent, target, task, kind), trials in sorted(groups.items()):
        eligible = [r for r in trials if scored(r)]
        passed = [r for r in eligible if r["success"]]
        times = sorted(r["task_seconds"] for r in passed)
        token_values = [r["metrics"]["tokens"]["input_tokens"] + r["metrics"]["tokens"]["output_tokens"]
                        for r in eligible if r["metrics"].get("usage_complete")]
        all_usage = len(token_values) == len(eligible) and bool(eligible)
        costs = [r["metrics"].get("estimated_cost_usd") if r["metrics"].get("estimated_cost_usd") is not None
                 else r["metrics"].get("reported_cost_usd") for r in eligible]
        all_costs = bool(costs) and all(isinstance(v, (int, float)) and math.isfinite(v) and v >= 0 for v in costs)
        choice = {k: sum((r.get("tool_choice") or {}).get("outcome") == k for r in eligible) for k in OUTCOMES}
        choice["ungraded"] = sum(not r.get("tool_choice") for r in eligible)
        agent_seconds = [r["stages"]["agent"]["seconds"] for r in eligible if "seconds" in r.get("stages", {}).get("agent", {})]
        output.append({"agent": agent, "target": target, "task": task, "kind": kind,
                       "attempts": len(eligible), "unscored": len(trials) - len(eligible),
                       "setup_errors": sum(r["status"] == "setup_error" for r in trials),
                       "successes": len(passed), "success_interval_95": wilson(len(passed), len(eligible)), "unassisted_success_rate": len(passed) / len(eligible) if eligible else None,
                       "median_success_seconds": statistics.median(times) if times else None,
                       "p90_success_seconds": times[math.ceil(.9 * len(times)) - 1] if times else None,
                       "mean_tokens_per_attempt": statistics.mean(token_values) if all_usage else None,
                       "tokens_per_success_including_failures": sum(token_values) / len(passed) if passed and all_usage else None,
                       "estimated_usd_per_attempt": statistics.mean(costs) if all_costs else None,
                       "estimated_usd_per_success_including_failures": sum(costs) / len(passed) if passed and all_costs else None,
                       "tool_choice": choice,
                       "median_tokens_per_attempt": statistics.median(token_values) if all_usage else None,
                       "median_agent_seconds": statistics.median(agent_seconds) if agent_seconds else None,
                       "usage_complete_attempts": len(token_values),
                       "timeouts": sum(r["status"] == "timeout" for r in trials),
                       "cleanup_errors": sum(not r.get("cleanup_ok", True) for r in trials)})
    return output


def choice_cell(choice):
    cell = "/".join(str(choice[k]) for k in OUTCOMES)
    return cell + (f" (+{choice['ungraded']} ungraded)" if choice["ungraded"] else "")


def report(rows, output):
    data = summary(rows)
    dump(output / "summary.json", data)
    dump(output / "inference-summary.json", inference_summary(rows))
    dump(output / "token-diagnostics.json", [{"id": r.get("id"), "agent": r["agent"], "task": r["task"],
         **r.get("metrics", {}).get("token_diagnostics", {})} for r in rows])
    (output / "token-diagnostics.md").write_text(render_diagnostics(rows))
    lines = ["| Agent | Target | Task | Unassisted passes | Median seconds | Tokens / attempt | Tokens / success incl. failures | Estimated $ / success | Tool choice R/W/M/A/WA |",
             "| --- | --- | --- | --- | --- | --- | --- | --- | --- |"]
    def fmt(value):
        return "unknown" if value is None else f"{value:,.1f}"
    for row in data:
        lines.append(f"| {row['agent']} | {row['target']} | {row['task']} | {row['successes']}/{row['attempts']} | "
                     f"{fmt(row['median_success_seconds'])} | {fmt(row['mean_tokens_per_attempt'])} | "
                     f"{fmt(row['tokens_per_success_including_failures'])} | "
                     f"{fmt(row['estimated_usd_per_success_including_failures'])} | "
                     f"{choice_cell(row['tool_choice'])} |")
    (output / "summary.md").write_text("\n".join(lines) + "\n")


def rescore(results_dir, tasks, scenarios):
    # Regrade saved attempts from their logs; originals are never modified and nothing is executed.
    # metrics.tool_trace and tool_choice change; a scenario row also gets its status and success again,
    # by the live run's rule. Saved accounting (tokens, usage_complete, cost) is kept.
    import scenarios as sc  # imported here: scenarios.py imports this module
    agents = json.loads((results_dir / "manifest.json").read_text())["config"]["agents"]
    expects = {False: {t["id"]: t for t in tasks}, True: {s["id"]: s for s in scenarios}}
    out = results_dir / ("rescored-" + time.strftime("%Y%m%d-%H%M%S"))
    out.mkdir()
    rows = []
    for path in sorted(results_dir.glob("*/result.json")):
        row, directory = json.loads(path.read_text()), path.parent
        adapter = agents.get(row.get("agent"), {}).get("adapter")
        if adapter is None or not (directory / "agent.stdout").exists() or not isinstance(row.get("metrics"), dict):
            rows.append(row)
            continue
        events, _ = read_events(directory / "agent.stdout")
        extra = {k: read_jsonl(directory / f"{f}.jsonl") if (directory / f"{f}.jsonl").exists() else None
                 for k, f in (("proxy_records", "proxy"), ("cli_records", "cli"))}
        live = metrics(adapter, events)
        row["metrics"]["tool_trace"] = trace = build_trace(adapter, events, **extra)
        row["metrics"]["token_diagnostics"] = diagnostics(adapter, events, trace, normalize_usage)
        scenario = row.get("kind") == "scenario"
        if scenario:
            mcp_routing(live, extra["proxy_records"])
            for key in ("transcript_mcp_calls", "proxy_calls"):
                row["metrics"][key] = live[key]
        item = expects[scenario].get(row.get("task"), {})
        row["tool_choice"] = (grade(trace, item["expect"], live["final_text"], live["agent_completed"],
                                    group=row.get("group") if scenario else None, adapter=adapter,
                                    went_around=sc.is_local(row.get("target_config") or {}),
                                    target=graded_target(item, row.get("device_selector")))
                              if item.get("expect") else None)
        agent = row.get("stages", {}).get("agent")
        # A setup_error attempt never reached a verdict (it may have aborted after the agent ran), and an
        # infra status or an interruption is a limit a new verdict would erase.
        if scenario and isinstance(agent, dict) and row.get("status") not in NO_VERDICT_STATUSES:
            row["status"], row["success"] = scenario_verdict(adapter, live, agent, row["tool_choice"])
        rows.append(row)
    (out / "results.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows))
    report(rows, out)
    return out


def stage_plugin(treatment_root, destination):
    """Copy the plugin without its own MCP server, so a bare `wendy` from PATH is never loaded."""
    shutil.copytree(Path(treatment_root) / "plugins/wendy-agentic-coding", destination)
    manifest = destination / ".claude-plugin/plugin.json"
    data = json.loads(manifest.read_text())
    data.pop("mcpServers", None)
    dump(manifest, data)
    return destination


def tree_sha256(path):
    digest = hashlib.sha256()
    path = Path(path)
    for file in sorted((f for f in path.rglob("*") if f.is_file()), key=lambda f: f.relative_to(path).as_posix()):
        digest.update(file.relative_to(path).as_posix().encode() + b"\0" + file.read_bytes() + b"\0")
    return digest.hexdigest()


def harness_sha256(here=HERE):
    """Hash runner code, suite/model definitions, CLI policy and both capture trees."""
    here = Path(here)
    files = (list(here.glob("*.py")) + [here / name for name in ("cli_safe.json", "suites.json", "models.json")]
             + list((here / "canned").rglob("*")) + list((here / "canned-local").rglob("*")))
    digest = hashlib.sha256()
    for file in sorted((f for f in files if f.is_file()), key=lambda f: f.relative_to(here).as_posix()):
        digest.update(file.relative_to(here).as_posix().encode() + b"\0" + file.read_bytes() + b"\0")
    return digest.hexdigest()


def treatment_git(root):
    """(HEAD revision, sha256 of `git diff HEAD`); None for each when git fails."""
    def git(*args):
        done = subprocess.run(["git", *args], cwd=root, capture_output=True)
        return done.stdout if done.returncode == 0 else None
    revision, diff = git("rev-parse", "HEAD"), git("diff", "HEAD")
    return (revision.decode().strip() if revision is not None else None,
            hashlib.sha256(diff).hexdigest() if diff is not None else None)


def extra_wendy_servers(metric):
    return [n for n in metric.get("mcp_servers_observed") or [] if "wendy" in n.lower() and n != "wendy"]


def stage_context(workspace, adapter, wendy, state_dir=None, treatment_root=ROOT, server_env=None):
    # The supplied Wendy skills are part of the treatment. The benchmark's task
    # definitions, verifier, previous runs, and parent repository are not copied.
    skills = Path(treatment_root) / "plugins/wendy-agentic-coding/skills"
    if adapter == "codex":
        shutil.copytree(skills, workspace / ".agents/skills")
    if adapter == "claude":
        server = {"command": wendy, "args": ["mcp", "serve"]}
        env = {"WENDY_CONFIG_DIR": str(state_dir), "WENDY_SECRET_STORE": "file"} if state_dir is not None else {}
        env.update(server_env or {})
        if env:
            server["env"] = env
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


def list_all_tools(wendy, env):
    """Every tool `wendy mcp serve` exposes with all groups enabled, via a throwaway simulator-style state dir."""
    state = isolated_device_state({"kind": "simulator"})
    try:
        env = {**env, "WENDY_CONFIG_DIR": str(state), "WENDY_SECRET_STORE": "file"}
        with StdioMCP([wendy, "mcp", "serve", "--tool-groups", "all"], env) as mcp:
            mcp.initialize()
            return mcp.list_tools()
    finally:
        shutil.rmtree(state, ignore_errors=True)


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
    # A --device board has capabilities None until they are read during --execute.
    missing = required - set(target.get("capabilities") or [])
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
            matching = [r for r in rows if (r["agent"], r["target"], r["task"], r.get("kind", "task")) ==
                        (agent, entry["target"], entry["task"], entry.get("kind", "task"))]
            attempts = [r for r in matching if scored(r)]
            entry["agents"][agent] = {"attempts": len(attempts), "passed": sum(r["success"] for r in attempts),
                                      "unscored": len(matching) - len(attempts),
                                      "setup_errors": sum(r["status"] == "setup_error" for r in matching),
                                      "not_run": max(0, repetitions - len(matching))}
        result.append(entry)
    return result


def disposable(config, env=os.environ):
    """True only for a device that is allowed to break: the config says so and the agent socket is set."""
    return config.get("disposable_device") is True and bool(env.get("WENDY_AGENT_SOCKET"))


# Attempts that ended on a limit the model didn't cause stay out of its score.
UNSCORED_STATUSES = frozenset({"context_exceeded", "model_server_down", "interrupted", "timeout", "setup_error"})
NO_VERDICT_STATUSES = frozenset({"setup_error", "context_exceeded", "model_server_down", "interrupted"})
CONTEXT_OVERFLOW = re.compile(r"exceed_context_size|exceeds? the available context", re.I)
ERROR_EVENTS = ("error", "turn.failed")


def scored(row):
    """Whether an attempt counts in a pass rate. Infra limits and setup errors never do. A timeout or an
    interruption is unscored only on the local target; on a laptop it stays the model's attempt."""
    status = row.get("status")
    if status in ("context_exceeded", "model_server_down", "setup_error"):
        return False
    if status in ("timeout", "interrupted"):
        import scenarios as sc  # imported here: scenarios.py imports this module
        return not sc.is_local(row.get("target_config") or {})
    return True


def write_model_report(output):
    """Refresh report.md and report.json. A failure here must never stop the pass."""
    try:
        import model_report  # imported here: model_report imports this module
        model_report.write_report([output], output)
    except Exception as error:
        print(f"Warning: could not write the model report: {error}", file=sys.stderr)


def model_server_healthy(config, timeout=5):
    """True or False when the config names a health URL for the model server, else None (unknown)."""
    url = config.get("model_health_url")
    if not url:
        return None
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return 200 <= response.status < 300
    except (OSError, ValueError, http.client.HTTPException):  # a garbled reply as the server dies
        return False


MODEL_WAIT_SECONDS = 900


def wait_for_model(config, timeout=MODEL_WAIT_SECONDS, poll=5, health=None, sleep=time.sleep, clock=time.monotonic):
    """Wait for the model server to answer its health URL, so a restart after a crash isn't scored as the
    next attempt's failure. True when it is healthy or its health is unknown; False when it never recovers."""
    check = health if health is not None else (lambda: model_server_healthy(config))
    deadline = clock() + timeout
    while True:
        if check() is not False:
            return True
        if clock() >= deadline:
            return False
        sleep(poll)


def infra_status(events, config, health=None):
    """An unscored status when the agent's error came from the model server's limits, else None. Called
    after the agent phase on a disposable device only. `health()` says whether the model server answers."""
    errors = [e for e in events if e.get("type") in ERROR_EVENTS or (e.get("type") == "result" and e.get("is_error"))]
    if not errors:
        return None
    if any(CONTEXT_OVERFLOW.search(json.dumps(e)) for e in errors):
        return "context_exceeded"
    check = health if health is not None else (lambda: model_server_healthy(config))
    return "model_server_down" if check() is False else None


def device_markers(env=os.environ, uptime_path="/proc/uptime"):
    """What identifies this boot of the device and its agent socket, so a reboot or an agent restart
    between attempts shows in the rows."""
    try:
        uptime = Path(uptime_path).read_text().split()[0]
    except (OSError, IndexError):
        uptime = None
    try:
        inode = os.stat(env["WENDY_AGENT_SOCKET"]).st_ino
    except (KeyError, OSError):
        inode = None
    return {"uptime": uptime, "socket_inode": inode}


def load_rows(path):
    """The rows a pass saved. A last line cut short by a crash is ignored."""
    path = Path(path)
    if not path.exists():
        return []
    rows = []
    for line in path.read_text(errors="replace").splitlines():
        try:
            value = json.loads(line)
        except ValueError:
            continue
        if isinstance(value, dict):
            rows.append(value)
    return rows


def repair_rows(path):
    """load_rows for a resumed pass, rewriting the file from its valid rows when a line was torn, so the
    next append starts on a line of its own. Says so when it skips a line."""
    path = Path(path)
    rows = load_rows(path)
    if not path.exists():
        return rows
    text = path.read_text(errors="replace")
    lines = [line for line in text.splitlines() if line.strip()]
    if len(lines) != len(rows) or (text and not text.endswith("\n")):
        if len(lines) != len(rows):
            print(f"Skipped {len(lines) - len(rows)} unreadable line(s) in {path.name}; rewrote it from the valid rows.")
        temporary = path.with_name(path.name + ".tmp")
        temporary.write_text("".join(json.dumps(r) + "\n" for r in rows))
        os.replace(temporary, path)
    return rows


def recovered_rows(output, rows):
    """Rows for attempts whose result.json was saved but whose row never reached results.jsonl (the pass
    stopped during the sweep). What the sweep did is unknown."""
    known = {row.get("id") for row in rows}
    made = []
    for path in sorted(Path(output).glob("*/result.json")):
        try:
            row = json.loads(path.read_text())
        except ValueError:
            continue
        if not isinstance(row, dict) or not row.get("id") or row["id"] in known or "status" not in row:
            continue
        row.setdefault("stages", {})["sweep"] = {"unknown": True}
        known.add(row["id"])
        made.append(row)
    return made


def remaining_plan(plan, rows):
    """The plan items still to run. Each saved row completes one item of its (kind, agent, target, task);
    the first `interrupted` row per key doesn't count, so that attempt is retried once."""
    done, interrupted_seen = collections.Counter(), set()
    for row in rows:
        key = (row.get("kind", "task"), row.get("agent"), row.get("target"), row.get("task"))
        if row.get("status") == "interrupted" and key not in interrupted_seen:
            interrupted_seen.add(key)
            continue
        done[key] += 1
    left = []
    for kind, agent, target, task in plan:
        key = (kind, agent, target, task["id"])
        if done[key] > 0:
            done[key] -= 1
        else:
            left.append((kind, agent, target, task))
    return left


def interrupted_rows(output, rows):
    """A row for every attempt directory that started (it has task.json) but never saved result.json,
    unless a row for that attempt exists. The caller records them."""
    known = {row.get("id") for row in rows}
    made = []
    for directory in sorted(Path(output).iterdir()):
        task_file = directory / "task.json"
        if not directory.is_dir() or not task_file.exists() or (directory / "result.json").exists():
            continue
        ident = directory.name.rsplit("-", 1)[-1]
        if ident in known:
            continue
        try:
            task = json.loads(task_file.read_text())
            target_config = json.loads((directory / "target.json").read_text()) if (directory / "target.json").exists() else None
        except ValueError:
            continue
        if not isinstance(task, dict) or not task.get("id"):
            continue
        head = directory.name[:-len(ident) - 1]
        if not head.endswith("-" + task["id"]):
            continue
        agent, target = task.get("agent"), task.get("target")
        if not agent or not target:  # an attempt from before task.json named them: split at the first hyphen
            agent, _, target = head[:-len(task["id"]) - 1].partition("-")
        made.append({"id": ident, "kind": "scenario" if "group" in task else "task", "agent": agent,
                     "target": target, "task": task["id"], "target_config": target_config,
                     "status": "interrupted", "success": False, "cleanup_ok": True, "task_seconds": 0,
                     "workspace": None, "metrics": {}, "stages": {}, "tool_choice": None,
                     "note": "the attempt started and saved no result"})
    return made


def agent_timeout(config, task):
    # A scenario is any item with a group key. The config may override the task's own limit.
    override = (config.get("agent_seconds") or {}).get("scenario" if "group" in task else "task")
    values = [override if override is not None else task.get("timeout_seconds", 900)]
    if config.get("max_agent_seconds") is not None:
        values.append(config["max_agent_seconds"])
    if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not math.isfinite(v) or v <= 0 for v in values):
        raise ValueError("agent time limits must be finite positive numbers")
    return min(values)


def agent_metrics(adapter, stdout, result, prices):
    """Metrics for one agent stage; usage cut short by any stop is unknown, never partial."""
    events, invalid = read_events(stdout)
    metric = metrics(adapter, events)
    metric["invalid_json_lines"] = invalid
    metric["usage_complete"] &= (not result["timed_out"] and not result["interrupted"]
                                 and not result.get("host_state_changed") and not result.get("no_progress")
                                 and invalid == 0)
    metric["estimated_cost_usd"] = estimate_cost(metric, prices)
    request_log = os.environ.get("WENDY_EVAL_MODEL_REQUESTS")
    if request_log and Path(request_log).is_file():
        metric["inference_requests"] = [r for r in read_jsonl(Path(request_log))
                                       if result.get("started_at", float("inf")) <= r.get("started_at", 0)
                                       <= result.get("finished_at", 0)]
    return metric, events


def attempt_status(result):
    return ("host_state_changed" if result.get("host_state_changed") else "no_progress"
            if result.get("no_progress") else "interrupted"
            if result["interrupted"] else "timeout" if result["timed_out"] else "failed")


def mcp_routing(metric, proxy_records):
    """Count the Wendy MCP calls the transcript shows (metric's trace, before the proxy's records
    replace them) and the calls the recording proxy logged; None when there is no proxy log."""
    metric["transcript_mcp_calls"] = sum(e["channel"] == "mcp" for e in metric["tool_trace"])
    metric["proxy_calls"] = None if proxy_records is None else len(proxy_records)


def unrouted(metric):
    """The transcript shows Wendy MCP calls and the proxy logged none: another server answered them."""
    return bool(metric.get("transcript_mcp_calls")) and metric.get("proxy_calls") == 0


def mcp_status(adapter, metric):
    """Claude must show exactly one Wendy server, the one the runner configured, and any agent's
    Wendy MCP calls must reach the recording proxy when there is one."""
    if adapter == "claude" and extra_wendy_servers(metric):
        return "mcp_contaminated"
    if unrouted(metric) or (adapter == "claude" and metric["mcp_servers_observed"] is None):
        return "mcp_unverified"
    return None


# A scenario attempt that ends this way never counts as a success, whatever its tool choice.
SCENARIO_NOT_SUCCESS = frozenset({"mcp_contaminated", "mcp_unverified", "host_state_changed", "interrupted"})


def scenario_verdict(adapter, metric, result, tool_choice):
    """(status, success) of a scenario attempt; the live run and --rescore both use this."""
    status = mcp_status(adapter, metric) or attempt_status(result)
    success = bool(tool_choice) and tool_choice.get("outcome") in ("right", "asked") and status not in SCENARIO_NOT_SUCCESS
    return ("passed" if success else status), success


def charge(spend, row, config, quiet=False):
    """Return (new spend, whether to continue). Unknown cost stops end-to-end runs; scenarios pay a reserve."""
    cost = row["metrics"].get("estimated_cost_usd")
    if cost is None:
        cost = row["metrics"].get("reported_cost_usd")
    if cost is not None:
        return spend + cost, True
    if row.get("kind") == "scenario":
        reserve = config.get("scenario_reserve_usd", .5)
        if not quiet:
            print(f"Usage unknown; charged the scenario reserve of ${reserve:.2f}.")
        return spend + reserve, True
    return spend, False


def stop_reason(row, config, env=os.environ):
    """Why the pass must stop after this attempt, or None. A disposable device keeps going past a failed
    cleanup and an unknown cost, because those become findings; the MCP and interruption stops stay."""
    if row["status"] == "mcp_contaminated":
        return "Another Wendy MCP server was loaded into Claude; stopping."
    if row["status"] == "mcp_unverified":
        if unrouted(row["metrics"]):
            return "The agent's Wendy MCP calls never reached the recording proxy; stopping."
        return "Claude reported no MCP server list; stopping because a second Wendy server can't be ruled out."
    if any(stage.get("interrupted") for stage in row["stages"].values()):
        return "Experiment interrupted; logs and cleanup result retained."
    if disposable(config, env):
        return None
    if row["status"] != "setup_error" and not charge(0, row, config, quiet=True)[1]:
        return "Usage cost is unknown. Stopping before spending on another trial."
    if not row["cleanup_ok"]:
        return "Cleanup failed. Stopping before another trial can inherit this state."
    return None


def start_attempt_logs():
    """Fresh empty proxy and CLI logs outside the results tree: Codex's sandboxed shell may write only to
    its workspace, its added dirs and /tmp, and the shim logs from inside that shell. Empty logs mean no
    calls, so transcript MCP and CLI entries never stand in for them. Returns (directory, proxy, cli)."""
    import scenarios as sc  # imported here: scenarios.py imports this module
    logs = Path(tempfile.mkdtemp(prefix="we-log-", dir=sc._tmp_root()))
    proxy_log, cli_log = logs / "proxy.jsonl", logs / "cli.jsonl"
    proxy_log.touch()
    cli_log.touch()
    return logs, proxy_log, cli_log


def rebuild_trace(metric, adapter, events, proxy_log, cli_log):
    """Replace the transcript's tool trace with the recorded one, counting routing first (it reads the
    transcript's MCP calls). Grading must run after this."""
    proxy_records = read_jsonl(proxy_log)
    mcp_routing(metric, proxy_records)
    metric["tool_trace"] = build_trace(adapter, events, proxy_records=proxy_records, cli_records=read_jsonl(cli_log))
    metric.setdefault("token_diagnostics", {})["tools"] = tool_payloads(metric["tool_trace"])
    return metric["tool_trace"]


def keep_attempt_logs(logs, directory):
    """Move the logs into the attempt directory and drop the temporary one."""
    for path in (logs / "proxy.jsonl", logs / "cli.jsonl"):
        if path.exists():
            shutil.move(str(path), str(directory / path.name))
    shutil.rmtree(logs, ignore_errors=True)


def trial(config, agent_name, target_name, task, output, wendy, treatment_root=ROOT, shim=None, tools_path=None):
    """One end-to-end attempt. On the local target the agent runs through the recording shim in pass-through
    mode (only the guarded actions are faked); fixtures and cleanup keep using the real binary."""
    import scenarios as sc  # imported here: scenarios.py imports this module
    agent, target = config["agents"][agent_name], config["targets"][target_name]
    local = sc.is_local(target)
    if local and (shim is None or tools_path is None):
        raise ValueError("a local-target task needs the shim and the tools list")
    agent_wendy = os.path.abspath(shim) if local else wendy
    ident = uuid.uuid4().hex[:12]
    directory = output / f"{agent_name}-{target_name}-{task['id']}-{ident}"
    directory.mkdir()
    workspace = Path(tempfile.mkdtemp(prefix="wendy-eval-"))
    state_dir = isolated_device_state(target) if config.get("isolate_device_state", True) else None
    variables = {"root": ROOT, "here": HERE, "treatment_root": treatment_root, "plugin_dir": directory / "plugin",
                 "workspace": workspace, "wendy": wendy,
                 "wendy_home": state_dir or Path.home() / ".wendy",
                 "wendy_build_cache": Path.home() / ".cache/wendy",
                 "docker_buildx_cache": Path(os.environ.get("DOCKER_CONFIG", Path.home() / ".docker")) / "buildx",
                 "wendy_cache": (Path.home() / "Library/Caches/wendy" if sys.platform == "darwin" else
                                 Path(os.environ.get("XDG_CACHE_HOME", Path.home() / ".cache")) / "wendy"),
                 "device": target["device"], "run_id": ident, "app_id": f"dev.wendy.eval.{ident}",
                 "vm_name": f"wendy-eval-{ident}", "port": config.get("port", 18765),
                 "model": agent["model"]}
    # Command templates shared with short scenarios; there they name the shared VM store and scenario file.
    variables.update(wendy_vm_home=variables["wendy_home"], scenario_config="")
    private_vm = needs_fixture_vm(task, target, state_dir)
    if private_vm or (target["kind"] == "simulator" and task.get("owns_vm")):
        variables["device"] = "vm:" + variables["vm_name"]
    variables.update({"evidence_dir": directory, "agent_events": directory / "agent.stdout",
                      "target_config": directory / "target.json"})
    dump(directory / "target.json", target)
    resolved = expand(task, variables)
    prompt_vars = prompt_variables(dict(variables, wendy=agent_wendy), target)  # the model is told the shim
    resolved["prompt"] = expand(task["prompt"], prompt_vars)
    resolved["prompt"] += f"\nThe Wendy executable for this environment is {agent_wendy}. Use that absolute path for Wendy CLI commands."
    if config.get("environment_context"):
        resolved["prompt"] += "\n" + expand(config["environment_context"], prompt_vars)
    if state_dir is not None:
        resolved["prompt"] += (f"\nWendy device/cloud configuration and its VM store are isolated at {state_dir} "
                               "through WENDY_CONFIG_DIR. Preserve that environment setting. Do not change the host's ~/.wendy configuration or identity pins.")
    variables["prompt"] = resolved["prompt"]
    logs = proxy_log = cli_log = None
    if local:
        scenario_config = directory / "scenario.json"
        variables["scenario_config"] = scenario_config
        logs, proxy_log, cli_log = start_attempt_logs()
        sc.write_scenario_config(
            scenario_config, real_wendy=os.path.abspath(wendy), shim=agent_wendy, tools_path=tools_path,
            canned_dir=sc.canned_dir_for(target), canned_set=task.get("canned_set", "tasks"), canned=[], allow_real=[],
            block=sc.guard_matchers(os.environ.get("WENDY_APP_ID", sc.BENCHMARK_APP)), cli_canned=[],
            device=variables["device"], vm_name=variables["vm_name"], proxy_log=proxy_log, cli_log=cli_log,
            passthrough=True)
    row = {"id": ident, "agent": agent_name, "target": target_name, "task": task["id"],
           "target_config": target, "model_requested": agent["model"], "workspace": str(workspace),
           "device_state_isolated": state_dir is not None,
           "device_selector": variables["device"],
           "agent_timeout_seconds": agent_timeout(config, task),
           "human_messages_after_start": 0, "human_approvals": 0, "success": False,
           "status": "setup_error", "task_seconds": 0, "metrics": {}, "stages": {}}
    if disposable(config):
        row["device_markers"] = device_markers()
    dump(directory / "task.json", dict(resolved, agent=agent_name, target=target_name))
    (directory / "prompt.txt").write_text(resolved["prompt"] + "\n")
    env = dict(os.environ, WENDY_ANALYTICS="false")
    if state_dir is not None:
        env["WENDY_CONFIG_DIR"] = str(state_dir)
        env["WENDY_SECRET_STORE"] = "file"
    env["PATH"] = str(Path(wendy).parent) + os.pathsep + env.get("PATH", "")
    agent_env = env
    if local:
        agent_env = dict(env, WENDY_EVAL_SCENARIO=str(variables["scenario_config"]),
                         PATH=str(Path(agent_wendy).parent) + os.pathsep + env["PATH"])
    guard = host_pin_guard()
    retry_guard = RetryGuard(directory / "agent.stdout", config.get("syntax_error_limit", 3))
    infra = None
    try:
        if private_vm:
            row["stages"], ready = fixture_vm(config, variables, workspace, directory, env)
            if not ready:
                return row
        stage_context(workspace, agent["adapter"], agent_wendy, state_dir, treatment_root,
                      server_env={"WENDY_EVAL_SCENARIO": str(variables["scenario_config"])} if local else None)
        if agent["adapter"] == "claude":
            stage_plugin(treatment_root, directory / "plugin")
        for phase in ("prepare", "agent", "verify"):
            if phase == "agent":
                argv = expand(agent["command"], dict(variables, wendy=agent_wendy))
                timeout = row["agent_timeout_seconds"]
            else:
                argv = resolved[phase]
                timeout = task.get("setup_timeout_seconds", 900) if phase == "prepare" else task.get("verify_timeout_seconds", 90)
            result = run_process(argv, workspace, directory / phase, timeout, agent_env if phase == "agent" else env,
                                 guard if phase == "agent" else None,
                                 retry_guard if phase == "agent" else None)
            row["stages"][phase] = result
            if phase == "prepare" and result["exit_code"] != 0:
                return row
            if phase == "agent":
                row["metrics"], events = agent_metrics(agent["adapter"], directory / "agent.stdout", result,
                                                       agent.get("prices_per_million"))
                if local:
                    # Grading must see what the proxy and shim recorded, not what the transcript shows.
                    rebuild_trace(row["metrics"], agent["adapter"], events, proxy_log, cli_log)
                row["task_seconds"] = result["seconds"]
                row["tool_choice"] = (grade(row["metrics"]["tool_trace"], task["expect"], row["metrics"]["final_text"],
                                            row["metrics"]["agent_completed"], adapter=agent["adapter"],
                                            went_around=local, target=graded_target(task, variables["device"]))
                                      if task.get("expect") else None)
                row["status"] = mcp_status(agent["adapter"], row["metrics"]) or attempt_status(result)
                infra = infra_status(events, config) if disposable(config) else None
                row["status"] = infra or row["status"]
            if phase == "verify":
                row["task_seconds"] += result["seconds"]
                row["outcome_verified"] = result["exit_code"] == 0
                row["success"] = (row["outcome_verified"] and row["stages"]["agent"]["exit_code"] == 0
                                  and row["metrics"]["agent_completed"] and not row["metrics"]["agent_errors"]
                                  and row["metrics"]["invalid_json_lines"] == 0
                                  and not row["stages"]["agent"].get("no_progress")
                                  and not row["stages"]["agent"].get("host_state_changed"))
                if row["status"] in ("mcp_contaminated", "mcp_unverified") or infra:
                    row["success"] = False
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
        if logs is not None:
            keep_attempt_logs(logs, directory)
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


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, default=HERE / "config.example.json")
    parser.add_argument("--tasks", type=Path, default=HERE / "tasks.json")
    parser.add_argument("--agent", action="append")
    parser.add_argument("--target", action="append")
    parser.add_argument("--task", action="append")
    parser.add_argument("--scenario", action="append", metavar="ID",
                        help="Short tool-choice scenario to run; 'all' selects every scenario. Without --task, no end-to-end tasks run.")
    parser.add_argument("--scenarios", type=Path, help="Scenario file (default: scenarios.json next to this script).")
    parser.add_argument("--device", metavar="SELECTOR",
                        help="Spare board for hardware scenarios (host:port or cloud://...), added as target \"board\"; needs --scenario.")
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--seed", type=int, default=1)
    parser.add_argument("--limit", type=int)
    parser.add_argument("--output", type=Path, default=HERE / "results")
    parser.add_argument("--treatment-root", type=Path, default=ROOT, help="Checkout whose skills and plugin the agents learn from.")
    parser.add_argument("--wendy", help="Wendy binary; overrides config \"wendy\".")
    parser.add_argument("--execute", action="store_true", help="Run the plan. Without this flag, make no model or device calls.")
    parser.add_argument("--calibrate", action="store_true", help="Check built-in fixtures with reference solutions; no model calls.")
    parser.add_argument("--resume", type=Path, metavar="DIR",
                        help="Continue the pass saved in DIR (created when absent): finished attempts are skipped, the first manifest and baseline apps are kept. Replaces --output.")
    parser.add_argument("--rescore", type=Path, metavar="DIR", help="Regrade a saved results directory from its logs; no model or device calls.")
    args = parser.parse_args(argv)
    import scenarios as sc  # imported here: scenarios.py imports this module
    scenario_file = args.scenarios or HERE / "scenarios.json"

    def load_scenarios():
        try:
            return sc.load_scenarios(scenario_file)
        except (OSError, ValueError) as error:
            parser.error(f"cannot load scenarios from {scenario_file}: {error}")

    if args.rescore:
        if args.execute:
            parser.error("--rescore cannot be combined with --execute")
        known = load_scenarios() if args.scenarios or scenario_file.exists() else []
        print(rescore(args.rescore.resolve(), json.loads(args.tasks.read_text()), known))
        return 0
    config = json.loads(args.config.read_text())
    tasks = json.loads(args.tasks.read_text())
    if args.repetitions < 1 or (args.limit is not None and args.limit < 1):
        parser.error("repetitions and limit must be positive")
    if args.device:
        if not args.scenario:
            parser.error("--device needs --scenario: the spare board is for short scenarios only")
        if "board" in config["targets"]:
            parser.error('the config already has a target named "board"; select it with --target board instead of --device')
        if args.device.startswith("vm:"):
            parser.error("--device names a spare board; a vm: selector is a simulator, which is never used as a board")
        config["targets"]["board"] = sc.board_target(args.device)
    scenarios = []
    if args.scenario:
        if args.calibrate:
            parser.error("--calibrate checks end-to-end fixtures and can't run scenarios")
        scenarios = load_scenarios()
    agents = args.agent or list(config["agents"])
    # Without --target, scenarios run on simulators and the --device board; end-to-end tasks run on every
    # configured target, also when scenarios are selected in the same run. The board is for scenarios only.
    if args.target:
        task_targets = scenario_targets = args.target
    else:
        scenario_targets = [name for name, t in config["targets"].items() if t.get("kind") == "simulator"]
        scenario_targets += ["board"] if args.device else []
        task_targets = [name for name in config["targets"] if not (args.device and name == "board")]
    selected_tasks = args.task or ([] if args.scenario else [t["id"] for t in tasks])
    scenario_ids = [s["id"] for s in scenarios]
    selected_scenarios = scenario_ids if "all" in (args.scenario or []) else args.scenario or []
    for selected, available in ((agents, config["agents"]), (task_targets + scenario_targets, config["targets"]),
                                (selected_tasks, [t["id"] for t in tasks]), (selected_scenarios, scenario_ids)):
        if any(name not in available for name in selected):
            parser.error("unknown selection: " + str(selected))
    if args.calibrate:
        return calibrate(config, [t for t in tasks if t["id"] in selected_tasks], task_targets, args)
    # Validate guard settings before creating workspaces or copying credentials.
    RetryGuard(args.output / "validation-only", config.get("syntax_error_limit", 3))

    def plan_all():
        try:
            covered = sc.scenario_coverage(scenarios, config["targets"], selected_scenarios, scenario_targets, sc.CANNED,
                                           lambda: sc.robot_prerequisites(config))
        except ValueError as error:  # a capture that would fail mid-attempt
            parser.error(str(error))
        coverage = plan_coverage(tasks, config["targets"], selected_tasks, task_targets) + covered
        ready = {(c["task"], c["target"]) for c in covered if c["status"] == "ready"}
        plan = [("task", agent, target, task_for_target(task, config["targets"][target])[0])
                for _ in range(args.repetitions) for task in tasks
                if task["id"] in selected_tasks for target in task_targets
                if config["targets"][target]["kind"] in task["environments"]
                if task_for_target(task, config["targets"][target])[1] is None for agent in agents]
        plan += [("scenario", agent, target, scenario)
                 for _ in range(args.repetitions) for scenario in scenarios
                 if scenario["id"] in selected_scenarios for target in scenario_targets
                 if (scenario["id"], target) in ready for agent in agents]
        random.Random(args.seed).shuffle(plan)
        if disposable(config):
            plan.sort(key=lambda item: item[0] != "scenario")  # stable: short scenarios first, then real tasks
        if args.limit:
            plan = plan[:args.limit]
        print(json.dumps({"coverage": coverage}, indent=2))
        print(json.dumps([{"agent": a, "target": t, "task": s["id"], **({"kind": "scenario"} if k == "scenario" else {}),
                           "agent_timeout_seconds": agent_timeout(config, s),
                           "syntax_error_limit": config.get("syntax_error_limit", 3),
                           **({"max_tool_calls": s["max_tool_calls"]} if k == "scenario" else {})}
                          for k, a, t, s in plan], indent=2))
        return coverage, plan

    coverage, plan = plan_all()
    if not args.execute:
        print(f"Plan only: {len(plan)} trials. Add --execute to run.")
        return 0
    if disposable(config) and not config.get("model_health_url"):
        parser.error('a disposable device needs "model_health_url" in the config: without it a dead model server '
                     "would be scored as the model's failure")
    # A board's capabilities (null, as for --device) are read only under --execute; until then its
    # hardware scenarios wait.
    detect = [name for name in scenario_targets if config["targets"][name]["kind"] in ("physical", "cloud", "local")
              and "capabilities" in config["targets"][name] and config["targets"][name]["capabilities"] is None
              and any(config["targets"][name]["kind"] in s["environments"]
                      for s in scenarios if s["id"] in selected_scenarios)]
    if not plan and not detect:
        parser.error("no runnable trials; configure the fixtures reported above")
    if any(config["targets"][name]["kind"] == "local"
           for name in {target for _, _, target, _ in plan} | set(detect)) and not disposable(config):
        parser.error('the local target runs the agent on the device under test: it needs "disposable_device": true '
                     "in the config and WENDY_AGENT_SOCKET set in the environment")
    # A task on the local target runs its agent through the shim too, so it gets the same command checks
    # and the chat hook check as a scenario.
    local_tasks = any(kind == "task" and config["targets"][target]["kind"] == "local" for kind, _, target, _ in plan)
    for name in agents:
        agent = config["agents"][name]
        if not agent.get("model") or "SET_" in agent["model"]:
            parser.error(f"pin a model for {name} in the config")
        if agent["adapter"] not in ("codex", "claude", "wendy"):
            parser.error(f"unknown adapter for {name}")
        command = agent["command"]
        pairs = list(zip(command, command[1:]))
        if agent["adapter"] == "claude" and ("--plugin-dir", "{plugin_dir}") not in pairs:
            parser.error('Claude must load the staged plugin: use "--plugin-dir", "{plugin_dir}"')
        if not (selected_scenarios or local_tasks):
            continue
        # In a scenario or local-task run, {wendy} is the shim, and only a Wendy MCP server or chat started
        # through it is recorded.
        if agent["adapter"] == "claude" and ("--strict-mcp-config" not in command
                                             or ("--mcp-config", "{workspace}/mcp.json") not in pairs):
            parser.error(f"{name} must load only the runner's MCP config: add \"--strict-mcp-config\", "
                         '"--mcp-config", "{workspace}/mcp.json" to its command')
        if agent["adapter"] == "codex":
            if 'mcp_servers.wendy.command="{wendy}"' not in command:
                parser.error(f'{name} must start its Wendy MCP server through the shim: add "-c", '
                             '"mcp_servers.wendy.command=\\"{wendy}\\"" to its command')
            # Codex starts MCP servers with a cleared environment and runs its shell in a sandbox.
            if not any("WENDY_EVAL_SCENARIO" in arg for arg in command):
                parser.error(f'{name} must pass the scenario to its MCP server: add "-c", '
                             '"mcp_servers.wendy.env.WENDY_EVAL_SCENARIO=\\"{scenario_config}\\"" to its command')
            if "{wendy_vm_home}" not in command:
                parser.error(f'{name} must be able to use the shared VM store: add "--add-dir", "{{wendy_vm_home}}" to its command')
        if agent["adapter"] == "wendy" and command[:1] != ["{wendy}"]:
            parser.error(f'{name} must run chat through the shim: start its command with "{{wendy}}"')
    for name in {target for _, _, target, _ in plan} | set(detect):
        target = config["targets"][name]
        if target["kind"] not in ("simulator", "cloud", "physical", "local"):
            parser.error(f"unknown environment for {name}")
        expected = {"cloud": "cloud://", "simulator": "vm:", "physical": "", "local": ""}[target["kind"]]
        if not target.get("device") or not target["device"].startswith(expected) or "SET_" in target["device"]:
            parser.error(f"{name} requires an explicit {expected} selector")
    reserve = config.get("scenario_reserve_usd", .5)
    if isinstance(reserve, bool) or not isinstance(reserve, (int, float)) or not math.isfinite(reserve) or reserve < 0:
        parser.error("scenario_reserve_usd must be a finite nonnegative number")
    wendy = shutil.which(args.wendy or config.get("wendy", "wendy"))
    if not wendy:
        parser.error("Wendy executable not found")
    wendy = os.path.abspath(wendy)
    chats = [name for name in agents if config["agents"][name]["adapter"] == "wendy"]
    if (selected_scenarios or local_tasks) and chats:
        import eval_wendy
        if not eval_wendy.has_chat_hook(wendy):  # the shim refuses it too; fail before any attempt
            parser.error(f"{wendy} predates the WENDY_CHAT_MCP_COMMAND hook, so Wendy chat ({', '.join(chats)}) "
                         "would start its own unrecorded MCP server; rebuild it to run scenarios with chat")
    if not (args.treatment_root / "plugins/wendy-agentic-coding/skills").is_dir():
        parser.error("--treatment-root has no plugins/wendy-agentic-coding/skills directory")
    if config.get("isolate_device_state", True) or selected_scenarios:
        require_state_isolation(wendy)  # scenarios always use private state
    if args.resume:
        output = args.resume.resolve()
        output.mkdir(parents=True, exist_ok=True)
        (output / "halt.json").unlink(missing_ok=True)  # a pass that finishes mustn't carry the old reason
    else:
        output = args.output.resolve() / (time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6])
        output.mkdir(parents=True)
    first = not (output / "manifest.json").exists()  # a resumed pass keeps the manifest it started with
    saved = repair_rows(output / "results.jsonl") if args.resume else []
    dump(output / "coverage.json", coverage_results(coverage, saved, agents, args.repetitions))
    snapshot = {"config": config, "tasks": tasks, "seed": args.seed,
                "config_sha256": hashlib.sha256(args.config.read_bytes()).hexdigest(),
                "task_sha256": hashlib.sha256(args.tasks.read_bytes()).hexdigest()}
    snapshot["runner_sha256"] = hashlib.sha256(Path(__file__).read_bytes()).hexdigest()
    snapshot["fixtures_sha256"] = hashlib.sha256((HERE / "fixtures.py").read_bytes()).hexdigest()
    snapshot["harness_sha256"] = harness_sha256()
    snapshot["wendy_binary_sha256"] = hashlib.sha256(Path(wendy).read_bytes()).hexdigest()
    snapshot["scenarios"] = scenarios
    snapshot["scenarios_sha256"] = hashlib.sha256(scenario_file.read_bytes()).hexdigest() if args.scenario else None
    source = output / "source"
    if first:
        source.mkdir(exist_ok=True)
        for file in HERE.glob("*.py"):
            shutil.copy2(file, source / file.name)
        if (HERE / "fixture_apps").exists():
            shutil.copytree(HERE / "fixture_apps", source / "fixture_apps", ignore=shutil.ignore_patterns("__pycache__"),
                            dirs_exist_ok=True)
    treatment_root = args.treatment_root.resolve()
    if first:
        for name, origin in (("chat", "go/internal/cli/chat"), ("config", "go/internal/shared/config")):
            (source / name).mkdir(exist_ok=True)
            for file in (treatment_root / origin).glob("*.go"):
                shutil.copy2(file, source / name / file.name)
    run_process(["git", "diff", "--", "go/internal/cli/chat", "go/internal/shared/config"],
                treatment_root, output / "candidate-diff", 15)
    # "revision" identifies the runner checkout; snapshot["treatment"]["revision"] identifies the treatment.
    for name, argv in [("wendy", [wendy, "--version"]), ("revision", ["git", "rev-parse", "HEAD"])]:
        snapshot[name] = run_process(argv, ROOT, output / name, 15)
    for name in agents:
        argv = expand(config["agents"][name]["command"], {"wendy": wendy})
        snapshot[name + "_version"] = run_process([argv[0], "--version"], ROOT, output / (name + "-version"), 15)
    tools = list_all_tools(wendy, dict(os.environ))
    try:
        saved_tools = json.loads((output / "tools.json").read_text())
    except (OSError, ValueError):
        saved_tools = None
    if first or not isinstance(saved_tools, list) or not saved_tools:  # a torn file would mislead the report
        dump_atomic(output / "tools.json", tools)
    snapshot["tools_sha256"] = hashlib.sha256((output / "tools.json").read_bytes()).hexdigest()
    revision, diff_sha256 = treatment_git(treatment_root)
    snapshot["argv"], snapshot["cwd"] = sys.argv, os.getcwd()
    snapshot["treatment"] = {"root": str(treatment_root),
                             "revision": revision, "diff_sha256": diff_sha256,
                             "skills_sha256": tree_sha256(treatment_root / "plugins/wendy-agentic-coding/skills")}
    spend = config.get("prior_spend_usd", 0)
    if not isinstance(spend, (int, float)) or not math.isfinite(spend) or spend < 0:
        parser.error("prior_spend_usd must be a finite nonnegative number")
    if not disposable(config):
        for row in saved:
            if row.get("status") != "setup_error":
                spend, _ = charge(spend, row, config, quiet=True)
    run_vms, removed_vms, unprepared, shim = {}, {}, {}, None
    on_disposable = disposable(config)
    baseline, sweep_failures, halt = None, 0, None

    def sweep_env_run(action):
        state = isolated_device_state({"kind": "local"})
        try:
            return action(sc.private_env(state, keep_socket=True))
        finally:
            shutil.rmtree(state, ignore_errors=True)

    def remove_run_vms(profiles=None):
        for profile in list(run_vms) if profiles is None else profiles:
            run_vm = run_vms.pop(profile, None)
            if run_vm is None:
                continue
            result = sc.remove_run_vm(run_vm, wendy, output)
            removed_vms[profile] = {"name": run_vm["name"], "ready": run_vm["ready"], "removal": result}
            if result.get("exit_code") != 0:
                print(f"Removing the shared {profile} VM failed; its state is kept at {result.get('device_state_retained')}.")
        dump(output / "run_vms.json", removed_vms)

    if selected_scenarios or local_tasks:
        import eval_wendy
        import preflight
        if on_disposable:
            # Keep the shim off the results volume, which is kept and uploaded as evidence.
            shim_dir = Path(tempfile.mkdtemp(prefix="we-bin-"))
            atexit.register(shutil.rmtree, shim_dir, True)
        else:
            shim_dir = output / "bin"
            shim_dir.mkdir()
        shim = eval_wendy.write_shim(shim_dir)
        if on_disposable:
            failures = []  # the agent runs on the device under test, so another reachable wendy is expected
            snapshot["preflight"] = "skipped: disposable_device"
        else:
            failures = preflight.check_host(shim, dict(os.environ, PATH=str(shim.parent) + os.pathsep + os.environ.get("PATH", "")))
        if failures:
            for failure in failures:
                print(f"Host check failed: {failure}")
            print("Short scenarios run only on a host where the shim is the only reachable wendy.")
            return 2
        for name in detect:
            board = config["targets"][name]
            capabilities, evidence = sc.detect_board(wendy, board)
            dump(output / f"capabilities-{name}.json", evidence)
            if capabilities is None:
                message = f"Could not read the capabilities of {name} ({board['device']}); see {output / f'capabilities-{name}.json'}."
                print(message)
                if on_disposable:
                    dump(output / "halt.json", {"reason": message})
                    write_model_report(output)
                    return 3
                return 2
            board["capabilities"] = capabilities
            print(f"{name} capabilities: {', '.join(capabilities) or 'none of wifi, bluetooth, camera'}")
        if detect:
            coverage, plan = plan_all()
            dump(output / "coverage.json", coverage_results(coverage, [], agents, args.repetitions))
            if not plan:
                print("No runnable trials; configure the fixtures reported above.")
                return 1
        profiles = sorted({s["vm_profile"] for kind, _, target, s in plan
                           if kind == "scenario" and config["targets"][target]["kind"] == "simulator"})
        try:
            for profile in profiles:
                print(f"Preparing the shared {profile} VM (unscored)", flush=True)
                try:
                    # Registered before anything is created, so an interrupt mid-preparation still removes it.
                    run_vms[profile] = sc.prepare_run_vm(config, wendy, output, profile,
                                                         register=lambda vm, p=profile: run_vms.__setitem__(p, vm))
                    why = None if run_vms[profile]["ready"] else sc.unready_reason(run_vms[profile])
                except Exception as error:  # one profile's failure must not abort the others
                    why = f"preparation raised {error!r}"
                if why:
                    # Its attempts become setup errors, outside every denominator; the other profiles run.
                    unprepared[profile] = f"{why}; see {output / ('run-vm-' + profile)}"
                    print(f"Preparing the shared {profile} VM failed ({unprepared[profile]}); "
                          "its scenario attempts are recorded as setup errors.")
                    remove_run_vms([profile])
        except BaseException:
            remove_run_vms()
            raise
    if on_disposable:
        try:
            baseline = sweep_env_run(lambda env: sc.baseline_apps(output / "baseline_apps.json", wendy, env))
        except sc.SWEEP_ERRORS as error:
            remove_run_vms()
            message = f"Could not read the apps on the device before the pass: {error}"
            print(message)
            dump(output / "halt.json", {"reason": message})
            write_model_report(output)
            return 3
    snapshot["run_vms"] = {p: {"name": v["name"], "device": v["device"]} for p, v in run_vms.items()}
    snapshot["unprepared_run_vms"] = unprepared
    if first:
        dump(output / "manifest.json", snapshot)
    rows = list(saved)
    for row in recovered_rows(output, rows) if args.resume else []:
        with (output / "results.jsonl").open("a") as f:
            f.write(json.dumps(row) + "\n")
        rows.append(row)
    for row in interrupted_rows(output, rows) if args.resume else []:
        # An attempt that started and saved nothing: record it now, so the next start doesn't find it again.
        next(output.glob(f"*-{row['id']}")).joinpath("result.json").write_text(json.dumps(row, indent=2) + "\n")
        with (output / "results.jsonl").open("a") as f:
            f.write(json.dumps(row) + "\n")
        rows.append(row)
    total, plan = len(plan), remaining_plan(plan, rows)
    ran = []
    if args.resume:
        # Recovered rows may have emptied the plan, so the loop below would never refresh these.
        report(rows, output)
        dump(output / "coverage.json", coverage_results(coverage, rows, agents, args.repetitions))
        if on_disposable:
            write_model_report(output)
    if on_disposable and not first:
        # The interrupted attempt may have left an app or a held port; clear them before the first attempt
        # so that failure isn't scored against the model.
        sweep = sweep_env_run(lambda env: sc.sweep_apps(wendy, baseline, sc.BENCHMARK_APP, env))
        try:
            history = json.loads((output / "resume_sweeps.json").read_text())
        except OSError:
            history = []
        except ValueError:  # a torn write: the history is a log, so start it again
            history = []
        if not isinstance(history, list):
            history = []
        dump_atomic(output / "resume_sweeps.json", history + [sweep])
        if sweep["failed"] or sweep["list_error"] is not None:
            sweep_failures += 1
    try:
        for i, (kind, agent, target, task) in enumerate(plan, total - len(plan) + 1):
            reserve = config.get("scenario_reserve_usd", .5) if kind == "scenario" else config.get("trial_reserve_usd", 15)
            if spend + reserve > config.get("budget_usd", 100):
                print("Stopping at the configured budget reserve.")
                break
            if on_disposable and not wait_for_model(config):
                halt = (f"The model server did not become healthy within {MODEL_WAIT_SECONDS} s; "
                        "stopping before an attempt is scored against a dead server.")
                print(halt)
                break
            print(f"[{i}/{total}] {agent} / {target} / {task['id']} "
                  f"({'scenario, ' if kind == 'scenario' else ''}agent limit {agent_timeout(config, task):g}s)", flush=True)
            on_vm = kind == "scenario" and config["targets"][target]["kind"] == "simulator"
            if on_vm and task["vm_profile"] in unprepared:
                row = sc.unprepared_attempt(config, agent, target, task, output, unprepared[task["vm_profile"]])
            elif kind == "scenario":
                row = sc.scenario_trial(config, agent, target, task, output, wendy, shim,
                                        run_vms.get(task["vm_profile"]) if on_vm else None,
                                        output / "tools.json", sc.canned_dir_for(config["targets"][target]), treatment_root)
            else:
                row = trial(config, agent, target, task, output, wendy, treatment_root,
                            shim=shim, tools_path=output / "tools.json")
            if on_disposable:
                # Anything the attempt left behind is removed before the next one; the sweep's result is part of the row.
                sweep = sweep_env_run(lambda env: sc.sweep_apps(wendy, baseline, sc.BENCHMARK_APP, env))
                row["stages"]["sweep"] = sweep
                swept_ok = not sweep["failed"] and sweep["list_error"] is None
                row["cleanup_ok"] &= swept_ok
                sweep_failures = 0 if swept_ok else sweep_failures + 1
            rows.append(row)
            ran.append(row)
            with (output / "results.jsonl").open("a") as f:
                f.write(json.dumps(row) + "\n")
            report(rows, output)
            dump(output / "coverage.json", coverage_results(coverage, rows, agents, args.repetitions))
            if on_disposable:
                write_model_report(output)
            reason = stop_reason(row, config)
            if reason is None and on_disposable and sweep_failures >= 2:
                reason = "The app sweep failed twice running; stopping before more leftovers pile up."
            if reason:
                print(reason)
                if on_disposable:
                    halt = reason
                break
            if row["status"] != "setup_error" and not on_disposable:
                spend, _ = charge(spend, row, config)
            print(f"  {row['status']} ({row['task_seconds']:.1f}s)", flush=True)
    finally:
        if run_vms:
            remove_run_vms()
    if halt:
        dump(output / "halt.json", {"reason": halt})
        write_model_report(output)
    print(f"Results: {output / 'summary.md'}")
    if halt:
        return 3
    if on_disposable:  # the runner records only exit 0 as finished; attempt outcomes are in the report
        return 0 if len(ran) == len(plan) else 1
    configured = all(entry["status"] == "ready" for entry in coverage)
    return 0 if configured and len(ran) == len(plan) and all(r["success"] and r["cleanup_ok"] for r in ran) else 1


if __name__ == "__main__":
    sys.exit(main())
