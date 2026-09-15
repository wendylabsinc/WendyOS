//go:build linux

package data

import (
	"fmt"
	"time"

	"golang.org/x/sys/unix"
)

func readBootTime() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, fmt.Errorf("CLOCK_BOOTTIME: %w", err)
	}
	return ts.Nano(), nil
}

// CaptureReceipt brackets an agent receipt stamp on CLOCK_BOOTTIME. The
// midpoint is the canonical timestamp and half the read interval is its bound.
func CaptureReceipt() (before, midpoint, after int64, err error) {
	before, err = readBootTime()
	if err != nil {
		return 0, 0, 0, err
	}
	after, err = readBootTime()
	if err != nil {
		return 0, 0, 0, err
	}
	return before, before + (after-before)/2, after, nil
}

// SampleMonotonicMapping sandwiches CLOCK_MONOTONIC between CLOCK_BOOTTIME
// reads. Suspend changes their offset and therefore naturally starts a new
// mapping segment in camera adapters.
func SampleMonotonicMapping() (MonotonicMappingSample, error) {
	var sample MonotonicMappingSample
	var mono unix.Timespec
	var err error
	sample.BootBeforeNanos, err = readBootTime()
	if err != nil {
		return sample, err
	}
	if err = unix.ClockGettime(unix.CLOCK_MONOTONIC, &mono); err != nil {
		return sample, fmt.Errorf("CLOCK_MONOTONIC: %w", err)
	}
	sample.MonotonicNanos = mono.Nano()
	sample.BootAfterNanos, err = readBootTime()
	if err != nil {
		return sample, err
	}
	sample.OffsetLowerNanos = sample.BootBeforeNanos - sample.MonotonicNanos
	sample.OffsetUpperNanos = sample.BootAfterNanos - sample.MonotonicNanos
	return sample, nil
}

func sandwichUTC() (ClockSample, error) {
	a, err := readBootTime()
	if err != nil {
		return ClockSample{}, err
	}
	u := time.Now().UnixNano()
	b, err := readBootTime()
	if err != nil {
		return ClockSample{}, err
	}
	return ClockSample{BootBeforeNanos: a, TargetNanos: u, BootAfterNanos: b}, nil
}

// CaptureUTCClockSample exposes the raw realtime sandwich to source adapters.
// It deliberately makes no confidence claim; adjtimex/Roughtime evidence lives
// in the episode manifest.
func CaptureUTCClockSample() (ClockSample, error) { return sandwichUTC() }

func systemClockUncertainty() (int64, string) {
	var tx unix.Timex
	if _, err := unix.Adjtimex(&tx); err != nil || tx.Status&unix.STA_UNSYNC != 0 {
		return int64(^uint64(0) >> 1), "unbounded"
	}
	errMicros := tx.Maxerror
	if tx.Esterror > errMicros {
		errMicros = tx.Esterror
	}
	if errMicros <= 0 {
		return int64(^uint64(0) >> 1), "unbounded"
	}
	return errMicros * int64(time.Microsecond), "system_reported"
}

func telemetryValues() map[string]any {
	var info unix.Sysinfo_t
	if err := unix.Sysinfo(&info); err != nil {
		return map[string]any{"available": false}
	}
	unit := uint64(info.Unit)
	return map[string]any{"available": true, "uptime_seconds": info.Uptime, "load_1": float64(info.Loads[0]) / 65536.0, "load_5": float64(info.Loads[1]) / 65536.0, "load_15": float64(info.Loads[2]) / 65536.0, "memory_total_bytes": uint64(info.Totalram) * unit, "memory_free_bytes": uint64(info.Freeram) * unit}
}
