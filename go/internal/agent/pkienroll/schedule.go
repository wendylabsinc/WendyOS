package pkienroll

import "time"

// Renewal timing. The device holds a pki-core device-tier leaf, which is 30
// days at most on the bearer-token path, so an unrenewed certificate is a
// one-month fuse rather than the one-year fuse the CAS leaf is. Nothing in the
// agent renewed anything before this, on any certificate, so these constants
// are the first statement of the policy the certificate proto has documented
// all along: refresh about two thirds of the way through the lifetime.
const (
	// renewFraction is how far through the leaf's own validity window the
	// renewal is attempted. Computed from NotBefore and NotAfter as issued,
	// never from an assumed tier length, so a policy change at the tenant
	// shortens the interval without a code change.
	renewFraction = 2.0 / 3.0

	// renewJitterFraction spreads the attempt over this share of the remaining
	// window after the 2/3 point. A fleet enrolled in one batch would otherwise
	// renew in one thundering herd, 20 days later to the second.
	renewJitterFraction = 0.1

	// backoffBase and backoffMax bound the retry interval after a failure.
	// The base is small because the first failure is usually a boot-time
	// network that is seconds away from working; the ceiling is an hour
	// because past that the operator, not the retry, is the fix.
	backoffBase = 1 * time.Minute
	backoffMax  = 1 * time.Hour

	// criticalWindow is how close to expiry a still-failing renewal starts
	// being logged loudly. Inside it the device is about to lose its data
	// platform identity, and past expiry pki-core will not renew at all
	// without an operator-signed grant the agent cannot produce, so the
	// message has to arrive while it can still be acted on.
	criticalWindow = 24 * time.Hour

	// pollFloor bounds how long the loop sleeps when it has nothing else to
	// say, so a clock jump backwards cannot park it for a month.
	pollFloor = 1 * time.Minute
)

// Schedule is one renewal decision: what to do now, and how long to wait if
// the answer is "not yet".
type Schedule struct {
	// Attempt is true when a renewal should be tried immediately.
	Attempt bool
	// Wait is how long to sleep before asking again. Zero when Attempt is true.
	Wait time.Duration
	// Critical is true when the leaf is inside criticalWindow of expiry and
	// renewal has already failed at least once — the state worth shouting
	// about, because a device with a healthy certificate 12 hours from renewal
	// is not news and a device 12 hours from losing its identity is.
	Critical bool
	// Expired is true once the leaf's validity has passed. Renewal is no
	// longer possible on its own; re-enrolment with a fresh token is.
	Expired bool
}

// NextRenewal decides what the renewal loop does next. It is pure: every input
// including the jitter is a parameter, so the whole policy is testable without
// a clock, a network or a random source.
//
// notBefore and notAfter are the issued leaf's own validity bounds. failures is
// the count of consecutive failed attempts, which selects the backoff and gates
// Critical. jitterFrac is a value in [0,1) — a random draw in production, a
// constant in tests.
func NextRenewal(notBefore, notAfter, now time.Time, failures int, jitterFrac float64) Schedule {
	if !notAfter.After(now) {
		// Past expiry, keep trying on the backoff anyway: a renewal cannot
		// succeed, but the attempt is what produces the error that says
		// re-enrolment is needed, and the alternative is a silent stop.
		return Schedule{Attempt: failures == 0, Wait: backoffFor(failures), Critical: true, Expired: true}
	}

	critical := failures > 0 && notAfter.Sub(now) <= criticalWindow

	if failures > 0 {
		// A failure means the 2/3 point is already behind us, so the only
		// question left is how long to wait before retrying.
		return Schedule{Wait: backoffFor(failures), Critical: critical}
	}

	lifetime := notAfter.Sub(notBefore)
	if lifetime <= 0 {
		// A leaf whose window is empty or inverted tells us nothing about when
		// to renew. Try now rather than compute a nonsense delay from it.
		return Schedule{Attempt: true}
	}
	renewAt := notBefore.Add(time.Duration(float64(lifetime) * renewFraction))
	if jitterFrac > 0 {
		remaining := notAfter.Sub(renewAt)
		if remaining > 0 {
			renewAt = renewAt.Add(time.Duration(float64(remaining) * renewJitterFraction * clampUnit(jitterFrac)))
		}
	}
	if !now.Before(renewAt) {
		return Schedule{Attempt: true, Critical: critical}
	}
	wait := renewAt.Sub(now)
	if wait < pollFloor {
		wait = pollFloor
	}
	return Schedule{Wait: wait, Critical: critical}
}

// backoffFor is exponential from backoffBase, capped at backoffMax.
func backoffFor(failures int) time.Duration {
	if failures <= 0 {
		return backoffBase
	}
	d := backoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	return d
}

func clampUnit(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f >= 1 {
		// Strictly below 1 so jitter never pushes the attempt to the expiry
		// instant itself.
		return 0.999
	}
	return f
}
