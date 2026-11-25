package fusioncapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func TestBatchEvaluationPreservesOriginalProofsAcrossMediaAndReverification(t *testing.T) {
	f := setup(t)
	want, err := Evaluate(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, f.verify, f.load)
	if err != nil {
		t.Fatal(err)
	}
	original := make([][]byte, len(f.proofs))
	for i := range f.proofs {
		original[i] = bytes.Clone(f.proofs[i])
	}
	calls := 0
	verify := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
		calls++
		if len(signed) != 4 {
			t.Fatal("partial verification batch")
		}
		for i, raw := range original {
			var expected serviceorigin.SignedRecord
			if json.Unmarshal(raw, &expected) != nil || !reflect.DeepEqual(expected, signed[i]) {
				t.Fatal("original proof replaced between rounds")
			}
		}
		return f.batchVerify(ctx, signed)
	}
	loads := 0
	got, err := EvaluateBatch(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, verify, func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		loads++
		// The caller may reuse its query buffers during another I/O operation.
		for _, raw := range f.proofs {
			clear(raw)
		}
		return f.load(ctx, ref)
	})
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if err != nil || got.State != fusion.HazardDetected || !bytes.Equal(gotJSON, wantJSON) || calls != 2 || loads != 3 {
		t.Fatalf("batch changed immutable result or skipped revalidation: calls=%d loads=%d err=%v", calls, loads, err)
	}
}

func TestBatchPreparationRejectsPartialReorderedAndRewrittenProjection(t *testing.T) {
	for _, mode := range []string{"partial", "extra", "reordered", "bytes", "projection", "failed"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			verify := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
				checked, err := f.batchVerify(ctx, signed)
				if err != nil {
					return nil, err
				}
				switch mode {
				case "partial":
					checked = checked[:3]
				case "extra":
					checked = append(checked, checked[0])
				case "reordered":
					checked[0], checked[1] = checked[1], checked[0]
				case "bytes":
					checked[0].Bytes = bytes.Clone(checked[1].Bytes)
				case "projection":
					checked[0].Record.Sequence++
				case "failed":
					return checked, errors.New("authority disappeared")
				}
				return checked, nil
			}
			records, decode, err := PrepareBatch(context.Background(), Options{Physical: f.clock}, f.proofs, verify)
			if err == nil || records != nil || decode != nil {
				t.Fatal("partial or mismatched batch escaped")
			}
		})
	}
}

func TestBatchPreparationBoundsAndDistinctSources(t *testing.T) {
	f := setup(t)
	for _, proofs := range [][][]byte{nil, append(f.proofs, f.proofs[0]), make([][]byte, MaxBatchRecords+1)} {
		if _, _, err := PrepareBatch(context.Background(), Options{Physical: f.clock}, proofs, f.batchVerify); err == nil {
			t.Fatal("unbounded or duplicate batch accepted")
		}
	}
	if _, _, err := PrepareBatch(context.Background(), Options{Physical: f.clock}, f.proofs, nil); err == nil {
		t.Fatal("missing verifier accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := PrepareBatch(ctx, Options{Physical: f.clock}, f.proofs, f.batchVerify); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled batch continued", err)
	}
	// Exercise the wrapper's distinct-source boundary independently of the
	// production verifier's stricter duplicate key/version rejection.
	individual := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
		var verified []serviceorigin.VerifiedRecord
		for _, s := range signed {
			v, err := f.verify(ctx, s)
			if err != nil {
				return nil, err
			}
			verified = append(verified, v)
		}
		return verified, nil
	}
	if _, _, err := PrepareBatch(context.Background(), Options{Physical: f.clock}, append(f.proofs, f.proofs[0]), individual); err == nil {
		t.Fatal("wrapper accepted a repeated source")
	}
}

func TestBatchPreparationOwnsOriginalsBeforeBlockingClock(t *testing.T) {
	f := setup(t)
	provider := clockFunc(func() (trustedtime.Interval, error) {
		for _, raw := range f.proofs {
			clear(raw)
		}
		return f.clock.Now()
	})
	components, decoder, err := PrepareBatch(context.Background(), Options{Physical: provider}, f.proofs, f.batchVerify)
	if err != nil || len(components) != 4 || decoder == nil {
		t.Fatal("clock callback changed original proof ownership", err)
	}
}

func TestBatchCaptureAgeAndAuthorityRecheckedAfterBlockingWork(t *testing.T) {
	for _, mode := range []string{"verification-age", "media-age", "media-revocation"} {
		t.Run(mode, func(t *testing.T) {
			f := setup(t)
			advancePhysical(f.clock, 4900*time.Millisecond)
			paused := f.cfg.Now()
			verify := func(ctx context.Context, signed []serviceorigin.SignedRecord) ([]serviceorigin.VerifiedRecord, error) {
				checked, err := f.batchVerify(ctx, signed)
				if mode == "verification-age" {
					advancePhysical(f.clock, 200*time.Millisecond)
				}
				return checked, err
			}
			loads := 0
			_, err := EvaluateBatch(context.Background(), f.cfg, Options{Physical: f.clock}, f.proofs, verify, func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
				loads++
				if loads == 1 {
					if mode == "media-age" {
						advancePhysical(f.clock, 200*time.Millisecond)
					}
					if mode == "media-revocation" {
						f.allowed = false
					}
				}
				return f.load(ctx, ref)
			})
			if err == nil || !f.cfg.Now().Equal(paused) {
				t.Fatal("batch retained stale authority or advanced source time")
			}
			if mode == "verification-age" && loads != 0 {
				t.Fatal("stale batch loaded media")
			}
		})
	}
}
