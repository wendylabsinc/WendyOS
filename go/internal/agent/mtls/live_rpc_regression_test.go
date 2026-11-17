package mtls

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type boundaryStream struct {
	grpc.ServerStream
	ctx  context.Context
	recv func() error
	sent int
}

func (s *boundaryStream) Context() context.Context { return s.ctx }
func (s *boundaryStream) SendMsg(any) error        { s.sent++; return nil }
func (s *boundaryStream) RecvMsg(any) error {
	if s.recv != nil {
		return s.recv()
	}
	return nil
}

func liveCheckFixture(t testing.TB, pq bool) (*crltest.PKI, func(context.Context, string) error, func(context.Context, string) error, context.Context) {
	t.Helper()
	p := crltest.New(t, pq)
	own := p.Issue(t, 10, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	remote := p.Issue(t, 42, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	previous := revocation.Default
	revocation.Default = revocation.New(p.Client)
	t.Cleanup(func() { revocation.Default = previous })
	_, check, err := newFloorTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(p.CA)
	// The previous production constructor supplied no path cache.
	uncached := buildPeerCertificateWindowVerifier(pool, []*x509.Certificate{p.CA}, nil, func(now, nb time.Time) (time.Time, time.Time) { return now, now }, nil, false, revocation.RevokedOnly)
	baseline := livePeerCheck(func(raw [][]byte, level revocation.Level) error { _, err := uncached(raw, time.Now()); return err })
	ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{remote.Cert}}}})
	return p, livePeerCheck(check), baseline, ctx
}

// Compare the production stream path with the former uncached verifier. This
// deterministic allocation budget catches accidentally dropping the path
// cache without relying on CPU speed or noisy wall-clock throughput limits.
func TestLiveStreamReusesCryptographicPath(t *testing.T) {
	for _, pq := range []bool{false, true} {
		t.Run(map[bool]string{false: "ECDSA", true: "MLDSA"}[pq], func(t *testing.T) {
			p, check, baseline, ctx := liveCheckFixture(t, pq)
			if err := check(ctx, ""); err != nil {
				t.Fatal(err)
			}
			revocation.Default.Refresh(context.Background())
			allocations := func(check func(context.Context, string) error) float64 {
				s := &liveServerStream{ServerStream: &boundaryStream{ctx: ctx}, check: func(ctx context.Context) error { return check(ctx, "") }}
				return testing.AllocsPerRun(100, func() {
					if err := s.SendMsg(nil); err != nil {
						t.Fatal(err)
					}
					if err := s.RecvMsg(nil); err != nil {
						t.Fatal(err)
					}
				})
			}
			cold, warm := allocations(baseline), allocations(check)
			t.Logf("send+receive allocations: uncached %.0f, production %.0f", cold, warm)
			if warm >= cold*0.75 {
				t.Fatalf("stream lost path reuse: production %.0f allocations, uncached %.0f", warm, cold)
			}
			// A staple must invalidate a warm proof even before its signature has
			// been checked by the manager. The next boundary authenticates it.
			revocation.Default.Offer(p.Sign(t, &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour), RevokedCertificateEntries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: time.Now().Add(-time.Minute)}}}))
			stream := &boundaryStream{ctx: ctx}
			s := &liveServerStream{ServerStream: stream, check: func(ctx context.Context) error { return check(ctx, "") }}
			if err := s.SendMsg(nil); status.Code(err) != codes.Unauthenticated || stream.sent != 0 {
				t.Fatalf("warm stream sent after stapled revocation: %v", err)
			}
			if err := s.RecvMsg(nil); status.Code(err) != codes.Unauthenticated {
				t.Fatalf("warm stream received after revocation: %v", err)
			}
		})
	}
}

func TestReceiveChecksOnceAfterBlockedMessage(t *testing.T) {
	calls := 0
	revoked := false
	stream := &boundaryStream{ctx: context.Background(), recv: func() error { revoked = true; return nil }}
	s := &liveServerStream{ServerStream: stream, check: func(context.Context) error {
		calls++
		if revoked {
			return revocation.ErrRevoked
		}
		return nil
	}}
	if err := s.RecvMsg(nil); !errors.Is(err, revocation.ErrRevoked) || calls != 1 {
		t.Fatalf("receive check calls=%d err=%v", calls, err)
	}
	stream.recv = func() error { return io.EOF }
	if err := s.RecvMsg(nil); err != io.EOF || calls != 1 {
		t.Fatalf("EOF check calls=%d err=%v", calls, err)
	}
}

func TestTLSFreshnessNotReusedFromWarmRevocationProof(t *testing.T) {
	p := crltest.New(t, false)
	own := p.Issue(t, 10, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	remote := p.Issue(t, 42, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/alice")
	previous := revocation.Default
	defer func() { revocation.Default = previous }()
	revocation.Default = revocation.New(p.Client)
	if err := revocation.Default.Configure(revocation.Config{Grace: 0}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Second)
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(10 * time.Second)}), 200)
	_, check, err := newTLSConfig(own.PEM, p.PEM, own.KeyPEM, nil, func(now, nb time.Time) (time.Time, time.Time) { return now, now }, func() time.Time { return now }, false)
	if err != nil {
		t.Fatal(err)
	}
	raw := [][]byte{remote.Cert.Raw}
	for _, level := range []revocation.Level{revocation.RevokedOnly, revocation.Session, revocation.Fresh} {
		if err := check(raw, level); err != nil {
			t.Fatal(err)
		}
	}
	p.Respond(nil, 503)
	now = now.Add(11 * time.Second)
	for _, level := range []revocation.Level{revocation.Session, revocation.Fresh} {
		if err := check(raw, level); !errors.Is(err, revocation.ErrUnavailable) {
			t.Fatalf("level %v reused stale freshness: %v", level, err)
		}
	}
	if err := check(raw, revocation.RevokedOnly); err != nil {
		t.Fatalf("established peer refused without revocation: %v", err)
	}
}

func BenchmarkLiveStreamBoundaries(b *testing.B) {
	for _, pq := range []bool{false, true} {
		b.Run(map[bool]string{false: "ECDSA", true: "MLDSA"}[pq], func(b *testing.B) {
			_, check, baseline, ctx := liveCheckFixture(b, pq)
			for _, mode := range []string{"previous_uncached", "production"} {
				b.Run(mode, func(b *testing.B) {
					verify := check
					if mode == "previous_uncached" {
						verify = baseline
					}
					stream := &liveServerStream{ServerStream: &boundaryStream{ctx: ctx}, check: func(ctx context.Context) error { return verify(ctx, "") }}
					if err := stream.SendMsg(nil); err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					b.SetBytes(32 << 10)
					b.ResetTimer()
					for range b.N {
						if err := stream.SendMsg(nil); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}
