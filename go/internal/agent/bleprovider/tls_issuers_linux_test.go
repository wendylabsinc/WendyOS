//go:build linux

package bleprovider

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"math"
	"math/big"
	"net"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// deviceKey is the leaf key type. Cloud v2 ACME enrollment issues ML-DSA-65
// device keys; older enrollment issues ECDSA keys.
type deviceKey int

const (
	ecdsaDeviceKey deviceKey = iota
	mldsaDeviceKey
)

// issuerCredentials mints a high (client) and low (server) BLE identity with
// the certificate sizes two Orin Nanos sent on 2026-10-25: a 3.9 KB leaf and
// issuers of 5.5, 6.8 and 7.5 KB below the root, 23.7 KB in all. An ML-DSA-65
// leaf is 5.8 KB, and the key also signs each handshake with 3.3 KB. shared
// gives both devices one issuer branch, as one enrollment PKI does. Otherwise
// each device has its own branch under one root, so a peer reaches it only
// through the issuers it sends.
func issuerCredentials(t *testing.T, shared bool, leafKey deviceKey) (high, low *localmesh.Credentials) {
	t.Helper()
	type issued struct {
		cert *x509.Certificate
		key  crypto.Signer
		pem  string
	}
	var serial int64
	mint := func(name string, device meshid.Device, parent *issued, size int, key crypto.Signer) issued {
		t.Helper()
		serial++
		template := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			BasicConstraintsValid: true, IsCA: device == meshid.Device{}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		}
		if template.IsCA {
			template.KeyUsage |= x509.KeyUsageCertSign
		} else {
			uri, err := url.Parse(device.Principal())
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{uri}
		}
		if size > 0 {
			value, err := asn1.Marshal(bytes.Repeat([]byte{0x5a}, size))
			if err != nil {
				t.Fatal(err)
			}
			template.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 1}, Value: value}}
		}
		issuer, signer := template, key
		if parent != nil {
			issuer, signer = parent.cert, parent.key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, issuer, key.Public(), signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return issued{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	}
	ecdsaKey := func() crypto.Signer {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	root := mint("root", meshid.Device{}, nil, 0, ecdsaKey())
	branch := func(name string) (issued, string) {
		issuer, chain := root, root.pem
		for i, size := range []int{7050, 6350, 5050} {
			issuer = mint(name+"-issuer-"+string(rune('a'+i)), meshid.Device{}, &issuer, size, ecdsaKey())
			chain = issuer.pem + chain
		}
		return issuer, chain
	}
	sharedIssuer, sharedChain := branch("shared")
	device := func(name string, id meshid.Device) *localmesh.Credentials {
		t.Helper()
		issuer, chain := sharedIssuer, sharedChain
		if !shared {
			issuer, chain = branch(name)
		}
		// The fixture's issuers sign with ECDSA. Padding stands in for the
		// rest of an ML-DSA-65 issuer's 3,309-byte signature.
		key, size := ecdsaKey(), 3400
		if leafKey == mldsaDeviceKey {
			mldsaKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
			if err != nil {
				t.Fatal(err)
			}
			key, size = mldsaKey, 3500
		}
		leaf := mint(name, id, &issuer, size, key)
		der, err := x509.MarshalPKCS8PrivateKey(leaf.key)
		if err != nil {
			t.Fatal(err)
		}
		credentials, err := localmesh.NewCredentials(leaf.pem, chain, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
		if err != nil {
			t.Fatal(err)
		}
		if len(credentials.Certificate.Certificate) != 4 {
			t.Fatalf("%s presents %d certificates, want leaf and three issuers", name, len(credentials.Certificate.Certificate))
		}
		return credentials
	}
	lowID := selfFixture.UUID()
	lowID[15]--
	return device("high", selfFixture), device("low", meshid.NewDevice(testTenant, lowID))
}

// airPath is one CoC on a virtual clock. Each direction carries
// bytesPerSecond and delivers a write oneWay after its last byte is sent;
// writes return at once, into the socket buffer. An end's clock moves only
// when it reads, so a minute of radio time runs in milliseconds and the timing
// is exact. A read that would complete after the end's budget fails as the
// handshake context deadline does.
type airPath struct {
	bytesPerSecond int
	oneWay         time.Duration
	// unbounded lifts both handshake budgets, to measure how long a
	// handshake that does not fit them would need.
	unbounded bool
}

// The CoC measured on 2026-10-25 carried about 1 KB/s; the radio adds about
// 300 ms each way.
var slowBLE = airPath{bytesPerSecond: 1000, oneWay: 300 * time.Millisecond}

var fastLink = airPath{bytesPerSecond: 1 << 30}

type airLink struct {
	path airPath
	mu   sync.Mutex
	cond sync.Cond
}

type airEnd struct {
	link    *airLink
	in, out *airQueue
	clock   time.Duration
	budget  time.Duration
	closed  bool
}

type airQueue struct {
	frames    []airFrame
	busyUntil time.Duration // when the last queued byte has been sent
	written   int
	first     int // size of the first write, the ClientHello record
	closed    bool
}

type airFrame struct {
	data []byte
	at   time.Duration
}

type airAddr struct{}

func (airAddr) Network() string { return "l2cap" }
func (airAddr) String() string  { return "air" }

func (p airPath) ends(clientStart, clientBudget, serverBudget time.Duration) (client, server *airEnd) {
	link := &airLink{path: p}
	link.cond.L = &link.mu
	up, down := &airQueue{}, &airQueue{}
	return &airEnd{link: link, in: down, out: up, clock: clientStart, budget: clientBudget},
		&airEnd{link: link, in: up, out: down, budget: serverBudget}
}

func (e *airEnd) Read(p []byte) (int, error) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	for len(e.in.frames) == 0 {
		if e.closed {
			return 0, net.ErrClosed
		}
		if e.in.closed {
			return 0, io.EOF
		}
		e.link.cond.Wait()
	}
	frame := &e.in.frames[0]
	if frame.at > e.budget {
		return 0, os.ErrDeadlineExceeded
	}
	e.clock = max(e.clock, frame.at)
	n := copy(p, frame.data)
	if frame.data = frame.data[n:]; len(frame.data) == 0 {
		e.in.frames = e.in.frames[1:]
	}
	return n, nil
}

func (e *airEnd) Write(p []byte) (int, error) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	if e.closed || e.out.closed {
		return 0, net.ErrClosed
	}
	if e.out.written == 0 {
		e.out.first = len(p)
	}
	e.out.written += len(p)
	e.out.busyUntil = max(e.clock, e.out.busyUntil) + time.Duration(len(p))*time.Second/time.Duration(e.link.path.bytesPerSecond)
	e.out.frames = append(e.out.frames, airFrame{bytes.Clone(p), e.out.busyUntil + e.link.path.oneWay})
	e.link.cond.Broadcast()
	return len(p), nil
}

func (e *airEnd) Close() error {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	e.closed, e.out.closed = true, true
	e.link.cond.Broadcast()
	return nil
}

func (e *airEnd) LocalAddr() net.Addr              { return airAddr{} }
func (e *airEnd) RemoteAddr() net.Addr             { return airAddr{} }
func (e *airEnd) SetDeadline(time.Time) error      { return nil }
func (e *airEnd) SetReadDeadline(time.Time) error  { return nil }
func (e *airEnd) SetWriteDeadline(time.Time) error { return nil }

func (e *airEnd) snapshot() (clock time.Duration, written, first int) {
	e.link.mu.Lock()
	defer e.link.mu.Unlock()
	return e.clock, e.out.written, e.out.first
}

type airHandshake struct {
	client, server           tls.ConnectionState
	clientErr, serverErr     error
	clientBytes, serverBytes int
	clientHelloBytes         int
	clientDone, serverDone   time.Duration
}

func (h airHandshake) err() error { return errors.Join(h.clientErr, h.serverErr) }

// handshakeOverAir times one BLE TLS handshake as dialLink and acceptLink
// bound it. The server's meshTLSHandshakeTimeout starts at accept, while the
// client may first spend up to meshInitialTuneBudget on the connection
// interval; the client then has meshTLSOutboundTimeout.
func handshakeOverAir(t *testing.T, path airPath, clientConfig, serverConfig *tls.Config) airHandshake {
	t.Helper()
	clientStart := meshInitialTuneBudget
	clientBudget, serverBudget := clientStart+meshTLSOutboundTimeout, meshTLSHandshakeTimeout
	if path.unbounded {
		clientBudget, serverBudget = time.Duration(math.MaxInt64), time.Duration(math.MaxInt64)
	}
	clientEnd, serverEnd := path.ends(clientStart, clientBudget, serverBudget)
	defer clientEnd.Close()
	defer serverEnd.Close()
	client, server := tls.Client(clientEnd, clientConfig), tls.Server(serverEnd, serverConfig)
	serverResult := make(chan error, 1)
	go func() {
		err := server.Handshake()
		if err != nil {
			_ = serverEnd.Close()
		}
		serverResult <- err
	}()
	var result airHandshake
	if result.clientErr = client.Handshake(); result.clientErr != nil {
		_ = clientEnd.Close()
	}
	select {
	case result.serverErr = <-serverResult:
	case <-time.After(20 * time.Second):
		t.Fatal("simulated handshake stalled")
	}
	result.client, result.server = client.ConnectionState(), server.ConnectionState()
	var clientDone, serverDone time.Duration
	clientDone, result.clientBytes, result.clientHelloBytes = clientEnd.snapshot()
	serverDone, result.serverBytes, _ = serverEnd.snapshot()
	result.clientDone, result.serverDone = clientDone-clientStart, serverDone
	return result
}

func issuerPeer(low *localmesh.Credentials) candidate {
	return candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.Device.UUID())}
}

// bleClientConfig is the configuration dialLink hands to tls.Client.
func bleClientConfig(t *testing.T, r *runtime, peer candidate) (*tls.Config, *meshid.Device) {
	t.Helper()
	config, authenticated, err := r.clientTLS(peer)
	if err != nil {
		t.Fatal(err)
	}
	config.CurvePreferences = []tls.CurveID{tls.X25519}
	return config, authenticated
}

// legacyClient models a peer from before issuer omission: it offers only
// ALPN and always presents its configured chain.
func legacyClient(config *tls.Config) *tls.Config {
	config.NextProtos = []string{ALPN}
	config.GetClientCertificate = nil
	return config
}

// legacyServer models a peer from before issuer omission.
func legacyServer(config *tls.Config, credentials *localmesh.Credentials) *tls.Config {
	config.GetCertificate = nil
	config.Certificates = []tls.Certificate{credentials.Certificate}
	return config
}

func certificateBytes(chain [][]byte) (size int) {
	for _, der := range chain {
		size += len(der)
	}
	return size
}

// acceptedPeer repeats acceptLink's check of the client's certificates.
func acceptedPeer(server *localmesh.Credentials, state tls.ConnectionState) (meshid.Device, error) {
	chain := make([][]byte, 0, len(state.PeerCertificates))
	for _, cert := range state.PeerCertificates {
		chain = append(chain, cert.Raw)
	}
	identity, err := server.Verify(chain, time.Now())
	return identity.Device, err
}

// Two devices of one enrollment PKI hold each other's issuers. A first-contact
// and a pinned reconnect handshake each carry only the two leaves.
func TestBLEHandshakeOmitsSharedIssuers(t *testing.T) {
	high, low := issuerCredentials(t, true, ecdsaDeviceKey)
	server := &runtime{cfg: Config{Credentials: low}}
	legacyConfig, _ := bleClientConfig(t, &runtime{cfg: Config{Credentials: high}}, issuerPeer(low))
	legacy := handshakeOverAir(t, fastLink, legacyClient(legacyConfig), legacyServer(server.makeServerTLS(), low))
	if legacy.err() != nil {
		t.Fatalf("legacy peers: %v", legacy.err())
	}
	for _, tc := range []struct {
		name  string
		known bool
	}{{name: "first contact"}, {name: "known peer", known: true}} {
		t.Run(tc.name, func(t *testing.T) {
			client := &runtime{cfg: Config{Credentials: high}}
			if tc.known {
				client.known = map[string]meshid.Device{issuerPeer(low).key(): low.Device}
			}
			config, authenticated := bleClientConfig(t, client, issuerPeer(low))
			trimmed := handshakeOverAir(t, fastLink, config, server.makeServerTLS())
			if trimmed.err() != nil || trimmed.client.DidResume || trimmed.client.NegotiatedProtocol != ALPN || *authenticated != low.Device {
				t.Fatalf("shared issuers: err=%v resumed=%v alpn=%q identity=%v", trimmed.err(), trimmed.client.DidResume, trimmed.client.NegotiatedProtocol, authenticated)
			}
			if peer, err := acceptedPeer(low, trimmed.server); err != nil || peer != high.Device {
				t.Fatalf("server accepted %v: %v", peer, err)
			}
			if len(trimmed.client.PeerCertificates) != 1 || len(trimmed.server.PeerCertificates) != 1 {
				t.Fatalf("server presented %d certificates and client %d, want only each leaf", len(trimmed.client.PeerCertificates), len(trimmed.server.PeerCertificates))
			}
			t.Logf("server flight %d -> %d bytes, client flight %d -> %d bytes, ClientHello %d -> %d bytes",
				legacy.serverBytes, trimmed.serverBytes, legacy.clientBytes, trimmed.clientBytes, legacy.clientHelloBytes, trimmed.clientHelloBytes)
			// Every issuer is larger than this allowance for handshake messages.
			const overhead = 1024
			if limit := certificateBytes(low.Certificate.Certificate[:1]) + overhead; trimmed.serverBytes > limit {
				t.Fatalf("server sent %d bytes, want at most its leaf plus %d (%d)", trimmed.serverBytes, overhead, limit)
			}
			if limit := certificateBytes(high.Certificate.Certificate[:1]) + overhead; trimmed.clientBytes > limit {
				t.Fatalf("client sent %d bytes, want at most its leaf plus %d (%d)", trimmed.clientBytes, overhead, limit)
			}
			// The hint must not push the cold ClientHello past two 240-byte CoC SDUs.
			if trimmed.clientHelloBytes > 2*maxWriteSDU {
				t.Fatalf("ClientHello %d bytes needs more than two CoC SDUs", trimmed.clientHelloBytes)
			}
		})
	}
}

// At the ~1 KB/s and 300 ms one-way delay measured between two Orin Nanos,
// two full chains outlast the server's handshake budget and two leaves fit.
// An ML-DSA-65 device key adds a larger leaf and a 3.3 KB signature to each
// flight, which leaves less of the budget spare.
func TestBLEHandshakeFitsBudgetAtOneKilobytePerSecond(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  deviceKey
	}{{"ECDSA device key", ecdsaDeviceKey}, {"ML-DSA-65 device key", mldsaDeviceKey}} {
		t.Run(tc.name, func(t *testing.T) {
			high, low := issuerCredentials(t, true, tc.key)
			server := &runtime{cfg: Config{Credentials: low}}
			legacyClientConfig := func() *tls.Config {
				config, _ := bleClientConfig(t, &runtime{cfg: Config{Credentials: high}}, issuerPeer(low))
				return legacyClient(config)
			}
			legacy := handshakeOverAir(t, slowBLE, legacyClientConfig(), legacyServer(server.makeServerTLS(), low))
			measure := slowBLE
			measure.unbounded = true
			needed := handshakeOverAir(t, measure, legacyClientConfig(), legacyServer(server.makeServerTLS(), low))
			t.Logf("full chains: %d bytes; client done after %v, server after %v; with the %v budget the server fails: %v",
				needed.clientBytes+needed.serverBytes, needed.clientDone, needed.serverDone, meshTLSHandshakeTimeout, legacy.serverErr)
			if legacy.serverErr == nil {
				t.Fatalf("full chains fit the budget after %v; the link no longer reproduces the field timeouts", legacy.serverDone)
			}
			config, authenticated := bleClientConfig(t, &runtime{cfg: Config{Credentials: high}}, issuerPeer(low))
			trimmed := handshakeOverAir(t, slowBLE, config, server.makeServerTLS())
			t.Logf("shared issuers omitted: %d bytes; client done after %v, server after %v",
				trimmed.clientBytes+trimmed.serverBytes, trimmed.clientDone, trimmed.serverDone)
			if trimmed.err() != nil || *authenticated != low.Device {
				t.Fatalf("handshake over a 1 KB/s link: %v identity=%v", trimmed.err(), authenticated)
			}
			if peer, err := acceptedPeer(low, trimmed.server); err != nil || peer != high.Device {
				t.Fatalf("server accepted %v: %v", peer, err)
			}
			if len(trimmed.client.PeerCertificates) != 1 || len(trimmed.server.PeerCertificates) != 1 || trimmed.serverDone > meshTLSHandshakeTimeout {
				t.Fatalf("server presented %d certificates and client %d; server done after %v", len(trimmed.client.PeerCertificates), len(trimmed.server.PeerCertificates), trimmed.serverDone)
			}
		})
	}
}

// A peer from before this change, or one with another issuer set, still
// exchanges and verifies full chains.
func TestBLEIssuerOmissionMixedVersions(t *testing.T) {
	sharedHigh, sharedLow := issuerCredentials(t, true, ecdsaDeviceKey)
	branchHigh, branchLow := issuerCredentials(t, false, ecdsaDeviceKey)
	for _, tc := range []struct {
		name      string
		high, low *localmesh.Credentials
		client    func(*tls.Config) *tls.Config
		server    func(*tls.Config, *localmesh.Credentials) *tls.Config
	}{
		{name: "legacy client", high: sharedHigh, low: sharedLow, client: legacyClient},
		{name: "legacy server", high: sharedHigh, low: sharedLow, server: legacyServer},
		// Separate issuing branches share only the root: each side needs the
		// other's issuers to reach it.
		{name: "different issuers", high: branchHigh, low: branchLow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, authenticated := bleClientConfig(t, &runtime{cfg: Config{Credentials: tc.high}}, issuerPeer(tc.low))
			if tc.client != nil {
				config = tc.client(config)
			}
			serverConfig := (&runtime{cfg: Config{Credentials: tc.low}}).makeServerTLS()
			if tc.server != nil {
				serverConfig = tc.server(serverConfig, tc.low)
			}
			result := handshakeOverAir(t, fastLink, config, serverConfig)
			if result.err() != nil || *authenticated != tc.low.Device || result.client.NegotiatedProtocol != ALPN {
				t.Fatalf("handshake: %v identity=%v alpn=%q", result.err(), authenticated, result.client.NegotiatedProtocol)
			}
			if peer, err := acceptedPeer(tc.low, result.server); err != nil || peer != tc.high.Device {
				t.Fatalf("server accepted %v: %v", peer, err)
			}
			if len(result.client.PeerCertificates) != 4 || len(result.server.PeerCertificates) != 4 {
				t.Fatalf("server presented %d certificates and client %d, want full chains", len(result.client.PeerCertificates), len(result.server.PeerCertificates))
			}
		})
	}
}

// Omitting issuers can only make verification fail. A peer that lacks an
// omitted issuer rejects the handshake; it never accepts an unverified leaf.
func TestBLEIssuerOmissionFailsClosed(t *testing.T) {
	high, low := issuerCredentials(t, false, ecdsaDeviceKey)
	if high.IssuerDigest() == "" || high.IssuerDigest() == low.IssuerDigest() {
		t.Fatalf("separate branches have digests %q and %q", high.IssuerDigest(), low.IssuerDigest())
	}
	t.Run("client claims the server's issuers", func(t *testing.T) {
		// A stale or forged hint makes the server send only its leaf. The
		// client cannot reach that leaf's issuer and refuses the server.
		config, authenticated := bleClientConfig(t, &runtime{cfg: Config{Credentials: high}}, issuerPeer(low))
		config.NextProtos = []string{ALPN, issuerHintPrefix + low.IssuerDigest()}
		result := handshakeOverAir(t, fastLink, config, (&runtime{cfg: Config{Credentials: low}}).makeServerTLS())
		if len(result.client.PeerCertificates) != 1 {
			t.Fatalf("server presented %d certificates, want only its leaf", len(result.client.PeerCertificates))
		}
		if result.clientErr == nil || *authenticated != (meshid.Device{}) {
			t.Fatalf("client accepted a server leaf without its issuers: identity=%v", authenticated)
		}
		if result.serverErr == nil {
			t.Fatal("server completed a handshake the client refused")
		}
	})
	t.Run("client omits issuers the server lacks", func(t *testing.T) {
		config, _ := bleClientConfig(t, &runtime{cfg: Config{Credentials: high}}, issuerPeer(low))
		config.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			leaf := high.LeafCertificate()
			return &leaf, nil
		}
		result := handshakeOverAir(t, fastLink, config, (&runtime{cfg: Config{Credentials: low}}).makeServerTLS())
		if len(result.server.PeerCertificates) != 1 {
			t.Fatalf("client presented %d certificates, want only its leaf", len(result.server.PeerCertificates))
		}
		if result.serverErr == nil || result.server.HandshakeComplete {
			t.Fatal("server accepted a client leaf without its issuers")
		}
	})
}
