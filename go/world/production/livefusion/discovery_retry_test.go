package livefusion

import (
	"context"
	"errors"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type queryFaultClient struct {
	appv1.WorldAppServiceClient
	calls int
	fault func(int) error
}

func (c *queryFaultClient) Query(ctx context.Context, q *appv1.QueryRequest, opts ...grpc.CallOption) (*appv1.QueryReply, error) {
	c.calls++
	if err := c.fault(c.calls); err != nil {
		return nil, err
	}
	return c.WorldAppServiceClient.Query(ctx, q, opts...)
}

func TestDiscoveryRetry(t *testing.T) {
	for _, code := range []codes.Code{codes.DeadlineExceeded, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 3
			client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
				if n <= 2 {
					return status.Error(code, "private failure details")
				}
				return nil
			}}
			out, err := Run(context.Background(), s.cfg, client)
			// Two failed discovery calls precede one successful discovery and
			// two authority Queries in each of the two verification batches.
			// Building result inputs reuses the final batch without another Query.
			if err != nil || out.Status != "complete" || !out.LocallyVerified || out.Attempts != 3 || client.calls != 7 || s.published != 1 || len(out.Inputs) != 4 || len(out.Media) != 3 {
				t.Fatalf("fresh recovery failed: status=%s attempts=%d calls=%d published=%d err=%v", out.Status, out.Attempts, client.calls, s.published, err)
			}
		})
	}
}

func TestDiscoveryBudget(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(int) error { return status.Error(codes.Unavailable, "private") }}
	started := time.Now()
	out, err := Run(context.Background(), s.cfg, client)
	if !errors.Is(err, ErrSource) || out.Status != "failed" || out.Attempts != 3 || client.calls != 3 || s.published != 0 || len(out.Inputs) != 0 || len(out.Media) != 0 || out.Evaluation != nil || len(out.RequestJSON) != 0 {
		t.Fatalf("discovery did not exhaust fixed budget: status=%s calls=%d err=%v", out.Status, client.calls, err)
	}
	if time.Since(started) < time.Second || out.ErrorCode != "source_query_rpc_Unavailable" || out.Diagnostics == nil || len(out.Diagnostics.Attempts) != 3 {
		t.Fatal("poll delay or final bounded diagnostic lost")
	}
	for i, attempt := range out.Diagnostics.Attempts {
		if attempt.QueryFailure != "source_query_rpc_Unavailable" || attempt.QueryRetryScheduled != (i < 2) || attempt.Query.Count != 1 || attempt.Verify.Count != 0 {
			t.Fatalf("incorrect attempt diagnostic: %+v", attempt)
		}
	}
}

func TestDiscoveryDenialNoRetry(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument, codes.ResourceExhausted, codes.Internal} {
		t.Run(code.String(), func(t *testing.T) {
			s, api := setup(t, "complete")
			s.cfg.MaxAttempts = 3
			client := &queryFaultClient{WorldAppServiceClient: api, fault: func(int) error { return status.Error(code, "private") }}
			out, err := Run(context.Background(), s.cfg, client)
			if !errors.Is(err, ErrSource) || out.Attempts != 1 || client.calls != 1 || s.published != 0 || out.Diagnostics.Attempts[0].QueryRetryScheduled {
				t.Fatal("non-transient refusal retried", err)
			}
		})
	}
}

func TestAuthQueryNoRetry(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
		if n == 2 {
			return status.Error(codes.DeadlineExceeded, "private")
		}
		return nil
	}}
	out, err := Run(context.Background(), s.cfg, client)
	if err == nil || out.Attempts != 1 || client.calls != 2 || s.published != 0 || out.FailureStage != "verify_input" || out.Diagnostics.Attempts[0].QueryRetryScheduled {
		t.Fatalf("authorization was retried: stage=%s calls=%d err=%v", out.FailureStage, client.calls, err)
	}
}

func TestDiscoveryCancel(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(int) error {
		time.AfterFunc(50*time.Millisecond, cancel)
		return status.Error(codes.Unavailable, "private")
	}}
	out, err := Run(ctx, s.cfg, client)
	if !errors.Is(err, context.Canceled) || client.calls != 1 || s.published != 0 || out.ErrorCode == "source_query_rpc_Unavailable" {
		t.Fatalf("cancellation did not stop bounded retry: calls=%d err=%v code=%s", client.calls, err, out.ErrorCode)
	}
}

func TestDiscoveryClearsPriorInputs(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 2
	s.cfg.MaxSkewMS = 1 // The first four-input tuple spans the 250 ms audio window.
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
		if n == 2 {
			return status.Error(codes.DeadlineExceeded, "private")
		}
		return nil
	}}
	out, err := Run(context.Background(), s.cfg, client)
	if !errors.Is(err, ErrSource) || out.Attempts != 2 || client.calls != 2 || s.published != 0 || len(out.Inputs) != 0 || len(out.Media) != 0 || out.Evaluation != nil || len(out.RequestJSON) != 0 {
		t.Fatalf("prior candidate survived failed discovery: attempts=%d inputs=%d err=%v", out.Attempts, len(out.Inputs), err)
	}
	if out.Diagnostics.Attempts[0].CandidateCount != 4 || out.Diagnostics.Attempts[1].CandidateCount != 0 || out.Diagnostics.Verify.Count != 0 {
		t.Fatal("prior attempt evidence lost or stale tuple verified")
	}
}

func TestDiscoveryDuration(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 16
	s.cfg.DurationMS = 1000
	s.cfg.PollIntervalMS = 5000
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(int) error { return status.Error(codes.Unavailable, "private") }}
	out, err := Run(context.Background(), s.cfg, client)
	if !errors.Is(err, context.DeadlineExceeded) || out.Attempts != 1 || client.calls != 1 || s.published != 0 || out.ErrorCode == "source_query_rpc_Unavailable" {
		t.Fatalf("outer deadline did not stop discovery: attempts=%d calls=%d code=%s err=%v", out.Attempts, client.calls, out.ErrorCode, err)
	}
}

func TestDiscoveryKeepsSourceBinding(t *testing.T) {
	s, api := setup(t, "complete")
	s.cfg.MaxAttempts = 3
	s.cfg.MaxSkewMS = 1
	s.faultQuery = 2
	s.faultKind = "replacement"
	client := &queryFaultClient{WorldAppServiceClient: api, fault: func(n int) error {
		if n == 2 {
			return status.Error(codes.Unavailable, "private")
		}
		return nil
	}}
	out, err := Run(context.Background(), s.cfg, client)
	if !errors.Is(err, ErrSource) || out.Attempts != 3 || client.calls != 3 || s.published != 0 || len(out.Inputs) != 0 || out.Evaluation != nil {
		t.Fatalf("source replacement repinned after discovery retry: attempts=%d err=%v", out.Attempts, err)
	}
	if out.ErrorCode == "source_query_rpc_Unavailable" || !out.Diagnostics.Attempts[1].QueryRetryScheduled || out.Diagnostics.Attempts[2].QueryRetryScheduled {
		t.Fatal("stale transport error hid changed source authority")
	}
}
