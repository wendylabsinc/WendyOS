"""Retained original-step local gas samples for opt-in simulation policy.

A fixed 100 ms sensor cadence is evaluated inside the unchanged physics step.
Reads copy the bounded history and never generate or freshen a measurement.
"""
from __future__ import annotations
import copy
import math
import time
from collections import deque

class EnvironmentMeter:
    def __init__(self, run_id, model_sha256, boot_id):
        self.scope = dict(schema='wendy.sim.local-environment.v1',
                          method='original-step-local-gas-v1', run_id=run_id,
                          boot_id=boot_id, model_sha256=model_sha256,
                          frame=dict(id='office-floor', revision='office-'+model_sha256[:16]),
                          sensor_id='gas-1', calibration_revision='go2-virtual-mounts-v1',
                          species='synthetic-test-gas', unit='ppm', provenance='synthetic')
        self.samples = deque(maxlen=64)
        self.sequence = 0
        self.last_step_ns = 0
        self.last_sample_ns = 0
        self.failed = False

    def step(self, simulation_s, measure, captured_ms=None):
        stamp = round(simulation_s*1e9)
        if self.failed or stamp <= self.last_step_ns:
            self.failed = True
            return
        self.last_step_ns = stamp
        if stamp-self.last_sample_ns < 100000000:
            return
        self.last_sample_ns = stamp
        self.sequence += 1
        value, uncertainty = measure(self.sequence)
        valid = all(math.isfinite(v) and 0 <= v <= 1e6 for v in (value, uncertainty))
        self.samples.append(dict(self.scope, sequence=self.sequence, simulation_ns=stamp,
                                 captured_unix_ms=int(time.time()*1000) if captured_ms is None else captured_ms,
                                 value_ppm=value if valid else 0., uncertainty_ppm=uncertainty if valid else 0., valid=valid))

    def snapshot(self):
        result = copy.deepcopy(list(self.samples))
        if self.failed and result:
            result[-1]['valid'] = False
        return dict(samples=result)
