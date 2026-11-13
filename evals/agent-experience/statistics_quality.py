"""Conservative, dependency-free uncertainty estimates for independent Bernoulli trials."""
import math


Z = 1.959963984540054


def wilson(successes, attempts):
    """Two-sided 95% Wilson score interval; no observations are unknown."""
    if not attempts:
        return None
    z = Z
    p = successes / attempts
    denominator = 1 + z * z / attempts
    center = (p + z * z / (2 * attempts)) / denominator
    radius = z * math.sqrt(p * (1 - p) / attempts + z * z / (4 * attempts * attempts)) / denominator
    return [max(0., center - radius), min(1., center + radius)]


def success_comparison(baseline, candidate, minimum=10, tolerance=.1):
    """Newcombe's Wilson-based interval for baseline minus candidate success rate.

    Pass means the upper interval bound excludes a loss larger than tolerance.
    Regression means the lower bound exceeds tolerance. Other results are uncertain.
    Intervals are per comparison, not a simultaneous guarantee across a whole suite.
    """
    b, c = baseline, candidate
    if not b['success_attempts'] or not c['success_attempts']:
        return {'verdict': 'insufficient_evidence', 'difference': None, 'interval': None}
    bp, cp = b['success_rate'], c['success_rate']
    bl, bu = b['success_interval']; cl, cu = c['success_interval']
    delta = bp - cp
    interval = [max(-1., delta - math.hypot(bp - bl, cu - cp)),
                min(1., delta + math.hypot(bu - bp, cp - cl))]
    enough = min(b['success_attempts'], c['success_attempts']) >= minimum
    verdict = ('regression' if interval[0] > tolerance else
               'pass' if interval[1] <= tolerance else 'insufficient_evidence') if enough else 'insufficient_evidence'
    return {'verdict': verdict, 'difference': delta, 'interval': interval}


def attempts_to_pass(tolerance):
    """Fewest attempts per side at which two sides with the same success rate can pass. It is reached
    when both succeed every time, where the interval's upper bound is z^2 / (n + z^2); None when no
    count is enough."""
    return math.ceil(Z * Z * (1 - tolerance) / tolerance) if tolerance > 0 else None
