"""Positive joint mechanical work, sampled after each unchanged original step.

No battery/electrical model or regeneration credit. The left endpoint is not
reconstructed: this is a post-step rectangle quadrature at the original 2 ms.
The meter observes torque and velocity; it never writes simulator state.
"""
from __future__ import annotations
import math
import time
import uuid

SCHEMA = 'wendy.sim.positive-actuator-work.v1'
METHOD = 'post-step-positive-joint-power-v1'


class WorkMeter:
    def __init__(self, run_id, model_sha256):
        self.sample = dict(schema=SCHEMA, method=METHOD, run_id=run_id,
                           boot_id=uuid.uuid4().hex, model_sha256=model_sha256,
                           sequence=0, simulation_ns=0, captured_unix_ms=0,
                           total_j=0., power_w=0., valid=False)
        self.failed = False

    def step(self, simulation_s, dt, torque, velocity, captured_ms=None):
        """Account once per original physics step, including standing and HOLD."""
        if self.failed:
            return
        try:
            torque, velocity = list(torque), list(velocity)
            if (len(torque) != 12 or len(velocity) != 12 or
                    not math.isfinite(dt) or not 0 < dt <= .01 or
                    not math.isfinite(simulation_s) or simulation_s <= 0 or
                    any(not math.isfinite(float(x)) for x in torque + velocity)):
                raise ValueError('invalid original-step work input')
            stamp = round(simulation_s * 1e9)
            last = self.sample['simulation_ns']
            if stamp <= last or (last and abs(stamp-last-round(dt*1e9)) > 2):
                raise ValueError('original step was skipped or reset')
            power = sum(max(0., float(a)*float(b)) for a, b in zip(torque, velocity))
            total = self.sample['total_j'] + power*dt
            if not math.isfinite(power) or not math.isfinite(total):
                raise ValueError('invalid accumulated work')
            self.sample.update(sequence=self.sample['sequence']+1,
                               simulation_ns=stamp,
                               captured_unix_ms=int(time.time()*1000) if captured_ms is None else captured_ms,
                               total_j=total, power_w=power, valid=True)
        except (ValueError, TypeError, OverflowError):
            self.failed = True
            self.sample['valid'] = False

    def snapshot(self):
        # Reading cannot freshen a stopped integrator or alias its retained state.
        return dict(self.sample)
