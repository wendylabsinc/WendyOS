"""AMCL observation health and stationary updates; never authorizes motion.

Fresh-observation throttling follows WendyOS 312028af4. All receipt ages use
monotonic wall time; scan stamps remain in the separate simulation clock.
"""
import math
import threading

VARIANCE_LIMIT = .0225


class NonOverlappingCall:
    """Skip reentrant timer callbacks until dispatch and acknowledgment finish."""

    def __init__(self):
        self.lock = threading.Lock()

    def run(self, callback):
        if not self.lock.acquire(False):
            return False
        try:
            callback()
            return True
        finally:
            self.lock.release()


def finite(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def age(now, received):
    if not finite(now) or not finite(received) or received <= 0 or now < received:
        return None
    return now - received


def status_measurements(pose, covariance):
    """Keep invalid raw ROS values from hiding health diagnostics behind HTTP 500."""
    clean_pose = pose if isinstance(pose, dict) and all(finite(pose.get(k)) for k in ('x', 'y', 'yaw')) else None
    clean_covariance = list(covariance) if len(covariance) == 36 and all(finite(v) for v in covariance) else []
    return clean_pose, clean_covariance


def health(*, now, sensor_time, pose_time, costmap_time, policy_ready,
           sensors_ready, scan_valid, pose, covariance, navigation_ready,
           planner_ready, passive):
    ages = dict(sensor_age_s=age(now, sensor_time), pose_age_s=age(now, pose_time),
                costmap_age_s=age(now, costmap_time))
    reasons = []
    for flag, reason in ((policy_ready, 'policy_unconfirmed'),
                         (sensors_ready, 'sensors_unready'), (scan_valid, 'scan_invalid')):
        if not flag:
            reasons.append(reason)
    for name, limit in (('sensor', .6), ('pose', 3.), ('costmap', 3.)):
        elapsed = ages[name + '_age_s']
        if elapsed is None or elapsed >= limit:
            reasons.append(name + '_stale')
    if not isinstance(pose, dict) or not all(finite(pose.get(k)) for k in ('x', 'y', 'yaw')):
        reasons.append('pose_invalid')
    variance = []
    if len(covariance) == 36 and all(finite(v) for v in covariance):
        variance = [covariance[i] for i in (0, 7, 35)]
    if not variance or any(v < 0 for v in variance):
        reasons.append('covariance_invalid')
    elif any(v >= VARIANCE_LIMIT for v in variance):
        reasons.append('covariance_high')
    if not navigation_ready:
        reasons.append('navigation_unavailable')
    if not planner_ready:
        reasons.append('planner_unavailable')
    return dict(schema='wendy.nav.localization-health.v1', ready=not reasons,
                reasons=reasons, **ages, variance=variance, variance_limit=VARIANCE_LIMIT,
                passive=passive.status(now), motion_resumed=False)


class PassiveUpdates:
    """At most one observation request in flight; completion has no pose effect."""

    def __init__(self):
        self.attempts = 0
        self.pending = None
        self.last_stamp = None
        self.last_attempt = None

    def begin(self, *, now, stamp, sensor_time, sensors_ready, scan_valid,
              speed, angular_speed, command, scope):
        elapsed = age(now, sensor_time)
        if (self.pending is not None or elapsed is None or elapsed >= .6
                or not sensors_ready or not scan_valid or not finite(stamp) or stamp < 0
                or (self.last_stamp is not None and stamp <= self.last_stamp)
                or (self.last_attempt is not None and
                    (not finite(now) or now - self.last_attempt < 2.))
                or not finite(speed) or not 0 <= speed < .08
                or not finite(angular_speed) or not 0 <= angular_speed < .1
                or len(command) != 3 or not all(finite(v) and abs(v) <= 1e-6 for v in command)):
            return None
        self.attempts += 1
        self.last_stamp, self.last_attempt = stamp, now
        # Request identity includes the observation and frame/mission snapshot.
        self.pending = (self.attempts, stamp, scope)
        return self.pending

    def finish(self, token):
        if self.pending == token:
            self.pending = None

    def status(self, now):
        return dict(attempts=self.attempts, pending=self.pending is not None,
                    last_scan_stamp=self.last_stamp,
                    last_attempt_age_s=age(now, self.last_attempt))
