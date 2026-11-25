package timesync

import (
	"errors"
	"testing"
	"time"
)

func TestRunDirectRetriesFailedClockApplicationOrObservation(t *testing.T) {
	for _, failure := range []string{"clock-write", "clock-observation"} {
		t.Run(failure, func(t *testing.T) {
			current := time.Unix(1_800_000_000, 0)
			const boot = int64(100 * time.Second)
			result := consensusResult(ConfidenceDegraded, 2)
			result.consensus.LowerOffsetNanos = current.Add(-time.Second).UnixNano() - boot
			result.consensus.UpperOffsetNanos = current.Add(time.Second).UnixNano() - boot
			h := newDirectHarness(3, result)
			h.manager.now = func() time.Time { return current }
			bootCalls := 0
			h.manager.boot = func() (int64, error) {
				bootCalls++
				// Each attempt reads BOOTTIME before applying, then brackets
				// the resulting wall-clock observation with two more reads.
				if failure == "clock-observation" && (bootCalls == 2 || bootCalls == 5) {
					return 0, errors.New("clock temporarily unavailable")
				}
				return boot, nil
			}
			applyCalls := 0
			h.manager.applyResult = func(time.Time) error {
				applyCalls++
				if failure == "clock-write" && applyCalls <= 2 {
					return errors.New("clock write temporarily refused")
				}
				return nil
			}
			h.run(t)
			if applyCalls != 3 {
				t.Fatalf("clock applications = %d, want 3", applyCalls)
			}
			want := []time.Duration{backoffSchedule[0], backoffSchedule[1], resyncInterval}
			if len(h.waits) != len(want) {
				t.Fatalf("waits = %v, want %v", h.waits, want)
			}
			for i, delay := range want {
				if h.waits[i] != delay {
					t.Errorf("wait %d = %v, want %v after %s", i, h.waits[i], delay, failure)
				}
			}
		})
	}
}
