package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// renewalRelay exercises the production presence loop over gRPC. Its issuance
// hook can block/fail independently of the live broker's offers and renewal ack.
type renewalRelay struct {
	*testRelay
	issueHook     func(context.Context, int, *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error)
	calls         atomic.Int32
	send          chan *pb.RegisterPresenceResponse
	requests      chan *pb.RegisterPresenceRequest
	fail          chan error
	lifetime      time.Duration
	initialJWS    string
	initialClaims claims
}

func (r *renewalRelay) IssuePresenceLease(ctx context.Context, req *pb.IssuePresenceLeaseRequest) (*pb.IssuePresenceLeaseResponse, error) {
	call := int(r.calls.Add(1))
	// Invoke the hook before minting a replacement, so a blocked/failed RPC
	// leaves offers bound to the current lease, as on an issuance timeout.
	if call > 1 {
		if _, err := r.issueHook(ctx, call, nil); err != nil {
			return nil, err
		}
	}
	l, err := r.testRelay.IssuePresenceLease(ctx, req)
	if err != nil {
		return nil, err
	}
	if call == 1 && r.lifetime > 0 {
		r.mu.Lock()
		r.lease.Exp = time.Now().Add(r.lifetime).Unix()
		l.PresenceLeaseJws = r.artifact(r.lease, leaseType)
		r.leaseJWS = l.PresenceLeaseJws
		r.mu.Unlock()
	}
	l.RenewAfter = timestamppb.New(time.Now().Add(time.Hour))
	if call == 1 {
		l.RenewAfter = timestamppb.New(time.Now())
		r.mu.Lock()
		r.initialJWS, r.initialClaims = l.PresenceLeaseJws, r.lease
		r.mu.Unlock()
	}
	return r.issueHook(ctx, call, l)
}

func (r *renewalRelay) RegisterPresence(s grpc.BidiStreamingServer[pb.RegisterPresenceRequest, pb.RegisterPresenceResponse]) error {
	m, err := s.Recv()
	if err != nil {
		return err
	}
	o := m.GetOpen()
	if o == nil {
		return fmt.Errorf("missing presence open")
	}
	c, err := r.verifier.verify(s.Context(), o.PresenceLeaseJws, leaseType, r.broker().Audience, time.Now())
	if err != nil {
		return err
	}
	pub, err := x509.ParsePKIXPublicKey(o.AgentSigningPublicKeySpkiDer)
	if err != nil {
		return err
	}
	ch := challenge()
	if err = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Challenge{Challenge: ch}}); err != nil {
		return err
	}
	m, err = s.Recv()
	if err != nil {
		return err
	}
	p := m.GetProof()
	if p == nil || !verifyProof(pub.(*ecdsa.PublicKey), "presence", c, o.PresenceLeaseJws, "agent-presence", ch, p.Signature) {
		return fmt.Errorf("invalid presence proof")
	}
	if err = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Accepted{Accepted: &pb.PresenceAccepted{LeaseExpiresAt: timestamppb.New(time.Unix(c.Exp, 0))}}}); err != nil {
		return err
	}
	close(r.present)
	received := make(chan *pb.RegisterPresenceRequest)
	failed := make(chan error, 1)
	go func() {
		for {
			m, err := s.Recv()
			if err != nil {
				failed <- err
				return
			}
			select {
			case received <- m:
			case <-s.Context().Done():
				return
			}
		}
	}()
	for {
		select {
		case m := <-received:
			select {
			case r.requests <- m:
			case <-s.Context().Done():
				return s.Context().Err()
			}
		case m := <-r.send:
			if err := s.Send(m); err != nil {
				return err
			}
		case err := <-r.fail:
			return err
		case err := <-failed:
			return err
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
}

type renewalOptions struct {
	lifetime time.Duration
	prepare  func(*Agent)
}

func startRenewalRelay(t *testing.T, hook func(context.Context, int, *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error), options ...renewalOptions) (*renewalRelay, *Agent, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &renewalRelay{testRelay: &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: v, present: make(chan struct{})}, issueHook: hook, send: make(chan *pb.RegisterPresenceResponse), requests: make(chan *pb.RegisterPresenceRequest, 8), fail: make(chan error)}
	if len(options) > 0 {
		r.lifetime = options[0].lifetime
	}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	v.relayDial = dial
	a := &Agent{Endpoint: r.endpoint, Verifier: v, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), sourceCheckInterval: time.Hour, dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	if len(options) > 0 && options[0].prepare != nil {
		options[0].prepare(a)
	}
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- a.runOnce(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			t.Error("presence attempt did not stop")
		}
		srv.Stop()
	})
	select {
	case <-r.present:
	case err := <-done:
		t.Fatalf("presence failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("presence not registered")
	}
	return r, a, cancel, done
}

func (r *renewalRelay) initialLeaseSnapshot() (string, claims) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.initialJWS, r.initialClaims
}

func renewalWait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for presence fixture")
	}
	var zero T
	return zero
}

func renewalSend(t *testing.T, r *renewalRelay, m *pb.RegisterPresenceResponse) {
	t.Helper()
	select {
	case r.send <- m:
	case <-time.After(3 * time.Second):
		t.Fatal("broker stream stopped receiving")
	}
}

func assertResumeLease(t *testing.T, a *Agent, want string) {
	t.Helper()
	l, err := readPresenceLease(filepath.Join(a.StateDir, "lease.pb"))
	if err != nil || l.PresenceLeaseJws != want {
		t.Fatalf("resume lease changed: %v", err)
	}
}

func TestPresenceRenewalProcessesOffersWhileRPCBlocked(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var inflight, maximum atomic.Int32
	r, a, cancel, done := startRenewalRelay(t, func(ctx context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call == 2 && l == nil {
			n := inflight.Add(1)
			defer inflight.Add(-1)
			maximum.Store(n)
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return l, nil
	})
	renewalWait(t, started)
	old, _ := r.initialLeaseSnapshot()
	offer := r.sessionOffer("during-renewal", nil, nil)
	renewalSend(t, r, offerMessage(offer))
	m := renewalWait(t, r.requests)
	if ack := m.GetOfferAcknowledgement(); ack == nil || ack.OfferId != offer.OfferId {
		t.Fatal("offer was not processed while renewal blocked")
	}
	assertResumeLease(t, a, old)
	if r.calls.Load() != 2 || maximum.Load() != 1 {
		t.Fatal("overlapping renewal RPCs")
	}
	close(release)
	m = renewalWait(t, r.requests)
	n, err := r.verifier.verify(context.Background(), m.GetRenewal().GetPresenceLeaseJws(), leaseType, r.broker().Audience, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Issuance alone must not replace the resume lease or signed expiry.
	assertResumeLease(t, a, old)
	renewalSend(t, r, &pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Renewed{Renewed: &pb.PresenceRenewed{LeaseExpiresAt: timestamppb.New(time.Unix(n.Exp, 0))}}})
	// An offer queued after the acknowledgement observes its owner-loop apply.
	renewalSend(t, r, offerMessage(r.sessionOffer("after-renewal", nil, nil)))
	if renewalWait(t, r.requests).GetOfferAcknowledgement() == nil {
		t.Fatal("renewed presence did not process offer")
	}
	cancel()
	renewalWait(t, done)
	if a.leaseClaims.JTI != n.JTI {
		t.Fatal("broker-acknowledged renewal not applied")
	}
	assertResumeLease(t, a, m.GetRenewal().PresenceLeaseJws)
}

func TestPresenceRenewalRetriesDeadlineWithValidPresence(t *testing.T) {
	failed := make(chan struct{})
	r, a, cancel, done := startRenewalRelay(t, func(_ context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call == 2 && l == nil {
			close(failed)
			return nil, status.Error(codes.DeadlineExceeded, "fixture transient timeout")
		}
		return l, nil
	})
	renewalWait(t, failed)
	old, _ := r.initialLeaseSnapshot()
	renewalSend(t, r, offerMessage(r.sessionOffer("after-deadline", nil, nil)))
	if renewalWait(t, r.requests).GetOfferAcknowledgement() == nil {
		t.Fatal("deadline ended live presence")
	}
	assertResumeLease(t, a, old)
	m := renewalWait(t, r.requests)
	if m.GetRenewal() == nil || r.calls.Load() != 3 {
		t.Fatal("transient deadline did not retry once")
	}
	n, err := r.verifier.verify(context.Background(), m.GetRenewal().PresenceLeaseJws, leaseType, r.broker().Audience, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	renewalSend(t, r, &pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Renewed{Renewed: &pb.PresenceRenewed{LeaseExpiresAt: timestamppb.New(time.Unix(n.Exp, 0))}}})
	renewalSend(t, r, offerMessage(r.sessionOffer("after-retry", nil, nil)))
	renewalWait(t, r.requests)
	cancel()
	renewalWait(t, done)
	if a.leaseClaims.JTI != n.JTI {
		t.Fatal("retried renewal not applied")
	}
}

func TestPresenceRenewalHardExpiryCancelsBlockedRPC(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	r, a, _, done := startRenewalRelay(t, func(ctx context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call == 2 && l == nil {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		}
		return l, nil
	}, renewalOptions{lifetime: 3 * time.Second})
	_, initial := r.initialLeaseSnapshot()
	expires := time.Unix(initial.Exp, 0)
	renewalWait(t, started)
	if err := renewalWait(t, done); err == nil || err.Error() != "Cloud presence lease expired" {
		t.Fatalf("wrong expiry: %v", err)
	}
	renewalWait(t, stopped)
	if time.Now().Before(expires) || time.Since(expires) > time.Second {
		t.Fatal("signed expiry was not enforced promptly")
	}
	if a.lease != nil || r.calls.Load() != 2 {
		t.Fatal("expired lease retained or renewed")
	}
	if _, err := os.Stat(filepath.Join(a.StateDir, "lease.pb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("expired resume lease persisted")
	}
}

func TestPresenceRenewalNeedsBrokerAckBeforeOldExpiry(t *testing.T) {
	r, a, _, done := startRenewalRelay(t, func(_ context.Context, _ int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		return l, nil
	}, renewalOptions{lifetime: 3 * time.Second})
	old, _ := r.initialLeaseSnapshot()
	if renewalWait(t, r.requests).GetRenewal() == nil {
		t.Fatal("missing renewal")
	}
	assertResumeLease(t, a, old)
	// Cloud's fresh verified response alone cannot extend the live stream.
	if err := renewalWait(t, done); err == nil || err.Error() != "Cloud presence lease expired" {
		t.Fatalf("unacknowledged renewal extended expiry: %v", err)
	}
	if a.lease != nil {
		t.Fatal("unacknowledged lease survived old expiry")
	}
}

func TestPresenceRenewalPermanentErrorsFailClosed(t *testing.T) {
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.Unavailable, codes.FailedPrecondition} {
		t.Run(code.String(), func(t *testing.T) {
			r, a, _, done := startRenewalRelay(t, func(_ context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
				if call == 2 && l == nil {
					return nil, status.Error(code, "fixture refusal")
				}
				return l, nil
			})
			err := renewalWait(t, done)
			if status.Code(err) != code {
				t.Fatalf("wrong refusal: %v", err)
			}
			if a.lease != nil || r.calls.Load() != 2 {
				t.Fatal("permanent refusal retained lease or retried")
			}
			if _, err := os.Stat(filepath.Join(a.StateDir, "lease.pb")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused lease persisted")
			}
		})
	}
}

func TestPresenceRenewalCancelStopsRPC(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	r, a, cancel, done := startRenewalRelay(t, func(ctx context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call == 2 && l == nil {
			close(started)
			<-ctx.Done()
			close(stopped)
			return nil, ctx.Err()
		}
		return l, nil
	})
	renewalWait(t, started)
	old, _ := r.initialLeaseSnapshot()
	cancel()
	if err := renewalWait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation: %v", err)
	}
	renewalWait(t, stopped)
	if r.calls.Load() != 2 {
		t.Fatal("canceled renewal retried")
	}
	assertResumeLease(t, a, old)
}

func TestPresenceRetainsLeaseOnPublicBrokerTransportFailure(t *testing.T) {
	for _, code := range []codes.Code{codes.DeadlineExceeded, codes.Unavailable, codes.PermissionDenied, codes.Unauthenticated, codes.FailedPrecondition} {
		t.Run(code.String(), func(t *testing.T) {
			r, a, _, done := startRenewalRelay(t, func(_ context.Context, _ int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
				return l, nil
			})
			old, _ := r.initialLeaseSnapshot()
			r.fail <- status.Error(code, "fixture stream failure")
			if err := renewalWait(t, done); status.Code(err) != code {
				t.Fatal(err)
			}
			retain := code == codes.DeadlineExceeded || code == codes.Unavailable
			if (a.lease != nil) != retain {
				t.Fatal("wrong broker failure lease retention")
			}
			if retain {
				assertResumeLease(t, a, old)
			}
		})
	}
}

func TestPresenceRenewalDefersNewLeaseOfferUntilRPCResult(t *testing.T) {
	issued, release := make(chan struct{}), make(chan struct{})
	r, a, cancel, done := startRenewalRelay(t, func(ctx context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call == 2 && l != nil {
			close(issued)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return l, nil
	})
	old, initial := r.initialLeaseSnapshot()
	renewalWait(t, issued)
	fresh := r.sessionOffer("new-lease-before-response", nil, nil)
	renewalSend(t, r, offerMessage(fresh))
	// An old bound offer remains responsive while the unknown new JTI waits.
	prior := r.sessionOffer("old-lease-during-response", nil, func(g *claims) { g.Presence = initial.JTI })
	renewalSend(t, r, offerMessage(prior))
	if ack := renewalWait(t, r.requests).GetOfferAcknowledgement(); ack == nil || ack.OfferId != prior.OfferId {
		t.Fatal("new lease offer was acked before verification, or blocked old offer")
	}
	select {
	case <-r.requests:
		t.Fatal("unknown presence binding acknowledged early")
	default:
	}
	assertResumeLease(t, a, old)
	if _, err := os.Stat(filepath.Join(a.StateDir, "offer-"+binding([]byte(fresh.OfferId))+".pb")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unknown binding persisted before lease verification")
	}
	close(release)
	if renewalWait(t, r.requests).GetRenewal() == nil {
		t.Fatal("missing broker renewal")
	}
	if ack := renewalWait(t, r.requests).GetOfferAcknowledgement(); ack == nil || ack.OfferId != fresh.OfferId {
		t.Fatal("deferred offer was not admitted under verified new lease")
	}
	cancel()
	renewalWait(t, done)
}

func TestPresenceRenewalDeadlineRetriesBackOffUntilExpiry(t *testing.T) {
	calls := make(chan time.Time, 8)
	r, a, _, done := startRenewalRelay(t, func(_ context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call > 1 && l == nil {
			calls <- time.Now()
			return nil, status.Error(codes.DeadlineExceeded, "fixture deadline")
		}
		return l, nil
	}, renewalOptions{lifetime: 6 * time.Second})
	first, second, third := renewalWait(t, calls), renewalWait(t, calls), renewalWait(t, calls)
	if second.Sub(first) < 900*time.Millisecond || third.Sub(second) < 1900*time.Millisecond {
		t.Fatal("renewal retries did not back off")
	}
	if err := renewalWait(t, done); err == nil || err.Error() != "Cloud presence lease expired" {
		t.Fatalf("retries extended signed expiry: %v", err)
	}
	if r.calls.Load() != 4 || a.lease != nil {
		t.Fatal("unexpected retries after expiry")
	}
}

func TestPresenceRenewalCancelJoinsWorkerBeforeReturn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var snapshots atomic.Int32
	_, _, cancel, done := startRenewalRelay(t, func(_ context.Context, _ int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		return l, nil
	}, renewalOptions{prepare: func(a *Agent) {
		a.Credentials = func() (string, string, []byte) {
			if snapshots.Add(1) == 2 {
				close(started)
				<-release
			}
			return "", "", nil
		}
	}})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	renewalWait(t, started)
	cancel()
	select {
	case err := <-done:
		t.Fatalf("presence returned with its worker still in credentials snapshot: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := renewalWait(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestPresenceRenewalDialDeadlineIsNotRetried(t *testing.T) {
	var dials atomic.Int32
	_, a, _, done := startRenewalRelay(t, func(_ context.Context, _ int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		return l, nil
	}, renewalOptions{prepare: func(a *Agent) {
		dial := a.dialCloud
		a.dialCloud = func(endpoint, cert, chain string, key []byte) (*grpc.ClientConn, error) {
			if dials.Add(1) == 2 {
				return nil, status.Error(codes.DeadlineExceeded, "fixture credential/dial failure")
			}
			return dial(endpoint, cert, chain, key)
		}
	}})
	if err := renewalWait(t, done); status.Code(err) != codes.DeadlineExceeded {
		t.Fatal(err)
	}
	if a.lease != nil || dials.Load() != 2 {
		t.Fatal("non-RPC deadline retained lease or retried")
	}
}
