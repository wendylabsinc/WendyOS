//go:build !linux

package data

import (
	"fmt"
	"time"
)

// Non-Linux builds exist for development only. The manifest explicitly names
// this domain so it can never be mistaken for Linux CLOCK_BOOTTIME evidence.
var processBoot = time.Now()

func readBootTime() (int64, error) { return time.Since(processBoot).Nanoseconds(), nil }

func CaptureReceipt() (before, midpoint, after int64, err error) {
	before, _ = readBootTime()
	after, _ = readBootTime()
	return before, before + (after-before)/2, after, nil
}

func SampleMonotonicMapping() (MonotonicMappingSample, error) {
	return MonotonicMappingSample{}, fmt.Errorf("CLOCK_MONOTONIC mapping is only available on Linux")
}

func sandwichUTC() (ClockSample, error) {
	a, _ := readBootTime()
	u := time.Now().UnixNano()
	b, _ := readBootTime()
	return ClockSample{BootBeforeNanos: a, TargetNanos: u, BootAfterNanos: b}, nil
}

func CaptureUTCClockSample() (ClockSample, error) { return sandwichUTC() }

func systemClockUncertainty() (int64, string) { return int64(^uint64(0) >> 1), "unbounded" }
func telemetryValues() map[string]any         { return map[string]any{"available": false} }
