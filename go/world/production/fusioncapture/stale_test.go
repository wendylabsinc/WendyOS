package fusioncapture

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestTypedStalenessFollowsCompleteAuthority(t *testing.T) {
	for _, fault := range []string{"stale", "authority", "forged", "clock-regression", "future"} {
		t.Run(fault, func(t *testing.T) {
			f := setup(t)
			calls := 0
			if fault != "future" {
				advancePhysical(f.clock, 6*time.Second)
			}
			verify := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
				calls++
				if fault == "forged" {
					signed[len(signed)-1].Signature[0] ^= 1
				}
				checked, err := f.batchVerify(ctx, signed)
				if err != nil {
					return nil, err
				}
				if fault == "authority" {
					return nil, errors.New("source revoked")
				}
				if fault == "clock-regression" {
					advancePhysical(f.clock, -time.Millisecond)
				}
				if fault == "future" {
					advancePhysical(f.clock, -time.Second)
				}
				return checked, nil
			}
			_, _, err := PrepareBatch(context.Background(), Options{Physical: f.clock}, f.proofs, verify)
			var stale *StaleCaptureError
			got := errors.As(err, &stale)
			if err == nil || calls != 1 || got != (fault == "stale") {
				t.Fatalf("fault=%s typed-stale=%v calls=%d err=%v", fault, got, calls, err)
			}
			if got && stale.Reason() != "physical_capture_expired" {
				t.Fatal("unbounded stale reason")
			}
		})
	}
}

func TestSourceClockFaultDominatesSimultaneousPhysicalExpiry(t *testing.T) {
	for _, call := range []int{3, 4} {
		t.Run(string(rune('0'+call)), func(t *testing.T) {
			f := setup(t)
			sourceNow := f.cfg.Now
			calls, batches := 0, 0
			f.cfg.Now = func() time.Time {
				calls++
				if calls == call {
					advancePhysical(f.clock, 6*time.Second)
					return sourceNow().Add(-time.Nanosecond)
				}
				return sourceNow()
			}
			verify := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
				batches++
				return f.batchVerify(ctx, signed)
			}
			_, err := EvaluateBatch(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, verify, f.load)
			var stale *StaleCaptureError
			if err == nil || errors.As(err, &stale) || calls != call || batches != call-2 {
				t.Fatal("source regression hidden by simultaneous physical expiry", calls, batches, err)
			}
		})
	}
}
