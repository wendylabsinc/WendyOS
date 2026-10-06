package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type resumptionPKI struct {
	ca                                 *x509.Certificate
	caKey                              *ecdsa.PrivateKey
	crl                                *crlFixture
	serverCertPEM, serverKeyPEM, caPEM string
	clientCert                         tls.Certificate
}

func newResumptionPKI(t *testing.T) resumptionPKI {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Resumption Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(48 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
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

	crl := serveTestCRL(t, caCert, caKey)
	leaf := func(cn string, eku x509.ExtKeyUsage) (string, string, tls.Certificate) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("gen key: %v", err)
		}
		tmpl := &x509.Certificate{
			SerialNumber:          big.NewInt(time.Now().UnixNano()),
			Subject:               pkix.Name{CommonName: cn},
			IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{eku},
			CRLDistributionPoints: []string{crl.server.URL},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
		if err != nil {
			t.Fatalf("create leaf: %v", err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatalf("marshal key: %v", err)
		}
		certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
		return certPEM, keyPEM, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}

	serverCertPEM, serverKeyPEM, _ := leaf("resumption-server", x509.ExtKeyUsageServerAuth)
	_, _, clientCert := leaf("resumption-client", x509.ExtKeyUsageClientAuth)
	return resumptionPKI{ca: caCert, caKey: caKey, crl: crl,
		serverCertPEM: serverCertPEM,
		serverKeyPEM:  serverKeyPEM,
		caPEM:         string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		clientCert:    clientCert,
	}
}

// resumptionEnv serves the given config and reports each connection's
// server-side DidResume; verifies counts full VerifyPeerCertificate runs.
//
// crypto/tls documents that a *tls.Config must not be modified after it is
// first used, and reads fields like WrapSession/UnwrapSession/
// SessionTicketsDisabled unguarded during each handshake. The accept-loop
// goroutine below starts using cfg as soon as newResumptionEnv launches it,
// so tests must never assign to cfg fields afterwards — that would race with
// the handshake goroutines under the Go memory model (a data race exists
// whether or not `go test -race` happens to observe it on a given run). All
// hook functions are therefore installed exactly once, before the accept
// loop starts; tests that need to change behavior mid-run do so only through
// the synchronized `now` / `stripWindow` indirection below, which the
// installed hooks consult via atomic loads on every handshake.
type resumptionEnv struct {
	pki         resumptionPKI
	revocation  *revocationChecker
	addr        string
	cfg         *tls.Config
	clientCert  tls.Certificate
	verifyCount *atomic.Int32
	srvResumed  chan bool

	// now backs the UnwrapSession clock; tests swap it via now.Store to
	// simulate time passing without touching cfg after the accept loop starts.
	now atomic.Pointer[func() time.Time]
	// stripWindow, when true, makes the installed WrapSession skip stamping
	// the client cert window — simulating a garbled/foreign ticket.
	stripWindow atomic.Bool
}

// newResumptionEnv builds the PKI, wires the server TLS config, and starts
// the accept loop. ticketsDisabled must be decided up front (rather than by
// mutating env.cfg.SessionTicketsDisabled after the fact) because that field,
// like the other cfg fields here, is read unguarded by the handshake
// goroutine once the accept loop is running.
func newResumptionEnv(t *testing.T, ticketsDisabled bool) *resumptionEnv {
	t.Helper()
	pki := newResumptionPKI(t)
	revocation := newRevocationChecker([]*x509.Certificate{pki.ca})
	cfg, err := newTLSConfig(pki.serverCertPEM, pki.caPEM, pki.serverKeyPEM, nil, time.Time{}, false, revocation)
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	env := &resumptionEnv{pki: pki, revocation: revocation, cfg: cfg, verifyCount: new(atomic.Int32), srvResumed: make(chan bool, 16)}
	realNow := time.Now
	env.now.Store(&realNow)

	inner := cfg.VerifyPeerCertificate
	cfg.VerifyPeerCertificate = func(rawCerts [][]byte, chains [][]*x509.Certificate) error {
		env.verifyCount.Add(1)
		return inner(rawCerts, chains)
	}

	// Re-wire WrapSession/UnwrapSession exactly once, before the accept loop
	// starts. The injected clock indirects through env.now so
	// TestResumptionDeclinedWhenCertWindowLapses can advance time later by
	// swapping the atomic pointer instead of re-wiring cfg.
	wireSessionTicketChecks(cfg, time.Time{}, func() time.Time {
		nowFn := env.now.Load()
		return (*nowFn)()
	})
	// Layer the stripWindow override on top of the just-installed WrapSession,
	// still before the accept loop starts. When stripWindow is set,
	// TestResumptionDeclinedWithoutWindowMetadata's tickets skip the window
	// stamp entirely, simulating a garbled/foreign ticket.
	baseWrap := cfg.WrapSession
	cfg.WrapSession = func(cs tls.ConnectionState, ss *tls.SessionState) ([]byte, error) {
		if env.stripWindow.Load() {
			return cfg.EncryptTicket(cs, ss)
		}
		return baseWrap(cs, ss)
	}
	if ticketsDisabled {
		cfg.SessionTicketsDisabled = true
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	env.addr = ln.Addr().String()
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
				env.srvResumed <- tc.ConnectionState().DidResume
				tc.Write([]byte{1})
				buf := make([]byte, 1)
				tc.Read(buf)
			}(c)
		}
	}()
	// The client verifies the fixture CA and loopback server identity.
	env.clientCert = pki.clientCert
	return env
}

func (env *resumptionEnv) dial(t *testing.T, cache tls.ClientSessionCache) (clientResumed, serverResumed bool) {
	t.Helper()
	conn, err := tls.Dial("tcp", env.addr, env.clientConfig(cache))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if v := conn.ConnectionState().Version; v != tls.VersionTLS13 {
		t.Fatalf("negotiated TLS %x, want TLS 1.3", v)
	}
	return conn.ConnectionState().DidResume, <-env.srvResumed
}

func TestResumptionSecondConnectionResumes(t *testing.T) {
	env := newResumptionEnv(t, false)
	cache := tls.NewLRUClientSessionCache(4)

	c1, s1 := env.dial(t, cache)
	if c1 || s1 {
		t.Fatalf("first connection resumed (client=%v server=%v)", c1, s1)
	}
	c2, s2 := env.dial(t, cache)
	if !c2 || !s2 {
		t.Fatalf("second connection did not resume (client=%v server=%v)", c2, s2)
	}
	if n := env.verifyCount.Load(); n != 1 {
		t.Errorf("full ML-DSA verification ran %d times, want exactly 1", n)
	}
}

func TestResumptionDeclinedWhenCertWindowLapses(t *testing.T) {
	env := newResumptionEnv(t, false)
	cache := tls.NewLRUClientSessionCache(4)
	env.dial(t, cache)

	// Swap the clock the installed UnwrapSession checks against to far past
	// the client cert's NotAfter: the server must DECLINE the ticket and
	// complete a FULL handshake — not error out. This only swaps the
	// synchronized env.now atomic pointer; it never re-wires or mutates
	// env.cfg itself, which the accept-loop goroutine is already using.
	future := func() time.Time { return time.Now().Add(3 * 365 * 24 * time.Hour) }
	env.now.Store(&future)
	c2, s2 := env.dial(t, cache)
	if c2 || s2 {
		t.Fatalf("stale-window ticket resumed (client=%v server=%v)", c2, s2)
	}
	if n := env.verifyCount.Load(); n != 2 {
		t.Errorf("full verification ran %d times, want 2 (decline forces re-verify)", n)
	}
}

func TestResumptionDeclinedWithoutWindowMetadata(t *testing.T) {
	env := newResumptionEnv(t, false)
	cache := tls.NewLRUClientSessionCache(4)

	// Issue tickets WITHOUT the window stamp (simulates a garbled/foreign
	// ticket): resumption must be declined, connection must still succeed.
	// This only flips the synchronized env.stripWindow flag consulted by the
	// WrapSession wrapper installed once in newResumptionEnv; env.cfg itself
	// is never mutated after the accept loop starts.
	env.stripWindow.Store(true)
	env.dial(t, cache)
	c2, s2 := env.dial(t, cache)
	if c2 || s2 {
		t.Fatalf("metadata-less ticket resumed (client=%v server=%v)", c2, s2)
	}
}

func TestResumptionTicketsDisabledStillConnects(t *testing.T) {
	// ticketsDisabled must be set before the accept loop starts (see
	// newResumptionEnv), so it is passed in rather than assigned afterwards.
	env := newResumptionEnv(t, true)
	cache := tls.NewLRUClientSessionCache(4)

	env.dial(t, cache)
	c2, s2 := env.dial(t, cache)
	if c2 || s2 {
		t.Fatalf("resumed with tickets disabled (client=%v server=%v)", c2, s2)
	}
}

func TestRevokedCertificateRejectedOnTLSResumption(t *testing.T) {
	env := newResumptionEnv(t, false)
	cache := tls.NewLRUClientSessionCache(4)
	env.dial(t, cache)
	clientResumed, serverResumed := env.dial(t, cache)
	if !clientResumed || !serverResumed {
		t.Fatal("test did not establish TLS resumption")
	}
	leaf, err := x509.ParseCertificate(env.clientCert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Second)
	env.pki.crl.publish(t, env.pki.ca, env.pki.caKey, 2, now, now.Add(time.Hour), leaf.SerialNumber)
	// Expire only the revocation cache. Preserve the live TLS session ticket.
	env.revocation.mu <- struct{}{}
	for key, entry := range env.revocation.cache {
		entry.fetched = time.Time{}
		env.revocation.cache[key] = entry
	}
	<-env.revocation.mu
	before := env.verifyCount.Load()
	conn, err := tls.Dial("tcp", env.addr, env.clientConfig(cache))
	if err == nil {
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(time.Second))
		_, err = conn.Read(make([]byte, 1))
	}
	if err == nil {
		t.Fatal("revoked session resumed successfully")
	}
	if env.verifyCount.Load() != before {
		t.Fatal("test used a full handshake instead of exercising resumed VerifyConnection")
	}
}

func (env *resumptionEnv) clientConfig(cache tls.ClientSessionCache) *tls.Config {
	roots := x509.NewCertPool()
	roots.AddCert(env.pki.ca)
	return &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{env.clientCert}, ClientSessionCache: cache, MinVersion: tls.VersionTLS12}
}
