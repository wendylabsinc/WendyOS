package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"
)

// meshPeerCert signs a leaf certificate carrying urn as its wendy identity SAN
// (the same shape testPeerLeafRaw produces for VerifyPeerCertificate-only
// tests), returned as a servable tls.Certificate so it can back a real TLS
// listener — this test needs an actual handshake (and a real session ticket)
// rather than a raw VerifyPeerCertificate call.
func meshPeerCert(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, urn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	u, err := url.Parse(urn)
	if err != nil {
		t.Fatalf("parsing urn %q: %v", urn, err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "mesh-peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// Both EKUs: the peer's VerifyPeerCertificate chain check (built by
		// NewTLSConfig for the server-verifying-client role) requires
		// ExtKeyUsageClientAuth even though this cert is presented in the
		// server role of the TLS handshake below — mirroring real mesh device
		// certs, which serve as both mesh client and mesh server.
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:        []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating leaf cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startMeshPeerServer runs a bare TLS 1.3 server presenting cert with Go's
// default session-ticket behavior (no custom WrapSession/UnwrapSession): this
// test is entirely about what the CLIENT does with a resumed session, not the
// agent's own server-side ticket logic (covered by resumption_test.go), so a
// plain tls.Config is the more faithful "any mesh peer" stand-in. Each
// accepted connection writes one byte after the handshake and waits for the
// client to close, mirroring the pattern in resumption_test.go and
// tlscache's cache_test.go so that a client Read after Dial reliably
// observes the post-handshake NewSessionTicket message before the ticket is
// needed for the next dial.
func startMeshPeerServer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				tc := tls.Server(c, cfg)
				if err := tc.Handshake(); err != nil {
					return
				}
				tc.Write([]byte{1})
				buf := make([]byte, 1)
				tc.Read(buf)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// dialMeshPeer dials addr with cfg and, on success, reads the one byte the
// server writes post-handshake so the caller can rely on the session
// ticket having been processed (Put on the client's session cache) before
// the connection is closed or another dial is attempted.
func dialMeshPeer(t *testing.T, addr string, cfg *tls.Config) (*tls.Conn, error) {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// TestNewClientTLSConfigExpectingPeerRejectsPinMismatchOnResume is the
// regression test for the "mesh peer pin bypassed on resumed connections"
// finding: NewClientTLSConfigExpectingPeer enforced the mesh anti-MITM pin
// (org + asset ID, see its doc comment) only inside VerifyPeerCertificate,
// which Go skips entirely on a resumed TLS 1.3 handshake — only
// VerifyConnection runs, with ConnectionState.PeerCertificates restored from
// the cached ticket rather than freshly presented. Because all mesh dials
// share one process-wide meshSessionCache AND one constant gRPC authority
// ("passthrough:///mesh-peer", see mesh_dialer.go's meshDialTarget), a ticket
// cached while dialing one asset could previously be resumed while a LATER
// dial "expects" a completely different asset, performing ZERO client-side
// verification of the peer's actual certificate.
//
// This test reproduces the shared-cache scenario directly against a real TLS
// server rather than through the gRPC/mesh-dialer plumbing: one physical
// server presents a fixed identity (asset 100 in org 7). Two
// NewClientTLSConfigExpectingPeer configs share one tls.ClientSessionCache —
// one correctly pinned to asset 100, one (mis)pinned to a different asset
// 999 — simulating two mesh dials to different devices that land on the same
// cache slot. The mispinned dial must fail once it resumes a session
// actually established with asset 100.
//
// Ordering note: Go's client evicts a ClientSessionCache entry whenever a
// handshake that attempted resumption subsequently fails (see
// crypto/tls/handshake_client.go's clientHandshake, RFC 5077 §3.2 ticket
// disposal on failure) — so the "legitimate resumption still works"
// assertion runs BEFORE the mispinned dial, not after: once the mispinned
// dial fails as expected, Go throws away the now-suspect cached ticket,
// which is a reasonable side effect but would make a THIRD, post-attack
// assertion meaningless (it would force a full handshake for an unrelated
// reason, telling us nothing about the pin-mismatch fix).
func TestNewClientTLSConfigExpectingPeerRejectsPinMismatchOnResume(t *testing.T) {
	ca, caKey, chainPEM := testCAKeyPair(t)
	peerCert := meshPeerCert(t, ca, caKey, "urn:wendy:org:7:asset:100")
	addr := startMeshPeerServer(t, peerCert)

	// The dialing side's own identity supplies the expected tenant (org 7,
	// matching the server); the bare server above never requests a client
	// certificate, so the cert itself is never presented.
	ownCertPEM, ownKeyPEM := testLeafCertificate(t, "sh/wendy/7/1")

	cache := tls.NewLRUClientSessionCache(4)

	cfgX, err := NewClientTLSConfigExpectingPeer(ownCertPEM, chainPEM, ownKeyPEM, nil, "100")
	if err != nil {
		t.Fatalf("NewClientTLSConfigExpectingPeer(asset 100): %v", err)
	}
	cfgX.ClientSessionCache = cache

	// Dial 1: full handshake, correctly pinned to the server's real identity.
	// Must succeed, must NOT resume (nothing cached yet), and caches a ticket.
	conn1, err := dialMeshPeer(t, addr, cfgX)
	if err != nil {
		t.Fatalf("first dial (correct pin, full handshake) failed: %v", err)
	}
	if conn1.ConnectionState().DidResume {
		t.Error("first connection unexpectedly resumed")
	}
	conn1.Close()

	// Dial 2: same cfgX, same cache. Must resume — this is the "no false
	// positives" check: the VerifyConnection re-verify added by the fix must
	// not break ordinary resumption to the SAME correctly pinned peer.
	conn2, err := dialMeshPeer(t, addr, cfgX)
	if err != nil {
		t.Fatalf("second dial (correct pin, expected resume) failed: %v", err)
	}
	if !conn2.ConnectionState().DidResume {
		t.Error("second connection with the correct pin did not resume")
	}
	conn2.Close()

	// Dial 3: a DIFFERENT NewClientTLSConfigExpectingPeer, pinned to a
	// DIFFERENT asset (999), but wired to the SAME session cache — simulating
	// a later mesh dial to a different device sharing meshSessionCache and
	// the constant mesh dial authority. The cache holds a ticket for the
	// server's real identity (asset 100, refreshed by dial 2's post-handshake
	// ticket), so the server will happily accept resumption; the fix must
	// catch the pin mismatch in VerifyConnection and fail the handshake.
	cfgY, err := NewClientTLSConfigExpectingPeer(ownCertPEM, chainPEM, ownKeyPEM, nil, "999")
	if err != nil {
		t.Fatalf("NewClientTLSConfigExpectingPeer(asset 999): %v", err)
	}
	cfgY.ClientSessionCache = cache

	conn3, err := dialMeshPeer(t, addr, cfgY)
	if err == nil {
		conn3.Close()
		t.Fatal("dial pinned to a different asset than the resumed session succeeded — the peer pin was bypassed on resumption")
	}
}
