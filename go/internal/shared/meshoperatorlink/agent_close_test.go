package meshoperatorlink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

// A temporarily stalled transport remains blocked until Close, just like an
// ordinary socket with a full send buffer. Reads remain independent.
type pausedAgentConn struct {
	net.Conn
	mu                   sync.Mutex
	paused               bool
	entered              chan struct{}
	closed               chan struct{}
	deadlines            chan time.Time
	enterOnce, closeOnce sync.Once
}

func (c *pausedAgentConn) Write(data []byte) (int, error) {
	c.mu.Lock()
	paused := c.paused
	c.mu.Unlock()
	if paused {
		c.enterOnce.Do(func() { close(c.entered) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(data)
}
func (c *pausedAgentConn) SetWriteDeadline(deadline time.Time) error {
	select {
	case c.deadlines <- deadline:
	default:
	}
	return c.Conn.SetWriteDeadline(deadline)
}
func (c *pausedAgentConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}
func TestAgentCloseUnblocksTransportWrite(t *testing.T) {
	for _, mode := range []string{"local", "remote", "attachment"} {
		t.Run(mode, func(t *testing.T) { testAgentCloseUnblocksTransportWrite(t, mode) })
	}
}
func testAgentCloseUnblocksTransportWrite(t *testing.T, mode string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: cert}
	clientRaw, serverRaw := net.Pipe()
	paused := &pausedAgentConn{Conn: clientRaw, entered: make(chan struct{}), closed: make(chan struct{}), deadlines: make(chan time.Time, 4)}
	client := tls.Client(paused, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "localhost", Certificates: []tls.Certificate{certificate}})
	server := tls.Server(serverRaw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	defer paused.Close()
	defer serverRaw.Close()
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.HandshakeContext(ctx) }()
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	l := &Link{conn: client, expires: now.Add(time.Hour), done: make(chan struct{}), outControl: make(chan control, 4), agentWake: make(chan struct{}, 1)}
	c := newAgentStream(l, 1, agentWindow)
	l.agent = c
	paused.mu.Lock()
	paused.paused = true
	paused.mu.Unlock()
	writeDone := make(chan error, 1)
	go func() { _, err := c.Write([]byte("normal pending management write")); writeDone <- err }()
	select {
	case <-paused.entered:
		select {
		case deadline := <-paused.deadlines:
			if remaining := time.Until(deadline); remaining <= 0 || remaining > 45*time.Second {
				t.Fatalf("shared frame inherited credential expiry instead of finite write budget: %s", remaining)
			}
		default:
			t.Fatal("pending physical frame has no bounded write deadline")
		}
	case <-ctx.Done():
		t.Fatal("management write did not reach transport")
	}
	closeDone := make(chan error, 1)
	go func() {
		switch mode {
		case "local":
			closeDone <- c.Close()
		case "remote":
			closeDone <- c.close(false)
		case "attachment":
			closeDone <- l.Close()
		}
	}()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("management close did not retire its owner")
	}
	select {
	case err := <-writeDone:
		if err == nil {
			t.Fatal("closed stream write succeeded")
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Log("closed pending TLS write:", err)
		}
	case <-time.After(500 * time.Millisecond):
		l.Close()
		<-writeDone
		t.Fatal("stream Close left a management writer blocked in the shared transport")
	}
}

// Releasing a completed management session must preserve the router lifetime.
func TestIdleAgentCloseKeepsRouterLive(t *testing.T) {
	l := &Link{expires: time.Now().Add(time.Hour), done: make(chan struct{}), outControl: make(chan control, 4), agentWake: make(chan struct{}, 1)}
	c := newAgentStream(l, 1, agentWindow)
	l.agent = c
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !l.Live() {
		t.Fatal("idle management close retired the routing carrier")
	}
	if _, err := c.Write([]byte("finished")); err == nil {
		t.Fatal("closed management stream accepted more output")
	}
	if n, err := c.Read(make([]byte, 1)); n != 0 || err == nil {
		t.Fatal("closed management stream did not finish", n, err)
	}
}
