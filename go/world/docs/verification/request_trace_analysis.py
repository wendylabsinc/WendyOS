#!/usr/bin/env python3
"""Summarize bounded local request traces without adding nested durations."""

import argparse
import hashlib
import json
from collections import Counter
from pathlib import Path


MAX_BYTES = 8 << 20
CLASSES = {"other", "challenge", "ack", "evaluate", "stop", "task_list", "publication", "repair", "repair_round", "query", "task_relay", "task_relay_round"}
PHASES = {
    "outer_authorization", "verifier_lock", "verifier_hold", "replay_lock",
    "replay_commit", "replay_append", "replay_sync", "replay_replace", "task_lock", "task_hold",
    "mission_check", "supervisor_check", "task_commit", "response",
}


def integer(value, minimum, maximum):
    if type(value) is not int or not minimum <= value <= maximum:
        raise ValueError("invalid integer in request trace")
    return value


def boolean(value):
    if type(value) is not bool:
        raise ValueError("invalid Boolean in request trace")
    return value


def overlap_evidence(record, events, records):
    """Intersect recorded waits and other holders; retain gaps as unattributed."""
    pairs = {"verifier_lock": "verifier_hold", "task_lock": "task_hold"}
    waits = sorted((e for e in record["events"] if e["phase"] in pairs and e["end_ns"] >= 0),
                   key=lambda e: e["end_ns"] - e["start_ns"], reverse=True)[:4]
    result = []
    for wait in waits:
        matches = []
        for hold in events:
            if hold["phase"] != pairs[wait["phase"]] or hold["request_id"] == record["id"] or hold["end_ns"] < 0:
                continue
            start, end = max(wait["start_ns"], hold["start_ns"]), min(wait["end_ns"], hold["end_ns"])
            if start < end:
                matches.append({"request_id": hold["request_id"], "class": records[hold["request_id"]]["class"],
                                "start_ns": start, "end_ns": end, "overlap_ns": end - start})
        intervals = sorted((m["start_ns"], m["end_ns"]) for m in matches)
        union, previous_end = 0, wait["start_ns"]
        for start, end in intervals:
            union += max(0, end - max(start, previous_end))
            previous_end = max(previous_end, end)
        duration = wait["end_ns"] - wait["start_ns"]
        result.append({"phase": wait["phase"], "start_ns": wait["start_ns"], "end_ns": wait["end_ns"],
                       "duration_ns": duration, "recorded_holder_union_ns": union,
                       "unattributed_ns": duration - union, "holders": matches[:32],
                       "holder_details_omitted": max(0, len(matches) - 32)})
    return result


def summarize(trace):
    if trace.get("schema") != "wendy.world.request-trace.v1":
        raise ValueError("unsupported trace schema")
    if trace.get("state") not in {"waiting", "recording", "draining", "complete"}:
        raise ValueError("invalid capture state")
    limits = trace["limits"]
    window = integer(limits["window_ms"], 1, 3000) * 1_000_000
    grace = integer(limits["grace_ms"], 1, 3000) * 1_000_000
    request_limit = integer(limits["max_requests"], 1, 2048)
    event_limit = integer(limits["max_events"], 1, 32768)
    elapsed = integer(trace["elapsed_ns"], 0, (1 << 63) - 1)
    requests, events = trace["requests"], trace["events"]
    if not isinstance(requests, list) or not isinstance(events, list):
        raise ValueError("trace arrays required")
    if len(requests) > request_limit or len(events) > event_limit:
        raise ValueError("trace exceeds configured limits")
    records = {}
    for record in requests:
        key = integer(record["id"], 1, request_limit)
        if key in records or record["class"] not in CLASSES:
            raise ValueError("duplicate request or unknown class")
        start = integer(record["start_ns"], 0, min(window - 1, elapsed))
        end = integer(record["end_ns"], -1, min(window + grace, elapsed))
        if end != -1 and end < start:
            raise ValueError("request completion precedes its start")
        integer(record["status"], 0, 599)
        boolean(record["canceled"])
        records[key] = {**record, "events": []}
    for event in events:
        request_id = integer(event["request_id"], 1, request_limit)
        if request_id not in records or event["phase"] not in PHASES:
            raise ValueError("orphan event or unknown phase")
        record = records[request_id]
        start = integer(event["start_ns"], record["start_ns"], min(window + grace, elapsed))
        end = integer(event["end_ns"], -1, min(window + grace, elapsed))
        if end != -1 and end < start:
            raise ValueError("event completion precedes its start")
        if record["end_ns"] != -1 and (start > record["end_ns"] or end > record["end_ns"]):
            raise ValueError("event extends beyond completed request")
        for name in ("canceled_at_start", "canceled", "failed"):
            boolean(event[name])
        record["events"].append(event)
    counters = trace["counters"]
    for value in counters.values():
        integer(value, 0, (1 << 64) - 1)
    unfinished_requests = sum(r["end_ns"] == -1 for r in requests)
    unfinished_events = sum(e["end_ns"] == -1 for e in events)
    if counters["unfinished_requests"] != unfinished_requests or counters["unfinished_events"] != unfinished_events:
        raise ValueError("unfinished counts disagree with retained records")

    classes = {}
    for name in sorted({r["class"] for r in requests}):
        rows = [r for r in requests if r["class"] == name]
        completed = [r["end_ns"] - r["start_ns"] for r in rows if r["end_ns"] >= 0]
        classes[name] = {
            "requests": len(rows), "completed": len(completed),
            "unfinished": len(rows) - len(completed),
            "canceled_at_completion": sum(r["canceled"] for r in rows),
            "http_error_statuses": dict(sorted(Counter(str(r["status"]) for r in rows if r["status"] >= 400).items())),
            "maximum_completed_duration_ns": max(completed, default=None),
            "completed_requests_over_250ms": sum(d > 250_000_000 for d in completed),
        }

    def detail(record):
        phases = {}
        for name in sorted({e["phase"] for e in record["events"]}):
            rows = [e for e in record["events"] if e["phase"] == name]
            durations = [e["end_ns"] - e["start_ns"] for e in rows if e["end_ns"] >= 0]
            phases[name] = {
                "count": len(rows), "completed": len(durations),
                "maximum_completed_duration_ns": max(durations, default=None),
                "canceled_at_start": sum(e["canceled_at_start"] for e in rows),
                "canceled_at_completion": sum(e["canceled"] for e in rows),
                "failed": sum(e["failed"] for e in rows),
            }
        return {
            "id": record["id"], "class": record["class"],
            "start_ns": record["start_ns"], "end_ns": record["end_ns"],
            "duration_ns": None if record["end_ns"] < 0 else record["end_ns"] - record["start_ns"],
            "status": record["status"], "canceled": record["canceled"], "phases": phases,
            "longest_recorded_lock_wait_overlaps": overlap_evidence(record, events, records),
        }

    ranked = sorted(records.values(), key=lambda r: (r["end_ns"] == -1,
                    elapsed - r["start_ns"] if r["end_ns"] < 0 else r["end_ns"] - r["start_ns"]), reverse=True)
    relevant = [r for r in ranked if r["class"] in {"challenge", "ack", "evaluate", "stop"}]
    return {
        "schema": "wendy.world.request-trace-analysis.v1",
        "instance_id": trace["instance_id"], "run_nonce": trace["run_nonce"],
        "state": trace["state"], "armed_at_unix_ms": trace["armed_at_unix_ms"],
        "elapsed_ns": elapsed, "limits": limits, "counters": counters,
        "class_summary": classes,
        "longest_control_requests": [detail(r) for r in relevant[:12]],
        "canceled_or_unfinished_control_requests": [detail(r) for r in relevant if r["canceled"] or r["end_ns"] < 0],
        "limits_of_interpretation": [
            "Phase intervals overlap; no sum of nested phases is reported.",
            "Control requests are individual HTTP handlers, not complete challenge/ACK exchanges; explicit background repair/relay operations have status zero.",
            "A wait identifies the waiting request, not an uncaptured competing holder.",
            "Holder overlap is a union of recorded intervals on this node; gaps remain unattributed and nested persistence is not added to that union.",
            "Storage intervals include scheduling and encoding unless the phase explicitly isolates an operation.",
            "Cancellation is sampled at boundaries; it does not timestamp the client's timeout.",
            "Missing, dropped or unfinished events prevent complete request attribution.",
            "This analysis does not infer that tracing caused or fixed availability failures.",
        ],
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("trace", type=Path)
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()
    with args.trace.open("rb") as stream:
        data = stream.read(MAX_BYTES + 1)
    if len(data) > MAX_BYTES:
        raise ValueError("trace exceeds hard byte bound")
    result = summarize(json.loads(data))
    result["input_sha256"] = hashlib.sha256(data).hexdigest()
    output = json.dumps(result, indent=2) + "\n"
    if args.out is None:
        print(output, end="")
    else:
        args.out.write_text(output)


if __name__ == "__main__":
    main()
