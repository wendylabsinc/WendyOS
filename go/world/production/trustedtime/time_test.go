package trustedtime

import (
	"errors"
	"testing"
	"time"
)

func TestHoldoverIncludesSuspendDriftAndFailsAcrossBoot(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	r := Reading{BootID: "boot-a", Elapsed: time.Second}
	a := Interval{Earliest: now.Add(-time.Millisecond), Latest: now.Add(time.Millisecond), BootID: r.BootID, Monotonic: r.Elapsed, Domain: "physical", Authenticated: true, Source: "test-verified-time"}
	h, err := NewHoldover(HoldoverConfig{Anchor: VerifiedAnchor{a, "test-proof"}, Read: func() (Reading, error) { return r, nil }, MaxHoldover: time.Hour, DriftPPM: 100, MaxWidth: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// A suspend-inclusive platform reading advances even while this process
	// did not run. One minute must incur a full minute's drift uncertainty.
	r.Elapsed += time.Minute
	w, err := h.Now()
	if err != nil {
		t.Fatal(err)
	}
	if !w.Earliest.Equal(a.Earliest.Add(time.Minute-6*time.Millisecond)) || !w.Latest.Equal(a.Latest.Add(time.Minute+6*time.Millisecond)) {
		t.Fatal("wrong drift interval", w)
	}
	r.BootID = "boot-b"
	if _, err = h.Now(); !errors.Is(err, ErrUntrusted) {
		t.Fatal("boot discontinuity admitted", err)
	}
	r.BootID = "boot-a"
	if _, err = h.Now(); err == nil {
		t.Fatal("discontinuity recovered without a new authenticated anchor")
	}
}

func TestHoldoverRejectsRollbackStalenessAndFakeTime(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	for _, mode := range []string{"rollback", "expired", "width", "simulation", "unauthenticated"} {
		t.Run(mode, func(t *testing.T) {
			r := Reading{BootID: "boot", Elapsed: time.Second}
			a := Interval{Earliest: now, Latest: now.Add(time.Millisecond), BootID: r.BootID, Monotonic: r.Elapsed, Domain: "physical", Authenticated: true, Source: "test-time"}
			if mode == "simulation" {
				a.Domain = "simulation"
			}
			if mode == "unauthenticated" {
				a.Authenticated = false
			}
			h, err := NewHoldover(HoldoverConfig{Anchor: VerifiedAnchor{a, "test-proof"}, Read: func() (Reading, error) { return r, nil }, MaxHoldover: time.Minute, DriftPPM: 1000, MaxWidth: 10 * time.Millisecond})
			if mode == "simulation" || mode == "unauthenticated" {
				if err == nil {
					t.Fatal("fake anchor accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "rollback":
				r.Elapsed--
			case "expired":
				r.Elapsed += time.Minute + time.Nanosecond
			case "width":
				r.Elapsed += 10 * time.Second
			}
			if _, err = h.Now(); err == nil {
				t.Fatal("invalid holdover accepted")
			}
		})
	}
}
