package pkienroll

import (
	"testing"
	"time"
)

// The schedule is the whole renewal policy, and it is a pure function so it can
// be pinned without a clock, a network or a random source. These tests are the
// specification: 2/3 of the leaf's own lifetime, jitter on top, exponential
// backoff on failure, and a loud state inside the last 24 hours.
func TestNextRenewalWaitsUntilTwoThirds(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)

	// Day 1 of 30: nothing to do, and the wait lands on the 2/3 point (day 20)
	// rather than on a fixed poll interval.
	now := notBefore.Add(24 * time.Hour)
	got := NextRenewal(notBefore, notAfter, now, 0, 0)
	if got.Attempt {
		t.Error("Attempt = true on day 1 of 30")
	}
	wantWait := notBefore.Add(20 * 24 * time.Hour).Sub(now)
	if got.Wait != wantWait {
		t.Errorf("Wait = %v, want %v", got.Wait, wantWait)
	}
	if got.Critical || got.Expired {
		t.Errorf("Critical = %v Expired = %v, want both false", got.Critical, got.Expired)
	}
}

func TestNextRenewalAttemptsAtTwoThirds(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)

	// Exactly the 2/3 point, no jitter.
	got := NextRenewal(notBefore, notAfter, notBefore.Add(20*24*time.Hour), 0, 0)
	if !got.Attempt {
		t.Error("Attempt = false at the 2/3 point")
	}
	if got.Wait != 0 {
		t.Errorf("Wait = %v, want 0 when attempting", got.Wait)
	}
}

func TestNextRenewalJitterDelaysTheAttempt(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)
	twoThirds := notBefore.Add(20 * 24 * time.Hour)

	// A full jitter draw pushes the attempt into the remaining third by
	// renewJitterFraction of it: one day of the ten that are left. Without
	// this a fleet enrolled in one batch renews in one thundering herd.
	got := NextRenewal(notBefore, notAfter, twoThirds, 0, 0.999)
	if got.Attempt {
		t.Error("Attempt = true at the 2/3 point with full jitter; want the jittered delay")
	}
	if got.Wait < 23*time.Hour || got.Wait > 25*time.Hour {
		t.Errorf("Wait = %v, want about 24h (a tenth of the remaining ten days)", got.Wait)
	}

	// And the jitter never pushes past expiry.
	if twoThirds.Add(got.Wait).After(notAfter) {
		t.Error("jitter pushed the attempt beyond the leaf's expiry")
	}
}

func TestNextRenewalNeverSleepsBelowThePollFloor(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)
	// One second short of the 2/3 point: a one-second sleep would busy-loop.
	got := NextRenewal(notBefore, notAfter, notBefore.Add(20*24*time.Hour-time.Second), 0, 0)
	if got.Attempt {
		t.Error("Attempt = true before the 2/3 point")
	}
	if got.Wait != pollFloor {
		t.Errorf("Wait = %v, want the poll floor %v", got.Wait, pollFloor)
	}
}

func TestNextRenewalBacksOffOnFailure(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)
	now := notBefore.Add(21 * 24 * time.Hour)

	for _, tc := range []struct {
		failures int
		want     time.Duration
	}{
		{1, backoffBase},
		{2, 2 * backoffBase},
		{3, 4 * backoffBase},
		{7, backoffMax},
		{99, backoffMax},
	} {
		got := NextRenewal(notBefore, notAfter, now, tc.failures, 0)
		if got.Attempt {
			t.Errorf("failures=%d: Attempt = true, want a backoff wait", tc.failures)
		}
		if got.Wait != tc.want {
			t.Errorf("failures=%d: Wait = %v, want %v", tc.failures, got.Wait, tc.want)
		}
	}
}

func TestNextRenewalCriticalOnlyWhenFailingNearExpiry(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)
	nearExpiry := notAfter.Add(-12 * time.Hour)

	// Healthy and 12 hours from renewal is not news.
	if got := NextRenewal(notBefore, notAfter, nearExpiry, 0, 0); got.Critical {
		t.Error("Critical = true with no failures; a healthy certificate must not shout")
	}
	// Failing and 12 hours from losing the identity is.
	if got := NextRenewal(notBefore, notAfter, nearExpiry, 1, 0); !got.Critical {
		t.Error("Critical = false while failing inside the 24h window")
	}
	// Failing but 9 days out is still just a retry.
	if got := NextRenewal(notBefore, notAfter, notAfter.Add(-9*24*time.Hour), 3, 0); got.Critical {
		t.Error("Critical = true nine days from expiry")
	}
}

func TestNextRenewalPastExpiry(t *testing.T) {
	notBefore := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(30 * 24 * time.Hour)

	got := NextRenewal(notBefore, notAfter, notAfter.Add(time.Hour), 0, 0)
	if !got.Expired {
		t.Error("Expired = false past NotAfter")
	}
	if !got.Critical {
		t.Error("Critical = false past NotAfter; an expired identity is always news")
	}
	// One attempt is still made, because the attempt is what produces the
	// error that says re-enrolment is needed.
	if !got.Attempt {
		t.Error("Attempt = false past expiry; the failure is the only signal")
	}
	// And once it has failed, it backs off rather than spinning.
	after := NextRenewal(notBefore, notAfter, notAfter.Add(time.Hour), 4, 0)
	if after.Attempt {
		t.Error("Attempt = true past expiry after four failures")
	}
	if after.Wait != 8*backoffBase {
		t.Errorf("Wait = %v, want %v", after.Wait, 8*backoffBase)
	}
}

func TestNextRenewalHandlesNonsenseWindow(t *testing.T) {
	now := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	// NotBefore after NotAfter tells us nothing about when to renew, so try
	// now rather than compute a negative delay from it.
	got := NextRenewal(now.Add(time.Hour), now.Add(time.Minute), now, 0, 0)
	if !got.Attempt {
		t.Error("Attempt = false for an inverted validity window")
	}
}

func TestClampUnit(t *testing.T) {
	if got := clampUnit(-1); got != 0 {
		t.Errorf("clampUnit(-1) = %v, want 0", got)
	}
	if got := clampUnit(5); got >= 1 {
		t.Errorf("clampUnit(5) = %v, want strictly below 1", got)
	}
	if got := clampUnit(0.25); got != 0.25 {
		t.Errorf("clampUnit(0.25) = %v", got)
	}
}
