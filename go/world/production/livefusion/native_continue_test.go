package livefusion

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The real fixture broker signs and acknowledges each call. This client loses
// only the first response, retaining the committed proof for the assertion.
type nativeLostAckClient struct {
	appv1.WorldAppServiceClient
	code      codes.Code
	requests  []*appv1.PublishRequest
	committed [][]byte
	cancel    context.CancelFunc
}

func (c *nativeLostAckClient) Publish(ctx context.Context, p *appv1.PublishRequest, options ...grpc.CallOption) (*appv1.PublishReply, error) {
	c.requests = append(c.requests, proto.Clone(p).(*appv1.PublishRequest))
	ack, err := c.WorldAppServiceClient.Publish(ctx, p, options...)
	if err != nil {
		return ack, err
	}
	c.committed = append(c.committed, bytes.Clone(ack.SignedRecordJson))
	if len(c.requests) == 1 {
		if c.cancel != nil {
			c.cancel()
		}
		return nil, status.Error(c.code, "fixture response lost")
	}
	return ack, nil
}

func freshNativeCaptures(t *testing.T, f *fixtureServer) {
	t.Helper()
	fresh := map[string][]byte{}
	for _, source := range f.cfg.Sources {
		fresh[source.SensorID] = captureAt(t, f, source.SensorID, 900*time.Millisecond)
	}
	f.mu.Lock()
	f.proofs = fresh
	f.sampleAdvance = 500 * time.Millisecond
	f.mu.Unlock()
}

func TestNativeSessionLostAckRetiresInputsAndAcceptsFreshObservation(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			f, api := setup(t, "complete")
			client := &nativeLostAckClient{WorldAppServiceClient: api, code: code}
			s, err := NewNative(f.cfg, client)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			unknown, err := s.Step(context.Background())
			if !errors.Is(err, ErrUncertain) || unknown.Status != "unknown_outcome" || unknown.LocallyVerified || len(client.committed) != 1 || len(unknown.SignedRecordJSON) != 0 {
				t.Fatalf("lost acknowledgment: %s %v", unknown.Status, err)
			}
			for i := 0; i < 2; i++ {
				duplicate, err := s.Step(context.Background())
				if err != nil || duplicate.Status != "insufficient_evidence" || duplicate.LocallyVerified || len(client.requests) != 1 {
					t.Fatalf("same tuple retried: %s %v", duplicate.Status, err)
				}
			}
			freshNativeCaptures(t, f)
			fresh, err := s.Step(context.Background())
			if err != nil || fresh.Status != "complete" || !fresh.LocallyVerified || len(client.requests) != 2 || f.published != 2 {
				t.Fatalf("fresh observation: %s %v", fresh.Status, err)
			}
			if unknown.Evaluation.InputDigest == fresh.Evaluation.InputDigest || bytes.Equal(unknown.RequestJSON, fresh.RequestJSON) || proto.Equal(client.requests[0], client.requests[1]) {
				t.Fatal("fresh publication reused old input digest or request")
			}
			_, first, err := decodeEnvelope(client.committed[0])
			if err != nil {
				t.Fatal(err)
			}
			_, second, err := decodeEnvelope(fresh.SignedRecordJSON)
			if err != nil {
				t.Fatal(err)
			}
			if first.Epoch != second.Epoch || first.Sequence != 1 || second.Sequence != 2 {
				t.Fatal("broker source sequence was reset or reused")
			}
			if len(s.attempted) != 2 || len(s.window.retired) != 2 || unknown.Status != "unknown_outcome" || unknown.LocallyVerified {
				t.Fatal("attempt history changed or unknown was relabeled")
			}
		})
	}
}

func TestNativeContinuousReportsUnknownAndFreshAcknowledgments(t *testing.T) {
	f, api := setup(t, "complete")
	f.cfg.MaxTicks = 3
	client := &nativeLostAckClient{WorldAppServiceClient: api, code: codes.Unavailable}
	var receipts []TickResult
	out, err := RunNativeContinuous(context.Background(), f.cfg, client, func(r TickResult) error {
		receipts = append(receipts, r)
		if r.Tick == 2 {
			freshNativeCaptures(t, f)
		}
		return nil
	})
	if err != nil || out.Status != "complete_with_unknown_outcomes" || out.Ticks != 3 || out.Published != 1 || out.UnknownOutcomes != 1 || len(client.requests) != 2 {
		t.Fatalf("summary: %+v %v", out, err)
	}
	if len(receipts) != 3 || receipts[0].Status != "unknown_outcome" || receipts[0].LocallyVerified || receipts[1].Status != "insufficient_evidence" || !receipts[2].LocallyVerified {
		t.Fatal("receipts lost the original unknown outcome")
	}
}

func TestNativeUnknownContinuationKeepsTerminalScopeAndAckFailures(t *testing.T) {
	for _, mode := range []string{"permission", "unauthenticated", "malformed-ack", "wrong-ack", "source-query", "generation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			fixtureMode := "complete"
			if mode == "wrong-ack" {
				fixtureMode = mode
			}
			f, api := setup(t, fixtureMode)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &nativeLostAckClient{WorldAppServiceClient: api, code: codes.Unavailable}
			selected := appv1.WorldAppServiceClient(client)
			switch mode {
			case "permission":
				client.code = codes.PermissionDenied
			case "unauthenticated":
				client.code = codes.Unauthenticated
			case "cancel":
				client.cancel = cancel
			case "malformed-ack":
				f.publishHook = func(context.Context, *appv1.PublishReply) (*appv1.PublishReply, error) {
					return &appv1.PublishReply{Accepted: true, SignedRecordJson: []byte("invalid")}, nil
				}
				selected = api
			case "wrong-ack":
				selected = api
			}
			s, err := NewNative(f.cfg, selected)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			first, err := s.Step(ctx)
			if !errors.Is(err, ErrUncertain) || first.Status != "unknown_outcome" || first.LocallyVerified {
				t.Fatal(first.Status, err)
			}
			if mode == "source-query" || mode == "generation" {
				freshNativeCaptures(t, f)
				f.mu.Lock()
				if mode == "source-query" {
					f.faultQuery, f.faultKind = 2, "rpc"
				} else {
					f.clockGeneration = 8
				}
				f.mu.Unlock()
				if _, err = s.Step(context.Background()); err == nil || errors.Is(err, ErrUncertain) {
					t.Fatal("scope loss was ignored", err)
				}
			}
			if _, err = s.Step(context.Background()); !errors.Is(err, ErrClosed) || f.published != 1 {
				t.Fatal("terminal error permitted another publication", err)
			}
		})
	}
}
