package localmesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Test identities are real device principals: a tenant UUID and a device UUID,
// the same two values the mesh address and link ordering are derived from.
// Device A is the lower UUID, device B the higher.
const (
	testTenantUUID  = "0b8f3a67-2c1d-4e5f-9a8b-0c1d2e3f4051"
	testOtherTenant = "1c9a4b78-3d2e-4f60-9b8c-1d2e3f405162"
	testDeviceAID   = "2b1c0000-0000-4000-8000-000000000445"
	testDeviceBID   = "2b1c0000-0000-4000-8000-000000000460"
	testDeviceCID   = "2b1c0000-0000-4000-8000-000000000470"
)

func testPrincipal(tenant, device string) string {
	return certs.DeviceSPIFFEURI(tenant, device)
}

func testDevice(t testing.TB, principal string) meshid.Device {
	t.Helper()
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	return device
}

func testDevices(t testing.TB) (meshid.Device, meshid.Device) {
	t.Helper()
	return testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)),
		testDevice(t, testPrincipal(testTenantUUID, testDeviceBID))
}

func testTenant(t testing.TB) meshid.Tenant {
	t.Helper()
	tenant, err := meshid.ParseTenant(testTenantUUID)
	if err != nil {
		t.Fatal(err)
	}
	return tenant
}

// testPKI is one shared CA so a cross-tenant certificate in a test is rejected
// for its principal, never because it happened to be signed by another key.
type testCA struct {
	certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	chainPEM    string
}

var testPKI = sync.OnceValue(func() testCA {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "wendy net test CA"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		panic(err)
	}
	return testCA{ca, caKey, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
})

// testIssue mints a leaf for one principal under the shared test CA.
func testIssue(t testing.TB, principal string) (certPEM, keyPEM string) {
	t.Helper()
	pki := testPKI()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, pki.certificate, &key.PublicKey, pki.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func testCredentials(t testing.TB) (*Credentials, *Credentials) {
	return testCredentialsWithPEM(t, nil)
}

func testCredentialsWithPEM(t testing.TB, capture func(meshid.Device, string, string, string)) (*Credentials, *Credentials) {
	t.Helper()
	chainPEM := testPKI().chainPEM
	issue := func(principal string) *Credentials {
		certPEM, keyPEM := testIssue(t, principal)
		if capture != nil {
			capture(testDevice(t, principal), certPEM, chainPEM, keyPEM)
		}
		credentials, err := NewCredentials(certPEM, chainPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return credentials
	}
	return issue(testPrincipal(testTenantUUID, testDeviceAID)), issue(testPrincipal(testTenantUUID, testDeviceBID))
}

// testCredentialsFor builds credentials for an arbitrary principal under the
// same CA - a different device, or the same device UUID in another tenant.
func testCredentialsFor(t testing.TB, principal string) *Credentials {
	t.Helper()
	chainPEM := testPKI().chainPEM
	certPEM, keyPEM := testIssue(t, principal)
	credentials, err := NewCredentials(certPEM, chainPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

// testForeignCredentials mints credentials whose chain is signed by a CA this
// mesh does not trust, so a rejected handshake can only be about trust.
func testForeignCredentials(t testing.TB, principal string) *Credentials {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "untrusted wendy net CA"}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	leaf, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf})), chainPEM, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func TestLocalMeshQUICHandshakeAndControl(t *testing.T) {
	a, b := testCredentials(t)
	serverTLS, err := b.PeerTLS(a.Device)
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
		stream, err := OpenControl(ctx, conn, b.Device, a.Device)
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
	clientTLS, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := quic.DialAddr(ctx, listener.Addr().String(), clientTLS, QUICConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "done")
	stream, err := OpenControl(ctx, conn, a.Device, b.Device)
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

// TestCredentialsPinPeerPrincipal is the anti-spoofing contract: a dial pinned
// to one device principal accepts that device and nothing else, and the same
// device UUID under a different tenant is a different device.
func TestCredentialsPinPeerPrincipal(t *testing.T) {
	a, b := testCredentials(t)
	sameUUIDOtherTenant := testCredentialsFor(t, testPrincipal(testOtherTenant, testDeviceBID))
	pinned, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	if err = pinned.VerifyPeerCertificate(b.Certificate.Certificate, nil); err != nil {
		t.Fatalf("pinned peer rejected: %v", err)
	}
	if err = pinned.VerifyPeerCertificate(sameUUIDOtherTenant.Certificate.Certificate, nil); err == nil {
		t.Fatal("same device UUID in another tenant satisfied the pin")
	}
	if err = pinned.VerifyPeerCertificate(a.Certificate.Certificate, nil); err == nil {
		t.Fatal("own certificate satisfied the peer pin")
	}
	verified, err := a.Verify(b.Certificate.Certificate, time.Now())
	if err != nil || verified.Device.Principal() != b.Device.Principal() {
		t.Fatal(verified, err)
	}
	if _, err = a.Verify(sameUUIDOtherTenant.Certificate.Certificate, time.Now()); err == nil {
		t.Fatal("peer outside the tenant accepted")
	}
	if _, err = a.Verify(b.Certificate.Certificate, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired identity accepted")
	}
}

// TestCredentialsRejectNonDeviceIdentity is the no-fallback contract: an
// identity that is not a device principal - a legacy numeric URN, a user, or a
// device name that is not a UUID - never becomes a mesh identity.
func TestCredentialsRejectNonDeviceIdentity(t *testing.T) {
	chainPEM := testPKI().chainPEM
	for _, principal := range []string{
		"urn:wendy:org:64:asset:445",
		testPrincipal(testTenantUUID, "service/user-8f2c1d34-5e6f-4a7b-9c8d-1e2f30415263"),
		testPrincipal(testTenantUUID, "rack-7/orin"),
		testPrincipal(testTenantUUID, "2B1C0000-0000-4000-8000-000000000445"),
	} {
		certPEM, keyPEM := testIssue(t, principal)
		if _, err := NewCredentials(certPEM, chainPEM, keyPEM); err == nil {
			t.Fatalf("principal %s accepted as a wendy net identity", principal)
		}
	}
}

func TestCredentialsRejectPeerOutsideTenant(t *testing.T) {
	a, _ := testCredentials(t)
	other := testDevice(t, testPrincipal(testOtherTenant, testDeviceAID))
	if _, err := a.PeerTLS(other); err == nil {
		t.Fatal("peer outside the tenant got a TLS config")
	}
	if _, err := a.PeerTLS(a.Device); err == nil {
		t.Fatal("self link accepted")
	}
}

type tlsByteConn struct {
	net.Conn
	written atomic.Int64
}

type countedPacketConn struct {
	net.PacketConn
	written atomic.Int64
}

func (c *countedPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(p, addr)
	c.written.Add(int64(n))
	return n, err
}

// Two fresh UDP transports model a NAN-to-LAN carrier change: the QUIC
// connection and socket are replaced, but the authenticated peer and TLS
// security context are identical. The ticket must survive that change without
// allowing a different peer or protocol to use it.
func TestLinkQUICResumesAcrossCarriers(t *testing.T) {
	a, b := testCredentials(t)
	type result struct {
		resumed                  bool
		clientBytes, serverBytes int64
	}
	handshake := func(carrier, alpn, scope string) (result, error) {
		clientTLS, err := a.PeerTLSWithTickets(b.Device, alpn, scope)
		if err != nil {
			return result{}, err
		}
		serverTLS, err := b.PeerTLSWithTickets(a.Device, alpn, scope)
		if err != nil {
			return result{}, err
		}
		var serverSocket, clientSocket net.PacketConn
		if carrier == "lan" {
			// The configured and discovered LAN paths frame QUIC packets on
			// a TCP socket. QUIC's TLS session state must also work here.
			tcpListener, listenErr := net.Listen("tcp4", "127.0.0.1:0")
			if listenErr != nil {
				return result{}, listenErr
			}
			clientTCP, dialErr := net.Dial("tcp4", tcpListener.Addr().String())
			if dialErr != nil {
				tcpListener.Close()
				return result{}, dialErr
			}
			serverTCP, acceptErr := tcpListener.Accept()
			tcpListener.Close()
			if acceptErr != nil {
				clientTCP.Close()
				return result{}, acceptErr
			}
			serverSocket, err = NewTCPPacketConn(serverTCP)
			if err == nil {
				clientSocket, err = NewTCPPacketConn(clientTCP)
			}
			if err != nil {
				serverTCP.Close()
				clientTCP.Close()
				return result{}, err
			}
		} else {
			serverSocket, err = net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return result{}, err
			}
			clientSocket, err = net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				serverSocket.Close()
				return result{}, err
			}
		}
		defer serverSocket.Close()
		defer clientSocket.Close()
		serverMeter := &countedPacketConn{PacketConn: serverSocket}
		clientMeter := &countedPacketConn{PacketConn: clientSocket}
		serverTransport := &quic.Transport{Conn: serverMeter}
		defer serverTransport.Close()
		clientTransport := &quic.Transport{Conn: clientMeter}
		defer clientTransport.Close()
		listener, err := serverTransport.Listen(serverTLS, QUICConfig())
		if err != nil {
			return result{}, err
		}
		defer listener.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		serverDone := make(chan error, 1)
		serverStopped := make(chan struct{})
		go func() {
			defer close(serverStopped)
			conn, err := listener.Accept(ctx)
			if err != nil {
				serverDone <- err
				return
			}
			defer conn.CloseWithError(0, "done")
			stream, err := conn.AcceptStream(ctx)
			if err == nil {
				var one [1]byte
				_, err = io.ReadFull(stream, one[:])
			}
			serverDone <- err
			// Reading the application byte does not prove the client has
			// processed its post-handshake session ticket. Keep the server
			// alive until the caller has observed the ticket or failed.
			<-ctx.Done()
		}()
		defer func() {
			cancel()
			<-serverStopped
		}()
		conn, err := clientTransport.Dial(ctx, listener.Addr(), clientTLS, QUICConfig())
		if err != nil {
			return result{}, err
		}
		defer conn.CloseWithError(0, "done")
		state := conn.ConnectionState()
		if ApplicationProtocol(state.TLS.NegotiatedProtocol) != alpn {
			return result{}, fmt.Errorf("unexpected QUIC ALPN %q", state.TLS.NegotiatedProtocol)
		}
		stream, err := conn.OpenStreamSync(ctx)
		if err == nil {
			_, err = stream.Write([]byte{1})
		}
		if err != nil {
			return result{}, err
		}
		select {
		case err = <-serverDone:
			if err != nil {
				return result{}, err
			}
		case <-ctx.Done():
			return result{}, ctx.Err()
		}
		// QUIC sends the TLS 1.3 ticket after the handshake. Wait until the
		// client processes it before tearing down this fixture socket.
		ticketPoll := time.NewTicker(10 * time.Millisecond)
		defer ticketPoll.Stop()
		for {
			if _, ok := clientTLS.ClientSessionCache.Get(clientTLS.ServerName); ok {
				break
			}
			select {
			case <-ctx.Done():
				return result{}, fmt.Errorf("QUIC session ticket was not received: %w", ctx.Err())
			case <-ticketPoll.C:
			}
		}
		return result{state.TLS.DidResume, clientMeter.written.Load(), serverMeter.written.Load()}, nil
	}
	cold, err := handshake("nan", LinkALPN, LinkQUICSessionScope)
	if err != nil || cold.resumed {
		t.Fatalf("cold NAN QUIC: %+v %v", cold, err)
	}
	warm, err := handshake("lan", LinkALPN, LinkQUICSessionScope)
	if err != nil || !warm.resumed {
		t.Fatalf("LAN QUIC after NAN: %+v %v", warm, err)
	}
	t.Logf("QUIC NAN-to-LAN packet payload bytes: cold client=%d server=%d total=%d; resumed client=%d server=%d total=%d",
		cold.clientBytes, cold.serverBytes, cold.clientBytes+cold.serverBytes,
		warm.clientBytes, warm.serverBytes, warm.clientBytes+warm.serverBytes)
	// Packet totals include timing-dependent retransmissions. DidResume above
	// proves resumption; retain byte counts as a diagnostic, not a pass condition.
	separate, err := handshake("nan", "wendy-app-mesh/1", "app-quic")
	if err != nil || separate.resumed {
		t.Fatalf("app QUIC resumed wendy net link ticket: %+v %v", separate, err)
	}
	separate, err = handshake("nan", LinkALPN, "ble-tls")
	if err != nil || separate.resumed {
		t.Fatalf("BLE TLS scope resumed wendy net link ticket: %+v %v", separate, err)
	}
	var peerCerts []*x509.Certificate
	for _, der := range b.Certificate.Certificate {
		cert, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		peerCerts = append(peerCerts, cert)
	}
	wrongAsset, err := a.PeerTLSWithTickets(testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)), LinkALPN, LinkQUICSessionScope)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrongAsset.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: peerCerts}); err == nil {
		t.Fatal("resumed cross-carrier QUIC bypassed peer asset pin")
	}
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
		clientCfg, err := client.PeerTLSWithTickets(b.Device, alpn, transport)
		if err != nil {
			return measuredHandshake{}, err
		}
		serverCfg, err := b.PeerTLSWithTickets(client.Device, alpn, transport)
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
		if state := <-serverState; ApplicationProtocol(state.NegotiatedProtocol) != alpn || !state.HandshakeComplete {
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

	// Keep the client's real ticket while aggregate pressure evicts its server
	// secret. TLS must authenticate fully, then issue a usable fresh ticket.
	memory := b.ticketCache()
	memory.mu.Lock()
	memory.maxEntries = 1
	memory.mu.Unlock()
	memory.put(sessionKey{scope: "pressure", key: "replacement"}, []byte{1}, nil)
	memory.mu.Lock()
	memory.maxEntries = meshSessionCacheEntries
	memory.mu.Unlock()
	if fallback, err := handshake(a, "wendy-mesh-test/1", "ble-tls"); err != nil || fallback.resumed {
		t.Fatalf("evicted server secret failed full mTLS fallback: %+v %v", fallback, err)
	}
	if fresh, err := handshake(a, "wendy-mesh-test/1", "ble-tls"); err != nil || !fresh.resumed {
		t.Fatalf("post-eviction replacement ticket did not resume: %+v %v", fresh, err)
	}
	if other, err := handshake(a, "wendy-mesh-test/1", "nan-quic"); err != nil || other.resumed {
		t.Fatalf("cross-transport ticket reused=%+v err=%v", other, err)
	}
	if other, err := handshake(a, "wendy-mesh-test/2", "ble-tls"); err != nil || other.resumed {
		t.Fatalf("cross-ALPN ticket reused=%+v err=%v", other, err)
	}
	correct, err := a.PeerTLSWithTickets(b.Device, "wendy-mesh-test/1", "ble-tls")
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
	wrong, err := a.PeerTLSWithTickets(testDevice(t, testPrincipal(testTenantUUID, testDeviceCID)), "wendy-mesh-test/1", "ble-tls")
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.VerifyConnection(resumedState); err == nil {
		t.Fatal("resumed handshake skipped peer asset pin")
	}
	// Reconstructing Credentials (as live reprovisioning does via restart)
	// creates new in-memory caches and ticket keys even for identical PEMs.
	var cert, chain, key string
	_, _ = testCredentialsWithPEM(t, func(asset meshid.Device, c, ca, k string) {
		if asset.UUID().String() == testDeviceAID {
			cert, chain, key = c, ca, k
		}
	})
	newClient, err := NewCredentials(cert, chain, key)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuilding credentials must drop resumption state even with the same trust.
	if state, err := handshake(newClient, "wendy-mesh-test/1", "ble-tls"); err != nil || state.resumed {
		t.Fatalf("credential replacement retained tickets: %+v %v", state, err)
	}
}
