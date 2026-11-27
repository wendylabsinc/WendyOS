"""Offline fixtures test saved timing consistency, never agent authority."""

import copy
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import rpc_timing as timing


def allowlist():
    return dict(version=1, phase="world-active", services=[
        dict(app="com.wendy.world.multimodal", service="fusion", generation=timing.UINT64_MAX)])


def event(sequence=1):
    return dict(version=1, sequence=sequence, method="PhysicalClock",
                **allowlist()["services"][0], code="OK", inclusive=True,
                stages=[dict(stage="authorize", count=4, sum_ns=120, max_ns=40),
                        dict(stage="peer", count=4, sum_ns=80, max_ns=30),
                        dict(stage="total", count=1, sum_ns=200, max_ns=200)])


def line(value):
    return (json.dumps(value, separators=(",", ":")) + "\n").encode()


class RPCTimingTests(unittest.TestCase):
    def test_remote_authorization_reason_stages_preserve_old_events(self):
        names = ("remote_record_bound_omitted", "remote_peer_grant_omitted",
                 "remote_source_authority_omitted", "remote_interpretation_omitted",
                 "remote_grant_window_omitted", "remote_callback_window_omitted")
        old = event()
        current = dict(event(2), version=2, method="Query")
        current["stages"] = [dict(stage=name, count=1, sum_ns=0, max_ns=0) for name in names]
        current["stages"].append(dict(stage="total", count=1, sum_ns=200, max_ns=200))
        self.assertEqual(timing.extract(line(old)+line(current), allowlist())["events"], [old, current])
        for name in names:
            bad = copy.deepcopy(current)
            bad["stages"][0]["stage"] = name+"/PRIVATE_SECRET"
            self.refused(line(bad), "invalid_or_duplicate_stage")

    def test_archive_stages_are_fixed_inclusive_and_preserve_old_events(self):
        names = ("archive_prune", "archive_remember", "archive_usage",
                 "archive_verify", "archive_commit")
        old = event()
        current = dict(event(2), version=2, method="Publish")
        current["stages"] = [dict(stage=name, count=1, sum_ns=100, max_ns=100)
                             for name in names]
        current["stages"].append(dict(stage="total", count=1, sum_ns=200, max_ns=200))
        report = timing.extract(line(old) + line(current), allowlist())
        self.assertEqual(report["events"], [old, current])
        group = next(row for row in report["summary"] if row["method"] == "Publish")
        self.assertEqual(group["stages"], current["stages"])
        self.assertNotEqual(sum(row["sum_ns"] for row in group["stages"][:-1]),
                            group["stages"][-1]["sum_ns"])
        for name in names:
            bad = copy.deepcopy(current)
            bad["stages"][0]["stage"] = name + "/PRIVATE_SECRET"
            self.refused(line(bad), "invalid_or_duplicate_stage")

    def test_source_authority_is_a_bounded_rpc_method(self):
        value = dict(event(), version=2, method="GetSourceAuthority")
        report = timing.extract(line(value), allowlist())
        self.assertEqual(report["events"], [value])
        self.assertEqual(report["summary"][0]["method"], "GetSourceAuthority")
        self.assertEqual(report["summary"][0]["rpc_count"], 1)
        self.refused(line(dict(value, method="GetSourceAuthority/PRIVATE_SECRET")))

    def refused(self, raw, expected=None, commissioned=None):
        with self.assertRaises(timing.Refused) as caught:
            timing.extract(raw, allowlist() if commissioned is None else commissioned)
        if expected:
            self.assertEqual(str(caught.exception), expected)
        self.assertNotIn("PRIVATE_SECRET", str(caught.exception))
        return str(caught.exception)

    def test_v2_refusal_interruption_and_background_counts_preserve_v1(self):
        old = event()
        current = event(2)
        current.update(version=2, code="Unavailable",
                       refusal=dict(point="peer_before_scope", reason="pin_closed", watcher_reason="pin_dead"),
                       pin_interruptions=[dict(point="peer_before_scope", count=2)])
        watcher = event(3)
        watcher.update(version=2, method="PeerWatch",
                       stages=[dict(stage="total", count=1, sum_ns=10, max_ns=10)],
                       pin_interruptions=[dict(point="peer_watcher", count=1)])
        raw = b"PRIVATE_SECRET unrelated\n" + line(old) + line(current) + line(watcher)
        report = timing.extract(raw, allowlist())
        self.assertEqual(report["version"], 2)
        self.assertEqual(report["kind"], "wendy.agentmultimodal.rpc-timing.v2")
        self.assertEqual(report["events"], [old, current, watcher])
        groups = {row["method"]: row for row in report["summary"]}
        self.assertEqual(groups["PhysicalClock"]["rpc_count"], 2)
        self.assertEqual(groups["PhysicalClock"]["refusals"], [dict(current["refusal"], count=1)])
        self.assertEqual(groups["PhysicalClock"]["pin_interruptions"], current["pin_interruptions"])
        self.assertEqual(groups["PeerWatch"]["rpc_count"], 0)
        self.assertEqual(groups["PeerWatch"]["background_event_count"], 1)
        self.assertTrue(groups["PeerWatch"]["total_measures_diagnostic_emission_only"])
        self.assertNotIn(b"PRIVATE_SECRET", timing.encode(report))
        legacy = timing.extract(line(old), allowlist())
        self.assertEqual(legacy["version"], 1)
        self.assertEqual(legacy["events"], [old])
        self.assertNotIn("refusals", legacy["summary"][0])
        self.assertNotIn("pin_interruptions", legacy["summary"][0])
        self.assertNotIn("background_event_count", legacy["summary"][0])

    def test_v2_fixed_fields_never_export_arbitrary_refusal_text(self):
        base = dict(event(), version=2, refusal=dict(point="authorize", reason="invalid_binding"))
        mutations = (
            lambda x: x.update(version=1),
            lambda x: x.update(version=3),
            lambda x: x.update(refusal=None),
            lambda x: x.update(refusal=[]),
            lambda x: x["refusal"].pop("reason"),
            lambda x: x["refusal"].update(error="PRIVATE_SECRET"),
            lambda x: x["refusal"].update(point="PRIVATE_SECRET"),
            lambda x: x["refusal"].update(reason="PRIVATE_SECRET"),
            lambda x: x["refusal"].update(watcher_reason="PRIVATE_SECRET"),
            lambda x: x["refusal"].update(reason=[]),
            lambda x: x["refusal"].update(point={}),
            lambda x: x.update(pin_interruptions=[dict(point="PRIVATE_SECRET", count=1)]),
            lambda x: x.update(pin_interruptions=[dict(point="response", count=1, error="PRIVATE_SECRET")]),
        )
        for mutate in mutations:
            value = copy.deepcopy(base)
            mutate(value)
            self.refused(line(value))
        self.refused(line(dict(event(), method="PeerWatch")), "invalid_fixed_enum")
        self.refused(line(dict(event(), method="AuthorityWatch")), "invalid_fixed_enum")

    def test_v2_interruption_counters_are_bounded_unique_and_ordered(self):
        for bad in (None, {}, [], [dict(point="response", count=0)],
                    [dict(point="response", count=True)],
                    [dict(point="response", count=timing.UINT64_MAX + 1)],
                    [dict(point="response", count=1)] * 2,
                    [dict(point="authority", count=1)],
                    [dict(point=[], count=1)]):
            self.refused(line(dict(event(), version=2, pin_interruptions=bad)))
        counters = [dict(point=point, count=2) for point in reversed(timing.INTERRUPTION_POINTS)]
        value = dict(event(), version=2, pin_interruptions=counters)
        report = timing.extract(line(value), allowlist())
        self.assertEqual([x["point"] for x in report["events"][0]["pin_interruptions"]], list(timing.INTERRUPTION_POINTS))
        value["pin_interruptions"] = [dict(point="response", count=timing.UINT64_MAX)]
        self.refused(line(value) + line(dict(value, sequence=2)), "integer_outside_bound")

    def test_v2_refusal_summary_keeps_distinct_watcher_causes(self):
        rows = []
        for seq, watcher in enumerate((None, "pin_dead", "pin_interrupt_limit", "pin_dead"), 1):
            refusal = dict(point="response", reason="pin_closed")
            if watcher is not None:
                refusal["watcher_reason"] = watcher
            rows.append(dict(event(seq), version=2, code="Unavailable", refusal=refusal))
        report = timing.extract(b"".join(line(row) for row in rows), allowlist())
        counts = {row.get("watcher_reason"): row["count"] for row in report["summary"][0]["refusals"]}
        self.assertEqual(counts, {None: 1, "pin_dead": 2, "pin_interrupt_limit": 1})

    def test_mixed_log_exports_only_commissioned_events_and_stage_aggregates(self):
        second = event(3)  # Dropped events may leave gaps.
        second["code"] = "Unavailable"
        second["stages"][0] = dict(stage="authorize", count=2, sum_ns=70, max_ns=45)
        raw = b'normal log PRIVATE_SECRET /private/credential\n'
        raw += b'{"level":"error","message":"PRIVATE_SECRET"}\n'
        raw += line(event()) + b'invalid unrelated JSON { PRIVATE_SECRET\n' + line(second)
        report = timing.extract(raw, allowlist())
        self.assertEqual(report["source_log"], dict(sha256=hashlib.sha256(raw).hexdigest(), bytes=len(raw)))
        self.assertEqual(report["event_count"], 2)
        self.assertEqual([x["sequence"] for x in report["events"]], [1, 3])
        group = report["summary"][0]
        self.assertEqual(group["generation"], timing.UINT64_MAX)
        self.assertEqual(group["rpc_count"], 2)
        stages = {row["stage"]: row for row in group["stages"]}
        self.assertEqual(stages["authorize"], dict(stage="authorize", count=6, sum_ns=190, max_ns=45))
        self.assertEqual(stages["total"]["sum_ns"], 400)
        self.assertNotEqual(stages["total"]["sum_ns"], stages["authorize"]["sum_ns"] + stages["peer"]["sum_ns"])
        self.assertTrue(report["saved_log_consistency_only"])
        self.assertTrue(report["missing_events_do_not_prove_absence"])
        self.assertFalse(report["current_authority_verified"])
        self.assertFalse(report["end_to_end_correlation"])
        encoded = timing.encode(report)
        self.assertNotIn(b"PRIVATE_SECRET", encoded)
        self.assertNotIn(b"/private/credential", encoded)
        self.assertEqual(allowlist()["services"][0]["generation"], timing.UINT64_MAX)

    def test_unknown_fields_fixed_enums_and_scope_refused_without_echo(self):
        mutations = {
            "unknown-field": lambda x: x.update(secret="PRIVATE_SECRET"),
            "missing-field": lambda x: x.pop("app"),
            "method": lambda x: x.update(method="PRIVATE_SECRET"),
            "code": lambda x: x.update(code="PRIVATE_SECRET"),
            "app": lambda x: x.update(app="PRIVATE_SECRET"),
            "service": lambda x: x.update(service="other"),
            "generation": lambda x: x.update(generation=7),
            "stage": lambda x: x["stages"][0].update(stage="PRIVATE_SECRET"),
            "stage-field": lambda x: x["stages"][0].update(secret="PRIVATE_SECRET"),
            "unicode": lambda x: x.update(app="\ud800"),
            "not-inclusive": lambda x: x.update(inclusive=False),
            "numeric-inclusive": lambda x: x.update(inclusive=1),
        }
        for name, mutate in mutations.items():
            with self.subTest(name=name):
                value = event()
                mutate(value)
                self.refused(line(value))

    def test_duplicate_keys_malformed_events_and_escaped_markers_refused(self):
        duplicate = line(event()).replace(b'"sequence":1', b'"sequence":1,"sequence":2')
        self.refused(duplicate, "duplicate_json_key")
        nested = line(event()).replace(b'"count":4', b'"count":4,"count":5', 1)
        self.refused(nested, "duplicate_json_key")
        self.refused(line(event()).replace(b'"sequence":1', b'"sequence":NaN'), "noninteger_json_number")
        self.refused(b'{"version":1,"sequence":1,"method":"PRIVATE_SECRET",broken}\n', "invalid_json")
        self.refused(b'{"version":1,"sequence":1,"method":"PhysicalClock", "stages":\xff}\n', "invalid_json")
        self.refused(b'[' + line(event()).strip() + b']\n', "invalid_object_fields")
        self.refused(line(event()).strip() + b' {"PRIVATE_SECRET":1}\n', "invalid_json")
        escaped = duplicate
        for name in (b"version", b"sequence", b"method", b"generation", b"inclusive", b"stages"):
            escaped = escaped.replace(b'"' + name + b'"', b'"' + b''.join((b'\\u00' + format(c, '02x').encode()) for c in name) + b'"')
        self.refused(escaped, "duplicate_json_key")
        # Vertical tabs must not split a malformed JSONL object into ignored pieces.
        self.refused(line(event()).replace(b'"sequence":1', b'"sequence":1\v'), "invalid_json")

    def test_integer_and_measurement_bounds(self):
        for field in ("version", "sequence", "generation"):
            for value in (True, False, -1, 0, 1.0, "1", timing.UINT64_MAX + 1):
                with self.subTest(field=field, value=value):
                    row = event()
                    row[field] = value
                    self.refused(line(row))
        for field, bad in (("count", True), ("count", 0), ("count", timing.UINT64_MAX + 1),
                           ("sum_ns", -1), ("sum_ns", timing.INT64_MAX + 1), ("max_ns", 1.5)):
            with self.subTest(field=field, bad=bad):
                row = event()
                row["stages"][0][field] = bad
                self.refused(line(row))
        for measurement in (dict(stage="peer", count=1, sum_ns=2, max_ns=1),
                            dict(stage="peer", count=2, sum_ns=10, max_ns=2),
                            dict(stage="peer", count=2, sum_ns=1, max_ns=2)):
            row = event()
            row["stages"][1] = measurement
            self.refused(line(row), "inconsistent_measurement")

    def test_total_and_unique_stages(self):
        row = event()
        row["stages"].pop()
        self.refused(line(row), "one_total_measurement_required")
        row = event()
        row["stages"][-1]["count"] = 2
        self.refused(line(row), "one_total_measurement_required")
        row = event()
        row["stages"].append(copy.deepcopy(row["stages"][0]))
        self.refused(line(row), "invalid_or_duplicate_stage")
        row = event()
        row["stages"][-1].update(sum_ns=20, max_ns=20)
        self.refused(line(row), "stage_exceeds_rpc_total")
        row = event()
        row["stages"].reverse()
        report = timing.extract(line(row), allowlist())
        self.assertEqual([x["stage"] for x in report["events"][0]["stages"]], ["authorize", "peer", "total"])

    def test_phase_sequences_never_reset_and_empty_log_is_not_success_evidence(self):
        for sequence in (1, 2):
            self.refused(line(event(2)) + line(event(sequence)), "event_sequence_not_increasing")
        self.refused(line(event())[:-1], "truncated_log_tail")
        self.refused(b"unrelated PRIVATE_SECRET partial", "truncated_log_tail")
        for raw in (b"", b"ordinary PRIVATE_SECRET\n"):
            report = timing.extract(raw, allowlist())
            self.assertEqual(report["event_count"], 0)
            self.assertEqual(report["summary"], [])
            self.assertTrue(report["missing_events_do_not_prove_absence"])

    def test_explicit_allowlist_required_and_cannot_be_derived_from_events(self):
        bad = ({}, dict(version=1, phase="world-active", services=[]),
               dict(version=1, phase="PRIVATE_SECRET/path", services=allowlist()["services"]),
               {**allowlist(), "services": allowlist()["services"] * 2},
               {**allowlist(), "services": [dict(app="*", service="fusion", generation=7)]},
               {**allowlist(), "secret": "PRIVATE_SECRET"})
        for value in bad:
            with self.subTest(value=value):
                self.refused(line(event()), commissioned=value)
        row = event()
        row["service"] = "camera-a"
        self.refused(line(row), "uncommissioned_service")
        commissioned = allowlist()
        commissioned["services"].append({key: row[key] for key in ("app", "service", "generation")})
        report = timing.extract(line(event()) + line({**row, "sequence": 2}), commissioned)
        self.assertEqual(len(report["summary"]), 2)

    def test_event_line_count_source_bytes_and_output_are_all_bounded(self):
        self.refused(line(event())[:-1] + b" " * timing.MAX_EVENT_LINE_BYTES + b"\n", "event_line_budget_exceeded")
        small_allowlist = dict(version=1, phase="test", services=[dict(app="a", service="b", generation=1)])
        small = dict(version=1, sequence=1, method="Clock", app="a", service="b", generation=1,
                     code="OK", inclusive=True, stages=[dict(stage="total", count=1, sum_ns=0, max_ns=0)])
        raw = b"".join(line({**small, "sequence": n}) for n in range(1, timing.MAX_EVENTS + 2))
        self.refused(raw, "event_budget_exceeded", small_allowlist)
        padded = b"".join(line({**small, "sequence": n})[:-1] + b" " * 3000 + b"\n" for n in range(1, 400))
        self.refused(padded, "event_budget_exceeded", small_allowlist)
        with patch.object(timing, "MAX_OUTPUT_BYTES", 400):
            self.refused(line(small), "output_byte_budget_exceeded", small_allowlist)
        with patch.object(timing, "MAX_LOG_BYTES", 10):
            self.refused(b"PRIVATE_SECRET\n", "log_byte_budget_exceeded")

    def test_summary_overflow_refuses_instead_of_wrapping_or_saturating(self):
        row = event()
        row["stages"] = [dict(stage="total", count=1, sum_ns=timing.INT64_MAX, max_ns=timing.INT64_MAX)]
        self.refused(line(row) + line({**row, "sequence": 2}), "integer_outside_bound")

    def test_near_full_agent_event_budget_keeps_all_events_and_summary(self):
        lines, byte_count = [], 0
        for sequence in range(1, timing.MAX_EVENTS + 1):
            candidate = line(event(sequence))
            if byte_count + len(candidate) > timing.MAX_EVENT_BYTES:
                break
            lines.append(candidate)
            byte_count += len(candidate)
        raw = b"".join(lines)
        self.assertLess(timing.MAX_EVENT_BYTES - len(raw), len(candidate))
        report = timing.extract(raw, allowlist())
        self.assertEqual(report["event_count"], len(lines))
        self.assertEqual(report["observed_event_bytes"], len(raw))
        self.assertEqual(report["summary"][0]["rpc_count"], len(lines))
        self.assertGreater(len(timing.encode(report)), timing.MAX_EVENT_BYTES)
        self.assertLessEqual(len(timing.encode(report)), timing.MAX_OUTPUT_BYTES)
        self.refused(raw + candidate, "event_budget_exceeded")

    def test_file_reader_rejects_symlink_special_and_oversize_inputs(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = root / "PRIVATE_SECRET.log"
            raw = b"normal PRIVATE_SECRET\n" + line(event())
            source.write_bytes(raw)
            self.assertEqual(timing.extract_file(source, allowlist()), timing.extract(raw, allowlist()))
            linked = root / "link"
            linked.symlink_to(source)
            with self.assertRaisesRegex(timing.Refused, "^input_unavailable$"):
                timing.extract_file(linked, allowlist())
            fifo = root / "fifo"
            os.mkfifo(fifo)
            with self.assertRaisesRegex(timing.Refused, "^bounded_regular_input_required$"):
                timing.extract_file(fifo, allowlist())
            with source.open("wb") as out:
                out.truncate(timing.MAX_LOG_BYTES + 1)
            with self.assertRaisesRegex(timing.Refused, "^bounded_regular_input_required$"):
                timing.extract_file(source, allowlist())

    def test_cli_sanitizes_success_and_all_refusals(self):
        script = Path(timing.__file__)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source, commission = root / "PRIVATE_SECRET.log", root / "PRIVATE_SECRET.allowlist"
            source.write_bytes(b"PRIVATE_SECRET\n" + line(event()))
            commission.write_bytes(line(allowlist()))
            command = [sys.executable, str(script), "--log", str(source), "--allowlist", str(commission)]
            result = subprocess.run(command, capture_output=True, check=False, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout)["event_count"], 1)
            self.assertNotIn(b"PRIVATE_SECRET", result.stdout + result.stderr)
            source.write_bytes(line({**event(), "secret": "PRIVATE_SECRET"}))
            refused = subprocess.run(command, capture_output=True, check=False, timeout=5)
            self.assertEqual(refused.returncode, 2)
            self.assertEqual(refused.stdout, b"")
            self.assertNotIn(b"PRIVATE_SECRET", refused.stderr)
            source.unlink()
            missing = subprocess.run(command, capture_output=True, check=False, timeout=5)
            self.assertEqual(missing.returncode, 2)
            self.assertNotIn(b"PRIVATE_SECRET", missing.stdout + missing.stderr)
            invalid = subprocess.run([sys.executable, str(script), "--PRIVATE_SECRET"], capture_output=True, check=False, timeout=5)
            self.assertEqual(invalid.returncode, 2)
            self.assertNotIn(b"PRIVATE_SECRET", invalid.stdout + invalid.stderr)


if __name__ == "__main__":
    unittest.main()
