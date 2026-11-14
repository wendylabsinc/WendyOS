package mtls

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const certRejectedMessage = "mTLS client certificate rejected"

// remoteLogServer is NewServer's gRPC server on loopback TCP, with everything
// it logs at WARN and above captured.
type remoteLogServer struct {
	addr           string
	logs           *observer.ObservedLogs
	valid, expired tls.Certificate
}

func startRemoteLogServer(t *testing.T) *remoteLogServer {
	t.Helper()
	now := time.Now()
	root, rootKey := expiryTestCertificate(t, "remote log root", now, now.Add(24*time.Hour), nil, nil, true)
	leaf := func(name string, expiry time.Time) (*x509.Certificate, *ecdsa.PrivateKey) {
		return expiryTestCertificate(t, name, now, expiry, root, rootKey, false)
	}
	serverCert, serverKey := leaf("remote log server", now.Add(time.Hour))
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatal(err)
	}
	core, logs := observer.New(zap.WarnLevel)
	server, err := NewServer(
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCert.Raw})),
		string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})),
		zap.New(core), time.Time{}, certs.Scope{}, interceptor.OrgModeOff)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)

	fixture := &remoteLogServer{addr: listener.Addr().String(), logs: logs}
	for _, client := range []struct {
		into   *tls.Certificate
		name   string
		expiry time.Time
	}{
		{&fixture.valid, "valid client", now.Add(time.Hour)},
		{&fixture.expired, "expired client", now.Add(-time.Hour)},
	} {
		cert, key := leaf(client.name, client.expiry)
		*client.into = tls.Certificate{Certificate: [][]byte{cert.Raw}, PrivateKey: key}
	}
	return fixture
}

// handshake connects presenting cert and waits for the server's verdict: its
// first HTTP/2 bytes, or the alert that answers a rejected certificate. A TLS
// 1.3 client finishes its own handshake before the server has looked at the
// client certificate, so only the read reports a rejection. local is the
// client's end of the connection, which is the address the server sees.
func (s *remoteLogServer) handshake(t *testing.T, cert tls.Certificate, cache tls.ClientSessionCache) (resumed bool, local string, err error) {
	t.Helper()
	raw, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(raw, &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, // the test root names no host; the server's checks are under test
		NextProtos:         []string{"h2"},
		ClientSessionCache: cache,
	})
	if err = conn.Handshake(); err == nil {
		_, err = conn.Read(make([]byte, 1))
	}
	return conn.ConnectionState().DidResume, raw.LocalAddr().String(), err
}

// rejection returns the fields of the server's only rejection log line.
func (s *remoteLogServer) rejection(t *testing.T) map[string]any {
	t.Helper()
	entries := s.logs.FilterMessage(certRejectedMessage).All()
	if len(entries) != 1 {
		t.Fatalf("rejection log lines = %d, want 1: %v", len(entries), s.logs.All())
	}
	return entries[0].ContextMap()
}

// An operator reading the journal has to be able to tell which machine holds
// the certificate the agent turned away, so the rejection line itself names
// the address that presented it.
func TestServerRejectionLogNamesRemoteAddress(t *testing.T) {
	server := startRemoteLogServer(t)
	_, local, err := server.handshake(t, server.expired, nil)
	if err == nil {
		t.Fatal("server accepted an expired client certificate")
	}
	fields := server.rejection(t)
	if fields["remote"] != local {
		t.Errorf("remote = %v, want the client's address %s; fields: %v", fields["remote"], local, fields)
	}
	// The address joins the existing line, not a second one the operator
	// would have to match up with the reason.
	if fields["subject"] != "expired client" {
		t.Errorf("subject = %v, want the rejected certificate's", fields["subject"])
	}
	if reason, _ := fields["error"].(string); !strings.Contains(reason, "certificate expired") {
		t.Errorf("error = %q, want the expiry reason", reason)
	}
}

// The address reaches the log through a per-connection copy of the server's
// TLS config. A ticket issued through one copy must still resume through the
// next, or every reconnect would pay for a full ML-DSA verification.
func TestServerResumesSessionsWhileNamingRemoteAddress(t *testing.T) {
	server := startRemoteLogServer(t)
	cache := tls.NewLRUClientSessionCache(4)
	for attempt, want := range []bool{false, true, true} {
		resumed, _, err := server.handshake(t, server.valid, cache)
		if err != nil {
			t.Fatalf("connection %d: %v", attempt+1, err)
		}
		if resumed != want {
			t.Fatalf("connection %d resumed = %v, want %v", attempt+1, resumed, want)
		}
	}
	if rejected := server.logs.FilterMessage(certRejectedMessage).Len(); rejected != 0 {
		t.Fatalf("valid client logged %d rejections: %v", rejected, server.logs.All())
	}

	// Same server, so the resumptions above went through the copies that
	// carry the address.
	_, local, err := server.handshake(t, server.expired, nil)
	if err == nil {
		t.Fatal("server accepted an expired client certificate")
	}
	if remote := server.rejection(t)["remote"]; remote != local {
		t.Errorf("remote = %v after resumptions, want the client's address %s", remote, local)
	}
}

// A verifier used without a connection has no address to report. The line
// must leave the field out, not fill it with a placeholder.
func TestRejectionLogOmitsRemoteWithoutConnection(t *testing.T) {
	now := time.Now()
	root, rootKey := expiryTestCertificate(t, "no connection root", now, now.Add(24*time.Hour), nil, nil, true)
	expired, _ := expiryTestCertificate(t, "expired peer", now, now.Add(-time.Hour), root, rootKey, false)
	core, logs := observer.New(zap.WarnLevel)
	verify, err := NewPeerCertificateVerifier(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})), zap.New(core), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verify([][]byte{expired.Raw}, now); err == nil {
		t.Fatal("verifier accepted an expired certificate")
	}
	entries := logs.FilterMessage(certRejectedMessage).All()
	if len(entries) != 1 {
		t.Fatalf("rejection log lines = %d, want 1: %v", len(entries), logs.All())
	}
	if remote, present := entries[0].ContextMap()["remote"]; present {
		t.Errorf("remote = %v on a rejection with no connection, want the field absent", remote)
	}
}

// Callers add their own checks by wrapping VerifyPeerCertificate on the config
// the exported constructors return. Those configs must therefore serve with
// the verifier the caller left on them, never with a per-connection
// replacement that knows nothing of the wrapper.
func TestNewTLSConfigServesCallerWrappedVerifier(t *testing.T) {
	pki := newResumptionPKI(t)
	cfg, err := NewTLSConfig(pki.serverCertPEM, pki.caPEM, pki.serverKeyPEM, zap.NewNop(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused by the caller's check")
	inner := cfg.VerifyPeerCertificate
	cfg.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
		if err := inner(raw, chains); err != nil {
			return err
		}
		return refused
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	go func() {
		conn := tls.Client(client, &tls.Config{Certificates: []tls.Certificate{pki.clientCert}, InsecureSkipVerify: true})
		_ = conn.Handshake()
		_, _ = conn.Read(make([]byte, 1))
	}()
	if err := tls.Server(server, cfg).Handshake(); !errors.Is(err, refused) {
		t.Fatalf("handshake error = %v, want the caller's check to have run", err)
	}
}
