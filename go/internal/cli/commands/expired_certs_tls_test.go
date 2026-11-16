package commands

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Observe the actual client certificates on the fallback TLS endpoint. A
// selector-only test cannot detect a caller accidentally bypassing the filter.
func TestExpiredCertsOnFallbackTLS(t *testing.T) {
	for _, onlyExpired := range []bool{false, true} {
		name := "valid alternative excludes expired session"
		if onlyExpired {
			name = "all expired retains diagnostic handshake"
		}
		t.Run(name, func(t *testing.T) {
			valid := selfSignedCLICert(t, 2)
			expired := selfSignedCLICertUntil(t, 2, time.Now().Add(-time.Hour))
			// Both stored sessions trust the same device CA, so any refusal here is
			// about the client credential, not an unrelated server trust failure.
			expired.PemCertificateChain = valid.PemCertificateChain
			pair, err := tls.X509KeyPair([]byte(valid.PemCertificate), []byte(valid.PemPrivateKey))
			if err != nil {
				t.Fatal(err)
			}
			ca, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}
			serverLeaf := *ca
			serverLeaf.Subject.CommonName = "sh/wendy/2/42"
			serverLeaf.RawSubject = nil
			pair.Certificate[0], err = x509.CreateCertificate(rand.Reader, &serverLeaf, ca, ca.PublicKey, pair.PrivateKey)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", deadAgentAddr(t))
			if err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			var presented [][]byte
			server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
				Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAnyClientCert, SessionTicketsDisabled: true,
				VerifyConnection: func(state tls.ConnectionState) error {
					leaf := state.PeerCertificates[0]
					mu.Lock()
					presented = append(presented, append([]byte(nil), leaf.Raw...))
					mu.Unlock()
					if time.Now().After(leaf.NotAfter) {
						return errors.New("expired session certificate")
					}
					return nil
				},
			})))
			agentpb.RegisterWendyAgentServiceServer(server, versionOnlyAgent{})
			done := make(chan struct{})
			go func() { defer close(done); _ = server.Serve(listener) }()
			t.Cleanup(func() { server.Stop(); <-done })
			stored := []config.CertificateInfo{expired}
			want := expired
			if !onlyExpired {
				stored = append(stored, valid)
				want = valid
			}
			cfg := &config.Config{Auth: []config.AuthConfig{{Certificates: stored}}}
			cfg.SetDevicePin("cert-fallback.local", 2, "cloud.test:443", "42", "")
			setTempConfig(t, cfg)
			setPinCache(t)
			withTempCertOrderCache(t)
			primary := deadAgentAddr(t)
			target := newDialTarget("cert-fallback.local", primary)
			target.Candidates = []string{primary, listener.Addr().String()}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, _, err := dialAgentLadderWithCerts(ctx, target, stored)
			if conn != nil {
				defer conn.Close()
			}
			if onlyExpired {
				if conn != nil || err == nil {
					t.Fatalf("expired credentials returned connection=%v error=%v", conn, err)
				}
			} else if conn == nil || err != nil {
				t.Fatalf("valid fallback connection=%v error=%v", conn, err)
			}
			expected, err := tls.X509KeyPair([]byte(want.PemCertificate), []byte(want.PemPrivateKey))
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(presented) == 0 {
				t.Fatal("fallback endpoint never received a client certificate")
			}
			for _, raw := range presented {
				if !bytes.Equal(raw, expected.Certificate[0]) {
					t.Fatal("fallback endpoint received the wrong stored credential")
				}
			}
		})
	}
}
