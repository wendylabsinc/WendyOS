#!/usr/bin/env python3
"""Measure live publication/physics rates in the ROS runtime image.

Source the Humble and Unitree overlays, then run from /opt/wendy-go2 with
GO2_NATIVE=1 and the profile's loopback Cyclone configuration. This measures
producer counters, not subscriber delivery or the ten-minute VM acceptance gate.
Use the separate standard/native/navigation apps for those checks.
"""

import argparse
from collections import defaultdict, deque
import json
import os
from pathlib import Path
import sys
import time

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--seconds", type=float, default=30)
    parser.add_argument("--warmup", type=float, default=5)
    parser.add_argument("--render", action="store_true")
    parser.add_argument("--profile", action="store_true", help="include the latest 30,000 stage durations")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if not np.isfinite(args.seconds) or args.seconds < 1 or not np.isfinite(args.warmup) or args.warmup < 0:
        parser.error("seconds must be at least 1 and warmup must be nonnegative")

    from go2_sim.runtime import Runtime
    from go2_sim.ros import RobotObservations
    from go2_sim.slow_sensors import SlowObservations
    from go2_sim.sensors import PhysicsSampler
    from go2_sim.lidar import Lidar
    from go2_sim.native_state import NativeState

    timings = defaultdict(lambda: deque(maxlen=30000))

    def instrument(cls, name):
        original = getattr(cls, name)

        def measured(*values, **kwargs):
            started = time.perf_counter_ns()
            try:
                return original(*values, **kwargs)
            finally:
                timings[f"{cls.__name__}.{name}"].append((time.perf_counter_ns() - started) / 1e6)
        setattr(cls, name, measured)

    if args.profile:
        for cls, methods in ((RobotObservations, ("sample", "publish_imu", "publish_motion")),
                             (SlowObservations, ("sample", "publish_camera")),
                             (PhysicsSampler, ("capture", "state", "feet")),
                             (Lidar, ("sample",)), (NativeState, ("publish",))):
            for method in methods:
                instrument(cls, method)
    runtime = Runtime(ros=True, render=args.render)
    try:
        runtime.start()
        time.sleep(args.warmup)
        before = runtime.status()
        time.sleep(args.seconds)
        after = runtime.status()
        elapsed = after["metrics"]["wall_seconds"] - before["metrics"]["wall_seconds"]
        rates = {key: (after["metrics"][key] - before["metrics"][key]) / elapsed
                 for key in ("physics_steps", "policy_updates", "camera_frames", "cpu_seconds")}
        ros = {kind: {key: (after["ros"][kind][key] - before["ros"][kind].get(key, 0)) / elapsed
                      for key in after["ros"][kind]} for kind in ("samples", "native_samples")}
        targets = {"physics": rates["physics_steps"] >= 475,
                   "policy": rates["policy_updates"] >= 47.5,
                   "imu": ros["samples"].get("imu", 0) >= 190,
                   "joints": ros["samples"].get("joints", 0) >= 47.5,
                   "lidar": ros["samples"].get("scan", 0) >= 9.5,
                   "cloud": ros["samples"].get("cloud", 0) >= 9.5}
        if os.environ.get("GO2_NATIVE") == "1":
            targets["lowstate"] = ros["native_samples"].get("lowstate", 0) >= 475
        if args.render:
            targets["camera"] = ros["samples"].get("camera", 0) >= 14
        queue = after["ros"]["snapshot_queue"]
        queue_delta = {key: queue[key] - before["ros"]["snapshot_queue"][key]
                       for key in ("captured", "consumed", "overflow", "expired")}
        result = {"kind": "live_producer_timing", "elapsed": elapsed, "mode": after["mode"],
                  "error": after["error"], "rates": rates, "ros": ros,
                  "snapshot_queue": queue, "snapshot_queue_delta": queue_delta,
                  "targets": targets, "rate_targets_met": all(targets.values()) and not after["error"],
                  "timing_ms": {key: {"calls": len(values), "mean": float(np.mean(values)),
                                      "p95": float(np.percentile(values, 95)),
                                      "p99": float(np.percentile(values, 99))}
                                for key, values in timings.items()}}
    finally:
        runtime.close()
    encoded = json.dumps(result, indent=2) + "\n"
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded)
    print(encoded, end="")
    return 0 if result["rate_targets_met"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
