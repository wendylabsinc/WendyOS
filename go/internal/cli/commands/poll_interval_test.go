package commands

import (
	"testing"
	"time"
)

func TestIncreasingRefreshIntervalRampsToLimit(t *testing.T) {
	var interval increasingRefreshInterval
	maxDelay := 3 * time.Second

	want := []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		3 * time.Second,
		3 * time.Second,
	}

	for i, expected := range want {
		if got := interval.delay(maxDelay); got != expected {
			t.Fatalf("delay %d = %v, want %v", i, got, expected)
		}
	}
}

func TestIncreasingRefreshIntervalCapsInitialDelay(t *testing.T) {
	var interval increasingRefreshInterval
	maxDelay := 100 * time.Millisecond

	for i := 0; i < 3; i++ {
		if got := interval.delay(maxDelay); got != maxDelay {
			t.Fatalf("delay %d = %v, want %v", i, got, maxDelay)
		}
	}
}
