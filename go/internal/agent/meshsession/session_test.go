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
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestResolveRequiresManifestAndRoute(t *testing.T) {
	const org, asset = 64, 460
	prefix := netip.MustParsePrefix("10.88.1.204/32")
	router, _ := localmesh.RouterID(org, asset)
	snapshot := localmesh.NodeSnapshot{Routes: []babel.Route{{Prefix: prefix, RouterID: router}}}
	if _, err := Resolve(snapshot, org, asset); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("route without manifest: %v", err)
	}
	snapshot.Devices = []localmesh.Manifest{{Org: org, Asset: asset, Expires: time.Now().Add(time.Minute).UnixMilli()}}
	endpoint, err := Resolve(snapshot, org, asset)
	if err != nil || endpoint.String() != "10.88.1.204:43021" {
		t.Fatalf("endpoint = %v, %v", endpoint, err)
	}
	snapshot.Routes[0].Unreachable = true
	if _, err := Resolve(snapshot, org, asset); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("unreachable route: %v", err)
	}
	snapshot.Routes[0].Unreachable = false
	snapshot.Devices[0].Expires = time.Now().Add(-time.Second).UnixMilli()
	if _, err := Resolve(snapshot, org, asset); !errors.Is(err, ErrNoRoute) {
		t.Fatalf("expired manifest: %v", err)
	}
}

type allowPort func(uint16) bool

func TestInitialPacketFitsRoutedMeshMTU(t *testing.T) {
	cfg := quicConfig()
	const ipv4UDPHeaders = 20 + 8
	if int(cfg.InitialPacketSize)+ipv4UDPHeaders > localmesh.TunnelMTU || !cfg.DisablePathMTUDiscovery {
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
		t.Fatalf("resumed authorized asset rejected: %v", err)
	}
	if err := serverConfig.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: []*x509.Certificate{}}); err == nil {
		t.Fatal("resumed anonymous asset accepted")
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
		cfg, err := a.PeerTLSWithTickets(b.Asset, ALPN, "app-quic")
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

func (f allowPort) DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error) {
	if !f(port) {
		return nil, ErrDenied
	}
	return dial()
}

func fixtureCredentials(t *testing.T) (*localmesh.Credentials, *localmesh.Credentials) {
	return fixtureCredentialExpiry(t, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
}
func fixtureCredentialExpiry(t *testing.T, aExpiry, bExpiry time.Time, issuerExpiry ...time.Time) (*localmesh.Credentials, *localmesh.Credentials) {
	a, b, _ := fixtureCredentialExpiryWithChain(t, aExpiry, bExpiry, issuerExpiry...)
	return a, b
}

func fixtureCredentialExpiryWithChain(t *testing.T, aExpiry, bExpiry time.Time, issuerExpiry ...time.Time) (*localmesh.Credentials, *localmesh.Credentials, string) {
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
	issue := func(asset int32, expires time.Time) *localmesh.Credentials {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(fmt.Sprintf("urn:wendy:org:64:asset:%d", asset))
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(asset)), NotBefore: ca.NotBefore, NotAfter: expires, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		c, err := localmesh.NewCredentials(64, asset, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return issue(445, aExpiry), issue(460, bExpiry), chain
}

func TestAuthenticatedAppFlowAndPortDenial(t *testing.T) {
	a, b := fixtureCredentials(t)
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
	client, err := Dial(ctx, a, b.Asset, endpoint, port)
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
	_, err = Dial(ctx, a, b.Asset, endpoint, port+1)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("unpublished port: %v", err)
	}
	_, err = Dial(ctx, a, b.Asset+1, endpoint, port)
	if err == nil {
		t.Fatal("wrong asset was accepted")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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
	tlsConfig, err := a.PeerTLS(b.Asset)
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
