#!/usr/bin/env python3
"""Extract bounded RPC timings from one completed private agent log phase.

API: extract(log_bytes, allowlist) returns a JSON-safe, reconstructed report.
extract_file(log_path, allowlist) additionally checks a bounded regular file.
The allowlist is explicit commissioning data, never inferred from log content:
  {"version":1,"phase":"world-active","services":[
    {"app":"com.wendy.world.multimodal","service":"fusion","generation":7}]}

CLI: python3 rpc_timing.py --log PRIVATE_LOG --allowlist COMMISSIONED_JSON
The CLI writes only sanitized JSON to stdout. It never copies unrelated log
lines, file paths or error text. A separate invocation is required for each
agent process/phase: event sequences may have gaps but must strictly increase.
The complete input must end at a newline; a partial final line is refused.
Recognized events retain the agent's 1 MiB budget. The reconstructed report
allows 2 MiB so its metadata and summaries do not crowd out those events.

Summaries add the same stage across RPCs, never nested stages within an RPC.
Durations are inclusive local profiling data, not authority time or evidence
of admission/delivery. There are no end-to-end correlation timestamps. Missing
events may reflect disabled tracing, queue/budget drops or process termination.
This validates saved-log consistency; it authenticates neither agent nor time.
Version 2 adds fixed refusal classifications and same-pidfd interruption counts.
PeerWatch and AuthorityWatch describe bounded background diagnostics, not RPCs;
their total duration measures event emission, not the preceding peer check.
Version 1 inputs retain their original report shape.
The fixed archive stages measure existing work only. archive_commit includes
its transaction callback; archive_verify includes decoding and current checks,
so neither is a disjoint fsync or cryptographic benchmark.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import sys


MAX_LOG_BYTES = 32 << 20
MAX_ALLOWLIST_BYTES = 64 << 10
MAX_EVENT_LINE_BYTES = 8 << 10
MAX_EVENTS = 4096
MAX_EVENT_BYTES = 1 << 20
MAX_OUTPUT_BYTES = 2 << 20
MAX_SERVICES = 64
UINT64_MAX = (1 << 64) - 1
INT64_MAX = (1 << 63) - 1
METHODS = ("Clock", "PhysicalClock", "Query", "GetEvidence", "PutEvidence",
           "GetRecordProof", "Publish", "PublishStream", "GetSourceAuthority")
WATCH_METHODS = ("PeerWatch", "AuthorityWatch")
REFUSAL_POINTS = ("authorize", "peer_before_scope", "peer_after_scope", "authority",
                  "response", "peer_watcher", "authority_watcher")
REFUSAL_REASONS = ("context_canceled", "context_deadline", "service_closed",
                   "invalid_binding", "pin_closed", "pin_dead", "pin_event",
                   "pin_syscall", "pin_interrupt_limit", "pin_unknown",
                   "credential_lookup", "scoped_peer_refused", "peer_changed",
                   "publisher_done", "publisher_refused")
INTERRUPTION_POINTS = ("peer_before_scope", "peer_after_scope", "response", "peer_watcher")
CODES = ("OK", "Canceled", "Unknown", "InvalidArgument", "DeadlineExceeded",
         "NotFound", "AlreadyExists", "PermissionDenied", "ResourceExhausted",
         "FailedPrecondition", "Aborted", "OutOfRange", "Unimplemented",
         "Internal", "Unavailable", "DataLoss", "Unauthenticated")
STAGES = ("authorize", "peer", "config", "publisher", "admission_wait",
          "identity_wait", "identity_held", "journal_wait", "journal_snapshot",
          "verify", "journal_schema", "schema", "source_clock", "evidence_wait",
          "evidence_read", "evidence_floor", "journal_commit", "replica",
          "finalize", "archive_prune", "archive_remember", "archive_usage",
          "archive_verify", "archive_commit", "remote_candidate",
          "remote_epoch_omitted", "remote_verify_omitted", "remote_authority_omitted",
          "remote_membership_omitted", "remote_window_omitted", "remote_record_omitted",
          "remote_visible", "remote_record_bound_omitted", "remote_peer_grant_omitted",
          "remote_source_authority_omitted", "remote_interpretation_omitted",
          "remote_grant_window_omitted", "remote_callback_window_omitted", "total")
EVENT_KEYS = frozenset(("version", "sequence", "method", "app", "service",
                        "generation", "code", "inclusive", "stages"))
V2_OPTIONAL_KEYS = frozenset(("refusal", "pin_interruptions"))
MEASUREMENT_KEYS = frozenset(("stage", "count", "sum_ns", "max_ns"))
_MARKERS = re.compile(rb'"(version|sequence|method|generation|inclusive|stages)"\s*:')
_ASCII_ESCAPE = re.compile(rb'\\u00([0-9a-fA-F]{2})')
_IDENTIFIER = re.compile(r"[A-Za-z0-9_.-]+", re.ASCII)
_PHASE = re.compile(r"[a-z][a-z0-9-]{0,63}", re.ASCII)


class Refused(ValueError):
    """Contains only a fixed diagnostic code, never input data or paths."""


def _strict_json(raw):
    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise Refused("duplicate_json_key")
            result[key] = value
        return result

    def not_integer(_):
        raise Refused("noninteger_json_number")

    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=pairs,
                          parse_float=not_integer, parse_constant=not_integer)
    except Refused:
        raise
    except (ValueError, UnicodeError, RecursionError, OverflowError):
        raise Refused("invalid_json") from None


def _keys(value, expected):
    if type(value) is not dict or set(value) != set(expected):
        raise Refused("invalid_object_fields")


def _integer(value, minimum, maximum):
    if type(value) is not int or not minimum <= value <= maximum:
        raise Refused("integer_outside_bound")
    return value


def _identifier(value, limit):
    if type(value) is not str or not 1 <= len(value) <= limit or _IDENTIFIER.fullmatch(value) is None:
        raise Refused("invalid_commissioned_identifier")
    return value


def _commissioned(allowlist):
    _keys(allowlist, ("version", "phase", "services"))
    _integer(allowlist["version"], 1, 1)
    phase = allowlist["phase"]
    if type(phase) is not str or _PHASE.fullmatch(phase) is None:
        raise Refused("invalid_phase")
    services = allowlist["services"]
    if type(services) is not list or not 1 <= len(services) <= MAX_SERVICES:
        raise Refused("invalid_service_allowlist")
    expected = set()
    for row in services:
        _keys(row, ("app", "service", "generation"))
        key = (_identifier(row["app"], 256), _identifier(row["service"], 128),
               _integer(row["generation"], 1, UINT64_MAX))
        if key in expected:
            raise Refused("duplicate_commissioned_service")
        expected.add(key)
    return phase, expected


def _recognized(line):
    # The private sink writes bare JSON objects. Prefix-bearing normal log
    # messages are unrelated data. Conservative markers also identify broken
    # objects/arrays before strict decoding, so malformed timing is not hidden.
    stripped = line.lstrip(b" \t\r")
    if not stripped.startswith((b"{", b"[")):
        return False
    # JSON ASCII key escapes cannot disguise a recognized malformed event.
    # This is recognition only; the original bytes still face strict parsing.
    recognized_text = _ASCII_ESCAPE.sub(lambda match: bytes((int(match[1], 16),)), stripped)
    fields = set(_MARKERS.findall(recognized_text))
    return bool(fields & {b"stages", b"inclusive"} or
                b"sequence" in fields and fields & {b"version", b"method"} or
                {b"method", b"generation"} <= fields)


def _event(value, expected):
    if type(value) is not dict or "version" not in value:
        raise Refused("invalid_object_fields")
    version = _integer(value["version"], 1, 2)
    if version == 1:
        _keys(value, EVENT_KEYS)
    elif not EVENT_KEYS <= set(value) or not set(value) <= EVENT_KEYS | V2_OPTIONAL_KEYS:
        raise Refused("invalid_object_fields")
    sequence = _integer(value["sequence"], 1, UINT64_MAX)
    key = (_identifier(value["app"], 256), _identifier(value["service"], 128),
           _integer(value["generation"], 1, UINT64_MAX))
    if key not in expected:
        raise Refused("uncommissioned_service")
    method, code = value["method"], value["code"]
    methods = METHODS if version == 1 else METHODS + WATCH_METHODS
    if type(method) is not str or method not in methods or type(code) is not str or code not in CODES:
        raise Refused("invalid_fixed_enum")
    if value["inclusive"] is not True:
        raise Refused("inclusive_timing_required")
    measurements = value["stages"]
    if type(measurements) is not list or not 1 <= len(measurements) <= len(STAGES):
        raise Refused("invalid_measurement_count")
    stages = {}
    for measurement in measurements:
        _keys(measurement, MEASUREMENT_KEYS)
        name = measurement["stage"]
        if type(name) is not str or name not in STAGES or name in stages:
            raise Refused("invalid_or_duplicate_stage")
        count = _integer(measurement["count"], 1, UINT64_MAX)
        total = _integer(measurement["sum_ns"], 0, INT64_MAX)
        maximum = _integer(measurement["max_ns"], 0, INT64_MAX)
        if maximum > total or total > count * maximum or count == 1 and total != maximum:
            raise Refused("inconsistent_measurement")
        stages[name] = dict(stage=name, count=count, sum_ns=total, max_ns=maximum)
    total = stages.get("total")
    if total is None or total["count"] != 1:
        raise Refused("one_total_measurement_required")
    if any(row["max_ns"] > total["sum_ns"] for row in stages.values()):
        raise Refused("stage_exceeds_rpc_total")
    result = dict(version=version, sequence=sequence, method=method, app=key[0],
                  service=key[1], generation=key[2], code=code, inclusive=True,
                  stages=[stages[name] for name in STAGES if name in stages])
    if "refusal" in value:
        refusal = value["refusal"]
        if type(refusal) is not dict or set(refusal) not in ({"point", "reason"}, {"point", "reason", "watcher_reason"}):
            raise Refused("invalid_refusal_fields")
        if type(refusal["point"]) is not str or refusal["point"] not in REFUSAL_POINTS:
            raise Refused("invalid_fixed_enum")
        for name in ("reason", "watcher_reason"):
            if name in refusal and (type(refusal[name]) is not str or refusal[name] not in REFUSAL_REASONS):
                raise Refused("invalid_fixed_enum")
        result["refusal"] = dict(refusal)
    if "pin_interruptions" in value:
        counters = value["pin_interruptions"]
        if type(counters) is not list or not 1 <= len(counters) <= len(INTERRUPTION_POINTS):
            raise Refused("invalid_interruption_count")
        counts = {}
        for counter in counters:
            _keys(counter, ("point", "count"))
            point = counter["point"]
            if type(point) is not str or point not in INTERRUPTION_POINTS or point in counts:
                raise Refused("invalid_or_duplicate_interruption_point")
            counts[point] = _integer(counter["count"], 1, UINT64_MAX)
        result["pin_interruptions"] = [dict(point=point, count=counts[point]) for point in INTERRUPTION_POINTS if point in counts]
    return result


def _summary(events):
    groups = {}
    for event in events:
        key = (event["app"], event["service"], event["generation"], event["method"])
        group = groups.setdefault(key, dict(rpc_count=0, codes={}, stages={}, refusals={}, interruptions={}))
        group["rpc_count"] += 1
        group["codes"][event["code"]] = group["codes"].get(event["code"], 0) + 1
        for stage in event["stages"]:
            row = group["stages"].setdefault(stage["stage"], dict(stage=stage["stage"], count=0, sum_ns=0, max_ns=0))
            row["count"] = _integer(row["count"] + stage["count"], 1, UINT64_MAX)
            row["sum_ns"] = _integer(row["sum_ns"] + stage["sum_ns"], 0, INT64_MAX)
            row["max_ns"] = max(row["max_ns"], stage["max_ns"])
        if "refusal" in event:
            refusal = event["refusal"]
            reason_key = (refusal["point"], refusal["reason"], refusal.get("watcher_reason", ""))
            group["refusals"][reason_key] = group["refusals"].get(reason_key, 0) + 1
        for counter in event.get("pin_interruptions", []):
            point = counter["point"]
            group["interruptions"][point] = _integer(group["interruptions"].get(point, 0) + counter["count"], 1, UINT64_MAX)
    summaries = []
    for key, group in sorted(groups.items()):
        row = dict(app=key[0], service=key[1], generation=key[2], method=key[3],
                   rpc_count=group["rpc_count"],
                   codes=[dict(code=code, count=group["codes"][code]) for code in CODES if code in group["codes"]],
                   stages=[group["stages"][name] for name in STAGES if name in group["stages"]])
        if key[3] in WATCH_METHODS:
            row.update(rpc_count=0, background_event_count=group["rpc_count"], total_measures_diagnostic_emission_only=True)
        if group["refusals"]:
            row["refusals"] = [dict(point=reason[0], reason=reason[1], count=count,
                                    **({"watcher_reason": reason[2]} if reason[2] else {}))
                               for reason, count in sorted(group["refusals"].items())]
        if group["interruptions"]:
            row["pin_interruptions"] = [dict(point=point, count=group["interruptions"][point])
                                        for point in INTERRUPTION_POINTS if point in group["interruptions"]]
        summaries.append(row)
    return summaries


def encode(report):
    """Canonical compact output; reject rather than truncate an oversized report."""
    raw = (json.dumps(report, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode("ascii")
    if len(raw) > MAX_OUTPUT_BYTES:
        raise Refused("output_byte_budget_exceeded")
    return raw


def extract(log_bytes, allowlist):
    """Validate one complete phase. No signatures or clock authority are verified."""
    if type(log_bytes) is not bytes or len(log_bytes) > MAX_LOG_BYTES:
        raise Refused("log_byte_budget_exceeded")
    phase, expected = _commissioned(allowlist)
    if log_bytes and not log_bytes.endswith(b"\n"):
        raise Refused("truncated_log_tail")
    events, previous, event_bytes = [], 0, 0
    for content in log_bytes.split(b"\n")[:-1]:
        line = content + b"\n"
        if not _recognized(line):
            continue
        if len(line) > MAX_EVENT_LINE_BYTES:
            raise Refused("event_line_budget_exceeded")
        event_bytes += len(line)
        if len(events) >= MAX_EVENTS or event_bytes > MAX_EVENT_BYTES:
            raise Refused("event_budget_exceeded")
        event = _event(_strict_json(line), expected)
        if event["sequence"] <= previous:
            raise Refused("event_sequence_not_increasing")
        previous = event["sequence"]
        events.append(event)
    version = 2 if any(event["version"] == 2 for event in events) else 1
    report = dict(version=version, kind=f"wendy.agentmultimodal.rpc-timing.v{version}", phase=phase,
                  source_log=dict(sha256=hashlib.sha256(log_bytes).hexdigest(), bytes=len(log_bytes)),
                  event_count=len(events), observed_event_bytes=event_bytes, events=events,
                  summary=_summary(events), saved_log_consistency_only=True,
                  inclusive_durations=True, current_authority_verified=False,
                  end_to_end_correlation=False, missing_events_do_not_prove_absence=True)
    encode(report)
    return report


def _read_regular(path, limit):
    # Bound both stat size and actual read. O_NONBLOCK prevents an unexpected
    # special file from blocking before fstat rejects it. No path is exported.
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
        with os.fdopen(fd, "rb") as source:
            before = os.fstat(source.fileno())
            if not stat.S_ISREG(before.st_mode) or before.st_size > limit:
                raise Refused("bounded_regular_input_required")
            raw = source.read(limit + 1)
            after = os.fstat(source.fileno())
            if len(raw) > limit or len(raw) != before.st_size or (before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (after.st_size, after.st_mtime_ns, after.st_ctime_ns):
                raise Refused("input_changed_or_exceeded_bound")
            return raw
    except Refused:
        raise
    except (OSError, ValueError):
        raise Refused("input_unavailable") from None


def extract_file(log_path, allowlist):
    return extract(_read_regular(log_path, MAX_LOG_BYTES), allowlist)


class _Parser(argparse.ArgumentParser):
    def error(self, _message):
        raise Refused("invalid_command_arguments")


def main(argv=None):
    parser = _Parser(prog="rpc_timing.py", description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--log", type=Path, required=True)
    parser.add_argument("--allowlist", type=Path, required=True)
    try:
        args = parser.parse_args(argv)
        allowlist = _strict_json(_read_regular(args.allowlist, MAX_ALLOWLIST_BYTES))
        report = extract_file(args.log, allowlist)
        sys.stdout.buffer.write(encode(report))
        return 0
    except Refused as error:
        print("RPC timing extraction refused: " + str(error), file=sys.stderr)
        return 2
    except OSError:
        print("RPC timing extraction refused: output_unavailable", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
