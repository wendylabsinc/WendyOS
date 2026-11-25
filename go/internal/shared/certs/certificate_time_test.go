package certs

import (
	"testing"
	"time"
)

func TestCertificateTimePolicyUsesOnlyExplicitLocalUncertainty(t *testing.T) {
	floor := time.Unix(1800000000, 0)
	for _, test := range []struct {
		name            string
		wall, leaf      time.Time
		uncertain, want bool
	}{
		{"boot", floor.Add(-598 * time.Second), floor.Add(12 * time.Hour), true, true},
		{"at-floor", floor, floor.Add(12 * time.Hour), true, true},
		{"past-cap", floor.Add(-598 * time.Second), floor.Add(24*time.Hour + time.Nanosecond), true, false},
		{"known-utc", floor.Add(-598 * time.Second), floor.Add(time.Minute), false, false},
		{"wall-above-anchor", floor.Add(time.Second), floor.Add(time.Minute), true, true},
		{"known-base", floor.Add(-598 * time.Second), floor.Add(-time.Second), false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := CertificateTimePolicy{Floor: func() time.Time { return floor }, Uncertain: func() bool { return test.uncertain }}
			if got := !policy.LeafTime(test.wall, test.leaf).Before(test.leaf); got != test.want {
				t.Fatalf("leaf eligible=%v want=%v", got, test.want)
			}
		})
	}
	// The policy consumes a progressing local provider without borrowing the
	// peer's timestamp as the source of either the floor or elapsed time.
	elapsed := 2 * time.Hour
	wall := floor.Add(-598 * time.Second).Add(elapsed)
	p := CertificateTimePolicy{Floor: func() time.Time { return floor.Add(elapsed) }, Uncertain: func() bool { return true }}
	if p.LeafTime(wall, floor.Add(elapsed+time.Minute)).Before(floor.Add(elapsed + time.Minute)) {
		t.Fatal("local progressing floor disappeared during offline operation")
	}
}
