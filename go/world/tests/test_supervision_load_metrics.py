"""Deterministic tests; no Docker, clocks, files or network are consulted."""
import copy
import unittest

from supervision_load_metrics import (
    BOUNDS_NS, COUNTER_NAMES, DURATION_NAMES, INT64_MAX, SCHEMA, UINT64_MAX,
    metric_delta, validate_snapshot, SCHEMA_V1, SCHEMA_NAMES, DELTA_SCHEMA,
)


def snapshot(schema=SCHEMA):
    durations, counters = SCHEMA_NAMES[schema]
    return {
        "schema": schema, "enabled": True, "bucket_semantics": "noncumulative",
        "bucket_upper_bounds_ns": list(BOUNDS_NS), "instance_id": "a" * 32,
        "captured_at_unix_ms": 1_700_000_000_000, "uptime_ns": 1_000_000,
        "durations": [{"name": name, "count": 0, "sum_ns": 0, "max_ns": 0,
                       "buckets": [0] * len(BOUNDS_NS), "saturated": False}
                      for name in durations],
        "counters": [{"name": name, "value": 0, "saturated": False}
                     for name in counters],
    }


def later(value):
    result = copy.deepcopy(value)
    result["captured_at_unix_ms"] += 100
    result["uptime_ns"] += 100_000_000
    return result


def observe(value, amount, metric=0):
    row = value["durations"][metric]
    row["count"] += 1
    row["sum_ns"] += amount
    row["max_ns"] = max(row["max_ns"], amount)
    index = next(i for i, bound in enumerate(BOUNDS_NS) if bound is None or amount <= bound)
    row["buckets"][index] += 1


class MetricDeltaTests(unittest.TestCase):
    def test_exact_versions_preserve_provenance_and_cardinality(self):
        for schema in (SCHEMA_V1, SCHEMA):
            with self.subTest(schema=schema):
                before = snapshot(schema)
                after = later(before)
                observe(after, 5_000)
                after["counters"][-1]["value"] = 7
                self.assertIsNone(validate_snapshot(before))
                result = metric_delta(before, after)
                self.assertEqual(result["schema"], DELTA_SCHEMA)
                self.assertEqual(result["source_snapshot_schema"], schema)
                self.assertEqual([r["name"] for r in result["durations"]], list(SCHEMA_NAMES[schema][0]))
                self.assertEqual([r["name"] for r in result["counters"]], list(SCHEMA_NAMES[schema][1]))
                self.assertEqual(result["counters"][-1]["value"], 7)
                self.assertEqual(result["durations"][0]["count"], 1)

    def test_cross_version_deltas_are_refused_in_both_directions(self):
        for first, second in ((SCHEMA_V1, SCHEMA), (SCHEMA, SCHEMA_V1)):
            before, after = snapshot(first), later(snapshot(second))
            saved = copy.deepcopy((before, after))
            with self.assertRaisesRegex(ValueError, "schema changed"):
                metric_delta(before, after)
            self.assertEqual((before, after), saved)

    def test_schema_cannot_relabel_or_mix_metric_sets(self):
        for first, second in ((SCHEMA_V1, SCHEMA), (SCHEMA, SCHEMA_V1)):
            changed = snapshot(first)
            changed["schema"] = second
            with self.assertRaises(ValueError):
                validate_snapshot(changed)
        for schema in (SCHEMA_V1, SCHEMA):
            for field in ("durations", "counters"):
                changed = snapshot(schema)
                changed[field][-1]["name"] = "unknown.label"
                with self.assertRaises(ValueError):
                    validate_snapshot(changed)
        for invalid in (None, True, [], {}, "wendy.world.runtimeobs.v3"):
            changed = snapshot()
            changed["schema"] = invalid
            with self.assertRaises(ValueError):
                validate_snapshot(changed)

    def test_v2_storage_and_repair_totals_are_independent(self):
        before = snapshot()
        after = later(before)
        expected_durations = ("world_store.commit", "proof_store.commit", "node.lock_wait", "http.stop")
        for index, name in enumerate(DURATION_NAMES):
            if name in expected_durations:
                observe(after, (index + 1) * 1_000, metric=index)
        for index, row in enumerate(after["counters"]):
            row["value"] = (index + 1) * 1024
        result = metric_delta(before, after)
        for row in result["durations"]:
            self.assertEqual(row["count"], int(row["name"] in expected_durations))
        for index, row in enumerate(result["counters"]):
            self.assertEqual(row["value"], (index + 1) * 1024)
        self.assertEqual(len(result["durations"]), 12)
        self.assertEqual(len(result["counters"]), 16)

    def test_validate_lifetime_snapshot_preserves_input_without_creating_window(self):
        value = snapshot()
        observe(value, 500_000_000)
        value["counters"][0]["value"] = 1
        saved = copy.deepcopy(value)
        self.assertIsNone(validate_snapshot(value))
        self.assertEqual(value, saved)
        with self.assertRaisesRegex(ValueError, "uptime did not advance"):
            metric_delta(value, copy.deepcopy(value))

    def test_validate_lifetime_snapshot_rejects_invalid_without_mutation(self):
        for change in (
            lambda s: s.pop("instance_id"),
            lambda s: s.update(uptime_ns=-1),
            lambda s: s["durations"][0].update(count=1),
            lambda s: s["counters"][0].update(saturated=True),
            lambda s: s.update(principal="unsupported"),
        ):
            value = snapshot()
            change(value)
            saved = copy.deepcopy(value)
            with self.assertRaises(ValueError):
                validate_snapshot(value)
            self.assertEqual(value, saved)

    def test_window_deltas_keep_lifetime_maximum_and_bucket_bounds(self):
        before = snapshot()
        observe(before, 500_000_000)
        after = later(before)
        for sample in (0, 100, 1000, 1001):
            observe(after, sample)
        after["counters"][0]["value"] = 3
        saved = copy.deepcopy((before, after))
        result = metric_delta(before, after)
        self.assertEqual((before, after), saved)
        self.assertEqual(result["duration_ns"], 100_000_000)
        row = result["durations"][0]
        self.assertEqual(row["count"], 4)
        self.assertEqual(row["sum_ns"], 2101)
        self.assertEqual(row["max_ns_process_lifetime"], 500_000_000)
        self.assertEqual(row["percentile_upper_bounds_ns"], {"p50": 1000, "p95": 10000, "p99": 10000})
        self.assertEqual(row["percentile_bound_status"]["p95"], "finite")
        self.assertEqual(result["counters"][0]["value"], 3)
        self.assertEqual(result["maximum_scope"], "process_lifetime")

    def test_empty_and_overflow_quantiles_are_explicit(self):
        before = snapshot()
        after = later(before)
        observe(after, 60_000_000_001)
        result = metric_delta(before, after)
        self.assertIsNone(result["durations"][0]["percentile_upper_bounds_ns"]["p95"])
        self.assertEqual(result["durations"][0]["percentile_bound_status"]["p95"], "overflow_bucket")
        self.assertEqual(result["durations"][1]["percentile_bound_status"]["p95"], "empty")

    def test_exact_upper_bound_remains_in_finite_bucket(self):
        before = snapshot()
        after = later(before)
        observe(after, 60_000_000_000)
        self.assertEqual(metric_delta(before, after)["durations"][0]["percentile_upper_bounds_ns"]["p99"], 60_000_000_000)

    def test_rejects_malformed_schema_fields_and_cardinality(self):
        changes = {
            "missing": lambda s: s.pop("enabled"),
            "unknown": lambda s: s.update(principal="private"),
            "schema": lambda s: s.update(schema="other"),
            "disabled": lambda s: s.update(enabled=False),
            "enabled-int": lambda s: s.update(enabled=1),
            "cumulative": lambda s: s.update(bucket_semantics="cumulative"),
            "short-bounds": lambda s: s["bucket_upper_bounds_ns"].pop(),
            "changed-bound": lambda s: s["bucket_upper_bounds_ns"].__setitem__(0, 999),
            "finite-overflow": lambda s: s["bucket_upper_bounds_ns"].__setitem__(-1, 100000000000),
            "short-durations": lambda s: s["durations"].pop(),
            "extra-duration": lambda s: s["durations"].append(copy.deepcopy(s["durations"][0])),
            "unknown-name": lambda s: s["durations"][0].update(name="principal:private"),
            "duplicate-name": lambda s: s["durations"][1].update(name=DURATION_NAMES[0]),
            "extra-metric-field": lambda s: s["durations"][0].update(path="private"),
            "short-counters": lambda s: s["counters"].pop(),
            "counter-name": lambda s: s["counters"][0].update(name="other"),
            "counter-field": lambda s: s["counters"][0].update(nonce="private"),
        }
        for label, change in changes.items():
            with self.subTest(label=label):
                before = snapshot()
                after = later(before)
                change(after)
                with self.assertRaises(ValueError):
                    metric_delta(before, after)

    def test_rejects_noninteger_negative_overflow_and_saturation(self):
        for value in (True, False, -1, 1.0, float("nan"), float("inf"), UINT64_MAX + 1, "1", None):
            with self.subTest(value=value):
                before = snapshot()
                after = later(before)
                after["counters"][0]["value"] = value
                with self.assertRaises(ValueError):
                    metric_delta(before, after)
        for field in ("durations", "counters"):
            for saturation in (True, 0, "false", None):
                with self.subTest(field=field, saturation=saturation):
                    before = snapshot()
                    after = later(before)
                    after[field][0]["saturated"] = saturation
                    with self.assertRaises(ValueError):
                        metric_delta(before, after)

    def test_rejects_inconsistent_histograms_and_maxima(self):
        changes = {
            "count": lambda r: r.update(count=2),
            "sum": lambda r: r.update(sum_ns=1),
            "maximum": lambda r: r.update(max_ns=1000),
            "too-large-max": lambda r: r.update(max_ns=INT64_MAX + 1),
            "truncated": lambda r: r["buckets"].pop(),
            "negative-bin": lambda r: r["buckets"].__setitem__(0, -1),
            "float-bin": lambda r: r["buckets"].__setitem__(0, 0.0),
            "bin-count": lambda r: r["buckets"].__setitem__(0, 1),
        }
        for label, change in changes.items():
            with self.subTest(label=label):
                before = snapshot()
                after = later(before)
                observe(after, 5000)
                change(after["durations"][0])
                with self.assertRaises(ValueError):
                    metric_delta(before, after)

    def test_rejects_resets_and_impossible_window_sum(self):
        before = snapshot()
        observe(before, 5000)
        after = later(snapshot())
        with self.assertRaises(ValueError):
            metric_delta(before, after)
        after = later(before)
        after["counters"][0]["value"] = 1
        before["counters"][0]["value"] = 2
        with self.assertRaises(ValueError):
            metric_delta(before, after)
        # Both cumulative snapshots are plausible, but their claimed difference
        # cannot contain a 5000ns maximum with only 2001ns of added sum.
        before = snapshot()
        observe(before, 4000)
        after = later(before)
        observe(after, 5000)
        after["durations"][0]["sum_ns"] = 6001
        with self.assertRaises(ValueError):
            metric_delta(before, after)

    def test_rejects_restarts_invalid_capture_and_empty_windows(self):
        for change in (
            lambda s: s.update(instance_id="b" * 32),
            lambda s: s.update(instance_id="A" * 32),
            lambda s: s.update(instance_id="short"),
            lambda s: s.update(uptime_ns=1_000_000),
            lambda s: s.update(uptime_ns=-1),
            lambda s: s.update(uptime_ns=True),
            lambda s: s.update(uptime_ns=INT64_MAX + 1),
            lambda s: s.update(captured_at_unix_ms=0),
            lambda s: s.update(captured_at_unix_ms=1),
            lambda s: s.update(captured_at_unix_ms=1_699_999_999_999),
            lambda s: s.update(captured_at_unix_ms=float("inf")),
        ):
            before = snapshot()
            after = later(before)
            change(after)
            with self.assertRaises(ValueError):
                metric_delta(before, after)

    def test_zero_duration_observations_and_uint64_counter_are_valid(self):
        before = snapshot()
        after = later(before)
        observe(after, 0)
        after["counters"][0]["value"] = UINT64_MAX
        result = metric_delta(before, after)
        self.assertEqual(result["durations"][0]["sum_ns"], 0)
        self.assertEqual(result["durations"][0]["percentile_upper_bounds_ns"]["p50"], 1000)
        self.assertEqual(result["counters"][0]["value"], UINT64_MAX)


if __name__ == "__main__":
    unittest.main()
