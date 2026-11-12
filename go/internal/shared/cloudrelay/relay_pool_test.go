package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type poolGrant struct {
	claims claims
	key    *ecdsa.PrivateKey
}

type poolBroker struct {
	pb.UnimplementedTunnelBrokerV2ServiceServer
	mu     sync.Mutex
	grants map[string]poolGrant
	proofs int
}

func (b *poolBroker) JoinSession(stream pb.TunnelBrokerV2Service_JoinSessionServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	if len(md.Get("authorization")) != 0 {
		return status.Error(codes.Unauthenticated, "caller identity leaked")
	}
	open, err := stream.Recv()
	if err != nil {
		return err
	}
	b.mu.Lock()
	grant, ok := b.grants[open.GetOpen().GetSessionGrantJws()]
	b.mu.Unlock()
	if !ok {
		return status.Error(codes.PermissionDenied, "unknown grant")
	}
	ch := challenge()
	if err := stream.Send(&pb.JoinSessionResponse{Message: &pb.JoinSessionResponse_Challenge{Challenge: ch}}); err != nil {
		return err
	}
	proof, err := stream.Recv()
	if err != nil {
		return err
	}
	label := "caller"
	if open.GetOpen().GetRole() == pb.JoinRole_JOIN_ROLE_AGENT {
		label = "agent"
	}
	p := proof.GetProof()
	if p == nil || !bytes.Equal(p.ChallengeId, ch.ChallengeId) || !verifyProof(&grant.key.PublicKey, "join", grant.claims, open.GetOpen().SessionGrantJws, label, ch, p.Signature) {
		return status.Error(codes.PermissionDenied, "invalid fresh proof")
	}
	b.mu.Lock()
	b.proofs++
	b.mu.Unlock()
	if err := stream.Send(&pb.JoinSessionResponse{Message: &pb.JoinSessionResponse_Accepted{Accepted: &pb.JoinAccepted{Role: open.GetOpen().Role, RelayExpiresAt: timestamppb.New(time.Unix(grant.claims.RelayExp, 0))}}}); err != nil {
		return err
	}
	for {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := stream.Send(&pb.JoinSessionResponse{Message: &pb.JoinSessionResponse_Frame{Frame: message.GetFrame()}}); err != nil {
			return err
		}
	}
}

func newPoolBroker(t testing.TB, secure ...bool) (*poolBroker, *pb.BrokerInstance, func(string) (*grpc.ClientConn, error), *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	broker := &poolBroker{grants: make(map[string]poolGrant)}
	var options []grpc.ServerOption
	var transport credentials.TransportCredentials = insecure.NewCredentials()
	if len(secure) > 0 && secure[0] {
		fixture := httptest.NewTLSServer(nil)
		certificate := fixture.TLS.Certificates[0]
		roots := x509.NewCertPool()
		roots.AddCert(fixture.Certificate())
		fixture.Close()
		options = append(options, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})))
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots})
	}
	server := grpc.NewServer(options...)
	pb.RegisterTunnelBrokerV2ServiceServer(server, broker)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	var dials atomic.Int32
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		dials.Add(1)
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(transport.Clone()))
	}
	return broker, &pb.BrokerInstance{Endpoint: listener.Addr().String(), Audience: "broker/incarnation/one"}, dial, &dials
}

func (b *poolBroker) grant(t testing.TB, endpoint *pb.BrokerInstance) (string, poolGrant) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	artifact := fmt.Sprintf("grant-%d", len(b.grants))
	grant := poolGrant{claims: claims{Aud: endpoint.Audience, JTI: artifact, Session: artifact, Exp: time.Now().Add(time.Minute).Unix(), RelayExp: time.Now().Add(time.Minute).Unix()}, key: key}
	b.grants[artifact] = grant
	return artifact, grant
}

func (b *poolBroker) open(t testing.TB, ctx context.Context, v *Verifier, endpoint *pb.BrokerInstance, service string) *Session {
	t.Helper()
	artifact, grant := b.grant(t, endpoint)
	s, err := join(ctx, endpoint, artifact, grant.claims, grant.key, pb.JoinRole_JOIN_ROLE_CALLER, v, service)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func poolEcho(t testing.TB, session *Session) {
	t.Helper()
	if err := session.Send([]byte("echo"), false); err != nil {
		t.Fatal(err)
	}
	got, half, err := session.Recv()
	if err != nil || half || string(got) != "echo" {
		t.Fatalf("echo = %q, %v, %v", got, half, err)
	}
}

func TestRelayPoolSessionIsolationAndFreshProof(t *testing.T) {
	broker, endpoint, dial, dials := newPoolBroker(t, true)
	v := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close() })
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(t.Context(), metadata.Pairs("authorization", "must-not-leak")), 5*time.Second)
	defer cancel()
	first := broker.open(t, ctx, v, endpoint, "wendy-agent")
	second := broker.open(t, ctx, v, endpoint, "wendy-agent")
	if first.conn != second.conn || dials.Load() != 1 {
		t.Fatal("concurrent sessions did not share transport")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	poolEcho(t, second)
	_ = second.Close()
	third := broker.open(t, ctx, v, endpoint, "wendy-agent")
	poolEcho(t, third)
	if third.conn != first.conn || dials.Load() != 1 {
		t.Fatal("sequential session did not reuse transport")
	}
	broker.mu.Lock()
	defer broker.mu.Unlock()
	if broker.proofs != 3 {
		t.Fatalf("holder proofs = %d, want 3", broker.proofs)
	}
}

func TestRelayPoolCancellationAndExpiryReleaseOnlyOneSession(t *testing.T) {
	broker, endpoint, dial, _ := newPoolBroker(t)
	v := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sibling := broker.open(t, ctx, v, endpoint, "wendy-agent")
	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%v", deadline), func(t *testing.T) {
			var child context.Context
			var stop context.CancelFunc
			if deadline {
				child, stop = context.WithTimeout(ctx, 100*time.Millisecond)
			} else {
				child, stop = context.WithCancel(ctx)
			}
			defer stop()
			s := broker.open(t, child, v, endpoint, "wendy-agent")
			if !deadline {
				stop()
			}
			_, _, err := s.Recv()
			if err == nil {
				t.Fatal("cancelled stream remained readable")
			}
			poolEcho(t, sibling)
			waitPool(t, func() bool {
				v.relays.mu.Lock()
				defer v.relays.mu.Unlock()
				return len(v.relays.entries) == 1 && v.relays.entries[0].refs == 1
			})
		})
	}
}

func TestRelayPoolPartitionsAndOwnerClose(t *testing.T) {
	broker, endpoint, dial, dials := newPoolBroker(t)
	v := &Verifier{relayDial: dial}
	other := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close(); _ = other.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	management := broker.open(t, ctx, v, endpoint, "wendy-agent")
	application := broker.open(t, ctx, v, endpoint, "tcp-8080")
	independent := broker.open(t, ctx, other, endpoint, "wendy-agent")
	incarnation := &pb.BrokerInstance{Endpoint: endpoint.Endpoint, Audience: "broker/incarnation/two"}
	rotated := broker.open(t, ctx, v, incarnation, "wendy-agent")
	if dials.Load() != 4 {
		t.Fatalf("partition dials = %d, want 4", dials.Load())
	}
	_ = v.Close()
	for _, s := range []*Session{management, application, rotated} {
		if _, _, err := s.Recv(); err == nil {
			t.Fatal("owner close did not stop owned session")
		}
	}
	poolEcho(t, independent)
	if _, _, err := v.relays.acquire(ctx, relayPoolKey{}, dial); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed pool acquire: %v", err)
	}
}

func TestRelayPoolIdleEvictionAndFailedConnection(t *testing.T) {
	_, endpoint, dial, dials := newPoolBroker(t)
	v := &Verifier{relayDial: dial, relays: relayPool{idleTimeout: 20 * time.Millisecond}}
	t.Cleanup(func() { _ = v.Close() })
	key := relayPoolKey{endpoint: endpoint.Endpoint, audience: endpoint.Audience}
	conn, release, err := v.relays.acquire(t.Context(), key, dial)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	waitPool(t, func() bool { return conn.GetState() == connectivity.Shutdown })
	second, releaseSecond, err := v.relays.acquire(t.Context(), key, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	_ = second.Close()
	third, releaseThird, err := v.relays.acquire(t.Context(), key, dial)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseThird()
	if third == second || dials.Load() != 3 {
		t.Fatal("failed transport was reused")
	}
}

func TestRelayPoolCapacityAndCancelledWaiter(t *testing.T) {
	_, endpoint, dial, dials := newPoolBroker(t)
	v := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close() })
	var releases []func()
	for i := range relayPoolLimit {
		_, release, err := v.relays.acquire(t.Context(), relayPoolKey{endpoint: endpoint.Endpoint, audience: fmt.Sprint(i)}, dial)
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	key := relayPoolKey{endpoint: endpoint.Endpoint, audience: "new"}
	if _, _, err := v.relays.acquire(ctx, key, dial); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full pool: %v", err)
	}
	if dials.Load() != relayPoolLimit {
		t.Fatal("pool exceeded connection bound")
	}
	releases[0]()
	_, release, err := v.relays.acquire(t.Context(), key, dial)
	if err != nil {
		t.Fatal(err)
	}
	release()
	for _, release := range releases {
		release()
	}
}

func TestRelayPoolConcurrentAcquireAndDialFailure(t *testing.T) {
	_, endpoint, dial, dials := newPoolBroker(t)
	v := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close() })
	key := relayPoolKey{endpoint: endpoint.Endpoint}
	failed := errors.New("factory failed")
	if _, _, err := v.relays.acquire(t.Context(), key, func(string) (*grpc.ClientConn, error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	connections := make(chan *grpc.ClientConn, 32)
	for range cap(connections) {
		group.Go(func() {
			conn, release, err := v.relays.acquire(t.Context(), key, dial)
			if err != nil {
				t.Error(err)
				return
			}
			connections <- conn
			release()
		})
	}
	group.Wait()
	close(connections)
	var first *grpc.ClientConn
	for conn := range connections {
		if first == nil {
			first = conn
		}
		if first != conn {
			t.Fatal("concurrent acquisition dialed duplicate connection")
		}
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d", dials.Load())
	}
}

func waitPool(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("pool did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRelayPoolPendingFactoryCloseAndWaiterCancellation(t *testing.T) {
	_, endpoint, dial, dials := newPoolBroker(t)
	for _, closePool := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%v", closePool), func(t *testing.T) {
			v := &Verifier{}
			t.Cleanup(func() { _ = v.Close() })
			started, unblock := make(chan struct{}), make(chan struct{})
			factory := func(endpoint string) (*grpc.ClientConn, error) {
				close(started)
				<-unblock
				return dial(endpoint)
			}
			key := relayPoolKey{endpoint: endpoint.Endpoint}
			ownerDone := make(chan error, 1)
			go func() {
				_, release, err := v.relays.acquire(t.Context(), key, factory)
				if release != nil {
					release()
				}
				ownerDone <- err
			}()
			<-started
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			waitCtx := &poolWaitContext{Context: ctx, waiting: make(chan struct{})}
			waiter := make(chan error, 1)
			go func() {
				_, release, err := v.relays.acquire(waitCtx, key, dial)
				if release != nil {
					release()
				}
				waiter <- err
			}()
			<-waitCtx.waiting
			if closePool {
				_ = v.Close()
			} else {
				cancel()
			}
			select {
			case err := <-waiter:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("waiting acquisition: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("waiter stayed blocked on factory")
			}
			close(unblock)
			err := <-ownerDone
			if closePool {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("closed factory owner: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				before := dials.Load()
				_, release, err := v.relays.acquire(t.Context(), key, dial)
				if err != nil {
					t.Fatal(err)
				}
				release()
				if dials.Load() != before {
					t.Fatal("cancelled waiter poisoned reuse")
				}
			}
		})
	}
}

func TestRelayPoolRefusalAndGrantExpiry(t *testing.T) {
	broker, endpoint, dial, dials := newPoolBroker(t)
	v := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sibling := broker.open(t, ctx, v, endpoint, "wendy-agent")
	artifact, grant := broker.grant(t, endpoint)
	if _, err := join(ctx, endpoint, "unknown", grant.claims, grant.key, pb.JoinRole_JOIN_ROLE_CALLER, v, "wendy-agent"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("refusal: %v", err)
	}
	poolEcho(t, sibling)
	grant.claims.RelayExp = time.Now().Unix() + 1
	broker.mu.Lock()
	broker.grants[artifact] = grant
	broker.mu.Unlock()
	s, err := join(ctx, endpoint, artifact, grant.claims, grant.key, pb.JoinRole_JOIN_ROLE_CALLER, v, "wendy-agent")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err := s.Recv(); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("expired grant stream: %v", err)
	}
	poolEcho(t, sibling)
	if dials.Load() != 1 {
		t.Fatal("RPC refusal/expiry discarded healthy shared connection")
	}
}

func TestRelayPoolMaxAgeDoesNotInterruptActiveSession(t *testing.T) {
	broker, endpoint, dial, dials := newPoolBroker(t)
	v := &Verifier{relayDial: dial}
	t.Cleanup(func() { _ = v.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	old := broker.open(t, ctx, v, endpoint, "wendy-agent")
	v.relays.mu.Lock()
	v.relays.entries[0].created = time.Now().Add(-relayPoolMaxAge)
	v.relays.mu.Unlock()
	fresh := broker.open(t, ctx, v, endpoint, "wendy-agent")
	if old.conn == fresh.conn || dials.Load() != 2 {
		t.Fatal("expired transport reused")
	}
	poolEcho(t, old)
	poolEcho(t, fresh)
	_ = old.Close()
	waitPool(t, func() bool { return old.conn.GetState() == connectivity.Shutdown })
}

func BenchmarkRelaySessionJoin(b *testing.B) {
	for _, reuse := range []bool{false, true} {
		b.Run(fmt.Sprintf("reuse=%v", reuse), func(b *testing.B) {
			broker, endpoint, dial, dials := newPoolBroker(b, true)
			v := &Verifier{relayDial: dial}
			b.Cleanup(func() { _ = v.Close() })
			ctx, cancel := context.WithCancel(b.Context())
			defer cancel()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if !reuse {
					v = &Verifier{relayDial: dial}
				}
				artifact, grant := broker.grant(b, endpoint)
				s, err := join(ctx, endpoint, artifact, grant.claims, grant.key, pb.JoinRole_JOIN_ROLE_CALLER, v, "wendy-agent")
				if err != nil {
					b.Fatal(err)
				}
				_ = s.Close()
				if !reuse {
					_ = v.Close()
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(dials.Load())/float64(b.N), "connections/op")
		})
	}
}

// Done is evaluated when acquire reaches its pending-factory select, letting the
// test close the owner only after the waiter is actually parked on that path.
type poolWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *poolWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}
