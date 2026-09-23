package localmesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
)

type tlsByteConn struct {
	net.Conn
	written atomic.Int64
}

func (c *tlsByteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func TestScopedPeerTLSResumesOnlyWithinCredentialALPNAndTransport(t *testing.T) {
	a, b := testCredentials(t)
	type measuredHandshake struct {
		resumed                                                    bool
		clientHandshake, clientTotal, serverHandshake, serverTotal int64
	}
	handshake := func(client *Credentials, alpn, transport string) (measuredHandshake, error) {
		clientCfg, err := client.PeerTLSWithTickets(b.Asset, alpn, transport)
		if err != nil {
			return measuredHandshake{}, err
		}
		serverCfg, err := b.PeerTLSWithTickets(client.Asset, alpn, transport)
		if err != nil {
			return measuredHandshake{}, err
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return measuredHandshake{}, err
		}
		defer listener.Close()
		serverErr := make(chan error, 1)
		serverState := make(chan tls.ConnectionState, 1)
		type serverCount struct{ handshake, total int64 }
		serverBytes := make(chan serverCount, 1)
		go func() {
			raw, err := listener.Accept()
			if err != nil {
				serverErr <- err
				return
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
			meter := &tlsByteConn{Conn: raw}
			server := tls.Server(meter, serverCfg)
			defer server.Close()
			if err := server.Handshake(); err != nil {
				serverErr <- err
				return
			}
			serverState <- server.ConnectionState()
			handshakeBytes := meter.written.Load()
			_, err = server.Write([]byte("x"))
			serverBytes <- serverCount{handshakeBytes, meter.written.Load()}
			serverErr <- err
		}()
		raw, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			return measuredHandshake{}, err
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		meter := &tlsByteConn{Conn: raw}
		peer := tls.Client(meter, clientCfg)
		defer peer.Close()
		if err := peer.Handshake(); err != nil {
			return measuredHandshake{}, err
		}
		clientHandshake := meter.written.Load()
		var one [1]byte
		if _, err := io.ReadFull(peer, one[:]); err != nil {
			return measuredHandshake{}, err
		}
		if err := <-serverErr; err != nil {
			return measuredHandshake{}, err
		}
		if state := <-serverState; state.NegotiatedProtocol != alpn || !state.HandshakeComplete {
			t.Fatalf("server TLS state = %+v", state)
		}
		sizes := <-serverBytes
		return measuredHandshake{peer.ConnectionState().DidResume, clientHandshake, meter.written.Load(), sizes.handshake, sizes.total}, nil
	}
	cold, err := handshake(a, "wendy-mesh-test/1", "ble-tls")
	if err != nil || cold.resumed {
		t.Fatalf("first BLE handshake=%+v err=%v", cold, err)
	}
	warm, err := handshake(a, "wendy-mesh-test/1", "ble-tls")
	if err != nil || !warm.resumed {
		t.Fatalf("second BLE handshake=%+v err=%v", warm, err)
	}
	t.Logf("TLS CoC-like stream bytes cold: client handshake=%d total=%d, server handshake=%d total=%d; resumed: client handshake=%d total=%d, server handshake=%d total=%d",
		cold.clientHandshake, cold.clientTotal, cold.serverHandshake, cold.serverTotal,
		warm.clientHandshake, warm.clientTotal, warm.serverHandshake, warm.serverTotal)
	if warm.clientTotal+warm.serverTotal >= cold.clientTotal+cold.serverTotal {
		t.Fatal("resumed handshake did not save TLS record bytes")
	}
	if other, err := handshake(a, "wendy-mesh-test/1", "nan-quic"); err != nil || other.resumed {
		t.Fatalf("cross-transport ticket reused=%+v err=%v", other, err)
	}
	if other, err := handshake(a, "wendy-mesh-test/2", "ble-tls"); err != nil || other.resumed {
		t.Fatalf("cross-ALPN ticket reused=%+v err=%v", other, err)
	}
	correct, err := a.PeerTLSWithTickets(b.Asset, "wendy-mesh-test/1", "ble-tls")
	if err != nil {
		t.Fatal(err)
	}
	var restored []*x509.Certificate
	for _, der := range b.Certificate.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		restored = append(restored, cert)
	}
	resumedState := tls.ConnectionState{DidResume: true, PeerCertificates: restored}
	if err := correct.VerifyConnection(resumedState); err != nil {
		t.Fatalf("restored pinned peer rejected: %v", err)
	}
	wrong, err := a.PeerTLSWithTickets(b.Asset+1, "wendy-mesh-test/1", "ble-tls")
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.VerifyConnection(resumedState); err == nil {
		t.Fatal("resumed handshake skipped peer asset pin")
	}
	// Reconstructing Credentials (as live reprovisioning does via restart)
	// creates new in-memory caches and ticket keys even for identical PEMs.
	var cert, chain, key string
	_, _ = testCredentialsWithPEM(t, func(asset int32, c, ca, k string) {
		if asset == 445 {
			cert, chain, key = c, ca, k
		}
	})
	newClient, err := NewCredentials(64, 445, cert, chain, key)
	if err != nil {
		t.Fatal(err)
	}
	// This credential belongs to another fixture CA and cannot authenticate
	// against b, which also proves no cache entry crosses a trust rotation.
	if _, err := handshake(newClient, "wendy-mesh-test/1", "ble-tls"); err == nil {
		t.Fatal("rotated trust accepted old peer")
	}
}

func testCredentials(t testing.TB) (*Credentials, *Credentials) {
	return testCredentialsWithPEM(t, nil)
}

func testCredentialsWithPEM(t testing.TB, capture func(int32, string, string, string)) (*Credentials, *Credentials) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	leaf := func(asset int32) *Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(fmt.Sprintf("urn:wendy:org:64:asset:%d", asset))
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(asset) + 1), NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		kd, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
		if capture != nil {
			capture(asset, certPEM, chain, keyPEM)
		}
		c, err := NewCredentials(64, asset, certPEM, chain, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return leaf(445), leaf(460)
}

func TestLocalMeshQUICHandshakeAndControl(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Asset)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := quic.ListenAddr("127.0.0.1:0", serverTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, err := listener.Accept(ctx)
		if err != nil {
			result <- err
			return
		}
		defer conn.CloseWithError(0, "done")
		stream, err := OpenControl(ctx, conn, 64, b.Asset, a.Asset)
		if err != nil {
			result <- err
			return
		}
		m, err := ReadControl(stream)
		if err == nil && m.Kind != "bundle" {
			err = fmt.Errorf("unexpected message %s", m.Kind)
		}
		if err == nil {
			_, err = b.Verify(m.Bundle, time.Now())
		}
		result <- err
	}()
	clientTLS, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	stream, err := OpenControl(ctx, conn, 64, a.Asset, b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	if err = WriteControl(stream, ControlMessage{Kind: "bundle", Bundle: a.Certificate.Certificate}); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
}

func TestCredentialsRejectWrongOrigin(t *testing.T) {
	a, b := testCredentials(t)
	tlsConfig, err := a.PeerTLS(461)
	if err != nil {
		t.Fatal(err)
	}
	if err = tlsConfig.VerifyPeerCertificate(b.Certificate.Certificate, nil); err == nil {
		t.Fatal("wrong peer asset accepted")
	}
	verified, err := a.Verify(b.Certificate.Certificate, time.Now())
	if err != nil || verified.Asset != 460 {
		t.Fatal(verified, err)
	}
	if _, err = a.Verify(b.Certificate.Certificate, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired identity accepted")
	}
}

func TestPeerTLSConfigCacheKeepsPinsAndCallerSettingsIsolated(t *testing.T) {
	a, b := testCredentials(t)
	first, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.PeerTLS(b.Asset)
	if err != nil {
		t.Fatal(err)
	}
	if first == second || &first.Certificates[0].Certificate[0][0] != &second.Certificates[0].Certificate[0][0] {
		t.Fatal("per-peer config was not cloned from one parsed identity")
	}
	first.NextProtos = []string{"changed-by-caller"}
	if len(second.NextProtos) != 1 || second.NextProtos[0] != LinkALPN ||
		second.ClientSessionCache != nil || !second.SessionTicketsDisabled {
		t.Fatalf("peer config settings leaked: %+v", second)
	}
	wrong, err := a.PeerTLS(b.Asset + 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.VerifyPeerCertificate(b.Certificate.Certificate, nil); err == nil {
		t.Fatal("another peer's cached config bypassed the identity pin")
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, err := a.PeerTLS(b.Asset)
			if err != nil || cfg.VerifyPeerCertificate(b.Certificate.Certificate, nil) != nil {
				t.Errorf("concurrent cached peer config: %v", err)
			}
		}()
	}
	wg.Wait()
}

func BenchmarkPeerTLSCached(b *testing.B) {
	a, peer := testCredentials(b)
	if _, err := a.PeerTLS(peer.Asset); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := a.PeerTLS(peer.Asset); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPeerTLSUncached(b *testing.B) {
	var certPEM, chainPEM, keyPEM string
	_, _ = testCredentialsWithPEM(b, func(asset int32, cert, chain, key string) {
		if asset == 445 {
			certPEM, chainPEM, keyPEM = cert, chain, key
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, keyPEM, nil, "460"); err != nil {
			b.Fatal(err)
		}
	}
}

func TestCredentialsRejectMismatchedLocalIdentity(t *testing.T) {
	testCredentialsWithPEM(t, func(asset int32, cert, chain, key string) {
		if _, err := NewCredentials(65, asset, cert, chain, key); err == nil {
			t.Fatal("certificate from a different org accepted")
		}
		if _, err := NewCredentials(64, asset+1, cert, chain, key); err == nil {
			t.Fatal("certificate from a different asset accepted")
		}
	})
}
