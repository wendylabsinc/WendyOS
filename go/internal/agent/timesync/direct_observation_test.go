package timesync

import (
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestDirectObservationDistinguishesApplyFromVerifiedUTC(t *testing.T) {
	current := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	const bootBefore = int64(100 * time.Second)
	for _, test := range []struct {
		name       string
		offset     time.Duration
		noop, fail bool
		within     bool
		outcome    string
	}{
		{"598s-behind", -598 * time.Second, false, false, true, "apply-completed"},
		{"already-in-interval", 500 * time.Millisecond, false, false, true, "no-step-requested"},
		{"ahead-cannot-rewind", 598 * time.Second, false, false, false, "unconfirmed"},
		{"nil-error-without-clock-change", -598 * time.Second, true, false, false, "unconfirmed"},
		{"clock-write-failure", -598 * time.Second, false, true, false, "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			core, logs := observer.New(zap.DebugLevel)
			m := NewManager(zap.New(core), "")
			utc := current.Add(test.offset)
			m.now = func() time.Time { return utc }
			boot := bootBefore
			m.boot = func() (int64, error) { value := boot; boot += int64(20 * time.Millisecond); return value, nil }
			writeFailure := errors.New("fixture clock write refused")
			m.applyResult = func(at time.Time) error {
				if test.fail {
					return writeFailure
				}
				if !test.noop && at.After(utc) {
					utc = at
				}
				return nil
			}
			consensus := Consensus{Confidence: ConfidenceDegraded, Quorum: 2, LowerOffsetNanos: current.Add(-time.Second).UnixNano() - bootBefore, UpperOffsetNanos: current.Add(time.Second).UnixNano() - bootBefore,
				Evidence: []RoughtimeEvidence{{Server: "one", Included: true, LocalAddress: "10.88.0.1:1001", RemoteAddress: "192.0.2.1:2002", SendBootNanos: 1, ReceiveBootNanos: 2}, {Server: "two", Included: true, LocalAddress: "10.88.0.1:1002", RemoteAddress: "192.0.2.2:2002"}, {Server: "excluded", Included: false}}}
			observed := 0
			m.SetDirectSyncObserver(func(o DirectSyncObservation) {
				observed++
				if !m.clockMu.TryLock() {
					t.Error("observer called under clock lock")
				} else {
					m.clockMu.Unlock()
				}
				if !m.mu.TryLock() {
					t.Error("observer called under Manager lock")
				} else {
					m.mu.Unlock()
				}
				m.SetDirectSyncObserver(nil)
				if o.UTCWithinVerifiedInterval != test.within {
					t.Error("observer received false UTC confirmation")
				}
			})
			o := m.applyDirect(consensus)
			if observed != 1 || !o.ApplyAttempted || o.Source != "direct" || o.UTCWithinVerifiedInterval != test.within {
				t.Fatalf("unexpected observation: %+v", o)
			}
			if test.fail && !errors.Is(o.ApplyError, writeFailure) {
				t.Fatal("clock apply error lost", o.ApplyError)
			}
			if o.UTCBeforeApply != current.Add(test.offset) || o.UTCAfterApply != utc || o.BootBeforeApplyNanos != bootBefore || o.BootAfterApplyStartNanos != bootBefore+int64(20*time.Millisecond) || o.BootAfterApplyNanos != bootBefore+int64(40*time.Millisecond) {
				t.Fatal("before/after clocks not recorded", o)
			}
			if o.VerifiedEarliest != current.Add(-time.Second+40*time.Millisecond) || o.VerifiedLatest != current.Add(time.Second+20*time.Millisecond) {
				t.Fatal("proof interval was not extrapolated by boottime", o)
			}
			if len(o.Paths) != 2 || o.Paths[0].LocalAddress != "10.88.0.1:1001" || o.Paths[1].Server != "two" {
				t.Fatal("included socket path evidence lost", o.Paths)
			}
			complete := logs.FilterMessage("timesync: direct Roughtime apply complete").All()
			if len(complete) != 1 || complete[0].ContextMap()["outcome"] != test.outcome || complete[0].ContextMap()["utc_within_verified_interval"] != test.within {
				t.Fatal("apply log claimed wrong result", complete)
			}
			ready := logs.FilterMessage("timesync: direct Roughtime quorum ready").All()
			if len(ready) != 1 || ready[0].ContextMap()["source"] != "direct" {
				t.Fatal("direct pre-apply evidence missing", ready)
			}
		})
	}
}

func TestDirectObservationBootFailureDoesNotApply(t *testing.T) {
	m := NewManager(nil, "")
	m.now = func() time.Time { return time.Unix(1800000000, 0) }
	failed := errors.New("fixture boot clock unavailable")
	m.boot = func() (int64, error) { return 0, failed }
	m.applyResult = func(time.Time) error { t.Fatal("applied without boot anchor"); return nil }
	o := m.applyDirect(Consensus{Confidence: ConfidenceDegraded, Quorum: 2, LowerOffsetNanos: 1, UpperOffsetNanos: 2})
	if o.ApplyAttempted || o.UTCWithinVerifiedInterval || !errors.Is(o.ObservationError, failed) {
		t.Fatal("boot failure claimed trustworthy apply", o)
	}
}

func TestDirectObservationDoesNotTurnSampleLatencyIntoClockGrace(t *testing.T) {
	current := time.Unix(1800000000, 0).UTC()
	const before = int64(100 * time.Second)
	m := NewManager(nil, "")
	m.now = func() time.Time { return current.Add(30 * time.Millisecond) }
	boot := before
	m.boot = func() (int64, error) { value := boot; boot += int64(20 * time.Millisecond); return value, nil }
	m.applyResult = func(time.Time) error { return nil }
	o := m.applyDirect(Consensus{Confidence: ConfidenceDegraded, Quorum: 2, LowerOffsetNanos: current.Add(-time.Millisecond).UnixNano() - before, UpperOffsetNanos: current.Add(time.Millisecond).UnixNano() - before})
	if o.ObservationError == nil || o.UTCWithinVerifiedInterval {
		t.Fatal("UTC read latency widened the signed proof interval", o)
	}
}
