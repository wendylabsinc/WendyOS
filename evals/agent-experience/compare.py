"""Compare a candidate benchmark run against a baseline run.

Flags follow fixed rules so a quiet report can be trusted: tool choice (every candidate attempt is
not good where every baseline attempt was), tokens and time (candidate median beyond the baseline
maximum plus a margin, with at least two known values per side). Unknown usage is counted, never
treated as zero. Python standard library only.
"""
import argparse
import collections
import json
from pathlib import Path
import statistics
import subprocess
import sys
import time
import uuid

from benchmark import SCENARIO_NOT_SUCCESS

OUTCOMES = ("right", "wrong", "missed", "asked")
TRACKED = ("runner_sha256", "harness_sha256", "task_sha256", "scenarios_sha256")


def _read_json(path, default=None):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError):
        return default


def _read_rows(path):
    rows = []
    try:
        lines = Path(path).read_text().splitlines()
    except OSError:
        return rows
    for line in lines:
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if isinstance(item, dict):
            rows.append(item)
    return rows


def load_run(directory):
    directory = Path(directory)
    rescored = sorted(directory.glob("rescored-*/results.jsonl"))
    source = rescored[-1] if rescored else directory / "results.jsonl"
    tools = _read_json(directory / "tools.json", [])
    if isinstance(tools, dict):
        tools = tools.get("tools", [])
    return {"dir": directory, "rows": _read_rows(source), "manifest": _read_json(directory / "manifest.json", {}),
            "tools": tools}


def _key(row):
    return ("scenario" if row.get("kind") == "scenario" else "task", row.get("task"), row.get("target"), row.get("agent"))


def _tokens(row):
    metrics = row.get("metrics") or {}
    tokens = metrics.get("tokens")
    if not metrics.get("usage_complete") or not isinstance(tokens, dict):
        return None
    try:
        return tokens["input_tokens"] + tokens["output_tokens"]
    except (KeyError, TypeError):
        return None


def _seconds(row):
    value = ((row.get("stages") or {}).get("agent") or {}).get("seconds")
    return value if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def _median(values):
    return statistics.median(values) if values else None


def side_stats(rows):
    setup_errors = sum(r.get("status") == "setup_error" for r in rows)
    rows = [r for r in rows if r.get("status") != "setup_error"]
    outcomes = {k: 0 for k in OUTCOMES}
    outcomes["ungraded"] = 0
    for r in rows:
        outcome = (r.get("tool_choice") or {}).get("outcome")
        outcomes[outcome if outcome in OUTCOMES else "ungraded"] += 1
    tokens = [t for t in map(_tokens, rows) if t is not None]
    seconds = [s for s in map(_seconds, rows) if s is not None]
    # Good as scenario_verdict counts success: right or asked, and not ended by a status that rules it out.
    good = sum((r.get("tool_choice") or {}).get("outcome") in ("right", "asked")
               and r.get("status") not in SCENARIO_NOT_SUCCESS for r in rows)
    calls = [(r.get("metrics") or {}).get("tool_calls") for r in rows]
    calls = [x for x in calls if isinstance(x, int) and not isinstance(x, bool)]
    used = set()
    for r in rows:
        for e in ((r.get("metrics") or {}).get("tool_trace") or []):
            if isinstance(e, dict) and e.get("channel") in ("mcp", "cli") and e.get("name"):
                used.add(e["name"])
    return {"attempts": len(rows), "setup_errors": setup_errors, "outcomes": outcomes, "good": good,
            "median_tokens": _median(tokens), "max_tokens": max(tokens, default=None),
            "known_usage": len(tokens), "unknown_usage": len(rows) - len(tokens),
            "median_seconds": _median(seconds), "max_seconds": max(seconds, default=None),
            "known_seconds": len(seconds), "median_tool_calls": _median(calls), "tools": sorted(used)}


def _get(manifest, path):
    node = manifest
    for part in path:
        node = node.get(part) if isinstance(node, dict) else None
    return node


def _version_text(run, agent):
    try:
        return (run["dir"] / f"{agent}-version.stdout").read_text().strip()
    except OSError:
        return None


def _mismatches(baseline, candidate, rows):
    found = []
    bm, cm = baseline["manifest"], candidate["manifest"]
    for name in TRACKED:
        if bm.get(name) != cm.get(name):
            found.append(name)
    agents = sorted({k[3] for k in rows if k[3]})
    targets = sorted({k[2] for k in rows if k[2]})
    for agent in agents:
        if _version_text(baseline, agent) != _version_text(candidate, agent):
            found.append(f"{agent}_version.stdout")
        if _get(bm, ("config", "agents", agent, "model")) != _get(cm, ("config", "agents", agent, "model")):
            found.append(f"config.agents.{agent}.model")
    for target in targets:
        for field in ("kind", "device"):
            if _get(bm, ("config", "targets", target, field)) != _get(cm, ("config", "targets", target, field)):
                found.append(f"config.targets.{target}.{field}")
    return found


def _metric_flag(base, cand, margin, count, median, maximum):
    if base[count] < 2 or cand[count] < 2:
        return "insufficient"
    return "flag" if cand[median] > base[maximum] * (1 + margin) else "ok"


def _changed_tools(scenario, baseline, candidate):
    def index(run):
        return {t.get("name"): t for t in run["tools"] if isinstance(t, dict)}
    b, n = index(baseline), index(candidate)
    names = sorted(set(scenario.get("captures") or []) | set(scenario.get("canned") or []))
    out = []
    for name in names:
        x, y = b.get(name), n.get(name)
        if x is None and y is None:
            continue
        if any((x or {}).get(f) != (y or {}).get(f) for f in ("description", "inputSchema")):
            out.append(name)
    return out


def _rollup(items):
    totals = collections.defaultdict(lambda: {"baseline": {k: 0 for k in OUTCOMES}, "candidate": {k: 0 for k in OUTCOMES}})
    for name, side, stats in items:
        for k in OUTCOMES:
            totals[name][side][k] += stats["outcomes"][k]
    return dict(sorted(totals.items()))


def compare(baseline, candidate, scenarios, margin=.25):
    groups_by_row = collections.defaultdict(lambda: {"baseline": [], "candidate": []})
    for side, run in (("baseline", baseline), ("candidate", candidate)):
        for r in run["rows"]:
            groups_by_row[_key(r)][side].append(r)
    known = {s["id"]: s for s in scenarios or []}
    for run in (baseline, candidate):
        for s in (run["manifest"].get("scenarios") or []):
            if isinstance(s, dict) and "id" in s:
                known[s["id"]] = s
    result_rows, group_items, agent_items = [], [], []
    for key in sorted(groups_by_row, key=lambda k: tuple(str(x) for x in k)):
        kind, task, target, agent = key
        sides = groups_by_row[key]
        b, n = side_stats(sides["baseline"]), side_stats(sides["candidate"])
        one_sided = not sides["baseline"] or not sides["candidate"]
        flags, insufficient = [], []
        if not one_sided:
            if b["attempts"] > 0 and b["good"] == b["attempts"] and n["attempts"] > 0 and n["good"] == 0:
                flags.append("tool_choice")
            for name, args in (("tokens", ("known_usage", "median_tokens", "max_tokens")),
                               ("time", ("known_seconds", "median_seconds", "max_seconds"))):
                verdict = _metric_flag(b, n, margin, *args)
                if verdict == "flag":
                    flags.append(name)
                elif verdict == "insufficient":
                    insufficient.append(name)
        scenario = known.get(task) if kind == "scenario" else None
        group = (scenario or {}).get("group") or next((r.get("group") for r in sides["baseline"] + sides["candidate"] if r.get("group")), None)
        result_rows.append({
            "kind": kind, "task": task, "target": target, "agent": agent, "group": group,
            "baseline": b, "candidate": n, "flags": flags, "insufficient": insufficient,
            "tools_added": sorted(set(n["tools"]) - set(b["tools"])),
            "tools_removed": sorted(set(b["tools"]) - set(n["tools"])),
            "canned_tools_changed": _changed_tools(scenario, baseline, candidate) if scenario else [],
            "one_sided": one_sided})
        for side, stats in (("baseline", b), ("candidate", n)):
            group_items.append((group or "end-to-end", side, stats))
            agent_items.append((agent, side, stats))
    return {"margin": margin, "mismatches": _mismatches(baseline, candidate, groups_by_row),
            "rows": result_rows, "groups": _rollup(group_items), "agents": _rollup(agent_items)}


def _rwma(stats):
    errors = stats.get("setup_errors", 0)
    plural = "s" if errors != 1 else ""
    if not stats["attempts"]:
        return f"no scored attempts ({errors} setup error{plural})" if errors else "-"
    o = stats["outcomes"]
    text = "/".join(f"{o[k]}{k[0].upper()}" for k in OUTCOMES) + f" of {stats['attempts']}"
    return text + (f" ({errors} setup error{plural})" if errors else "")


def _cell(value):
    return str(value).replace("|", "\\|")


def _pair(b, c, unknown_b, unknown_c):
    def one(v, unknown):
        text = "unknown" if v is None else f"{v:g}"
        return text + (f" (+{unknown} unknown)" if unknown and v is not None else "")
    return f"{one(b, unknown_b)} → {one(c, unknown_c)}"


def render_markdown(result):
    lines = ["# Tool-choice comparison", ""]
    if result["mismatches"]:
        lines += ["**Runs differ in fields that should match:** " + ", ".join(f"`{m}`" for m in result["mismatches"]), ""]
    lines += ["| Task | Agent | Target | Baseline R/W/M/A | Candidate R/W/M/A | Tokens (b → c) | Seconds (b → c) | Flags |",
              "| --- | --- | --- | --- | --- | --- | --- | --- |"]
    for r in result["rows"]:
        b, n = r["baseline"], r["candidate"]
        flags = [f + (" (confirmed)" if f in r.get("confirmed", []) else
                      f" (unconfirmed: {r['unconfirmed'][f]})" if f in r.get("unconfirmed", {}) else "")
                 for f in r["flags"]]
        if r["one_sided"]:
            flags.append("one side only")
        lines.append(f"| {_cell(r['task'])} | {_cell(r['agent'])} | {_cell(r['target'])} | {_rwma(b)} | {_rwma(n)} | "
                     f"{_pair(b['median_tokens'], n['median_tokens'], b['unknown_usage'], n['unknown_usage'])} | "
                     f"{_pair(b['median_seconds'], n['median_seconds'], 0, 0)} | {_cell(', '.join(flags))} |")
    notes = [f"- {r['task']}: reply text of {', '.join(r['canned_tools_changed'])} is shared between runs; "
             "recapture against the candidate to measure it"
             for r in result["rows"] if r["canned_tools_changed"]]
    if notes:
        lines += ["", "## Not measured", ""] + list(dict.fromkeys(notes))
    return "\n".join(lines) + "\n"


def _key_of(r):
    return (r["kind"], r["task"], r["target"], r["agent"])


def _strip_flags(argv, names):
    out, skip = [], False
    for word in argv:
        if skip:
            skip = False
        elif word in names:
            skip = True
        elif any(word.startswith(n + "=") for n in names):
            pass
        else:
            out.append(word)
    return out


def _confirm(candidate, flagged, count):
    """Rerun each flagged (kind, task, agent) once; return {(kind, task, agent): outcome}.

    Reruns write to a fresh directory, and only the rows that directory holds are merged.
    """
    manifest = candidate["manifest"]
    argv = manifest.get("argv") or []
    if not argv:
        return {k: {"exit": "no argv in the manifest", "rows": []} for k in flagged}
    base = [sys.executable] + argv if str(argv[0]).endswith(".py") else list(argv)
    base = _strip_flags(base, {"--task", "--scenario", "--agent", "--repetitions", "--output", "--limit"})
    base = [w for w in base if w != "--execute"]
    invocation = candidate["dir"] / "confirm" / (time.strftime("%Y%m%d-%H%M%S") + "-" + uuid.uuid4().hex[:6])
    outcomes, merged = {}, []
    for kind, task, agent in sorted(flagged):
        command = base + ["--output", str(invocation), "--scenario" if kind == "scenario" else "--task", task,
                          "--agent", agent, "--repetitions", str(count), "--execute"]
        before = set(invocation.glob("*/results.jsonl"))
        try:
            code = subprocess.run(command, cwd=manifest.get("cwd") or None, check=False).returncode
        except OSError as error:
            code = f"could not start ({error})"
        rows = [r for path in sorted(set(invocation.glob("*/results.jsonl")) - before) for r in _read_rows(path)]
        merged += rows
        outcomes[(kind, task, agent)] = {"exit": code, "rows": rows}
    candidate["rows"] = candidate["rows"] + merged
    return outcomes


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--output", type=Path)
    parser.add_argument("--margin", type=float, default=.25)
    parser.add_argument("--allow-mismatch", action="store_true")
    parser.add_argument("--confirm", type=int, metavar="N")
    args = parser.parse_args(argv)
    baseline, candidate = load_run(args.baseline), load_run(args.candidate)
    scenarios = []
    if not (baseline["manifest"].get("scenarios") or candidate["manifest"].get("scenarios")):
        try:
            import scenarios as sc
            scenarios = sc.load_scenarios(Path(__file__).resolve().parent / "scenarios.json")
        except (ImportError, OSError, ValueError):
            scenarios = []
    result = compare(baseline, candidate, scenarios, args.margin)
    if result["mismatches"] and not args.allow_mismatch:
        print("The runs differ in: " + ", ".join(result["mismatches"]) + " (pass --allow-mismatch to compare anyway)")
    elif args.confirm and any(r["flags"] for r in result["rows"]):
        first = {_key_of(r): list(r["flags"]) for r in result["rows"] if r["flags"]}
        reruns = _confirm(candidate, {k[:2] + k[3:] for k in first}, args.confirm)
        result = compare(baseline, candidate, scenarios, args.margin)
        for r in result["rows"]:
            key = _key_of(r)
            r["confirmed"], r["unconfirmed"] = [], {}
            if key not in first:
                continue  # not flagged before the reruns, so nothing was rerun for it
            rerun = reruns[key[:2] + key[3:]]
            for flag in r["flags"]:
                if flag not in first[key]:
                    continue
                added = sum(x.get("status") != "setup_error" and _key(x) == key for x in rerun["rows"])
                r["rerun_exit"] = rerun["exit"]
                if not added:
                    r["unconfirmed"][flag] = (f"rerun failed: exit {rerun['exit']}" if rerun["exit"] not in (0, 1)
                                              else "rerun added no attempts")
                else:
                    r["confirmed"].append(flag)
    output = args.output or (args.candidate / "compare")
    output.mkdir(parents=True, exist_ok=True)
    (output / "compare.json").write_text(json.dumps(result, indent=2) + "\n")
    (output / "compare.md").write_text(render_markdown(result))
    print(render_markdown(result))
    if result["mismatches"] and not args.allow_mismatch:
        return 2
    return 1 if any(r["flags"] for r in result["rows"]) else 0


if __name__ == "__main__":
    sys.exit(main())
