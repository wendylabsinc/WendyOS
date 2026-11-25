package livefusion

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"testing"
	"time"
)

func TestFailedRunRetainsBoundedActualDiagnostics(t *testing.T) {
	s, api := setup(t, "forged")
	out, err := Run(context.Background(), s.cfg, api)
	if err == nil || out.Diagnostics == nil {
		t.Fatal("failed verification omitted diagnostics")
	}
	d := out.Diagnostics
	if d.ElapsedNS <= 0 || d.Query.Count != s.queries || d.Query.Count != 1 || d.Verify.Count != 4 || d.Verify.Batches != 1 || d.Clock.Count != d.Clock.PhysicalCount+d.Clock.CombinedCount || d.Clock.PhysicalCount != 2 || d.Clock.CombinedCount != 1 || d.Clock.Count < 2 || d.Clock.First == nil || d.Clock.Last == nil || len(d.Attempts) != 1 {
		t.Fatalf("incorrect aggregate counts: query=%d verify=%d clock=%d attempts=%d", d.Query.Count, d.Verify.Count, d.Clock.Count, len(d.Attempts))
	}
	a := d.Attempts[0]
	if a.CandidateCount != 4 || a.MinCaptureUnixNS <= 0 || a.MaxCaptureUnixNS < a.MinCaptureUnixNS || a.CaptureSpanNS != 250000000 || a.Query.Count != 1 || a.Verify.Count != 4 || a.Verify.Batches != 1 || a.ElapsedNS <= 0 || len(a.Physical.Checks) != 1 || a.Physical.Checks[0].Phase != "initial_prepare" {
		t.Fatal("candidate and initial guard diagnostics disagree")
	}
	if d.Clock.First.EarliestUnixNS > d.Clock.First.LatestUnixNS || d.Clock.Last.EarliestUnixNS > d.Clock.Last.LatestUnixNS || d.Clock.First.RoundTripNS < 0 {
		t.Fatal("invalid recorded physical interval")
	}
}
func TestDiagnosticsDoNotAddRPCsOrAppearInSuccessfulReceipt(t *testing.T) {
	s, api := setup(t, "complete")
	out, err := Run(context.Background(), s.cfg, api)
	if err != nil || out.Status != "complete" || out.Diagnostics != nil {
		t.Fatal("successful receipt changed", err)
	}
	// Two whole-set verification rounds, each with two complete source
	// queries, plus the initial candidate query. Diagnostics add no RPCs.
	t.Logf("successful whole-set path: %d Clock RPCs, %d Query RPCs", s.clocks, s.queries)
	if s.queries != 5 || s.clocks != 25 || s.combinedClocks != 8 || s.physicalClocks != 17 {
		t.Fatalf("diagnostics added or removed RPCs: Clock=%d Query=%d", s.clocks, s.queries)
	}
}

func TestPhysicalDiagnosticOmitsSourceTime(t *testing.T) {
	d := ClockDiagnostics{}
	d.recordPhysical(appclock.PhysicalObservation{Physical: trustedtime.Interval{Earliest: time.Unix(1700000000, 0), Latest: time.Unix(1700000001, 0)}, RoundTrip: time.Millisecond}, nil, time.Millisecond)
	data, err := json.Marshal(d)
	if err != nil || bytes.Contains(data, []byte("sample_unix_ns")) || d.PhysicalCount != 1 || d.CombinedCount != 0 || d.Count != 1 {
		t.Fatal("physical-only diagnostic invented source time", err)
	}
}

func TestNoPublicationDiagnostics(t *testing.T) {
	for _, mode := range []string{"missing", "no-hazard", "skew"} {
		t.Run(mode, func(t *testing.T) {
			fixtureMode := mode
			if mode == "skew" {
				fixtureMode = "complete"
			}
			s, api := setup(t, fixtureMode)
			s.cfg.MaxAttempts = 2
			if mode == "skew" {
				s.cfg.MaxSkewMS = 1
			}
			out, err := Run(context.Background(), s.cfg, api)
			if err != nil || out.Status == "complete" || out.Diagnostics == nil || out.LocallyVerified || s.published != 0 || len(out.RequestJSON) != 0 {
				t.Fatalf("non-publication outcome lost diagnostics: status=%s err=%v", out.Status, err)
			}
			d := out.Diagnostics
			if d.Publication != nil || d.ElapsedNS <= 0 || d.Query.Count != s.queries || d.Clock.Count != s.clocks || len(d.Attempts) != out.Attempts {
				t.Fatal("non-publication diagnostics changed actual work")
			}
			if mode == "skew" {
				if d.Verify.Count != 0 || d.Query.Count != 2 || d.Clock.Count != 1 || len(out.Media) != 0 {
					t.Fatal("incoherent candidates reached verification or media")
				}
				for _, attempt := range d.Attempts {
					if attempt.CandidateCount != 4 || attempt.CaptureSpanNS != int64(250*time.Millisecond) {
						t.Fatal("original capture interval omitted")
					}
				}
			}
			raw, marshalErr := json.Marshal(out)
			if marshalErr != nil || !bytes.Contains(raw, []byte(`"diagnostics"`)) || len(raw) > MaxResultBytes {
				t.Fatal("bounded diagnostics missing from actual receipt", marshalErr)
			}
		})
	}
}

func TestFinalCombinedClockRefusesSourceExpiryWhilePhysicalCaptureIsFresh(t *testing.T) {
	s, api := setup(t, "complete")
	client := &combinedClockFaultClient{WorldAppServiceClient: api, before: func(n int) {
		// Result construction retains a final combined read after EvaluateBatch.
		if n == 6 {
			s.mu.Lock()
			s.sampleAdvance = 4 * time.Second
			s.mu.Unlock()
		}
	}}
	out, err := Run(context.Background(), s.cfg, client)
	if err != nil || out.Status != "insufficient_evidence" || s.published != 0 || len(out.RequestJSON) != 0 || out.LocallyVerified {
		t.Fatalf("expired source time reached mutation: stage=%s status=%s published=%d err=%v", out.FailureStage, out.Status, s.published, err)
	}
	if out.Evaluation == nil || out.Evaluation.Hazard == nil || len(out.Inputs) != 4 || len(out.Media) != 3 || s.queries != 5 {
		t.Fatal("test did not reach prepublication boundary with all verified inputs and media")
	}
	d := out.Diagnostics
	if d == nil || d.Attempts[0].RetryReason != "sample_capture_expired" {
		t.Fatal("prepublication source expiry was not retained")
	}
	if d == nil || len(d.Attempts) != 1 || d.Verify.Batches != 2 || d.Clock.PhysicalCount != 14 || d.Clock.CombinedCount != 6 {
		t.Fatal("verification rounds or final combined source check skipped")
	}
	last := d.Clock.Last
	if last == nil || last.SampleUnixNS < out.Evaluation.Hazard.ValidUntil.UnixNano() || last.LatestUnixNS-d.Attempts[0].MinCaptureUnixNS >= int64(5*time.Second) {
		t.Fatal("test did not isolate source expiry from physical capture age")
	}
}
