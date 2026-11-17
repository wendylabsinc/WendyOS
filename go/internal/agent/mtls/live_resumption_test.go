package mtls

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Exercise the actual gRPC server wiring. An operator issuer first learned
// from the peer is then omitted under ALPN hints and restored from a TLS
// ticket. Unary and streaming RPCs must still validate that exact path.
func TestLiveRPCAfterHintedAndResumedTLS(t *testing.T) {
	for _, pq := range []bool{false, true} {
		t.Run(map[bool]string{false: "ECDSA", true: "MLDSA"}[pq], func(t *testing.T) {
			previous := revocation.Default
			revocation.Default = revocation.New(&http.Client{Transport: failingTransport{}})
			defer func() { revocation.Default = previous }()
			root := crltest.New(t, pq)
			issuer := crltest.New(t, pq)
			tmpl := *issuer.CA
			tmpl.Subject.CommonName = "operator issuer"
			der, err := x509.CreateCertificate(rand.Reader, &tmpl, root.CA, issuer.Key.Public(), root.Key)
			if err != nil {
				t.Fatal(err)
			}
			issuer.CA, err = x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			issuer.PEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			const tenant = "11111111-1111-4111-8111-111111111111"
			own := root.Issue(t, 10, "spiffe://wendy.sh/tenant/"+tenant+"/device/22222222-2222-4222-8222-222222222222")
			operator := issuer.Issue(t, 42, "spiffe://wendy.sh/tenant/"+tenant+"/operator/alice")
			cfg, check, err := newFloorTLSConfig(own.PEM, root.PEM, own.KeyPEM, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			states := make(chan tls.ConnectionState, 8)
			srv, err := newServerWithTLSConfig(cfg, check, root.PEM, nil, certs.Scope{TenantUUID: tenant}, interceptor.OrgModeStrict, grpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
				p, _ := peer.FromContext(ctx)
				states <- p.AuthInfo.(credentials.TLSInfo).State
				return next(ctx, req)
			}))
			if err != nil {
				t.Fatal(err)
			}
			healthService := health.NewServer()
			healthpb.RegisterHealthServer(srv, healthService)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			go srv.Serve(listener)
			defer srv.Stop()
			pair, err := certs.TLSKeyPair(operator.PEM, issuer.PEM+root.PEM, operator.KeyPEM)
			if err != nil {
				t.Fatal(err)
			}
			material := meshalpn.NewCertificates(pair, []*x509.Certificate{root.CA})
			cache := tls.NewLRUClientSessionCache(4)
			for round := 0; round < 3; round++ {
				if round == 1 {
					cache = tls.NewLRUClientSessionCache(4)
				} // Force a second, hinted full handshake.
				roots := x509.NewCertPool()
				roots.AddCert(root.CA)
				client := &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13, ClientSessionCache: cache, ServerName: "device", InsecureSkipVerify: true, VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
					leaf, err := x509.ParseCertificate(raw[0])
					if err != nil {
						return err
					}
					_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
					return err
				}}
				material.ConfigureClient(client, "h2")
				sent := 0
				selectCert := client.GetClientCertificate
				client.GetClientCertificate = func(info *tls.CertificateRequestInfo) (*tls.Certificate, error) {
					c, err := selectCert(info)
					if c != nil {
						sent = len(c.Certificate)
					}
					return c, err
				}
				conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(credentials.NewTLS(client)))
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				service := healthpb.NewHealthClient(conn)
				_, err = service.Check(ctx, &healthpb.HealthCheckRequest{})
				if err != nil {
					cancel()
					conn.Close()
					t.Fatalf("round %d RPC: %v", round, err)
				}
				state := <-states
				if state.DidResume != (round == 2) {
					t.Fatalf("round %d resumed=%v", round, state.DidResume)
				}
				if round == 2 && sent != 0 {
					t.Fatalf("resumed TLS unexpectedly sent a certificate/staple: %d", sent)
				}
				if round == 1 && sent != 1 {
					t.Fatalf("hinted handshake sent %d certificates, want leaf only", sent)
				}
				if len(state.PeerCertificates) != 2 {
					t.Fatalf("round %d RPC lost operator issuer: %d certificates", round, len(state.PeerCertificates))
				}
				watch, err := service.Watch(ctx, &healthpb.HealthCheckRequest{})
				if err == nil {
					_, err = watch.Recv()
				}
				if err != nil {
					t.Fatalf("round %d streaming RPC: %v", round, err)
				}
				if round == 2 {
					// A fresh staple must revoke the already-open, resumed stream,
					// even though the cached TLS presentation omitted its issuer.
					now := time.Now()
					revocation.Default.Offer(issuer.Sign(t, &x509.RevocationList{
						Number: big.NewInt(2), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour),
						RevokedCertificateEntries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now.Add(-time.Minute)}},
					}))
					healthService.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
					// The standard health service maps a failed Send to Canceled.
					if _, err := watch.Recv(); status.Code(err) != codes.Unauthenticated && status.Code(err) != codes.Canceled {
						t.Fatalf("resumed stream survived revocation: %v", err)
					}
					if _, err := service.Check(ctx, &healthpb.HealthCheckRequest{}); status.Code(err) != codes.Unauthenticated {
						t.Fatalf("resumed connection admitted RPC after revocation: %v", err)
					}
				}
				cancel()
				conn.Close()
			}
		})
	}
}
