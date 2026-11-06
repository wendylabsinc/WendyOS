package meshsession

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Test identities are real device principals: one tenant UUID and three
// device UUIDs, the same values the mesh addresses are derived from. Device
// A is the lower UUID, device B the higher; device C is a third device in
// the same tenant for mismatch scenarios.
const (
	fixtureTenant  = "0b8f3a67-2c1d-4e5f-9a8b-0c1d2e3f4051"
	fixtureDeviceA = "2b1c0000-0000-4000-8000-000000000445"
	fixtureDeviceB = "2b1c0000-0000-4000-8000-000000000460"
	fixtureDeviceC = "2b1c0000-0000-4000-8000-000000000470"
)

func fixturePrincipal(device string) string {
	return certs.DeviceSPIFFEURI(fixtureTenant, device)
}

func fixtureDevice(t *testing.T, device string) meshid.Device {
	t.Helper()
	parsed, err := meshid.DeviceFromPrincipal(fixturePrincipal(device))
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func TestResolveRouteOnly(t *testing.T) {
	target := fixtureDevice(t, fixtureDeviceB)
	address := target.MeshAddress()
	prefix := netip.PrefixFrom(address, 128)
	// Deterministic addressing plus one live route is enough: no manifest
	// for the target has arrived, and Resolve consults none.
	snapshot := localmesh.NodeSnapshot{Routes: []babel.Route{{Prefix: prefix}}}
	endpoint, err := Resolve(snapshot, target)
	if err != nil || endpoint.String() != netip.AddrPortFrom(address, Port).String() {
		t.Fatalf("route without manifest: endpoint=%v err=%v", endpoint, err)
	}
	// A manifest that has arrived must not change the answer.
	snapshot.Devices = []localmesh.Manifest{{Device: target.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli()}}
	if endpoint, err = Resolve(snapshot, target); err != nil || endpoint.String() != netip.AddrPortFrom(address, Port).String() {
		t.Fatalf("endpoint = %v, %v", endpoint, err)
	}
	// No route at all.
	if _, err := Resolve(localmesh.NodeSnapshot{}, target); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("absent route: %v", err)
	}
	snapshot.Routes[0].Unreachable = true
	if _, err := Resolve(snapshot, target); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("unreachable route: %v", err)
	}
	snapshot.Routes[0].Unreachable = false
	snapshot.Routes[0].Local = true
	if _, err := Resolve(snapshot, target); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("local route: %v", err)
	}
}

type allowPort func(uint16) bool

func TestInitialPacketFitsRoutedMeshMTU(t *testing.T) {
	cfg := quicConfig()
	const ipv6UDPHeaders = 40 + 8
	if int(cfg.InitialPacketSize)+ipv6UDPHeaders > localmesh.TunnelMTU || !cfg.DisablePathMTUDiscovery {
		t.Fatalf("QUIC packet can exceed the routed mesh MTU: %+v", cfg)
	}
}

func TestAppQUICResumesWithinCredentialLifetime(t *testing.T) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, allowPort(func(uint16) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := server.TLSConfig()
	if serverConfig.SessionTicketsDisabled || serverConfig.VerifyConnection == nil {
		t.Fatal("app server lacks authenticated resumption")
	}
	var peerCerts []*x509.Certificate
	for _, der := range a.Certificate.Certificate {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		peerCerts = append(peerCerts, cert)
	}
	if err := serverConfig.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: peerCerts}); err != nil {
		t.Fatalf("resumed authorized device rejected: %v", err)
	}
	if err := serverConfig.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: []*x509.Certificate{}}); err == nil {
		t.Fatal("resumed anonymous device accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("app QUIC listener did not start")
	}
	var coldBytes int64
	for i := 0; i < 2; i++ {
		cfg, err := a.PeerTLSWithTickets(b.Device, ALPN, "app-quic")
		if err != nil {
			t.Fatal(err)
		}
		udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		packets := &countingPacketConn{PacketConn: udp}
		conn, err := quic.Dial(ctx, packets, net.UDPAddrFromAddrPort(endpoint), cfg, quicConfig())
		if err != nil {
			_ = udp.Close()
			t.Fatal(err)
		}
		if got := conn.ConnectionState().TLS.DidResume; got != (i == 1) {
			t.Fatalf("app QUIC handshake %d resumed=%v", i, got)
		}
		// Let the post-handshake NewSessionTicket arrive before closing.
		time.Sleep(100 * time.Millisecond)
		if i == 0 {
			coldBytes = packets.written.Load()
		} else {
			warmBytes := packets.written.Load()
			t.Logf("app QUIC TLS ticket client UDP bytes: cold=%d resumed=%d", coldBytes, warmBytes)
		}
		_ = conn.CloseWithError(0, "test next handshake")
		_ = udp.Close()
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func (f allowPort) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if !f(port) {
		return nil, ErrDenied
	}
	return dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(int(port))))
}

func fixtureCredentials(t *testing.T) (*localmesh.Credentials, *localmesh.Credentials) {
	a, b := fixtureCredentialExpiry(t, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	return a, b
}
func fixtureCredentialExpiry(t *testing.T, aExpiry, bExpiry time.Time, issuerExpiry ...time.Time) (*localmesh.Credentials, *localmesh.Credentials) {
	a, b, _, _ := fixtureCredentialExpiryWithChain(t, aExpiry, bExpiry, issuerExpiry...)
	return a, b
}

// fixtureCredentialExpiryWithChain mints three device-principal leaves (A, B,
// C — all in one tenant) under a single CA, so a mismatch test fails for its
// principal, never because it happened to be signed by another key. It
// returns the credentials, in device order, plus the CA chain PEM.
func fixtureCredentialExpiryWithChain(t *testing.T, aExpiry, bExpiry time.Time, issuerExpiry ...time.Time) (*localmesh.Credentials, *localmesh.Credentials, *localmesh.Credentials, string) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if len(issuerExpiry) != 0 {
		ca.NotAfter = issuerExpiry[0]
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	issue := func(device string, expires time.Time) *localmesh.Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, err := url.Parse(fixturePrincipal(device))
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: ca.NotBefore, NotAfter: expires, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		c, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return issue(fixtureDeviceA, aExpiry), issue(fixtureDeviceB, bExpiry), issue(fixtureDeviceC, bExpiry), chain
}

func TestAuthenticatedAppFlowAndPortDenial(t *testing.T) {
	a, b, c := fixtureCredentialsWithThird(t)
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
	port := uint16(app.Addr().(*net.TCPAddr).Port)
	server, err := NewServer(b, allowPort(func(p uint16) bool { return p == port }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("QUIC listener did not start")
	}
	client, err := Dial(ctx, a, b.Device, endpoint, port)
	if err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	var echo [5]byte
	if _, err := io.ReadFull(client, echo[:]); err != nil {
		t.Fatal(err)
	}
	if string(echo[:]) != "hello" {
		t.Fatalf("echo = %q", echo)
	}
	_ = client.Close()
	_, err = Dial(ctx, a, b.Device, endpoint, port+1)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("unpublished port: %v", err)
	}
	_, err = Dial(ctx, a, c.Device, endpoint, port)
	if err == nil {
		t.Fatal("wrong device was accepted")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// fixtureCredentialsWithThird returns the A and B fixture credentials plus a
// third credential for device C in the same tenant, all under one CA.
func fixtureCredentialsWithThird(t *testing.T) (*localmesh.Credentials, *localmesh.Credentials, *localmesh.Credentials) {
	a, b, c, _ := fixtureCredentialExpiryWithChain(t, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	return a, b, c
}

// TestPrincipalPinningRefusesWrongDevice answers a dial that intends device B
// with a server authenticated as a different device principal (C, same
// tenant). The client pins B's full principal, so the handshake must fail and
// no app stream may reach the authorizer.
func TestPrincipalPinningRefusesWrongDevice(t *testing.T) {
	a, _, c := fixtureCredentialsWithThird(t)
	target := fixtureDevice(t, fixtureDeviceB)
	authorizer := &countingAuthorizer{port: Port}
	server, err := NewServer(c, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("QUIC listener did not start")
	}
	// The client intends device B and dials B's deterministic mesh endpoint;
	// device C answers at the socket.
	_, err = Dial(ctx, a, target, netip.AddrPortFrom(target.MeshAddress(), endpoint.Port()), Port)
	if err == nil {
		t.Fatal("device C answered a dial pinned to device B")
	}
	// Any handshake failure is acceptable as long as it is the handshake
	// failing; a port denial would mean the stream reached the app.
	if errors.Is(err, ErrDenied) {
		t.Fatalf("wrong-device dial reached port authorization: %v", err)
	}
	if calls := authorizer.calls.Load(); calls != 0 {
		t.Fatalf("wrong-device dial authorized %d app streams", calls)
	}
	cancel()
	_ = <-done
}

func TestAppPortHelloMayArriveAfterBLEDelay(t *testing.T) {
	a, b := fixtureCredentials(t)
	app, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	port := uint16(app.Addr().(*net.TCPAddr).Port)
	server, err := NewServer(b, allowPort(func(p uint16) bool { return p == port }))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx, "127.0.0.1:0") }()
	var endpoint netip.AddrPort
	for i := 0; i < 100; i++ {
		if addr := server.Addr(); addr != nil {
			endpoint = netip.MustParseAddrPort(addr.String())
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !endpoint.IsValid() {
		t.Fatal("QUIC listener did not start")
	}
	tlsConfig, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	tlsConfig.NextProtos = []string{ALPN}
	conn, err := quic.DialAddr(ctx, endpoint.String(), tlsConfig, quicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "test complete")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var hello [6]byte
	copy(hello[:4], "WAS1")
	binary.BigEndian.PutUint16(hello[4:], port)
	if _, err := stream.Write(hello[:1]); err != nil {
		t.Fatal(err)
	}
	// The old five-second server stream deadline closed this valid session.
	time.Sleep(6 * time.Second)
	if _, err := stream.Write(hello[1:]); err != nil {
		t.Fatal(err)
	}
	_ = stream.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack [1]byte
	if _, err := io.ReadFull(stream, ack[:]); err != nil || ack[0] != 1 {
		t.Fatalf("late app port acknowledgement = %v, %v", ack, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
