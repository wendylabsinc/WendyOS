package livefusion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSessionMissingExpiredSelectionWaitsAndFreshCaptureRecovers(t *testing.T) {
	for _, boundary := range []int{2, 5} { // Initial and final post-media verification.
		for _, expiry := range []string{"physical", "sample"} {
			t.Run(fmt.Sprintf("query-%d/%s", boundary, expiry), func(t *testing.T) {
				fixture, api := setup(t, "lidar")
				physicalAdvance := time.Duration(0)
				client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
					if n == boundary {
						fixture.mu.Lock()
						defer fixture.mu.Unlock()
						fixture.omitted = map[string]bool{"gas-1": true}
						if expiry == "physical" {
							physicalAdvance = 6 * time.Second
							fixture.clock.advance(physicalAdvance)
						} else {
							fixture.sampleAdvance = 4 * time.Second
						}
					}
					return nil
				}}
				s, err := New(fixture.cfg, client)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				out, err := s.Step(context.Background())
				if err != nil || out.Status != "insufficient_evidence" || out.LocallyVerified || fixture.published != 0 || client.calls != boundary {
					t.Fatalf("expired selection stopped session: %s %v queries=%d published=%d", out.Status, err, client.calls, fixture.published)
				}
				if out.Diagnostics == nil || len(out.Diagnostics.Attempts) != 1 || out.Diagnostics.Attempts[0].RetryReason != expiry+"_capture_expired" {
					t.Fatal("missing bounded expiry reason", out.Diagnostics)
				}
				// Even if a test server returns the same old originals again,
				// retirement prevents another verification or publication.
				fixture.mu.Lock()
				fixture.omitted = nil
				fixture.mu.Unlock()
				repeat, err := s.Step(context.Background())
				if err != nil || repeat.Status != "insufficient_evidence" || fixture.published != 0 || client.calls != boundary+1 || repeat.Diagnostics.Verify.Batches != 0 {
					t.Fatal("retired tuple was retried", repeat.Status, err)
				}
				fresh := map[string][]byte{}
				for _, source := range fixture.cfg.Sources {
					fresh[source.SensorID] = captureWithBounds(t, fixture, source.SensorID, 4400*time.Millisecond, -physicalAdvance, 3*time.Second)
				}
				fixture.mu.Lock()
				fixture.proofs, fixture.sampleAdvance = fresh, 4*time.Second
				fixture.mu.Unlock()
				restored, err := s.Step(context.Background())
				if err != nil || !restored.LocallyVerified || restored.Tick != 3 || fixture.published != 1 {
					t.Fatalf("fresh capture did not recover: %s %v", restored.Status, err)
				}
				for _, p := range restored.Inputs {
					if !bytes.Equal(p.EnvelopeJSON, fresh[p.Reference.Key.Source.Stream]) {
						t.Fatal("recovery changed an original or reused an expired capture")
					}
				}
			})
		}
	}
}

func TestSessionMissingSelectionDoesNotHideRefusalOrClockFault(t *testing.T) {
	for _, fault := range []string{"unexpired", "denied", "replacement", "clock-regression"} {
		t.Run(fault, func(t *testing.T) {
			fixture, api := setup(t, "complete")
			client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
				if n != 2 {
					return nil
				}
				fixture.mu.Lock()
				defer fixture.mu.Unlock()
				fixture.omitted = map[string]bool{"gas-1": true}
				if fault != "unexpired" {
					fixture.clock.advance(6 * time.Second)
				}
				switch fault {
				case "denied":
					return status.Error(codes.PermissionDenied, "source unavailable")
				case "replacement":
					fixture.proofs["camera-a"] = fixture.replacement
				case "clock-regression":
					fixture.sampleAdvance = -time.Second
				}
				return nil
			}}
			s, err := New(fixture.cfg, client)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			out, err := s.Step(context.Background())
			if err == nil || out.Status != "failed" || fixture.published != 0 {
				t.Fatalf("%s was hidden by expiry: %s %v", fault, out.Status, err)
			}
			if _, err = s.Step(context.Background()); !errors.Is(err, ErrClosed) || client.calls != 2 {
				t.Fatal("terminal refusal retried", err, client.calls)
			}
		})
	}
}

func TestOneShotMissingExpiredSelectionRemainsTerminal(t *testing.T) {
	fixture, api := setup(t, "complete")
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
		if n == 2 {
			fixture.mu.Lock()
			fixture.omitted = map[string]bool{"gas-1": true}
			fixture.clock.advance(6 * time.Second)
			fixture.mu.Unlock()
		}
		return nil
	}}
	out, err := Run(context.Background(), fixture.cfg, client)
	if err == nil || out.Status != "failed" || fixture.published != 0 || client.calls != 2 {
		t.Fatal("one-shot refusal changed", out.Status, err)
	}
}
