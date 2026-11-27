"""Pure, bounded validation and window deltas for runtimeobs diagnostics.

No individual observations are available. Percentiles are nearest-rank bucket
upper bounds, never interpolated quantiles. Maxima remain process-lifetime data.
"""

import re

UINT64_MAX = (1 << 64) - 1
INT64_MAX = (1 << 63) - 1
SCHEMA_V1 = "wendy.world.runtimeobs.v1"
SCHEMA = "wendy.world.runtimeobs.v2"
DURATION_NAMES_V1 = (
    "http.challenge", "http.ack", "http.evaluate", "http.other",
    "supervisor.exchange", "authority.lock_wait", "replay.checkpoint_commit",
    "task_store.commit",
)
COUNTER_NAMES_V1 = (
    "http.failures", "supervisor.exchange_failures", "replay.checkpoint_failures",
    "replay.checkpoint_noops", "task_store.commit_failures",
)
DURATION_NAMES = DURATION_NAMES_V1 + (
    "world_store.commit", "proof_store.commit", "node.lock_wait", "http.stop",
)
COUNTER_NAMES = COUNTER_NAMES_V1 + (
    "world_store.commit_failures", "world_store.serialized_bytes",
    "proof_store.commit_failures", "proof_store.serialized_bytes",
    "repair.rounds", "repair.round_failures", "repair.records_fetched",
    "repair.applied", "repair.duplicate", "repair.obsolete", "repair.rejected",
)
SCHEMA_NAMES = {
    SCHEMA_V1: (DURATION_NAMES_V1, COUNTER_NAMES_V1),
    SCHEMA: (DURATION_NAMES, COUNTER_NAMES),
}
DELTA_SCHEMA = "wendy.world.runtimeobs.delta.v2"
BOUNDS_NS = (
    1_000, 10_000, 100_000, 500_000, 1_000_000, 2_000_000, 5_000_000,
    10_000_000, 25_000_000, 50_000_000, 100_000_000, 200_000_000,
    250_000_000, 500_000_000, 1_000_000_000, 2_000_000_000,
    5_000_000_000, 10_000_000_000, 30_000_000_000, 60_000_000_000, None,
)
SNAPSHOT_KEYS = frozenset((
    "schema", "enabled", "bucket_semantics", "bucket_upper_bounds_ns",
    "durations", "counters", "instance_id", "captured_at_unix_ms", "uptime_ns",
))
DURATION_KEYS = frozenset(("name", "count", "sum_ns", "max_ns", "buckets", "saturated"))
COUNTER_KEYS = frozenset(("name", "value", "saturated"))


def _object(value, keys):
    if type(value) is not dict or value.keys() != keys:
        raise ValueError("unexpected diagnostic fields")


def _uint(value, maximum=UINT64_MAX):
    # bool is an int subclass; it is never a valid count or timestamp.
    if type(value) is not int or value < 0 or value > maximum:
        raise ValueError("invalid bounded diagnostic integer")
    return value


def _bucket_index(duration):
    for index, upper in enumerate(BOUNDS_NS):
        if upper is None or duration <= upper:
            return index
    raise ValueError("missing overflow bucket")


def _histogram(count, total, buckets, maximum, exact_maximum):
    if sum(buckets) != count:
        raise ValueError("histogram count mismatch")
    if count == 0:
        if total != 0 or (exact_maximum and maximum != 0):
            raise ValueError("empty histogram has observations")
        return
    occupied = [index for index, amount in enumerate(buckets) if amount]
    maximum_index = _bucket_index(maximum)
    if occupied[-1] > maximum_index or (exact_maximum and occupied[-1] != maximum_index):
        raise ValueError("maximum disagrees with histogram")
    minimum_total = 0
    maximum_total = 0
    for index, amount in enumerate(buckets):
        lower = 0 if index == 0 else BOUNDS_NS[index - 1] + 1
        upper = BOUNDS_NS[index]
        minimum_total += lower * amount
        maximum_total += min(maximum, INT64_MAX if upper is None else upper) * amount
    if exact_maximum:
        # At least one observation must equal the reported maximum.
        lower = 0 if maximum_index == 0 else BOUNDS_NS[maximum_index - 1] + 1
        minimum_total += maximum - lower
    if not minimum_total <= total <= maximum_total:
        raise ValueError("sum disagrees with histogram bounds")


def _validate(snapshot):
    _object(snapshot, SNAPSHOT_KEYS)
    if type(snapshot["schema"]) is not str or snapshot["schema"] not in SCHEMA_NAMES or snapshot["enabled"] is not True:
        raise ValueError("enabled runtime measurements required")
    duration_names, counter_names = SCHEMA_NAMES[snapshot["schema"]]
    if snapshot["bucket_semantics"] != "noncumulative":
        raise ValueError("unsupported bucket semantics")
    instance = snapshot["instance_id"]
    if type(instance) is not str or re.fullmatch(r"[0-9a-f]{32}", instance) is None:
        raise ValueError("invalid process instance identifier")
    captured = _uint(snapshot["captured_at_unix_ms"], INT64_MAX)
    uptime = _uint(snapshot["uptime_ns"], INT64_MAX)
    if captured == 0 or uptime > captured * 1_000_000:
        raise ValueError("invalid capture time or uptime")
    bounds = snapshot["bucket_upper_bounds_ns"]
    if type(bounds) is not list or len(bounds) != len(BOUNDS_NS):
        raise ValueError("truncated bucket bounds")
    for actual, expected in zip(bounds, BOUNDS_NS):
        if expected is None:
            if actual is not None:
                raise ValueError("overflow bucket must be unbounded")
        elif _uint(actual, INT64_MAX) != expected:
            raise ValueError("unsupported bucket bounds")
    durations = snapshot["durations"]
    if type(durations) is not list or len(durations) != len(duration_names):
        raise ValueError("unexpected duration metric cardinality")
    for value, name in zip(durations, duration_names):
        _object(value, DURATION_KEYS)
        if value["name"] != name or value["saturated"] is not False:
            raise ValueError("unknown duration metric or saturated snapshot")
        count = _uint(value["count"])
        total = _uint(value["sum_ns"])
        maximum = _uint(value["max_ns"], INT64_MAX)
        buckets = value["buckets"]
        if type(buckets) is not list or len(buckets) != len(BOUNDS_NS):
            raise ValueError("truncated histogram")
        for amount in buckets:
            _uint(amount)
        _histogram(count, total, buckets, maximum, exact_maximum=True)
    counters = snapshot["counters"]
    if type(counters) is not list or len(counters) != len(counter_names):
        raise ValueError("unexpected counter cardinality")
    for value, name in zip(counters, counter_names):
        _object(value, COUNTER_KEYS)
        if value["name"] != name or value["saturated"] is not False:
            raise ValueError("unknown counter or saturated snapshot")
        _uint(value["value"])


def _percentile(count, buckets, percent):
    if count == 0:
        return None, "empty"
    rank = (count * percent + 99) // 100
    cumulative = 0
    for upper, amount in zip(BOUNDS_NS, buckets):
        cumulative += amount
        if cumulative >= rank:
            return upper, "overflow_bucket" if upper is None else "finite"
    raise ValueError("percentile exceeds histogram")


def validate_snapshot(snapshot):
    """Validate one complete process-lifetime snapshot without changing it.

    Returns None or raises ValueError. A valid snapshot alone establishes no
    measurement window or process-exit state; callers need independent evidence
    for either. In particular, identical snapshots cannot yield a time delta.
    """
    _validate(snapshot)


def metric_delta(before, after):
    """Validate two complete snapshots and return interval counts/sums/bins.

    Raises ValueError for unsupported, inconsistent, reset, saturated or empty
    time windows. Inputs are never mutated. Rates may use duration_ns, which is
    strictly positive and comes from the same process's monotonic uptime.
    Both inputs must use the same supported schema. Output uses delta.v2 and
    records source_snapshot_schema; historical v1 metrics are never filled
    with invented zero-valued v2 measurements.
    """
    _validate(before)
    _validate(after)
    if before["schema"] != after["schema"]:
        raise ValueError("diagnostic schema changed across snapshots")
    if before["instance_id"] != after["instance_id"]:
        raise ValueError("diagnostic process instance changed")
    if after["uptime_ns"] <= before["uptime_ns"]:
        raise ValueError("diagnostic uptime did not advance")
    if after["captured_at_unix_ms"] < before["captured_at_unix_ms"]:
        raise ValueError("capture clock moved backwards")
    durations = []
    for old, new in zip(before["durations"], after["durations"]):
        count = new["count"] - old["count"]
        total = new["sum_ns"] - old["sum_ns"]
        bins = [right - left for left, right in zip(old["buckets"], new["buckets"])]
        if count < 0 or total < 0 or any(value < 0 for value in bins) or new["max_ns"] < old["max_ns"]:
            raise ValueError("duration metric reset or moved backwards")
        if count == 0 and new["max_ns"] != old["max_ns"]:
            raise ValueError("maximum changed without an observation")
        _histogram(count, total, bins, new["max_ns"],
                   exact_maximum=new["max_ns"] > old["max_ns"])
        percentiles = {}
        statuses = {}
        for percent in (50, 95, 99):
            key = f"p{percent}"
            percentiles[key], statuses[key] = _percentile(count, bins, percent)
        durations.append({
            "name": new["name"], "count": count, "sum_ns": total,
            "max_ns_process_lifetime": new["max_ns"], "buckets": bins,
            "percentile_upper_bounds_ns": percentiles,
            "percentile_bound_status": statuses,
        })
    counters = []
    for old, new in zip(before["counters"], after["counters"]):
        difference = new["value"] - old["value"]
        if difference < 0:
            raise ValueError("counter reset or moved backwards")
        counters.append({"name": new["name"], "value": difference})
    return {
        "schema": DELTA_SCHEMA,
        "source_snapshot_schema": after["schema"],
        "instance_id": after["instance_id"],
        "bucket_semantics": "noncumulative",
        "bucket_upper_bounds_ns": list(BOUNDS_NS),
        "percentile_semantics": "nearest_rank_bucket_upper_bound",
        "maximum_scope": "process_lifetime",
        "start_captured_at_unix_ms": before["captured_at_unix_ms"],
        "end_captured_at_unix_ms": after["captured_at_unix_ms"],
        "duration_ns": after["uptime_ns"] - before["uptime_ns"],
        "durations": durations, "counters": counters,
    }
