package grpcclient_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// versionService records the TLS resumption state of each call's transport.
type versionService struct {
	agentpb.UnimplementedWendyAgentServiceServer
	mu      sync.Mutex
	resumed []bool
	states  []tls.ConnectionState
}

func (s *versionService) GetAgentVersion(ctx context.Context, _ *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p, _ := peer.FromContext(ctx)
	info := p.AuthInfo.(credentials.TLSInfo)
	s.mu.Lock()
	s.resumed = append(s.resumed, info.State.DidResume)
	s.states = append(s.states, info.State)
	s.mu.Unlock()
	return &agentpb.GetAgentVersionResponse{Version: "test"}, nil
}

func TestConnectWithTLSResumesAcrossConnections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_TLS_SESSION_STORE", "file")

	pki := newIntegrationPKI(t) // same generator as Task 7, PEM for client too
	srv, err := mtls.NewServer(pki.serverCertPEM, pki.caPEM, pki.serverKeyPEM,
		zap.NewNop(), time.Time{}, certs.Scope{}, interceptor.OrgModeOff)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	svc := &versionService{}
	agentpb.RegisterWendyAgentServiceServer(srv, svc)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	t.Cleanup(srv.Stop)

	certInfo := &config.CertificateInfo{
		PemCertificate:      pki.clientCertPEM,
		PemPrivateKey:       pki.clientKeyPEM,
		PemCertificateChain: pki.caPEM,
	}
	call := func() {
		conn, err := grpcclient.ConnectWithTLSAndPins(context.Background(), ln.Addr().String(), certInfo, nil)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
			t.Fatalf("GetAgentVersion: %v", err)
		}
	}

	call() // full handshake; ticket persists asynchronously afterwards

	// The Cache lives inside the connection we just closed; its async persist
	// races this second dial. Poll until resumption is observed (bounded).
	deadline := time.Now().Add(10 * time.Second)
	for {
		call()
		svc.mu.Lock()
		n := len(svc.resumed)
		resumed := svc.resumed[n-1]
		svc.mu.Unlock()
		if resumed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no connection resumed within 10s — ticket never persisted or offered")
		}
		time.Sleep(100 * time.Millisecond)
	}
	svc.mu.Lock()
	defer svc.mu.Unlock()
	if svc.resumed[0] {
		t.Error("first connection unexpectedly resumed")
	}
}

type integrationPKI struct {
	serverCertPEM, serverKeyPEM, caPEM string
	clientCertPEM, clientKeyPEM        string
}

func newIntegrationPKI(t *testing.T) integrationPKI {
	return integrationPKIWithIntermediate(t, false)
}
func integrationPKIWithIntermediate(t *testing.T, intermediate bool) integrationPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Resumption E2E CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}

	chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	if intermediate {
		issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		issuer := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "RPC shared issuer"}, NotBefore: caTmpl.NotBefore, NotAfter: caTmpl.NotAfter, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
		issuerDER, err := x509.CreateCertificate(rand.Reader, issuer, caCert, &issuerKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		caCert, err = x509.ParseCertificate(issuerDER)
		if err != nil {
			t.Fatal(err)
		}
		caKey = issuerKey
		chainPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuerDER})) + chainPEM
	}
	leaf := func(cn string, eku x509.ExtKeyUsage) (certPEM, keyPEM string) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("gen key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(time.Now().UnixNano()),
			Subject:      pkix.Name{CommonName: cn},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{eku},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create leaf: %v", err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	}

	serverCertPEM, serverKeyPEM := leaf("resumption-e2e-server", x509.ExtKeyUsageServerAuth)
	clientCertPEM, clientKeyPEM := leaf("resumption-e2e-client", x509.ExtKeyUsageClientAuth)
	return integrationPKI{
		serverCertPEM: serverCertPEM,
		serverKeyPEM:  serverKeyPEM,
		caPEM:         chainPEM,
		clientCertPEM: clientCertPEM,
		clientKeyPEM:  clientKeyPEM,
	}
}

// Exercise actual gRPC HTTP/2 RPC dispatch with binary ALPN names and a shared
// intermediate. Repeat dials exercise the per-handshake credential snapshots.
func TestNormalAgentRPCKnownIssuers(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pki := integrationPKIWithIntermediate(t, true)
	server, err := mtls.NewServer(pki.serverCertPEM, pki.caPEM, pki.serverKeyPEM, zap.NewNop(), time.Time{}, certs.Scope{}, interceptor.OrgModeOff)
	if err != nil {
		t.Fatal(err)
	}
	service := &versionService{}
	agentpb.RegisterWendyAgentServiceServer(server, service)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	identity := &config.CertificateInfo{PemCertificate: pki.clientCertPEM, PemPrivateKey: pki.clientKeyPEM, PemCertificateChain: pki.caPEM}
	for range 2 {
		conn, err := grpcclient.ConnectWithTLSAndPins(context.Background(), listener.Addr().String(), identity, nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		cancel()
		conn.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if len(service.states) != 2 {
		t.Fatalf("RPCs=%d", len(service.states))
	}
	for _, state := range service.states {
		app, hashes := meshalpn.Decode(state.NegotiatedProtocol)
		if app != "h2" || len(hashes) != 2 {
			t.Fatalf("application=%q CA hashes=%d", app, len(hashes))
		}
		if !state.DidResume && len(state.PeerCertificates) != 1 {
			t.Fatalf("received %d certificates", len(state.PeerCertificates))
		}
	}
}
