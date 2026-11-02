package meshsession

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func appClockFixture(t *testing.T, base, leafNB, leafNA, issuerNA time.Time, policy certs.CertificateTimePolicy) (*localmesh.Credentials, *localmesh.Credentials, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: base.Add(-48 * time.Hour), NotAfter: issuerNA, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	issue := func(id string) *localmesh.Credentials {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		uri, _ := url.Parse(fixturePrincipal(id))
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: leafNB, NotAfter: leafNA, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := x509.MarshalECPrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		c, err := localmesh.NewCredentialsWithTimePolicy(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded})), policy)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	return issue(fixtureDeviceA), issue(fixtureDeviceB), chain
}
func appPeerState(t *testing.T, c *localmesh.Credentials) tls.ConnectionState {
	t.Helper()
	state := tls.ConnectionState{}
	for _, der := range c.Certificate.Certificate {
		certificate, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		state.PeerCertificates = append(state.PeerCertificates, certificate)
	}
	return state
}
func TestAppSessionDeadlineProjectsPolicyRemaining(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	base := now.Add(time.Hour)
	p := certs.CertificateTimePolicy{Now: time.Now, Floor: func() time.Time { return base }, Uncertain: func() bool { return true }}
	for _, issuerFirst := range []bool{false, true} {
		leafNA, issuerNA := base.Add(30*time.Second), base.Add(time.Hour)
		if issuerFirst {
			leafNA, issuerNA = issuerNA, leafNA
		}
		a, b, _ := appClockFixture(t, base, now.Add(-time.Hour), leafNA, issuerNA, p)
		deadline, err := verifiedSessionExpiry(a, appPeerState(t, b))
		if err != nil {
			t.Fatal(err)
		}
		if remaining := time.Until(deadline); remaining > 31*time.Second || remaining < 28*time.Second {
			t.Fatalf("physical deadline %v instead of selected policy remaining", remaining)
		}
	}
}
func TestAppSessionGuardOnlyShortensAndLatchesFailure(t *testing.T) {
	base := time.Now().Truncate(time.Second).Add(time.Hour)
	var clock atomic.Value
	clock.Store(base)
	var uncertain atomic.Bool
	uncertain.Store(true)
	p := certs.CertificateTimePolicy{Now: time.Now, Floor: func() time.Time { return clock.Load().(time.Time) }, Uncertain: uncertain.Load}
	a, b, _ := appClockFixture(t, base, base.Add(time.Minute), base.Add(30*time.Minute), base.Add(time.Hour), p)
	state := appPeerState(t, b)
	guard, err := newSessionLifetime(a, state)
	if err != nil {
		t.Fatal(err)
	}
	original, _ := guard.current()
	// Revalidation owns the original peer bytes, not mutable TLS-state slices.
	state.PeerCertificates[0].Raw[0] ^= 1
	if _, err := guard.current(); err != nil {
		t.Fatal("retained mutable peer DER", err)
	}
	clock.Store(base.Add(-time.Minute))
	if next, err := guard.current(); err != nil || next.After(original) {
		t.Fatal("backwards correction extended initial cap", next, original, err)
	}
	uncertain.Store(false)
	if _, err := guard.current(); err == nil {
		t.Fatal("certainty-off left not-yet-valid path live")
	}
	uncertain.Store(true)
	if _, err := guard.current(); err == nil {
		t.Fatal("invalidated guard resurrected")
	}
}
func TestAppSessionGuardWatcherStopsOnSetupCancellation(t *testing.T) {
	stopped := make(chan struct{})
	guard := &sessionLifetime{deadline: time.Now().Add(time.Hour), check: func() (time.Time, error) { return time.Now().Add(time.Hour), nil }}
	var expired atomic.Bool
	done := guard.watch(stopped, func() { expired.Store(true) })
	close(stopped)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("setup cancellation leaked watcher")
	}
	if expired.Load() {
		t.Fatal("cancellation called credential expiry")
	}
}
func TestAppSessionsRetireIdleAfterPolicyChange(t *testing.T) {
	for _, transport := range []string{"TLS", "QUIC"} {
		for _, side := range []string{"client", "server"} {
			for _, change := range []string{"expiry", "certainty"} {
				t.Run(transport+"/"+side+"/"+change, func(t *testing.T) {
					base := time.Now().Truncate(time.Second).Add(time.Hour)
					var clock atomic.Value
					clock.Store(base)
					var uncertain atomic.Bool
					uncertain.Store(true)
					dynamic := certs.CertificateTimePolicy{Now: time.Now, Floor: func() time.Time { return clock.Load().(time.Time) }, Uncertain: uncertain.Load}
					static := certs.CertificateTimePolicy{Now: time.Now, Floor: func() time.Time { return base }, Uncertain: func() bool { return true }}
					nb := base.Add(-time.Hour)
					if change == "certainty" {
						nb = base.Add(time.Minute)
					}
					a, b, chain := appClockFixture(t, base, nb, base.Add(30*time.Minute), base.Add(time.Hour), dynamic)
					// Recreate just the unobserved endpoint under a static equivalent
					// policy, so each transport's client and server guards are independent.
					staticPeer := func(c *localmesh.Credentials) *localmesh.Credentials {
						key, err := x509.MarshalECPrivateKey(c.Certificate.PrivateKey.(*ecdsa.PrivateKey))
						if err != nil {
							t.Fatal(err)
						}
						copy, err := localmesh.NewCredentialsWithTimePolicy(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Certificate.Certificate[0]})), chain, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: key})), static)
						if err != nil {
							t.Fatal(err)
						}
						return copy
					}
					if side == "client" {
						b = staticPeer(b)
					} else {
						a = staticPeer(a)
					}
					port := tlsEchoApp(t)
					var flow net.Conn
					var pooled *Client
					if transport == "TLS" {
						_, address := startTLSAppTest(t, b, allowPort(func(p uint16) bool { return p == port }))
						var err error
						flow, err = dialTLS(context.Background(), a, b.Device, address, port)
						if err != nil {
							t.Fatal(err)
						}
					} else {
						address := lifecycleServer(t, b, allowPort(func(p uint16) bool { return p == port }), true)
						pooled = lifecycleClient(t, a)
						flow = lifecycleDial(t, pooled, b.Device, address, port)
					}
					defer flow.Close()
					if change == "expiry" {
						clock.Store(base.Add(2 * time.Hour))
					} else {
						uncertain.Store(false)
					}
					_ = flow.SetReadDeadline(time.Now().Add(2500 * time.Millisecond))
					var one [1]byte
					_, err := flow.Read(one[:])
					if err == nil {
						t.Fatal("idle connection survived policy change")
					}
					if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
						t.Fatal("only caller timeout retired session", err)
					}
					if pooled != nil {
						pooled.mu.Lock()
						entry := pooled.peers[b.Device]
						pooled.mu.Unlock()
						if entry != nil {
							select {
							case <-entry.conn.Context().Done():
							case <-time.After(time.Second):
								t.Fatal("remote expiry did not close pooled connection")
							}
							if _, err := entry.lifetime.current(); err == nil {
								t.Fatal("pool retained live expired guard")
							}
						}
					}
				})
			}
		}
	}
}
