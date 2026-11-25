package livepeople

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestNativeDiscoveryWitnessRemovesRepeatedQueryOnly(t *testing.T) {
	for _, native := range []bool{false, true} {
		name := "independent"
		create := New
		if native {
			name, create = "native", NewNative
		}
		t.Run(name, func(t *testing.T) {
			f, api := setupPeople(t)
			s, err := create(f.cfg, api, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			first, err := s.Step(context.Background())
			if err != nil || len(first.Publications) != 1 || !first.Publications[0].LocallyVerified || len(first.Inputs) != 2 {
				t.Fatalf("first publication: %s %v", first.Status, err)
			}
			if native && (f.queries != 1 || first.InputAuthority != "native_agent_admission") || !native && (f.queries != 7 || first.InputAuthority != "") {
				t.Fatalf("query count/receipt: %d %q", f.queries, first.InputAuthority)
			}
			for _, p := range first.Inputs {
				if !bytes.Equal(p.EnvelopeJSON, f.proofs[p.Reference.Key.Source.Stream]) {
					t.Fatal("original changed")
				}
			}
			freshPeopleCaptures(t, f)
			second, err := s.Step(context.Background())
			if err != nil || len(second.Publications) != 1 || !second.Publications[0].LocallyVerified || len(second.Inputs) != 4 {
				t.Fatalf("local historical support changed: %s %v", second.Status, err)
			}
			if native && f.queries != 2 || !native && f.queries != 14 {
				t.Fatalf("next Step reused witness: %d", f.queries)
			}
		})
	}
}

// This signed-RPC app fixture models a final native refusal. Separate actual
// Agent prepared-admission tests exercise the host dependency/authority store.
type nativeGateClient struct {
	appv1.WorldAppServiceClient
	f           *peopleFixture
	deny        bool
	attempts    int
	stepContext context.Context
}

func (c *nativeGateClient) GetEvidence(ctx context.Context, q *appv1.GetEvidenceRequest, options ...grpc.CallOption) (*appv1.GetEvidenceReply, error) {
	c.stepContext = context.WithoutCancel(ctx)
	return c.WorldAppServiceClient.GetEvidence(ctx, q, options...)
}

func (c *nativeGateClient) Publish(ctx context.Context, q *appv1.PublishRequest, options ...grpc.CallOption) (*appv1.PublishReply, error) {
	c.attempts++
	p, err := appv1.DecodePublish(q)
	if err != nil {
		return nil, err
	}
	r, err := resultrecord.Decode(p.Payload)
	if err != nil {
		return nil, err
	}
	c.f.mu.Lock()
	refused := c.deny
	for _, ref := range r.Inputs {
		current := c.f.proofs[ref.Key.Source.Stream]
		_, record, err := decodeEnvelope(current)
		if err != nil || record.Epoch != ref.Version.Epoch {
			refused = true
		}
	}
	c.f.mu.Unlock()
	if refused {
		return nil, status.Error(codes.PermissionDenied, "native fixture final dependency refusal")
	}
	return c.WorldAppServiceClient.Publish(ctx, q, options...)
}

func TestNativeFinalPublishRefusalAndWitnessDisposal(t *testing.T) {
	for _, mode := range []string{"replacement", "denial", "success", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setupPeople(t)
			gate := &nativeGateClient{WorldAppServiceClient: api, f: f}
			f.inferHook = func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				if mode == "replacement" {
					f.proofs["people-front"] = bytes.Clone(f.replacement)
				}
				if mode == "denial" {
					gate.deny = true
				}
			}
			if mode == "unknown" {
				f.mode = "uncertain"
			}
			s, err := NewNative(f.cfg, gate, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Step(context.Background())
			if f.queries != 1 || gate.attempts != 1 || len(r.Publications) != 1 {
				t.Fatal("native path did not reach final Publish once")
			}
			if mode == "success" {
				if err != nil || !r.Publications[0].LocallyVerified {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrUncertain) || r.Publications[0].LocallyVerified {
				t.Fatalf("refusal/unknown accepted: %v", err)
			}
			if gate.stepContext == nil {
				t.Fatal("no native Step context observed")
			}
			w, ok := gate.stepContext.Value(admittedKey{}).(*admittedOriginals)
			if !ok || w.owner != nil || w.proofs != nil {
				t.Fatal("witness survived Step return")
			}
			if _, err := s.authorizationOriginals(gate.stepContext); !errors.Is(err, ErrSource) {
				t.Fatal("completed Step witness reusable")
			}
			if mode == "replacement" || mode == "denial" {
				if len(f.published) != 0 {
					t.Fatal("refused request signed")
				}
				if _, err := s.Step(context.Background()); !errors.Is(err, ErrClosed) {
					t.Fatal("explicit refusal not terminal")
				}
			}
		})
	}
}

func TestNativeKeepsSignaturesMediaAndFinalClocks(t *testing.T) {
	for _, mode := range []string{"forged-source", "corrupt-media", "wrong-image", "sample-expiry", "physical-expiry", "reader-generation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, api := setupPeople(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.mode = mode
			f.inferHook = func() {
				f.mu.Lock()
				defer f.mu.Unlock()
				switch mode {
				case "sample-expiry":
					f.sampleNS = int64(4 * time.Second)
				case "physical-expiry":
					f.clock.advance(6 * time.Second)
				case "reader-generation":
					f.generation++
				case "cancel":
					cancel()
				}
			}
			s, err := NewNative(f.cfg, api, f.infer)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			r, err := s.Step(ctx)
			if mode == "sample-expiry" {
				if err != nil || r.Status != "waiting" {
					t.Fatalf("sample expiry: %s %v", r.Status, err)
				}
			} else if err == nil {
				t.Fatalf("%s accepted", mode)
			}
			if len(f.published) != 0 || len(r.Publications) != 0 || f.queries != 1 {
				t.Fatal("changed validation or repeated Query")
			}
		})
	}
}

func TestNativeWitnessRequiresCompleteQueryAndMatchingSession(t *testing.T) {
	f, api := setupPeople(t)
	s, err := NewNative(f.cfg, api, f.infer)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.authorizationOriginals(context.Background()); !errors.Is(err, ErrSource) {
		t.Fatal("missing witness accepted")
	}
	other, err := NewNative(f.cfg, api, f.infer)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	current, err := s.query(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, release := s.admittedContext(context.Background(), current)
	defer release()
	if _, err := other.authorizationOriginals(ctx); !errors.Is(err, ErrSource) {
		t.Fatal("cross-session witness accepted")
	}
	original := current[0]
	current[0] = proof{}
	admitted, err := s.authorizationOriginals(ctx)
	if err != nil || admitted[0].reference != original.reference {
		t.Fatal("caller slice modified witness")
	}
	if !containsOriginal(admitted, original) || containsOriginal(admitted, proof{}) {
		t.Fatal("remote support exactness lost")
	}
}

type partialNativeQuery struct {
	appv1.WorldAppServiceClient
	fail bool
}

func (c partialNativeQuery) Query(ctx context.Context, q *appv1.QueryRequest, options ...grpc.CallOption) (*appv1.QueryReply, error) {
	r, err := c.WorldAppServiceClient.Query(ctx, q, options...)
	if err == nil {
		if c.fail {
			return r, status.Error(codes.Unavailable, "partial native query")
		}
		r.Next = "incomplete"
	}
	return r, err
}

func TestNativeDoesNotWitnessPartialOrFailedQuery(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f, api := setupPeople(t)
		s, err := NewNative(f.cfg, partialNativeQuery{api, fail}, f.infer)
		if err != nil {
			t.Fatal(err)
		}
		r, err := s.Step(context.Background())
		if !errors.Is(err, ErrSource) || r.InputAuthority != "" || f.inferred != 0 || len(f.published) != 0 {
			t.Fatal("partial Query became admitted input")
		}
		if _, err := s.Step(context.Background()); !errors.Is(err, ErrClosed) || f.queries != 1 {
			t.Fatal("failed discovery retained a witness")
		}
		_ = s.Close()
	}
}

func TestRunNativeRetainsUnknownAndRequiresFreshObservations(t *testing.T) {
	f, api := setupPeople(t)
	f.cfg.MaxTicks, f.cfg.PollIntervalMS = 3, 50
	client := &lostPeopleAck{WorldAppServiceClient: api, loseAt: 1, code: codes.Unavailable}
	var results []Result
	summary, err := RunNative(context.Background(), f.cfg, client, f.infer, func(r Result) error {
		results = append(results, r)
		if r.Tick == 2 {
			freshPeopleCaptures(t, f)
		}
		return nil
	})
	if err != nil || summary.Status != "complete_with_unknown_outcomes" || summary.UnknownOutcomes != 1 || summary.PublicationAttempts != 2 || summary.Publications != 1 || f.queries != 3 {
		t.Fatalf("native continued run: %+v %v; %d queries", summary, err, f.queries)
	}
	if len(results) != 3 || results[0].Status != "unknown_outcome" || len(results[1].Publications) != 0 || !results[2].Publications[0].LocallyVerified {
		t.Fatal("unknown original was retried")
	}
}
