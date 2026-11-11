package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"go.uber.org/zap"
)

type graceApplicationAdmission struct{ address string }

func (a graceApplicationAdmission) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if port != 8080 {
		return nil, meshsession.ErrDenied
	}
	return dial(a.address)
}

// App identity retirement precedes the provisioning RPC's response grace: its
// parent control context can remain alive without admitting old app traffic.
func TestCloudApplicationRetirementClosesTrafficDuringControlGrace(t *testing.T) {
	source, target := applicationGraceCredentials(t)
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		conn, err := app.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := udpmessage.NewManager()
	defer manager.Close()
	var workers sync.WaitGroup
	launch := func(run func()) { workers.Add(1); go func() { defer workers.Done(); run() }() }
	listener, retire := startCloudApplicationEndpoint(ctx, target, graceApplicationAdmission{app.Addr().String()}, manager, zap.NewNop(), launch)
	defer retire()
	address := netip.AddrPortFrom(target.Device.MeshAddress(), meshsession.TLSPort)
	setup, stop := context.WithCancel(ctx)
	conn, err := meshsession.DialTLSWithDialer(setup, source, target.Device, address, 8080, func(ctx context.Context, _, _ string) (net.Conn, error) { return listener.Dial(ctx) })
	stop()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, 5)
	if _, err := io.ReadFull(conn, reply); err != nil || string(reply) != "ready" {
		t.Fatalf("app echo=%q: %v", reply, err)
	}
	// No caller is reading now. Retirement must abort the raw transport rather
	// than wait for a graceful TLS close_notify to cross the in-memory pipe.
	retired := make(chan struct{})
	go func() { retire(); close(retired) }()
	select {
	case <-retired:
	case <-time.After(time.Second):
		t.Fatal("application retirement blocked on an unread peer")
	}
	if ctx.Err() != nil {
		t.Fatal("retirement canceled the control-plane response grace")
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired endpoint workers did not drain without a peer read")
	}
	if _, err := conn.Read(reply); err == nil {
		t.Fatal("old app transport survived retirement")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("retirement left old app transport blocked")
	}
	if raw, err := listener.Dial(ctx); err == nil {
		raw.Close()
		t.Fatal("retired application callback still admitted transports")
	}
}

func applicationGraceCredentials(t *testing.T) (*localmesh.Credentials, *localmesh.Credentials) {
	t.Helper()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	issue := func(serial int64, id string) *localmesh.Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		principal, err := url.Parse(certs.DeviceSPIFFEURI("11111111-1111-4111-8111-111111111111", id))
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, URIs: []*url.URL{principal}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		credential, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})))
		if err != nil {
			t.Fatal(err)
		}
		return credential
	}
	return issue(2, "22222222-2222-4222-8222-222222222222"), issue(3, "33333333-3333-4333-8333-333333333333")
}
