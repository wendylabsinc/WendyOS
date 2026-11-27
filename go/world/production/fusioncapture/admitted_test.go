package fusioncapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func admittedProjection(_ context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.Record, error) {
	result := make([]serviceorigin.Record, len(signed))
	for i, input := range signed {
		if err := jws.DecodeStrict(input.Record, &result[i]); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func TestAdmittedEvaluationProjectsOncePreservingOriginalResult(t *testing.T) {
	f := setup(t)
	want, err := EvaluateBatch(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.batchVerify, f.load)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	got, err := EvaluateAdmitted(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs,
		func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.Record, error) {
			calls++
			return admittedProjection(ctx, signed)
		}, func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
			for _, proof := range f.proofs {
				clear(proof)
			}
			return f.load(ctx, ref)
		})
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if err != nil || calls != 1 || !bytes.Equal(gotJSON, wantJSON) {
		t.Fatal("admitted projection changed inference/lineage", calls, err)
	}
}

func TestAdmittedEvaluationRefusesChangedProjectionAndOriginalExpiry(t *testing.T) {
	for _, fault := range []string{"partial", "reordered", "changed", "media", "expiry"} {
		t.Run(fault, func(t *testing.T) {
			f := setup(t)
			loads := 0
			_, err := EvaluateAdmitted(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs,
				func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.Record, error) {
					out, err := admittedProjection(ctx, signed)
					switch fault {
					case "partial":
						out = out[:len(out)-1]
					case "reordered":
						out[0], out[1] = out[1], out[0]
					case "changed":
						out[0].Sequence++
					}
					return out, err
				}, func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
					loads++
					data, err := f.load(ctx, ref)
					if fault == "media" {
						data = bytes.Clone(data)
						data[len(data)-1] ^= 1
					}
					if fault == "expiry" && loads == 1 {
						advancePhysical(f.clock, 6*time.Second)
					}
					return data, err
				})
			if err == nil {
				t.Fatal("invalid admitted input accepted")
			}
			if fault == "expiry" {
				var stale *StaleCaptureError
				if !errors.As(err, &stale) || stale.Reason() != "physical_capture_expired" {
					t.Fatal("wrong original deadline result", err)
				}
			}
		})
	}
}

func TestAdmittedEvaluationCannotOutliveOriginalSigningWindow(t *testing.T) {
	f := setup(t)
	// These unsigned edits model already-admitted records with a short original
	// signing window. This test checks projection/time semantics, not signatures.
	for i, raw := range f.proofs {
		var signed serviceorigin.SignedRecord
		var record serviceorigin.Record
		if json.Unmarshal(raw, &signed) != nil || json.Unmarshal(signed.Record, &record) != nil {
			t.Fatal("fixture decode")
		}
		record.ExpiresAtUnixMS = f.clock.interval.Latest.Add(time.Second).UnixMilli()
		signed.Record, _ = json.Marshal(record)
		f.proofs[i], _ = json.Marshal(signed)
	}
	advanced := false
	_, err := EvaluateAdmitted(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, admittedProjection,
		func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
			if !advanced {
				advancePhysical(f.clock, 2*time.Second)
				advanced = true
			}
			return f.load(ctx, ref)
		})
	if !errors.Is(err, serviceorigin.ErrExpired) {
		t.Fatal("signing window extension", err)
	}
}

func TestAdmittedNegativeResultChecksSigningWindowAtFinalGroupClock(t *testing.T) {
	f := setup(t)
	f.cfg.GasThresholdPPM = 1000 // The evidence cannot produce a positive hazard.
	for i, raw := range f.proofs {
		var signed serviceorigin.SignedRecord
		var record serviceorigin.Record
		if json.Unmarshal(raw, &signed) != nil || json.Unmarshal(signed.Record, &record) != nil {
			t.Fatal("fixture decode")
		}
		record.ExpiresAtUnixMS = f.clock.interval.Latest.Add(time.Second).UnixMilli()
		signed.Record, _ = json.Marshal(record)
		f.proofs[i], _ = json.Marshal(signed)
	}
	calls := 0
	physical := clockFunc(func() (trustedtime.Interval, error) {
		calls++
		if calls == 5 {
			advancePhysical(f.clock, 2*time.Second)
		}
		return f.clock.Now()
	})
	_, err := EvaluateAdmitted(context.Background(), f.cfg, Options{Physical: physical}, f.proofs, admittedProjection, f.load)
	if !errors.Is(err, serviceorigin.ErrExpired) || calls != 5 {
		t.Fatal("negative result outlived final signing bound", calls, err)
	}
}
