package livepeople

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/pose"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The real gRPC fixture signs the publication before the client loses a reply.
// Retained committed proofs establish the Agent allocated distinct sequences.
type lostPeopleAck struct {
	appv1.WorldAppServiceClient
	loseAt    int
	code      codes.Code
	requests  []*appv1.PublishRequest
	committed [][]byte
	malformed bool
	cancel    context.CancelFunc
}

func (c *lostPeopleAck) Publish(ctx context.Context, q *appv1.PublishRequest, options ...grpc.CallOption) (*appv1.PublishReply, error) {
	c.requests = append(c.requests, proto.Clone(q).(*appv1.PublishRequest))
	ack, err := c.WorldAppServiceClient.Publish(ctx, q, options...)
	if err != nil {
		return ack, err
	}
	c.committed = append(c.committed, bytes.Clone(ack.SignedRecordJson))
	if len(c.requests) == c.loseAt {
		if c.cancel != nil {
			c.cancel()
		}
		if c.malformed {
			return &appv1.PublishReply{Accepted: true, SignedRecordJson: []byte("invalid")}, nil
		}
		return nil, status.Error(c.code, "fixture acknowledgment lost")
	}
	return ack, nil
}

func freshPeopleCaptures(t *testing.T, f *peopleFixture) {
	t.Helper()
	for _, source := range f.cfg.Sources {
		f.setCapture(t, source.SensorID, 18, 750*time.Millisecond)
	}
	f.mu.Lock()
	f.sampleNS = int64(time.Second)
	f.mu.Unlock()
}

func TestLostPeopleAckContinuesOnlyWithFreshObservations(t *testing.T) {
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			f, api := setupPeople(t)
			client := &lostPeopleAck{WorldAppServiceClient: api, loseAt: 1, code: code}
			s, err := New(f.cfg, client, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			unknown, err := s.Step(context.Background())
			if !errors.Is(err, ErrUncertain) || unknown.Status != "unknown_outcome" || len(unknown.Publications) != 1 || unknown.Publications[0].LocallyVerified || len(unknown.Publications[0].SignedRecordJSON) != 0 || unknown.Publications[0].RPCCode != code.String() {
				t.Fatalf("lost outcome: %s %v", unknown.Status, err)
			}
			originalReceipt, _ := json.Marshal(unknown)
			for i := 0; i < 2; i++ {
				duplicate, err := s.Step(context.Background())
				if err != nil || len(duplicate.Publications) != 0 || len(client.requests) != 1 || len(duplicate.Tracks) != 1 || duplicate.Tracks[0].State != "coasting" || !duplicate.Tracks[0].ValidUntil.Equal(unknown.Tracks[0].ValidUntil) || f.inferred != 2 {
					t.Fatalf("old observations repeated or renewed: %s %v", duplicate.Status, err)
				}
			}
			freshPeopleCaptures(t, f)
			fresh, err := s.Step(context.Background())
			if err != nil || len(fresh.Publications) != 1 || !fresh.Publications[0].LocallyVerified || len(client.requests) != 2 || proto.Equal(client.requests[0], client.requests[1]) || f.inferred != 4 {
				t.Fatalf("fresh observation did not progress: %s %v", fresh.Status, err)
			}
			_, first, err := decodeEnvelope(client.committed[0])
			if err != nil {
				t.Fatal(err)
			}
			_, second, err := decodeEnvelope(fresh.Publications[0].SignedRecordJSON)
			if err != nil || first.Epoch != second.Epoch || first.Sequence != 1 || second.Sequence != 2 {
				t.Fatal("Agent source counter was reset or reused")
			}
			body, err := trackcapture.ValidatePublication(second.Publication)
			if err != nil || !body.ObservedAt.Equal(time.Unix(0, f.cfg.SimulationAnchorUnixNS).Add(750*time.Millisecond)) || len(body.Current) != 2 {
				t.Fatalf("fresh original capture identity lost: %v", err)
			}
			for _, member := range body.Current {
				if member.Proof.Version.Sequence != 2 || member.CaptureSequence != 18 || !member.SampleTime.Equal(body.ObservedAt) || body.ValidUntil.After(member.ValidUntil) {
					t.Fatal("fresh publication changed original source version or deadline")
				}
			}
			after, _ := json.Marshal(unknown)
			if !bytes.Equal(originalReceipt, after) {
				t.Fatal("unknown receipt was changed after recovery")
			}
		})
	}
}

func TestLostSecondPeopleAckPreservesFirstPublication(t *testing.T) {
	f, api := setupPeople(t)
	client := &lostPeopleAck{WorldAppServiceClient: api, loseAt: 2, code: codes.Unavailable}
	infer := func(ctx context.Context, raw []byte) (pose.ImageResult, error) {
		out, err := f.infer(ctx, raw)
		second := out.Detections[0]
		second.LeftAnkle.X, second.RightAnkle.X = 290, 310
		out.Detections = append(out.Detections, second)
		return out, err
	}
	s, err := New(f.cfg, client, infer)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	unknown, err := s.Step(context.Background())
	if !errors.Is(err, ErrUncertain) || len(unknown.Publications) != 2 || !unknown.Publications[0].LocallyVerified || unknown.Publications[1].LocallyVerified || len(client.requests) != 2 {
		t.Fatalf("partial tick lost first acknowledgment: %s %v; %d publications", unknown.Status, err, len(unknown.Publications))
	}
	duplicate, err := s.Step(context.Background())
	if err != nil || len(duplicate.Publications) != 0 || len(client.requests) != 2 {
		t.Fatal("partial tick was replayed")
	}
	freshPeopleCaptures(t, f)
	fresh, err := s.Step(context.Background())
	if err != nil || len(fresh.Publications) != 2 || len(client.requests) != 4 {
		t.Fatalf("new groups did not progress: %s %v", fresh.Status, err)
	}
	for i, raw := range client.committed {
		_, record, err := decodeEnvelope(raw)
		if err != nil || record.Sequence != uint64(i+1) {
			t.Fatal("partial tick reused an Agent sequence")
		}
	}
}

func TestPeopleRunRetainsUnknownAndFreshReceipts(t *testing.T) {
	f, api := setupPeople(t)
	f.cfg.MaxTicks, f.cfg.PollIntervalMS = 3, 50
	client := &lostPeopleAck{WorldAppServiceClient: api, loseAt: 1, code: codes.Unavailable}
	var receipts []Result
	summary, err := Run(context.Background(), f.cfg, client, f.infer, func(r Result) error {
		receipts = append(receipts, r)
		if r.Tick == 2 {
			freshPeopleCaptures(t, f)
		}
		return nil
	})
	if err != nil || summary.Status != "complete_with_unknown_outcomes" || summary.UnknownOutcomes != 1 || summary.Ticks != 3 || summary.PublicationAttempts != 2 || summary.Publications != 1 || len(client.requests) != 2 {
		t.Fatalf("bounded continued run: %+v %v", summary, err)
	}
	if len(receipts) != 3 || receipts[0].Status != "unknown_outcome" || len(receipts[1].Publications) != 0 || !receipts[2].Publications[0].LocallyVerified {
		t.Fatal("unknown/duplicate/fresh receipt sequence changed")
	}
}

func TestPeopleUnknownContinuationKeepsRefusalAndMalformedAckTerminal(t *testing.T) {
	for _, mode := range []string{"permission", "unauthenticated", "malformed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setupPeople(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &lostPeopleAck{WorldAppServiceClient: api, loseAt: 1, code: codes.Unavailable}
			switch mode {
			case "permission":
				client.code = codes.PermissionDenied
			case "unauthenticated":
				client.code = codes.Unauthenticated
			case "malformed":
				client.malformed = true
			case "cancel":
				client.cancel = cancel
			}
			s, err := New(f.cfg, client, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			first, err := s.Step(ctx)
			if !errors.Is(err, ErrUncertain) || first.Status != "unknown_outcome" {
				t.Fatalf("ambiguous mutation lost: %s %v", first.Status, err)
			}
			freshPeopleCaptures(t, f)
			if _, err := s.Step(context.Background()); !errors.Is(err, ErrClosed) || len(client.requests) != 1 {
				t.Fatal("terminal failure continued")
			}
		})
	}
}
