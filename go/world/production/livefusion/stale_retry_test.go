package livefusion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"google.golang.org/grpc"
)

// Every capture is signed by the fixture's real service publisher and travels
// through the ordinary Unix gRPC client, verifier and media loader.
func TestStaleTupleThenFreshSignedTuple(t *testing.T) {
	for _, domain := range []string{"physical", "sample"} {
		t.Run(domain, func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 2
			first, fresh := map[string][]byte{}, map[string][]byte{}
			for _, source := range s.cfg.Sources {
				age, ttl := time.Duration(0), 3*time.Second
				if source.SensorID == "gas-1" {
					if domain == "physical" {
						age = 4900 * time.Millisecond
					} else {
						ttl = time.Second
					}
				}
				first[source.SensorID] = captureWithBounds(t, s, source.SensorID, 300*time.Millisecond, age, ttl)
				fresh[source.SensorID] = captureAt(t, s, source.SensorID, 1500*time.Millisecond)
			}
			s.clock.advance(200 * time.Millisecond)
			s.sampleAdvance = 1300 * time.Millisecond
			// Physical expiry is discovered after the first complete batch (3
			// queries); source expiry after both batches (5 queries).
			next := 4
			if domain == "sample" {
				next = 6
			}
			client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
				s.mu.Lock()
				defer s.mu.Unlock()
				if n == 1 {
					s.proofs = first
				}
				if n == next {
					s.proofs = fresh
				}
				return nil
			}}
			out, err := Run(context.Background(), s.cfg, client)
			if err != nil || out.Status != "complete" || out.Attempts != 2 || !out.LocallyVerified || s.published != 1 {
				t.Fatalf("fresh tuple not evaluated after %s expiry: status=%s attempts=%d publications=%d err=%v", domain, out.Status, out.Attempts, s.published, err)
			}
			for _, proof := range out.Inputs {
				if !bytes.Equal(proof.EnvelopeJSON, fresh[proof.Reference.Key.Source.Stream]) {
					t.Fatal("stale original reused or bytes changed")
				}
			}
		})
	}
}

func TestMixedFutureAndExpiredSignedCapturesAreTerminal(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	for _, source := range s.cfg.Sources {
		age := time.Duration(0)
		if source.SensorID == "audio-1" {
			age = 6 * time.Second
		}
		if source.SensorID == "camera-a" {
			age = -time.Second
		}
		s.proofs[source.SensorID] = captureWithBounds(t, s, source.SensorID, 300*time.Millisecond, age, 3*time.Second)
	}
	out, err := Run(context.Background(), s.cfg, api)
	if err == nil || out.Attempts != 1 || out.Status != "failed" || s.published != 0 || s.queries != 3 {
		t.Fatal("future capture was hidden by stale capture", out.Status, out.Attempts, err)
	}
	a := out.Diagnostics.Attempts[0]
	last := a.Physical.Checks[len(a.Physical.Checks)-1]
	if !last.Future || !last.Stale || a.RetryReason != "" {
		t.Fatal("test did not mix future and stale captures")
	}
}

type combinedClockFaultClient struct {
	appv1.WorldAppServiceClient
	calls  int
	before func(int)
	after  func(int, *appv1.ClockReply)
}

func (c *combinedClockFaultClient) Clock(ctx context.Context, p *appv1.ClockRequest, opts ...grpc.CallOption) (*appv1.ClockReply, error) {
	c.calls++
	if c.before != nil {
		c.before(c.calls)
	}
	reply, err := c.WorldAppServiceClient.Clock(ctx, p, opts...)
	if err == nil && c.after != nil {
		c.after(c.calls, reply)
	}
	return reply, err
}

func TestSampleClockRegressionDuringFinalValidationDoesNotRetry(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	client := &combinedClockFaultClient{WorldAppServiceClient: api, after: func(n int, p *appv1.ClockReply) {
		// Fifth combined read follows both full source-authority rounds.
		if n == 5 {
			p.SampleUnixNs--
		}
	}}
	out, err := Run(context.Background(), s.cfg, client)
	if err == nil || out.Status != "failed" || out.Attempts != 1 || s.published != 0 || s.queries != 5 || client.calls != 5 {
		t.Fatal("source clock regression retried", out.Status, out.Attempts, s.queries, err)
	}
	if out.Diagnostics.Attempts[0].RetryReason != "" {
		t.Fatal("clock failure classified as capture expiry")
	}
}

func TestSigningAuthorityExpiryAtResultClockDoesNotRetry(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	client := &combinedClockFaultClient{WorldAppServiceClient: api, before: func(n int) {
		// The sixth combined read is after both full batch verifications
		// and before any Publish. All original record authority has expired.
		if n == 6 {
			s.clock.advance(11 * time.Minute)
		}
	}}
	out, err := Run(context.Background(), s.cfg, client)
	if err == nil || out.Status != "failed" || out.Attempts != 1 || s.published != 0 || s.queries != 5 || out.FailureStage != "result_clock" {
		t.Fatal("authority expiry retried", out.Status, out.Attempts, out.FailureStage, err)
	}
	if len(out.RequestJSON) != 0 || out.Diagnostics.Attempts[0].RetryReason != "" {
		t.Fatal("expired signing authority reached retry or publication")
	}
}

func TestCaptureExpiryAtEachBatchBoundaryRemainsBounded(t *testing.T) {
	for query := 2; query <= 5; query++ {
		t.Run(string(rune('0'+query)), func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 2
			s.faultQuery, s.faultKind = query, "expiry"
			out, err := Run(context.Background(), s.cfg, api)
			if err != nil || out.Status != "insufficient_evidence" || out.Attempts != 2 || s.published != 0 || len(out.RequestJSON) != 0 {
				t.Fatal("capture expiry escaped bounded no-publication outcome", out.Status, out.Attempts, err)
			}
			first := out.Diagnostics.Attempts[0]
			if first.RetryReason != "physical_capture_expired" || out.Diagnostics.Attempts[1].EvaluationOutcome != "no_eligible_tuple" {
				t.Fatal("capture tuple retirement omitted")
			}
			if first.Verify.Batches != query/2 {
				t.Fatal("test missed the intended verification round", first.Verify.Batches)
			}
			if query >= 4 && len(s.owners) != 3 {
				t.Fatal("test missed actual media retrieval")
			}
			want := query
			if query%2 == 0 {
				want = query + 1
			}
			if s.queries != want+1 {
				t.Fatal("authority batch skipped or stale tuple reverified", s.queries, want+1)
			}
		})
	}
}

func TestSignedMalformedMediaDoesNotRetry(t *testing.T) {
	for _, sensor := range []string{"camera-a", "audio-1"} {
		t.Run(sensor, func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 3
			_, record, err := decodeEnvelope(s.original[sensor])
			if err != nil {
				t.Fatal(err)
			}
			capture, err := sensorrecord.Decode(record.Publication.Payload)
			if err != nil {
				t.Fatal(err)
			}
			sample, err := capture.Sample()
			if err != nil {
				t.Fatal(err)
			}
			bad := []byte("digest-correct but not camera or audio media")
			sample.Evidence.SHA256, sample.Evidence.Size = digest(bad), int64(len(bad))
			capture.SampleJSON, err = json.Marshal(sample)
			if err != nil {
				t.Fatal(err)
			}
			request, err := sensorrecord.Publication(capture, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			proof, err := s.publishers[sensor].Publish(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			s.proofs[sensor], err = json.Marshal(proof)
			if err != nil {
				t.Fatal(err)
			}
			s.media[sensor+"/"+sample.Evidence.SHA256] = bad
			out, err := Run(context.Background(), s.cfg, api)
			if err == nil || out.Status != "failed" || out.Attempts != 1 || s.published != 0 || out.ErrorCode != "evidence_refused" || s.queries != 3 {
				t.Fatal("validly signed malformed media became retryable insufficiency", out.Status, out.Attempts, out.ErrorCode, s.queries, err)
			}
		})
	}
}

func TestRetiredLatestTupleCannotBypassWindow(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 16
	s.cfg.DurationMS = 120000
	for _, source := range s.cfg.Sources {
		age := time.Duration(0)
		if source.SensorID == "gas-1" {
			age = 6 * time.Second
		}
		s.proofs[source.SensorID] = captureWithBounds(t, s, source.SensorID, 300*time.Millisecond, age, 3*time.Second)
	}
	out, err := Run(context.Background(), s.cfg, api)
	if err != nil || out.Status != "insufficient_evidence" || out.Attempts != 16 || s.published != 0 || s.queries != 18 {
		t.Fatalf("retired latest tuple was retried: status=%s queries=%d err=%v", out.Status, s.queries, err)
	}
	d := out.Diagnostics
	if d == nil || d.Verify.Batches != 1 || len(d.Attempts) != 16 || d.Attempts[0].RetryReason != "physical_capture_expired" || d.Attempts[0].EvaluationOutcome != "capture_expired" {
		t.Fatal("bounded stale evaluation result missing")
	}
	for _, a := range d.Attempts[1:] {
		if a.EvaluationOutcome != "no_eligible_tuple" || a.Verify.Batches != 0 || a.Query.Count != 1 {
			t.Fatal("unchanged retired tuple reached verification again")
		}
	}
}

func TestRetiredTupleStillChecksCurrentAuthority(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	for _, source := range s.cfg.Sources {
		age := time.Duration(0)
		if source.SensorID == "gas-1" {
			age = 6 * time.Second
		}
		s.proofs[source.SensorID] = captureWithBounds(t, s, source.SensorID, 300*time.Millisecond, age, 3*time.Second)
	}
	s.faultQuery, s.faultKind = 4, "replacement"
	out, err := Run(context.Background(), s.cfg, api)
	if err == nil || out.Status != "failed" || out.Attempts != 2 || s.published != 0 || s.queries != 4 {
		t.Fatalf("retirement hid current source replacement: status=%s queries=%d err=%v", out.Status, s.queries, err)
	}
}

func TestStaleRetryCancellation(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 16
	for _, source := range s.cfg.Sources {
		age := time.Duration(0)
		if source.SensorID == "gas-1" {
			age = 6 * time.Second
		}
		s.proofs[source.SensorID] = captureWithBounds(t, s, source.SensorID, 300*time.Millisecond, age, 3*time.Second)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
		if n == 4 {
			cancel()
		}
		return nil
	}}
	out, err := Run(ctx, s.cfg, client)
	if !errors.Is(err, context.Canceled) && ctx.Err() == nil {
		t.Fatal("cancellation was ignored", err)
	}
	if err == nil || out.Attempts > 2 || s.published != 0 || len(out.RequestJSON) != 0 {
		t.Fatal("canceled retry continued", out.Status, out.Attempts, err)
	}
}
