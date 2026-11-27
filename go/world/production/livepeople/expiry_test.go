package livepeople

import (
	"bytes"
	"context"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
)

// The fourth combined observation is the per-track check after the initial
// read, post-inference check and final batch check. Physical-only verifier
// reads still use the real signed-RPC fixture and are not intercepted.
type prePublishClockClient struct {
	appv1.WorldAppServiceClient
	observations int
	expire       func()
}

func (c *prePublishClockClient) Clock(ctx context.Context, q *appv1.ClockRequest, opts ...grpc.CallOption) (*appv1.ClockReply, error) {
	c.observations++
	if c.observations == 4 {
		c.expire()
	}
	return c.WorldAppServiceClient.Clock(ctx, q, opts...)
}

func TestUnattemptedTrackExpiryWaitsAndFreshCaptureRecovers(t *testing.T) {
	for _, dimension := range []string{"sample", "physical"} {
		t.Run(dimension, func(t *testing.T) {
			f, api := setupPeople(t)
			originals := map[string][]byte{}
			for id, raw := range f.proofs {
				originals[id] = bytes.Clone(raw)
			}
			client := &prePublishClockClient{WorldAppServiceClient: api, expire: func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				if dimension == "sample" {
					// Original sample .25s + person lifetime 1.25s.
					f.sampleNS = int64(1500 * time.Millisecond)
				} else {
					// The simulation clock remains paused while physical age elapses.
					f.clock.advance(5 * time.Second)
				}
			}}
			s, err := New(f.cfg, client, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			waiting, err := s.Step(context.Background())
			if err != nil || waiting.Status != "waiting" || len(waiting.Tracks) != 0 || len(waiting.Publications) != 0 || len(f.published) != 0 || len(waiting.Inputs) != 2 || f.inferred != 2 || client.observations != 4 {
				t.Fatalf("pre-publication expiry did not wait: status=%s tracks=%d publications=%d inputs=%d observations=%d err=%v", waiting.Status, len(waiting.Tracks), len(waiting.Publications), len(waiting.Inputs), client.observations, err)
			}
			for _, input := range waiting.Inputs {
				if !bytes.Equal(input.EnvelopeJSON, originals[input.Reference.Key.Source.Stream]) {
					t.Fatal("expired tick changed an original camera proof")
				}
			}
			f.mu.Lock()
			f.sampleNS = int64(2100 * time.Millisecond)
			f.mu.Unlock()
			f.setCapture(t, "people-front", 18, 2*time.Second)
			f.setCapture(t, "people-oblique", 18, 2*time.Second)
			fresh, err := s.Step(context.Background())
			if err != nil || fresh.Status != "complete" || len(fresh.Publications) != 1 || !fresh.Publications[0].LocallyVerified || len(f.published) != 1 || f.inferred != 4 {
				t.Fatalf("fresh captures did not recover: status=%s publications=%d err=%v", fresh.Status, len(fresh.Publications), err)
			}
			for _, input := range fresh.Inputs {
				if input.Reference.Version.Sequence != 2 || bytes.Equal(input.EnvelopeJSON, originals[input.Reference.Key.Source.Stream]) {
					t.Fatal("recovery reused an expired original")
				}
			}
			if !f.published[0].Timing.SampleTime.Equal(time.Unix(0, f.cfg.SimulationAnchorUnixNS).Add(2*time.Second)) || f.published[0].TTL > 1250*time.Millisecond {
				t.Fatal("recovery changed the original sample or extended person lifetime")
			}
		})
	}
}

func TestPausedCapturePhysicalExpiryWaitsAndFreshCaptureRecovers(t *testing.T) {
	for _, mode := range []string{"verified-stale", "missing-during-authorization"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setupPeople(t)
			s, err := New(f.cfg, api, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			first, err := s.Step(context.Background())
			if err != nil || len(first.Publications) != 1 {
				t.Fatalf("initial capture: status=%s publications=%d err=%v", first.Status, len(first.Publications), err)
			}
			beforeEvidence, beforeInference := f.evidence, f.inferred
			f.mu.Lock()
			if mode == "verified-stale" {
				// Simulation remains paused; the raw sample TTL is still live.
				f.clock.advance(6 * time.Second)
			} else {
				at := f.queries + 2 // Initial Query succeeds, then authority Query loses the originals.
				f.queryHook = func(n int) {
					if n == at {
						f.clock.advance(6 * time.Second)
						delete(f.proofs, "people-front")
						delete(f.proofs, "people-oblique")
					}
				}
			}
			f.mu.Unlock()
			waiting, err := s.Step(context.Background())
			if err != nil || waiting.Status != "waiting" || !waiting.SampleTime.Equal(first.SampleTime) || len(waiting.Inputs) != 0 || len(waiting.Publications) != 0 || len(f.published) != 1 || f.evidence != beforeEvidence || f.inferred != beforeInference {
				t.Fatalf("expired capture did not wait before media: status=%s inputs=%d publications=%d err=%v", waiting.Status, len(waiting.Inputs), len(waiting.Publications), err)
			}
			for _, track := range waiting.Tracks {
				if !track.ValidUntil.Equal(first.Tracks[0].ValidUntil) {
					t.Fatal("waiting renewed the original track")
				}
			}
			f.setCapture(t, "people-front", 18, 500*time.Millisecond)
			f.setCapture(t, "people-oblique", 18, 500*time.Millisecond)
			fresh, err := s.Step(context.Background())
			if err != nil || fresh.Status != "complete" || len(fresh.Publications) != 1 || !fresh.Publications[0].LocallyVerified || len(fresh.Inputs) != 2 || len(f.published) != 2 || f.inferred != beforeInference+2 {
				t.Fatalf("fresh capture did not recover: status=%s inputs=%d publications=%d err=%v", fresh.Status, len(fresh.Inputs), len(fresh.Publications), err)
			}
			for _, input := range fresh.Inputs {
				if input.Reference.Version.Sequence != 2 {
					t.Fatal("recovery reused an expired original")
				}
			}
		})
	}
}

func TestMissingUnexpiredCameraRemainsTerminal(t *testing.T) {
	f, api := setupPeople(t)
	f.queryHook = func(n int) {
		if n == 2 {
			delete(f.proofs, "people-front")
		}
	}
	s, err := New(f.cfg, api, f.infer)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Step(context.Background())
	if err == nil || r.Status != "failed" || f.evidence != 0 || f.inferred != 0 || len(f.published) != 0 {
		t.Fatal("missing unexpired authority became a waiting observation")
	}
	if _, err = s.Step(context.Background()); err == nil {
		t.Fatal("terminal authority failure was retried")
	}
}
