"""Read a pass's saved logs and write report.md and report.json: what the model did wrong, kept apart from
what Wendy's tools did wrong. No model calls, no network. Python standard library only.

    python3 model_report.py DIR [DIR...] [--out DIR]
"""
import argparse
import json
import os
import re
import sys
import tempfile
from collections import Counter
from pathlib import Path

import benchmark as b
import grading
import scenarios as sc
import tool_trace as tt

SECTIONS = (("model", "Model deviations"), ("tool", "Unexpected tool behavior"),
            ("undetermined", "Undetermined"))
SECTION_ORDER = {name: i for i, (name, _) in enumerate(SECTIONS)}
NOT_COUNTED = {
    "context_exceeded": "the prompt did not fit the model's context",
    "model_server_down": "the model server stopped answering",
    "interrupted": "the attempt was cut off (restart or stop)",
    "timeout": "the agent hit its time limit on the device",
    "setup_error": "the harness could not set the attempt up",
}
WARN_BELOW = 0.8
CODE = re.compile(r'^\s*\[([A-Z][A-Z0-9_]*)\]|"error_code"\s*:\s*"([A-Za-z0-9_]+)"')
ROUNDS = re.compile(r"stopped after \d+ model rounds")
CHECK_COMMANDS = {"doctor", "json validate"}  # a nonzero exit is their answer: the checks found a problem
USAGE_EXIT = 2
FINDING_KEYS = ("section", "item", "task", "expected", "detail", "excerpt", "trace_path")
# The model shares the container with the harness, so nothing stops it reading the scenarios, the fixtures, the
# saved passes, or calling the real CLI the recording wrapper runs. The Dockerfile's layout, unless on_device.json
# names other paths.
BENCHMARK_FILES = ("/opt/harness", "/results")
REAL_CLI = "/usr/local/bin/wendy"


def _command_text(entry):
    """The command a shell entry or a container_exec call ran, as text; None for any other call."""
    if entry.get("channel") == "shell":
        return str(entry.get("arguments"))
    arguments = entry.get("arguments")
    if entry.get("channel") == "mcp" and entry.get("name") == "container_exec" and isinstance(arguments, dict):
        command = arguments.get("command")
        return " ".join(map(str, command)) if isinstance(command, list) else str(command or "")
    return None


def _usable_path(path):
    """Absolute and not the root: a relative name or `/` would match nearly every command."""
    return isinstance(path, str) and path.startswith("/") and path.strip("/") != ""


def _names_path(command, path):
    """`command` names `path` itself or something under it, not a longer name or a URL ending the same way."""
    return re.search(rf"(?<![\w.:/-]){re.escape(path.rstrip('/'))}(?![\w.-])", command) is not None


def scored_share(rows):
    """The share of attempts that count toward the model's score; 0 when there are none."""
    rows = [r for r in rows if isinstance(r, dict)]
    return sum(b.scored(r) for r in rows) / len(rows) if rows else 0.0


def _dict(value):
    return value if isinstance(value, dict) else {}


def _read_json(path, default):
    try:
        return json.loads(Path(path).read_text(errors="replace"))
    except (OSError, ValueError):
        return default


def _records(path):
    return tt.read_jsonl(path) if Path(path).exists() else []


def attempt_dir(directory, row):
    found = sorted(Path(directory).glob(f"*-{row.get('id')}")) if row.get("id") else []
    return found[0] if found else Path(directory)


def _clip(text, n=100):
    text = " ".join(str(text).split())
    return text if len(text) <= n else text[:n - 1] + "..."


def _first_line(text):
    lines = str(text or "").strip().splitlines()
    return lines[0].strip() if lines else ""


def _line(entry):
    arguments = entry.get("arguments")
    shown = arguments if isinstance(arguments, str) else json.dumps(arguments, sort_keys=True, default=str)
    text = f"{entry.get('channel')} {entry.get('name')} {_clip(shown, 80)}"
    if entry.get("canned"):
        return text + " -> canned"
    if entry.get("is_error"):
        return text + f" -> error: {_clip(_first_line(entry.get('reply_text')), 100)}"
    if entry.get("is_error") is False:
        return text + " -> ok"
    return text


def _describe(matcher):
    kind = next((k for k in grading.KINDS if k in matcher), None)
    return f"{kind} {matcher[kind]}" if kind else str(matcher)


def _expected(expect):
    names = [_describe(m) for m in expect.get("right", []) if isinstance(m, dict)]
    if expect.get("ask"):
        names.append("asking the user")
    return " or ".join(names) if names else "no specific tool listed"


def _key(entry):
    return (entry.get("channel"), entry.get("name"), json.dumps(entry.get("arguments"), sort_keys=True, default=str))


def _error_code(text):
    match = CODE.search(text or "")
    return (match.group(1) or match.group(2)) if match else "error"


def _errored(entry, cli_records):
    """True when the call failed, False when it succeeded, None when nothing says."""
    if entry.get("channel") == "mcp":
        return entry.get("is_error")
    if entry.get("channel") == "cli":
        codes = [r.get("exit_code") for r in cli_records if r.get("argv") == entry.get("arguments")]
        return bool(codes[-1]) if codes and codes[-1] is not None else None
    return None


def _chronological(trace, events):
    """The trace in the order the model made the calls. build_trace lists transcript entries first, then
    every proxy entry, then CLI entries, so it is not chronological; the transcript is. Each transcript
    MCP call takes the proxy entry with the same name and arguments, to show its reply."""
    if not any(e.get("type") == "tool_start" for e in events):
        return trace
    unused = [e for e in trace if e.get("channel") == "mcp" and e.get("source") == "proxy"]
    ordered = []
    for group in tt.wendy_entries(events):
        for e in group:
            if e["channel"] == "mcp":
                match = next((u for u in unused if _key(u) == _key(e)), None)
                if match is not None:
                    unused.remove(match)
                    e = match
            ordered.append(e)
    return ordered + unused


def _unanswered(events):
    """Names of Wendy tools the transcript started and never saw a result for (chat's own tools are left out)."""
    answered = {(e.get("agent_id", ""), e.get("id")) for e in events if e.get("type") == "tool_result"}
    names = []
    for index, e in enumerate(events):
        if e.get("type") == "tool_start" and (e.get("agent_id", ""), e.get("id", f"#{index}")) not in answered:
            if e.get("tool") in tt.CHAT_BUILTINS or e.get("tool") == "workspace_exec":
                continue  # still running at the time limit: the harness's limit, not a tool that hung
            if e.get("tool") not in names:
                names.append(e.get("tool"))
    return names


def _restarted(row, previous):
    now, before = _dict(row.get("device_markers")), _dict(previous and previous.get("device_markers"))
    if not now or not before:
        return False
    if now.get("socket_inode") is not None and before.get("socket_inode") is not None \
            and now["socket_inode"] != before["socket_inode"]:
        return True
    try:
        return float(now["uptime"]) < float(before["uptime"])
    except (KeyError, TypeError, ValueError):
        return False


def _cleanup_detail(row):
    stages = _dict(row.get("stages"))
    sweep, cleanup, reset = (_dict(stages.get(k)) for k in ("sweep", "cleanup", "reset"))
    if sweep.get("failed") or sweep.get("list_error") is not None:
        return "the app sweep after the attempt failed"
    if cleanup.get("exit_code") not in (None, 0):
        return f"the task's cleanup exited {cleanup['exit_code']}"
    if reset.get("exit_code") not in (None, 0):
        return "the device's app list changed during the attempt"
    return "cleanup did not finish cleanly"


def classify(row, directory, tool_names, guards, previous=None, benchmark_files=BENCHMARK_FILES, real_cli=REAL_CLI):
    """The findings for one attempt. Limits the model didn't cause (context, server, interruption, a local
    timeout) get no model finding. `previous` is the row before this one, for the restart check."""
    adir = attempt_dir(directory, row)
    task_file = _dict(_read_json(adir / "task.json", {}))
    expect = _dict(task_file.get("expect"))
    task = row.get("task") or "unknown"
    metrics = _dict(row.get("metrics"))
    trace = [e for e in (metrics.get("tool_trace") or []) if isinstance(e, dict)]
    cli_records = [r for r in _records(adir / "cli.jsonl")]
    path = adir / "agent.stdout"
    events = b.read_events(path)[0] if path.exists() else []
    ordered = _chronological(trace, events)
    final_text = metrics.get("final_text") or ""
    findings = []

    def add(section, item, detail, index=None, argv=None):
        # The excerpt is cut, in transcript order, at trace position `index` or at the CLI call `argv`.
        if argv is not None:  # the transcript misses a CLI call hidden behind `timeout`; the shell line still shows it
            at = next((i for i, e in enumerate(ordered) if e.get("channel") == "cli" and e.get("arguments") == argv),
                      next((i for i, e in enumerate(ordered)
                            if e.get("channel") == "shell" and " ".join(argv) in str(e.get("arguments"))), None))
        elif index is not None:  # the entry itself, or the first call like it when the transcript has its own copy
            at = next((i for i, e in enumerate(ordered) if e is trace[index]),
                      next((i for i, e in enumerate(ordered) if _key(e) == _key(trace[index])), None))
        else:
            at = None
        window = ordered[:at + 1] if at is not None else ordered
        excerpt = "\n".join(_line(e) for e in window[-6:]) or (f"The model said: {_clip(final_text, 300)}" if final_text else "")
        findings.append({"section": section, "item": item, "task": task, "expected": _expected(expect),
                         "detail": detail, "excerpt": excerpt, "trace_path": str(adir)})

    if row.get("cleanup_ok") is False:
        add("tool", "Cleanup failed", _cleanup_detail(row))
    if _restarted(row, previous):
        add("tool", "Agent or device restart", "the agent socket or the device uptime changed since the previous attempt")
    counted = b.scored(row)
    timed_out = row.get("status") == "timeout" or bool(_dict(_dict(row.get("stages")).get("agent")).get("timed_out"))
    if not counted and row.get("status") != "timeout":
        return findings
    hung = set()
    for i, e in enumerate(trace):
        if e.get("channel") == "mcp" and e.get("source") == "proxy" and not e.get("canned") and e.get("is_error") is None:
            add("tool", "Hang", f"{e.get('name')} never answered", i)
            hung.add(e.get("name"))
    if timed_out:
        # The proxy logs unanswered calls only when it exits cleanly, and a timeout kills it first.
        for name in _unanswered(events):
            if name not in hung:
                add("tool", "Hang", f"{name} never answered", None)
    if not counted:
        return findings

    adapter = row.get("adapter")
    outcome = _dict(row.get("tool_choice")).get("outcome")
    metrics_errors = metrics.get("agent_errors")
    stopped = _dict(_dict(_dict(row.get("stages")).get("agent")).get("no_progress"))
    error_text = next((str(e.get("text")) for e in events if e.get("type") == "error" and e.get("text")), None)
    if error_text is None and metrics_errors:
        error_text = str(metrics_errors[0] if isinstance(metrics_errors, list) else metrics_errors)
    ran_out = bool(metrics_errors and error_text and ROUNDS.search(error_text))
    unjudged = None
    if row.get("status") == "mcp_unverified":
        unjudged = "status mcp_unverified: the model's tool calls never reached the proxy"
    elif stopped:  # the harness stopped chat, so chat's own "context canceled" error says nothing about the model
        add("model", "Hit the call limit", f"the harness stopped it at its limit of {stopped.get('limit')} tool calls")
    elif ran_out:
        add("model", "Ran out of rounds", ROUNDS.search(error_text).group(0))
    elif metrics_errors:
        unjudged = f"the agent reported an error: {_clip(error_text, 100)}"
    elif metrics.get("agent_completed") is False and row.get("status") != "timeout":
        unjudged = "the agent did not finish its turn"
    if unjudged:
        add("undetermined", "Undetermined", unjudged)
    asked = outcome == "missed" and "?" in final_text and not expect.get("ask")
    verifier_failed = row.get("kind", "task") == "task" and row.get("outcome_verified") is False

    blame = unjudged is None
    if not blame:
        outcome = None
    if outcome == "wrong":
        entry = _dict(row.get("tool_choice")).get("entry")
        e = trace[entry] if isinstance(entry, int) and 0 <= entry < len(trace) else {}
        add("model", "Wrong tool", f"called {e.get('name') or 'a forbidden path'}", entry if e else None)
    elif outcome == "went_around":
        plain = next((i for i, e in enumerate(trace) if grading._plain_shell(e)), None)
        add("model", "Went around", "ran a plain shell command instead of a Wendy tool", plain)
    elif asked:
        add("model", "Asked instead of acting", "ended on a question without doing the task")
    elif outcome == "missed" and not verifier_failed:
        if expect.get("ask") and not expect.get("right"):
            add("model", "Did not ask first", "this request needs the user's confirmation, and it didn't ask")
        else:
            add("model", "Missing tool", f"did none of: {_expected(expect)}")

    known = set(tool_names or ()) | tt.CHAT_BUILTINS | {"workspace_exec"}
    made_up = []
    for group in tt.wendy_entries(events) if tool_names is not None else ():  # None: no list to check against
        for e in group:
            if e["channel"] == "mcp" and e["name"] not in known and e["name"] not in made_up:
                made_up.append(e["name"])
    for name in made_up if blame else []:
        add("model", "Made-up tool", name)

    for i, e in enumerate(trace):
        if e.get("canned") and blame and any(grading.matches(m, e, adapter) for m in guards):
            add("model", "Touched the benchmark", f"{e.get('name')} was blocked by the device guard", i)
        elif e.get("channel") == "mcp" and not e.get("canned") and e.get("is_error") is True:
            text = e.get("reply_text") or ""
            refused = next((code for code in ("INVALID_ARGUMENT", "UNSUPPORTED") if code in text), None)
            if refused:  # the model asked for something the tool refuses
                if blame:
                    add("model", "Bad arguments", f"{e.get('name')} [{refused}]", i)
            elif _error_code(text) != "COMMAND_FAILED":  # that one is the model's own command exiting nonzero
                add("tool", "Tool error", f"{e.get('name')} [{_error_code(text)}]", i)
    for paths, detail in ((benchmark_files, f"read the harness's own files ({', '.join(benchmark_files)})"),
                          ((real_cli,), f"went past the recording wrapper to the real CLI at {real_cli}")):
        hit = next((i for i, e in enumerate(trace) if _command_text(e) is not None
                    and any(_names_path(_command_text(e), p) for p in paths)), None)
        if blame and hit is not None:  # once per attempt: the excerpt shows the first time
            add("model", "Read the benchmark", detail, hit)
    for record in cli_records:
        code = record.get("exit_code")
        if code in (None, 0) or record.get("canned"):
            continue
        argv = record.get("argv") if isinstance(record.get("argv"), list) else []
        path = " ".join(w for w in tt.cli_path(argv) if "/" not in w and w != ".")  # no temp paths in the grouping
        if code == USAGE_EXIT:
            if blame:
                add("model", "Bad arguments", f"wendy {path} [exit {code}]", argv=argv)
        elif (isinstance(code, int) and (code < 0 or code >= 128)) or path in CHECK_COMMANDS:
            continue  # killed by a signal, usually the model's own `| head`; or a check reporting what it found
        else:
            add("tool", "Tool error", f"wendy {path} [exit {code}]", argv=argv)

    start = 0
    while blame and start < len(ordered):
        end = start
        while end + 1 < len(ordered) and _key(ordered[end + 1]) == _key(ordered[start]):
            end += 1
        if end - start + 1 >= 3:
            add("model", "Repeated call", f"{ordered[start].get('name')} x{end - start + 1} with the same arguments",
                trace.index(ordered[end]) if any(e is ordered[end] for e in trace) else None)
        start = end + 1

    if blame and verifier_failed and outcome not in ("wrong", "went_around") and not asked:
        rights = [m for m in expect.get("right", []) if isinstance(m, dict)]
        if not rights:
            add("undetermined", "Undetermined", "the verifier failed and the task lists no expected tool")
        else:
            hits = [i for i, e in enumerate(trace)
                    if not e.get("canned") and any(grading.matches(m, e, adapter) for m in rights)]
            if stopped or ran_out:
                add("model", "Task not finished", "it was stopped before the verifier passed")
            elif not hits:
                add("model", "Task not finished", f"the verifier failed and {_expected(expect)} was never called")
            else:
                failed = _errored(trace[hits[-1]], cli_records)
                name = trace[hits[-1]].get("name")
                if failed is True:
                    add("model", "Task not finished", f"{name} replied with an error and the model went on", hits[-1])
                elif failed is False:
                    add("tool", "Said success, state wrong", f"{name} replied success but the verifier failed", hits[-1])
                else:
                    add("undetermined", "Undetermined", f"the verifier failed and {name} left no reply to judge", hits[-1])
    return findings


def group_findings(findings, attempts):
    """Findings of one pattern, once per attempt that showed it, most frequent first."""
    groups = {}
    for f in findings:
        g = groups.setdefault((f["section"], f["item"], f["detail"]), {
            "section": f["section"], "item": f["item"], "detail": f["detail"], "expected": f["expected"],
            "excerpt": f["excerpt"], "tasks": [], "trace_paths": [], "attempts": attempts})
        if f["trace_path"] not in g["trace_paths"] or f["task"] not in g["tasks"]:
            if f["trace_path"] not in g["trace_paths"]:
                g["trace_paths"].append(f["trace_path"])
            if f["task"] not in g["tasks"]:
                g["tasks"].append(f["task"])
    result = list(groups.values())
    for g in result:
        g["count"] = len(g["trace_paths"])
    return sorted(result, key=lambda g: (-g["count"], SECTION_ORDER[g["section"]], g["item"], g["detail"]))


def load_run(directory):
    directory = Path(directory)
    rows = [r for r in b.load_rows(directory / "results.jsonl")]
    info = _dict(_read_json(directory / "on_device.json", {}))
    listed = _read_json(directory / "tools.json", [])
    tool_names = [t.get("name") for t in listed if isinstance(t, dict)] if isinstance(listed, list) else []
    tool_names = tool_names or None  # unreadable or empty: any name would look made up
    guards = sc.guard_matchers(info.get("app_id") or sc.BENCHMARK_APP)
    paths = _dict(info.get("benchmark_paths"))
    files = paths.get("files") if isinstance(paths.get("files"), list) else []
    files = [p for p in files if _usable_path(p)] or BENCHMARK_FILES
    real_cli = paths.get("real_cli") if _usable_path(paths.get("real_cli")) else REAL_CLI
    findings, previous = [], None
    if tool_names is None and rows:
        findings.append({"section": "undetermined", "item": "Undetermined", "task": "", "expected": "",
                         "detail": "tools.json was unreadable or empty, so made-up tools were not checked",
                         "excerpt": "", "trace_path": str(directory / "tools.json")})
    for row in rows:
        try:
            findings += classify(row, directory, tool_names, guards, previous=previous,
                                 benchmark_files=tuple(files), real_cli=real_cli)
        except Exception as error:  # one odd row must not hide the rest of the pass
            findings.append({"section": "undetermined", "item": "Undetermined", "task": str(row.get("task")),
                             "expected": "", "detail": f"could not classify this attempt: {error!r}",
                             "excerpt": "", "trace_path": str(attempt_dir(directory, row))})
        if row.get("device_markers"):  # a row without markers (an interrupted attempt) is no baseline
            previous = row
    manifest = _dict(_read_json(directory / "manifest.json", {}))
    halt = _read_json(directory / "halt.json", None)
    model = info.get("model") or next((r.get("model_requested") for r in rows if r.get("model_requested")), None)
    return {"name": model or directory.name, "directory": str(directory), "info": info,
            "treatment_revision": _dict(manifest.get("treatment")).get("revision"),
            "halt": _dict(halt).get("reason", "no reason recorded") if halt is not None else None,
            "rows": rows, "findings": findings, "coverage": _read_json(directory / "coverage.json", []),
            "groups": group_findings(findings, len(rows))}


def _fmt(value):
    if value is None or value == "":
        return "-"
    if isinstance(value, dict):
        return ", ".join(f"{k}={v}" for k, v in value.items()) or "-"
    return str(value).replace("|", "/")


def _wall_minutes(directory):
    """Minutes from the first start (the manifest is written once) to the last saved row; None if either is missing."""
    try:
        seconds = (Path(directory) / "results.jsonl").stat().st_mtime - (Path(directory) / "manifest.json").stat().st_mtime
    except OSError:
        return None
    return f"{max(seconds, 0) / 60:.0f} min"


def _header_rows(run):
    info, rows = run["info"], run["rows"]
    def pick(*keys):
        return next((info[k] for k in keys if info.get(k) not in (None, "")), None)
    return [("Model", run["name"]), ("Revision", pick("revision")), ("Server settings", pick("server", "server_settings")),
            ("Context", pick("context")), ("Baseline prompt tokens", pick("baseline_prompt_tokens")),
            ("Tokens per second", pick("tokens_per_second", "speeds")),
            ("Wendy CLI commit", pick("cli_commit") or run["treatment_revision"]),
            ("Agent version", pick("agent_version")), ("OS version", pick("os_version")),
            ("Device", pick("device", "device_type")),
            ("Duration", _wall_minutes(run["directory"])),
            ("Agent time", f"{sum(r.get('task_seconds') or 0 for r in rows) / 60:.0f} min"),
            ("Attempts run", len(rows)),
            ("Attempts not run", sum(a.get("not_run", 0) for e in run["coverage"] if isinstance(e, dict)
                                     for a in _dict(e.get("agents")).values() if isinstance(a, dict))),
            ("Scored", f"{sum(b.scored(r) for r in rows)} of {len(rows)} ({scored_share(rows):.0%})")]


def _not_counted(run):
    by_status = {}
    for r in run["rows"]:
        status = r.get("status")
        if status in NOT_COUNTED and not b.scored(r):
            by_status.setdefault(status, []).append(r.get("task") or "unknown")
    return by_status


def render_markdown(runs):
    lines = ["# Model tool-use report", ""]
    for run in runs:
        prefix = f"{run['name']}: " if len(runs) > 1 else ""
        if run["halt"] is not None:
            lines += [f"**{prefix}This pass halted:** {run['halt']}", ""]
        if run["rows"] and scored_share(run["rows"]) < WARN_BELOW:
            lines += [f"**{prefix}Only {scored_share(run['rows']):.0%} of attempts were scored.** The rest hit "
                      "a limit the model didn't cause, so the findings below describe a small sample.", ""]
    headers = [_header_rows(r) for r in runs]
    lines += ["| | " + " | ".join(_fmt(h[0][1]) for h in headers) + " |", "| --- |" + " --- |" * len(runs)]
    for i in range(1, len(headers[0]) if headers else 0):
        lines.append(f"| {headers[0][i][0]} | " + " | ".join(_fmt(h[i][1]) for h in headers) + " |")
    lines += ["", "Model deviations are the model's own mistakes. Unexpected tool behavior is Wendy's tools, the agent "
              "or the device, and is never blamed on the model. Limits are listed separately and count for nothing.", ""]
    notes = []
    for run in runs:
        found = run["info"].get("notes")
        notes += [f"{(run['name'] + ': ') if len(runs) > 1 else ''}{n}"
                  for n in (found if isinstance(found, list) else []) if isinstance(n, str)]
    if notes:
        lines += ["## Notes", ""] + [f"- {n}" for n in notes] + [""]
    for section, title in SECTIONS:
        lines += [f"## {title}", ""]
        keys = {}
        for run in runs:
            for g in run["groups"]:
                if g["section"] == section:
                    keys.setdefault((g["item"], g["detail"]), 0)
                    keys[(g["item"], g["detail"])] += g["count"]
        if not keys:
            lines += ["None found.", ""]
            continue
        for (item, detail), _ in sorted(keys.items(), key=lambda kv: (-kv[1], kv[0])):
            matches = [(run, g) for run in runs for g in run["groups"]
                       if (g["section"], g["item"], g["detail"]) == (section, item, detail)]
            counts = "; ".join(
                (f"{run['name']} " if len(runs) > 1 else "") +
                f"{next((g['count'] for r, g in matches if r is run), 0)} of {len(run['rows'])} attempts"
                for run in runs)
            first = matches[0][1]
            lines += [f"### {item}: {detail} ({counts})", "",
                      f"- Expected: {first['expected']}", f"- Tasks: {', '.join(sorted({t for _, g in matches for t in g['tasks']}))}",
                      f"- Full trace: {first['trace_paths'][0]}", "", "```", first["excerpt"] or "(no trace entries)", "```", ""]
    lines += ["## Not counted against the model", ""]
    shown = False
    for run in runs:
        for status, tasks in sorted(_not_counted(run).items()):
            shown = True
            names = ", ".join(f"{t} x{n}" if n > 1 else t for t, n in sorted(Counter(tasks).items()))
            lines.append(f"- {(run['name'] + ': ') if len(runs) > 1 else ''}{status} ({len(tasks)}): "
                         f"{NOT_COUNTED[status]}. Tasks: {names}")
    lines += ["" if shown else "None.", ""] if not shown else [""]
    lines += ["## Not run", ""]
    shown = False
    for run in runs:
        who = (run["name"] + ": ") if len(runs) > 1 else ""
        for e in run["coverage"] if isinstance(run["coverage"], list) else []:
            if not isinstance(e, dict):
                continue
            if e.get("status") != "ready":
                shown = True
                where = f" on {e['target']}" if e.get("target") else ""
                lines.append(f"- {who}{e.get('task')}{where}: {e.get('reason') or e.get('status')}")
                continue
            left = sum(a.get("not_run", 0) for a in _dict(e.get("agents")).values() if isinstance(a, dict))
            if left:
                shown = True
                lines.append(f"- {who}{e.get('task')} on {e.get('target')}: {left} attempts not reached")
    lines += [""] if shown else ["Everything configured was run.", ""]
    return "\n".join(lines).rstrip("\n") + "\n"


def _json_run(run):
    keep = ("name", "directory", "info", "halt", "treatment_revision", "coverage", "groups")
    return {**{k: run[k] for k in keep}, "attempts": len(run["rows"]), "scored_share": scored_share(run["rows"]),
            "not_counted": _not_counted(run)}


def _replace(path, text):
    handle, temp = tempfile.mkstemp(dir=path.parent, prefix=f".{path.name}.", suffix=".tmp")
    try:
        with os.fdopen(handle, "w", errors="backslashreplace") as f:
            f.write(text)
        os.replace(temp, path)
    except BaseException:
        Path(temp).unlink(missing_ok=True)
        raise


def write_report(dirs, out):
    runs = [load_run(d) for d in dirs]
    out = Path(out)
    _replace(out / "report.md", render_markdown(runs))
    _replace(out / "report.json", json.dumps({"runs": [_json_run(r) for r in runs]}, indent=2) + "\n")


def main(argv=None):
    parser = argparse.ArgumentParser(description="Write report.md and report.json from saved pass directories.")
    parser.add_argument("dirs", nargs="+", type=Path)
    parser.add_argument("--out", type=Path, help="where to write the report (default: the first directory)")
    args = parser.parse_args(argv)
    out = args.out or args.dirs[0]
    write_report(args.dirs, out)
    print(f"Report: {out / 'report.md'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
