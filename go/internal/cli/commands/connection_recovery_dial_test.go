package commands

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// Serve one TLS rejection per connection, with the adjacent port closed so the
// ladder encounters a transport failure after the meaningful TLS failure.
func serveRecoveryRejection(t *testing.T, reject func(net.Conn)) string {
	t.Helper()
	listener, err := net.Listen("tcp", deadAgentAddr(t))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			reject(conn)
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-done })
	return listener.Addr().String()
}

func TestConnectionRecoveryRetainsTLSRejectionAcrossPorts(t *testing.T) {
	expired := selfSignedCLICertUntil(t, 2, time.Now().Add(-time.Hour))
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{expired}}}})
	setPinCache(t)
	withTempCertOrderCache(t)
	addr := serveRecoveryRejection(t, func(conn net.Conn) {
		var header [5]byte
		if _, err := io.ReadFull(conn, header[:]); err != nil {
			return
		}
		if _, err := io.CopyN(io.Discard, conn, int64(binary.BigEndian.Uint16(header[3:]))); err != nil {
			return
		}
		// TLS fatal bad_certificate alert in response to the ClientHello.
		_, _ = conn.Write([]byte{21, 3, 3, 0, 2, 2, 42})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := dialAgentLadder(ctx, newDialTarget("recovery-test.local", addr))
	if conn != nil {
		conn.Close()
		t.Fatal("TLS rejection must not fall through to plaintext")
	}
	if !errors.Is(err, errTLSHandshakeRejected) || certificateRefreshReason(err) == "" {
		t.Fatalf("lost expired-client evidence after the adjacent port refused: %v", err)
	}
	if cause := errors.Unwrap(err); cause == nil || !strings.Contains(cause.Error(), "bad certificate") {
		t.Fatalf("TLS cause was replaced by the later transport failure: %v", cause)
	}
}

func TestConnectionRecoveryDiagnosesStoredCredentialsBeforeFiltering(t *testing.T) {
	// A real server certificate exposes org 2 before verification fails. The
	// transport should only try the valid org-76 credential, but the diagnostic
	// must still see the stored, expired org-2 credential.
	server := selfSignedCLICert(t, 2)
	keyPair, err := tls.X509KeyPair([]byte(server.PemCertificate), []byte(server.PemPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(keyPair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.Subject.CommonName = "sh/wendy/2/42"
	leaf.RawSubject = nil
	keyPair.Certificate[0], err = x509.CreateCertificate(rand.Reader, leaf, leaf, leaf.PublicKey, keyPair.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	var attempts atomic.Int32
	addr := serveRecoveryRejection(t, func(conn net.Conn) {
		attempts.Add(1)
		_ = tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{keyPair}, NextProtos: []string{"h2"}}).Handshake()
	})
	for _, tc := range []struct {
		name         string
		orgCert      config.CertificateInfo
		wantRefresh  bool
		wantMismatch bool
	}{
		{"expired target credential", selfSignedCLICertUntil(t, 2, time.Now().Add(-time.Hour)), true, false},
		{"valid target credential", selfSignedCLICert(t, 2), false, false},
		{"missing target credential", config.CertificateInfo{}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			credentials := []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 76)}}}
			if tc.orgCert.OrganizationID != 0 {
				credentials = append(credentials, config.AuthConfig{Certificates: []config.CertificateInfo{tc.orgCert}})
			}
			setTempConfig(t, &config.Config{Auth: credentials})
			setPinCache(t)
			withTempCertOrderCache(t)
			stubOrgNameResolver(t, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			before := attempts.Load()
			conn, _, err := dialAgentLadder(ctx, newDialTarget("recovery-test.local", addr))
			if conn != nil {
				conn.Close()
				t.Fatal("certificate verification failure must not fall through to plaintext")
			}
			if got := certificateRefreshReason(err) != ""; got != tc.wantRefresh {
				t.Errorf("refresh advice = %v, want %v; error: %v", got, tc.wantRefresh, err)
			}
			wantAttempts := int32(1)
			if tc.orgCert.OrganizationID != 0 && !tc.wantRefresh {
				wantAttempts = 2
			}
			if got := attempts.Load() - before; got != wantAttempts {
				t.Errorf("TLS attempts = %d, want %d; expired sessions must stay filtered", got, wantAttempts)
			}
			var mismatch orgMismatchDeviceError
			if got := errors.As(err, &mismatch); got != tc.wantMismatch {
				t.Errorf("org mismatch = %v, want %v; error: %v", got, tc.wantMismatch, err)
			}
		})
	}
}

// Use the agent's real interceptor: an unrestricted certificate can complete
// TLS, then fail the stricter clientAuth check on the version RPC.
func TestConnectionRecoveryRetainsClientAuthRejection(t *testing.T) {
	for _, extraCredential := range []bool{false, true} {
		name := "closed adjacent port"
		if extraCredential {
			name = "later certificate verification failure"
		}
		t.Run(name, func(t *testing.T) {
			credential := selfSignedCLICert(t, 2)
			auth := []config.AuthConfig{{Certificates: []config.CertificateInfo{credential}}}
			if extraCredential {
				// Another session's unrelated CA cannot verify this device. That later
				// failure must not obscure the agent's explicit clientAuth diagnosis.
				auth = append(auth, config.AuthConfig{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}})
			}
			setTempConfig(t, &config.Config{Auth: auth})
			setPinCache(t)
			withTempCertOrderCache(t)
			pair, err := tls.X509KeyPair([]byte(credential.PemCertificate), []byte(credential.PemPrivateKey))
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", deadAgentAddr(t))
			if err != nil {
				t.Fatal(err)
			}
			var rejections atomic.Int32
			srv := grpc.NewServer(
				grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAnyClientCert})),
				grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
					if err := interceptor.CheckMTLS(ctx, zap.NewNop(), certs.Scope{}, interceptor.OrgModeOff); err != nil {
						rejections.Add(1)
						return nil, err
					}
					return handler(ctx, req)
				}),
			)
			agentpb.RegisterWendyAgentServiceServer(srv, versionOnlyAgent{})
			go func() { _ = srv.Serve(listener) }()
			t.Cleanup(srv.Stop)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := connectAgentAtAddressWithProvisionedHint(ctx, listener.Addr().String(), func() bool { return true })
			if conn != nil {
				conn.Close()
				t.Fatal("clientAuth rejection returned a connection")
			}
			if rejections.Load() == 0 {
				t.Fatal("fixture did not reach the agent's clientAuth check")
			}
			if status.Code(err) != codes.Unauthenticated || certificateRefreshReason(err) == "" {
				t.Fatalf("lost explicit clientAuth rejection: %v", err)
			}
			if !strings.Contains(err.Error(), "auth refresh-certs") || strings.Contains(err.Error(), "sync-time") {
				t.Fatalf("incorrect remedy for unusable client credentials: %v", err)
			}
		})
	}
}
