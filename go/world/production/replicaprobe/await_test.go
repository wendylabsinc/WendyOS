package replicaprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type awaitClient struct {
	*rpcFixture
	respond func(context.Context, int) (*appv1.QueryReply, error)
}

func (c *awaitClient) Query(ctx context.Context, request *appv1.QueryRequest, opts ...grpc.CallOption) (*appv1.QueryReply, error) {
	reply, err := c.rpcFixture.Query(ctx, request, opts...)
	if err != nil || c.respond == nil {
		return reply, err
	}
	return c.respond(ctx, c.queryCalls)
}

func awaiting(t *testing.T) (*Probe, *awaitClient) {
	t.Helper()
	_, f := fixture(t)
	c := &awaitClient{rpcFixture: f}
	p, err := New(f.cfg, c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, c
}

func awaitCommand(sequence uint32, expect, hash string) []byte {
	raw, _ := json.Marshal(Command{Version: 1, Sequence: sequence, Action: "await_query", Expect: expect, EnvelopeSHA256: hash})
	return raw
}

func TestAwaitDeniedUntilReadyPreservesEveryObservation(t *testing.T) {
	p, f := awaiting(t)
	f.respond = func(_ context.Context, n int) (*appv1.QueryReply, error) {
		if n == 1 {
			return nil, status.Error(codes.PermissionDenied, "private denial details")
		}
		if n == 2 {
			return nil, status.Error(codes.Unavailable, "private connection details")
		}
		return &appv1.QueryReply{}, nil
	}
	r, err := p.Execute(context.Background(), awaitCommand(1, "ready", ""))
	if err != nil || r.Status != "complete" || len(r.Attempts) != 3 || f.clockCalls != 3 || f.queryCalls != 3 || f.publishCalls != 0 {
		t.Fatal("bounded reads failed", err, r)
	}
	for i, entry := range r.Attempts {
		if entry.Attempt != i+1 || entry.Clock == nil || entry.ElapsedNS < 0 || entry.ElapsedNS > r.AwaitElapsedNS {
			t.Fatal("attempt metadata differs", entry)
		}
		for j := 0; j < i; j++ {
			if bytes.Equal(f.nonces[i], f.nonces[j]) {
				t.Fatal("clock challenge reused")
			}
		}
	}
	if r.Attempts[0].RPCCode != "PermissionDenied" || r.Attempts[1].RPCCode != "Unavailable" || r.Attempts[2].Outcome != "matched" || r.Attempts[2].ReplySHA256 != digest(r.ReplyJSON) || len(r.Originals) != 0 {
		t.Fatal("refusal or final reply lost", r)
	}
	raw, err := encoded(r, MaxReceiptBytes)
	if err != nil || bytes.Contains(raw, []byte("private")) {
		t.Fatal("raw backend error exported")
	}
	if _, err = p.Execute(context.Background(), command(2, "publish")); err != nil || f.publishCalls != 1 {
		t.Fatal("successful wait did not preserve next command", err)
	}
}

func TestAwaitPresentAndAbsentUseValidExactReplies(t *testing.T) {
	for _, expect := range []string{"present", "absent"} {
		t.Run(expect, func(t *testing.T) {
			p, f := awaiting(t)
			original := remoteOriginal(t, p, f.rpcFixture)
			initialClocks := f.clockCalls
			hash := ""
			if expect == "present" {
				hash = digest(original)
			}
			f.respond = func(_ context.Context, n int) (*appv1.QueryReply, error) {
				present := (expect == "present" && n == 2) || (expect == "absent" && n == 1)
				if present {
					return &appv1.QueryReply{SignedRecordJson: [][]byte{original}}, nil
				}
				return &appv1.QueryReply{}, nil
			}
			r, err := p.Execute(context.Background(), awaitCommand(1, expect, hash))
			if err != nil || len(r.Attempts) != 2 || f.clockCalls-initialClocks != 2 || r.Attempts[0].Outcome != "predicate_mismatch" || r.Attempts[1].Outcome != "matched" {
				t.Fatal("predicate failed", err, r)
			}
			if expect == "present" && (len(r.Originals) != 1 || !bytes.Equal(r.Originals[0].Envelope, original) || r.Attempts[1].EnvelopeSHA256 != hash) {
				t.Fatal("original bytes replaced")
			}
			if expect == "absent" && (len(r.Originals) != 0 || r.Attempts[1].EnvelopeSHA256 != "") {
				t.Fatal("old successful result leaked into empty reply")
			}
		})
	}
}

func TestAwaitAttemptsExhaustAndStopWithoutPublication(t *testing.T) {
	p, f := awaiting(t)
	f.respond = func(context.Context, int) (*appv1.QueryReply, error) {
		return nil, status.Error(codes.PermissionDenied, "denied")
	}
	r, err := p.Execute(context.Background(), awaitCommand(1, "ready", ""))
	if !errors.Is(err, ErrAwait) || r.Status != "failed" || len(r.Attempts) != MaxAwaitAttempts || f.queryCalls != MaxAwaitAttempts || f.clockCalls != MaxAwaitAttempts || f.publishCalls != 0 {
		t.Fatal("attempt bound changed", err, len(r.Attempts), f.queryCalls)
	}
	if _, err = p.Execute(context.Background(), command(2, "publish")); !errors.Is(err, ErrStopped) || f.publishCalls != 0 {
		t.Fatal("failed wait resumed")
	}
}

func TestAwaitSuccessfulWrongDigestWaitsForExactOriginal(t *testing.T) {
	p, f := awaiting(t)
	first, wanted := remoteOriginal(t, p, f.rpcFixture), remoteOriginal(t, p, f.rpcFixture)
	f.respond = func(_ context.Context, n int) (*appv1.QueryReply, error) {
		if n == 1 {
			return &appv1.QueryReply{SignedRecordJson: [][]byte{first}}, nil
		}
		return &appv1.QueryReply{SignedRecordJson: [][]byte{wanted}}, nil
	}
	r, err := p.Execute(context.Background(), awaitCommand(1, "present", digest(wanted)))
	if err != nil || len(r.Attempts) != 2 || r.Attempts[0].Outcome != "predicate_mismatch" || r.Attempts[0].EnvelopeSHA256 != digest(first) || r.Attempts[1].EnvelopeSHA256 != digest(wanted) || !bytes.Equal(r.Originals[0].Envelope, wanted) {
		t.Fatal("wrong digest became matching proof", err, r)
	}
	raw, err := encoded(r, MaxReceiptBytes)
	if err != nil || len(raw) > MaxReceiptBytes {
		t.Fatal("receipt exceeds bound")
	}
}

func TestAwaitCanceledBeforeClockDoesNoWork(t *testing.T) {
	p, f := awaiting(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, err := p.Execute(ctx, awaitCommand(1, "ready", ""))
	if !errors.Is(err, context.Canceled) || len(r.Attempts) != 0 || f.clockCalls != 0 || f.queryCalls != 0 || f.publishCalls != 0 {
		t.Fatal("canceled operation touched API", err, r)
	}
}

func TestAwaitWallBudgetAndCancellationAreTerminal(t *testing.T) {
	for _, mode := range []string{"budget", "parent", "late-success"} {
		t.Run(mode, func(t *testing.T) {
			p, f := awaiting(t)
			p.cfg.RequestTimeoutMS = 5000
			ctx := context.Background()
			if mode != "budget" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 40*time.Millisecond)
				defer cancel()
			}
			f.respond = func(ctx context.Context, _ int) (*appv1.QueryReply, error) {
				<-ctx.Done()
				if mode == "late-success" {
					return &appv1.QueryReply{}, nil
				}
				return nil, ctx.Err()
			}
			started := time.Now()
			r, err := p.Execute(ctx, awaitCommand(1, "ready", ""))
			if !errors.Is(err, context.DeadlineExceeded) || r.Status != "failed" || len(r.Attempts) == 0 || r.Attempts[len(r.Attempts)-1].Outcome != "canceled" || len(r.ReplyJSON) != 0 || f.publishCalls != 0 {
				t.Fatal("deadline became success", err, r)
			}
			if mode == "budget" && (time.Since(started) < AwaitBudget || time.Since(started) > AwaitBudget+2*time.Second || f.queryCalls != 2) {
				t.Fatal("fixed wall budget changed", time.Since(started), f.queryCalls)
			}
		})
	}
}

func TestAwaitClockIdentityAndMalformedRepliesNeverRetry(t *testing.T) {
	for _, mode := range []string{"generation", "boot", "regression", "wrong-scope", "unknown", "cursor", "bad-proof", "rpc-refused"} {
		t.Run(mode, func(t *testing.T) {
			p, f := awaiting(t)
			if mode == "wrong-scope" {
				q, _ := p.clock.Observe(context.Background())
				c, _ := DecodeCommand(command(1, "publish"))
				request, _ := p.request(c, q)
				reply, err := f.Publish(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				f.query = &appv1.QueryReply{SignedRecordJson: [][]byte{reply.SignedRecordJson}}
			}
			initialClocks, initialQueries, initialPubs := f.clockCalls, f.queryCalls, f.publishCalls
			f.respond = func(_ context.Context, n int) (*appv1.QueryReply, error) {
				if n == 1 {
					return nil, status.Error(codes.PermissionDenied, "first wait")
				}
				switch mode {
				case "wrong-scope":
					return f.query, nil
				case "unknown":
					reply := &appv1.QueryReply{}
					reply.ProtoReflect().SetUnknown([]byte{0x18, 1})
					return reply, nil
				case "cursor":
					return &appv1.QueryReply{Next: "unexpected"}, nil
				case "bad-proof":
					return &appv1.QueryReply{SignedRecordJson: [][]byte{[]byte("{}")}}, nil
				case "rpc-refused":
					return nil, status.Error(codes.Unauthenticated, "must not retry")
				default:
					return &appv1.QueryReply{}, nil
				}
			}
			f.clockMutation = func(reply *appv1.ClockReply) {
				if f.clockCalls-initialClocks == 2 {
					switch mode {
					case "generation":
						reply.DeploymentGeneration++
					case "boot":
						reply.ServerBootId = strings.Repeat("b", 32)
					case "regression":
						reply.SampleUnixNs--
					}
				}
			}
			r, err := p.Execute(context.Background(), awaitCommand(1, "ready", ""))
			if err == nil || r.Status != "failed" || len(r.Attempts) != 2 || f.publishCalls != initialPubs {
				t.Fatal("terminal boundary retried", mode, err, r)
			}
			queries := 2
			if mode == "generation" || mode == "boot" || mode == "regression" {
				queries = 1
				if r.Clock != nil || len(r.ReplyJSON) != 0 {
					t.Fatal("earlier observation retained")
				}
			}
			if f.queryCalls-initialQueries != queries {
				t.Fatal("query repeated after terminal fault")
			}
		})
	}
}

func TestAwaitStrictFieldsAndOrdinaryQueryStillFailStop(t *testing.T) {
	for _, raw := range []string{
		`{"version":1,"sequence":1,"action":"await_query"}`,
		`{"version":1,"sequence":1,"action":"await_query","expect":"present"}`,
		`{"version":1,"sequence":1,"action":"await_query","expect":"ready","envelope_sha256":""}`,
		`{"version":1,"sequence":1,"action":"query","expect":""}`,
		`{"version":1,"sequence":1,"action":"publish","value":1,"ttl_ms":100,"expect":"ready"}`,
		`{"version":1,"sequence":1,"action":"await_query","expect":"ready","attempts":100}`,
	} {
		if _, err := DecodeCommand([]byte(raw)); err == nil {
			t.Fatal("invalid command accepted", raw)
		}
	}
	p, f := awaiting(t)
	f.respond = func(context.Context, int) (*appv1.QueryReply, error) {
		return nil, status.Error(codes.Unavailable, "ordinary query fails")
	}
	if r, err := p.Execute(context.Background(), command(1, "query")); err == nil || r.Status != "failed" || len(r.Attempts) != 0 || f.queryCalls != 1 || f.clockCalls != 1 {
		t.Fatal("ordinary query retried", err, r)
	}
	if _, err := p.Execute(context.Background(), awaitCommand(2, "ready", "")); !errors.Is(err, ErrStopped) || f.queryCalls != 1 {
		t.Fatal("ordinary failure resumed")
	}
}
