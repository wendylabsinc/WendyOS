package livefusion

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPublicationUsesTwoEvaluationBatchesWithExactOriginals(t *testing.T) {
	for _, mode := range []string{"complete", "lidar"} {
		t.Run(mode, func(t *testing.T) {
			s, api := setup(t, mode)
			s.cfg.MaxAttempts = 3
			// A lost acknowledgment retains actual per-Run diagnostics. The
			// fixture signs the publication before this hook; no retry is allowed.
			s.publishHook = func(context.Context, *appv1.PublishReply) (*appv1.PublishReply, error) {
				return nil, status.Error(codes.Unavailable, "fixture lost acknowledgment")
			}
			out, err := Run(context.Background(), s.cfg, api)
			if !errors.Is(err, ErrUncertain) || out.Status != "unknown_outcome" || s.published != 1 || out.Attempts != 1 || out.Diagnostics == nil {
				t.Fatal("expected one uncertain mutation", out.Status, err)
			}
			d := out.Diagnostics
			if d.Verify.Batches != 2 || d.Verify.Count != 2*len(s.cfg.Sources) || d.Query.Count != 5 || s.queries != 5 || len(out.Media) != 3 {
				t.Fatalf("repeated verification or skipped work: batches=%d records=%d queries=%d", d.Verify.Batches, d.Verify.Count, s.queries)
			}
			checks := d.Attempts[0].Physical.Checks
			if len(checks) == 0 || checks[len(checks)-1].Phase != "final_batch" {
				t.Fatal("EvaluateBatch final common time check missing")
			}
			for _, input := range out.Inputs {
				if !bytes.Equal(input.EnvelopeJSON, s.original[input.Reference.Key.Source.Stream]) || input.Reference.EnvelopeSHA256 != digest(input.EnvelopeJSON) {
					t.Fatal("publication changed an original input")
				}
			}
		})
	}
}

func TestBuildValidatedRequiresExactOrderedFinalRecordsAndContext(t *testing.T) {
	s, _, proofs := candidateFixture(t, 350*time.Millisecond)
	// This tests only the pure projection's binding checks. Signature and
	// current-authority verification are exercised by the actual RPC test above.
	makeRecords := func() []serviceorigin.VerifiedRecord {
		verified := make([]serviceorigin.VerifiedRecord, len(proofs))
		for i, p := range proofs {
			verified[i] = serviceorigin.VerifiedRecord{Bytes: bytes.Clone(p.signed.Record), Record: p.record}
		}
		return verified
	}
	inputs, err := s.buildValidated(context.Background(), proofs, makeRecords())
	if err != nil || len(inputs) != len(proofs) {
		t.Fatal("exact projection refused", err)
	}
	for i, input := range inputs {
		if input.Reference != proofs[i].reference || input.Component.Version != proofs[i].reference.Version || !bytes.Equal(input.Component.Payload, proofs[i].record.Publication.Payload) {
			t.Fatal("projection changed original reference or payload")
		}
	}
	for _, fault := range []string{"missing", "order", "bytes", "record", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			verified := makeRecords()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch fault {
			case "missing":
				verified = verified[:len(verified)-1]
			case "order":
				verified[0], verified[1] = verified[1], verified[0]
			case "bytes":
				verified[0].Bytes[0] ^= 1
			case "record":
				verified[0].Record.Sequence++
			case "cancel":
				cancel()
			}
			if inputs, err := s.buildValidated(ctx, proofs, verified); err == nil || inputs != nil {
				t.Fatal("invalid final batch produced inputs")
			}
		})
	}
}
