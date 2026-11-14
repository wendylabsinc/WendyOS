"""Comparison of repeated runs with explicit absolute and relative tolerances."""

import math
import statistics

from metrics import zero_when_absent


DEFAULT_POLICY = {
    "agent.process.cpu_seconds.percent_mean": {"relative": .10, "absolute": .5},
    "agent.process.cpu_seconds_per_operation": {"relative": .10, "absolute": .0001},
    "agent.process.rss_bytes.mean": {"relative": .10, "absolute": 5 * 1024**2},
    "agent.process.rss_bytes.peak": {"relative": .10, "absolute": 5 * 1024**2},
    "agent.service.memory_bytes.peak": {"relative": .10, "absolute": 5 * 1024**2},
    "agent.process.read_bytes.total": {"relative": .10, "absolute": 1024**2},
    "agent.process.write_bytes.total": {"relative": .10, "absolute": 1024**2},
    "workload.seconds": {"relative": .10, "absolute": .5},
    "workload.latency_p95_seconds": {"relative": .10, "absolute": .005},
    "workload.operations_per_second": {"relative": .10, "absolute": .01, "direction": "higher"},
}


def validate_policy(policy):
    if not isinstance(policy, dict) or not policy:
        raise ValueError("comparison policy must be a nonempty JSON object")
    for metric, rule in policy.items():
        if (not isinstance(rule, dict) or set(rule) - {"relative", "absolute", "direction"}
                or rule.get("direction", "lower") not in ("lower", "higher")):
            raise ValueError(f"invalid policy for {metric}")
        for key in ("relative", "absolute"):
            value = rule.get(key)
            if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
                raise ValueError(f"invalid {key} threshold for {metric}")
    return policy


def validate(report):
    if not isinstance(report, dict) or report.get("schema") != 1 or report.get("status") != "ok":
        raise ValueError("only successful schema-1 reports can be compared")
    names = [s["name"] for s in report["compatibility"]["profile"]["scenarios"]]
    repetitions = report["compatibility"]["profile"]["repetitions"]
    grouped = {name: [] for name in names}
    for run in report["runs"]:
        if run.get("status") != "ok" or run["scenario"] not in grouped:
            raise ValueError("failed or unexpected workload in report")
        if not all(isinstance(v, (float, int)) and math.isfinite(v) for v in run["metrics"].values()):
            raise ValueError("non-finite metric in report")
        if not all(isinstance(v, (float, int)) and math.isfinite(v) and v >= 0
                   for v in run.get("resolution", {}).values()):
            raise ValueError("invalid metric resolution in report")
        grouped[run["scenario"]].append(run)
    for name, runs in grouped.items():
        if len(runs) != repetitions or len({r["repeat"] for r in runs}) != repetitions:
            raise ValueError(f"incomplete repetitions for {name}")
        if len({required_metrics(r) for r in runs}) != 1:
            raise ValueError(f"metric availability changed across repetitions of {name}")
    return grouped


def required_metrics(run):
    return frozenset(key for key in run["metrics"] if not zero_when_absent(key))


def metric_values(runs, metric):
    if zero_when_absent(metric):
        return [r["metrics"].get(metric, 0) for r in runs]
    return [r["metrics"][metric] for r in runs]


def compare(baseline, candidate, policy=None):
    old, new = validate(baseline), validate(candidate)
    if baseline["compatibility"] != candidate["compatibility"]:
        raise ValueError("incompatible environment, scope, or workload profile; establish a matching baseline")
    policy = validate_policy(DEFAULT_POLICY if policy is None else policy)
    rows = []
    for name in old:
        if required_metrics(old[name][0]) != required_metrics(new[name][0]):
            raise ValueError(f"metric availability changed for {name}")
        for metric, rule in policy.items():
            if not any(metric in r["metrics"] for r in old[name] + new[name]):
                rows.append({"scenario": name, "metric": metric, "status": "unavailable"})
                continue
            a = metric_values(old[name], metric)
            b = metric_values(new[name], metric)
            before, after = statistics.median(a), statistics.median(b)
            delta = after - before
            # A change of up to one counter step (such as a CPU clock tick) is
            # noise. The step is widened slightly because float arithmetic on
            # tick-based seconds can land a hair above an exact step.
            resolution = max(r.get("resolution", {}).get(metric, 0) for r in old[name] + new[name])
            tolerance = max(abs(before) * rule["relative"], rule["absolute"], resolution * (1 + 1e-9))
            worse = delta * (-1 if rule.get("direction") == "higher" else 1)
            status = "regression" if worse > tolerance else "improvement" if worse < -tolerance else "within tolerance"
            rows.append({"scenario": name, "metric": metric, "baseline": before, "candidate": after,
                         "delta": delta, "percent": 100 * delta / abs(before) if before else None,
                         "baseline_range": [min(a), max(a)], "candidate_range": [min(b), max(b)],
                         "status": status})
    if all(r["status"] == "unavailable" for r in rows):
        raise ValueError("policy did not match any measured metrics")
    return {"schema": 1, "baseline": baseline["revision"], "candidate": candidate["revision"],
            "regressions": sum(r["status"] == "regression" for r in rows), "rows": rows}


def markdown(result):
    lines = [f"Agent performance: {result['baseline']} → {result['candidate']}", "",
             f"{result['regressions']} regression(s). Values are medians across repetitions; ranges are min–max.", "",
             "| Scenario | Metric | Baseline [range] | Candidate [range] | Change | Result |",
             "| --- | --- | --- | --- | --- | --- |"]
    def cell(value):
        return str(value).replace("|", "\\|").replace("\n", " ")
    for row in result["rows"]:
        if row["status"] == "unavailable":
            values = [row["scenario"], row["metric"], "—", "—", "—", "unavailable"]
        else:
            values = [row["scenario"], row["metric"],
                      f"{row['baseline']:.4g} [{row['baseline_range'][0]:.4g}, {row['baseline_range'][1]:.4g}]",
                      f"{row['candidate']:.4g} [{row['candidate_range'][0]:.4g}, {row['candidate_range'][1]:.4g}]",
                      f"{row['percent']:+.1f}%" if row["percent"] is not None else f"{row['delta']:+.4g} absolute",
                      row["status"]]
        lines.append("| " + " | ".join(cell(v) for v in values) + " |")
    return "\n".join(lines) + "\n"
