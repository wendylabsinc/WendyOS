package livefusion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func publicationFailure(t *testing.T, out Result, err error, s *fixtureServer, stage, code string) *PublicationDiagnostics {
	t.Helper()
	s.mu.Lock()
	published, clocks, queries := s.published, s.clocks, s.queries
	s.mu.Unlock()
	if !errors.Is(err, ErrUncertain) || out.Status != "unknown_outcome" || out.ErrorCode != "mutation_outcome_unknown" || out.FailureStage != "publish_result" || out.LocallyVerified || published != 1 || len(out.RequestJSON) == 0 || len(out.Inputs) != 4 || len(out.Media) != 3 {
		t.Fatalf("publication outcome or prior evidence changed: status=%s published=%d err=%v", out.Status, published, err)
	}
	if out.Diagnostics == nil || out.Diagnostics.Publication == nil {
		t.Fatal("publication diagnostics missing")
	}
	d := out.Diagnostics.Publication
	if d.Attempts != 1 || d.ElapsedNS <= 0 || d.RPCElapsedNS <= 0 || d.PostAckElapsedNS <= 0 || d.ElapsedNS < d.RPCElapsedNS+d.PostAckElapsedNS || d.FailureStage != stage || d.GRPCCode != code {
		t.Fatalf("incorrect bounded publication diagnostics: %+v", d)
	}
	if queries != 5 || out.Diagnostics.Clock.Count != clocks || out.Diagnostics.Query.Count != queries {
		t.Fatal("diagnostics changed or failed to count source authorization calls")
	}
	return d
}

func TestPublicationTimeoutAfterFixtureCommitRemainsUnknown(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	committed := filepath.Join(t.TempDir(), "committed-proof.json")
	s.publishHook = func(ctx context.Context, ack *appv1.PublishReply) (*appv1.PublishReply, error) {
		// Persist the exact signed proof before deliberately losing the response.
		// This is a fixture commit, not production journal qualification.
		f, err := os.OpenFile(committed, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, status.Error(codes.Internal, "fixture write failed")
		}
		_, err = f.Write(ack.SignedRecordJson)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return nil, status.Error(codes.Internal, "fixture commit failed")
		}
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	out, err := Run(context.Background(), s.cfg, api)
	d := publicationFailure(t, out, err, s, "rpc", "DeadlineExceeded")
	if d.AckPresent || d.AckAccepted || d.ProofBytes != 0 || len(out.SignedRecordJSON) != 0 || d.RPCElapsedNS < int64(500*time.Millisecond) {
		t.Fatal("lost response invented an acknowledgment or missed RPC wait")
	}
	raw, err := os.ReadFile(committed)
	if err != nil {
		t.Fatal("server did not commit before response loss", err)
	}
	_, record, err := decodeEnvelope(raw)
	if err != nil {
		t.Fatal("fixture committed proof is malformed", err)
	}
	want, err := appv1.EncodePublish(record.Publication)
	var original appv1.PublishRequest
	if err != nil || protojson.Unmarshal(out.RequestJSON, &original) != nil || !proto.Equal(want, &original) {
		t.Fatal("unknown result did not retain the exact committed request")
	}
}

func TestPublicationReplyDiagnosticsPreserveBoundedOriginalProof(t *testing.T) {
	for _, tc := range []struct {
		name, stage, failure string
		change               func(*fixtureServer, *appv1.PublishReply)
	}{
		{"rejected", "ack_shape", "", func(_ *fixtureServer, p *appv1.PublishReply) { p.Accepted = false }},
		{"unknown-field", "ack_shape", "", func(_ *fixtureServer, p *appv1.PublishReply) { p.ProtoReflect().SetUnknown([]byte{0x18, 1}) }},
		{"empty-proof", "ack_decode", "", func(_ *fixtureServer, p *appv1.PublishReply) { p.SignedRecordJson = nil }},
		{"malformed-proof", "ack_decode", "", func(_ *fixtureServer, p *appv1.PublishReply) { p.SignedRecordJson = []byte("invalid signed proof") }},
		{"expired-after-ack", "ack_verify", "signature_expired", func(s *fixtureServer, _ *appv1.PublishReply) { s.clock.advance(11 * time.Minute) }},
		{"reader-replaced-after-ack", "ack_verify", "signature_clock_refused", func(s *fixtureServer, _ *appv1.PublishReply) { s.clockGeneration = 8 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 3
			var returned []byte
			var accepted bool
			s.publishHook = func(_ context.Context, p *appv1.PublishReply) (*appv1.PublishReply, error) {
				tc.change(s, p)
				returned, accepted = bytes.Clone(p.SignedRecordJson), p.Accepted
				return p, nil
			}
			out, err := Run(context.Background(), s.cfg, api)
			d := publicationFailure(t, out, err, s, tc.stage, "OK")
			if !d.AckPresent || d.AckAccepted != accepted || d.ProofBytes != len(returned) || d.FailureCode != tc.failure || !bytes.Equal(out.SignedRecordJSON, returned) {
				t.Fatalf("exact reply or local refusal classification lost: %+v", d)
			}
			if len(returned) > 0 && out.ProofSHA256 != digest(returned) || len(returned) == 0 && out.ProofSHA256 != "" {
				t.Fatal("diagnostic proof digest does not identify exact retained bytes")
			}
		})
	}
}

// A client decorator covers reply/error combinations that ordinary gRPC cannot
// deliver, while all prior source/media work and the one mutation use Unix RPC.
type publicationReplyClient struct {
	appv1.WorldAppServiceClient
	change func(*appv1.PublishReply) (*appv1.PublishReply, error)
}

func (c publicationReplyClient) Publish(ctx context.Context, p *appv1.PublishRequest, opts ...grpc.CallOption) (*appv1.PublishReply, error) {
	ack, err := c.WorldAppServiceClient.Publish(ctx, p, opts...)
	if err != nil {
		return ack, err
	}
	return c.change(ack)
}

func TestPublicationUnusualClientReplyStaysBoundedAndUnknown(t *testing.T) {
	for _, name := range []string{"nil", "oversized", "reply-with-error"} {
		t.Run(name, func(t *testing.T) {
			s, api := setup(t, "complete")
			var original []byte
			client := publicationReplyClient{WorldAppServiceClient: api, change: func(p *appv1.PublishReply) (*appv1.PublishReply, error) {
				original = bytes.Clone(p.SignedRecordJson)
				switch name {
				case "nil":
					return nil, nil
				case "oversized":
					p.SignedRecordJson = make([]byte, (256<<10)+1)
					return p, nil
				default:
					return p, status.Error(codes.PermissionDenied, "private server message must not enter diagnostics")
				}
			}}
			out, err := Run(context.Background(), s.cfg, client)
			stage, code := "ack_shape", "OK"
			if name == "reply-with-error" {
				stage, code = "rpc", "PermissionDenied"
			}
			d := publicationFailure(t, out, err, s, stage, code)
			if name == "nil" && (d.AckPresent || d.ProofBytes != 0) || name == "oversized" && (d.ProofBytes != (256<<10)+1 || len(out.SignedRecordJSON) != 0 || out.ProofSHA256 != "") || name == "reply-with-error" && (!bytes.Equal(out.SignedRecordJSON, original) || !d.AckPresent || !d.AckAccepted) {
				t.Fatal("malformed reply bypassed bounds or discarded bounded original bytes")
			}
			raw, err := json.Marshal(out)
			if err != nil || len(raw) > MaxResultBytes || bytes.Contains(raw, []byte("private server message")) {
				t.Fatal("publication diagnostics leaked error text or exceeded receipt bound")
			}
		})
	}
}

func TestPublicationOversizedRPCReplyDoesNotInventAcknowledgment(t *testing.T) {
	s, api := setup(t, "complete")
	s.publishHook = func(_ context.Context, p *appv1.PublishReply) (*appv1.PublishReply, error) {
		p.SignedRecordJson = make([]byte, (256<<10)+1)
		return p, nil
	}
	out, err := Run(context.Background(), s.cfg, api)
	d := publicationFailure(t, out, err, s, "rpc", "ResourceExhausted")
	if d.AckPresent || d.ProofBytes != 0 || len(out.SignedRecordJSON) != 0 {
		t.Fatal("receive-size refusal exposed an acknowledgment")
	}
}
