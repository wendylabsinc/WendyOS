import copy
import importlib.util
import pathlib
import unittest

path = pathlib.Path(__file__).resolve().parents[1] / "docs/verification/request_trace_analysis.py"
spec = importlib.util.spec_from_file_location("request_trace_analysis", path)
analysis = importlib.util.module_from_spec(spec)
spec.loader.exec_module(analysis)


def fixture():
    return {"schema": "wendy.world.request-trace.v1", "state": "complete",
            "instance_id": "fixture", "run_nonce": "fixture", "armed_at_unix_ms": 1,
            "elapsed_ns": 6_000_000_000,
            "limits": {"window_ms": 3000, "grace_ms": 3000, "max_requests": 2048, "max_events": 32768},
            "requests": [{"id": 1, "class": "ack", "start_ns": 2_990_000_000,
                          "end_ns": 3_290_000_000, "deadline_ns": -1, "status": 200, "canceled": True}],
            "events": [{"request_id": 1, "phase": name, "start_ns": start, "end_ns": end,
                        "canceled_at_start": False, "canceled": True, "failed": False}
                       for name, start, end in [("outer_authorization", 2_991_000_000, 3_280_000_000),
                                               ("replay_commit", 3_000_000_000, 3_270_000_000)]],
            "counters": {"dropped_requests": 0, "dropped_events": 0, "outside_window": 1,
                         "late_completions": 0, "unfinished_requests": 0, "unfinished_events": 0}}


class RequestTraceAnalysisTests(unittest.TestCase):
    def test_late_completion_and_overlapping_phases(self):
        result = analysis.summarize(fixture())
        request = result["longest_control_requests"][0]
        self.assertEqual(request["duration_ns"], 300_000_000)
        self.assertEqual(result["class_summary"]["ack"]["completed_requests_over_250ms"], 1)
        self.assertEqual(len(result["canceled_or_unfinished_control_requests"]), 1)
        self.assertNotIn("sum", str(request))

    def test_unfinished_is_not_zero_latency(self):
        value = fixture()
        value["requests"][0]["end_ns"] = -1
        value["events"][1]["end_ns"] = -1
        value["counters"]["unfinished_requests"] = 1
        value["counters"]["unfinished_events"] = 1
        result = analysis.summarize(value)
        self.assertIsNone(result["longest_control_requests"][0]["duration_ns"])
        self.assertIsNone(result["class_summary"]["ack"]["maximum_completed_duration_ns"])

    def test_orphans_and_invalid_timing_are_rejected(self):
        for mutation in (
            lambda v: v["events"][0].update(request_id=2),
            lambda v: v["events"][0].update(end_ns=3_300_000_000),
            lambda v: v["requests"].append(copy.deepcopy(v["requests"][0])),
            lambda v: v["counters"].update(unfinished_events=1),
            lambda v: v["requests"][0].update(canceled="false"),
        ):
            value = fixture()
            mutation(value)
            with self.assertRaises(ValueError):
                analysis.summarize(value)

    def test_unknown_or_overflow_is_not_silently_removed(self):
        value = fixture()
        value["counters"].update(dropped_events=3)
        self.assertEqual(analysis.summarize(value)["counters"]["dropped_events"], 3)
        value["events"][0]["phase"] = "an unrecognized phase"
        with self.assertRaises(ValueError):
            analysis.summarize(value)

    def test_holder_overlap_uses_union_and_retains_gaps(self):
        value = fixture()
        value["requests"].append({**value["requests"][0], "id": 2, "class": "evaluate"})
        value["events"] = [{"request_id": request, "phase": phase, "start_ns": start,
                            "end_ns": end, "canceled_at_start": False, "canceled": False, "failed": False}
                           for request, phase, start, end in [
                               (1, "task_lock", 3_000_000_000, 3_270_000_000),
                               (2, "task_hold", 3_010_000_000, 3_240_000_000),
                               (2, "task_hold", 3_010_000_000, 3_240_000_000),
                           ]]
        result = analysis.summarize(value)
        request = next(r for r in result["longest_control_requests"] if r["id"] == 1)
        wait = request["longest_recorded_lock_wait_overlaps"][0]
        self.assertEqual(wait["recorded_holder_union_ns"], 230_000_000)
        self.assertEqual(wait["unattributed_ns"], 40_000_000)


if __name__ == "__main__":
    unittest.main()
