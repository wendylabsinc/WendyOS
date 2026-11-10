package revocation

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
)

func TestRefreshRevocationAndOutage(t *testing.T) {
	for _, pq := range []bool{false, true} {
		t.Run(map[bool]string{false: "ECDSA", true: "MLDSA"}[pq], func(t *testing.T) {
			p := crltest.New(t, pq)
			leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
			m := strict(t, p.Client)
			path := []*x509.Certificate{leaf.Cert, p.CA}
			now := time.Now()
			for range 2 {
				if err := m.CheckPath(path, now, Session); err != nil {
					t.Fatal(err)
				}
			}
			if p.Hits.Load() != 1 {
				t.Fatal("cache did not reuse CRL")
			}
			revision := m.Revision()
			p.Publish(t, 2, 42)
			m.Refresh(context.Background())
			if m.Revision() == revision {
				t.Fatal("refresh did not invalidate crypto caches")
			}
			if err := m.CheckPath(path, now, Session); !errors.Is(err, ErrRevoked) {
				t.Fatalf("revoked peer accepted: %v", err)
			}
			p.Respond(nil, 503)
			m.Refresh(context.Background())
			if err := m.CheckPath(path, now.Add(2*time.Hour), Session); !errors.Is(err, ErrRevoked) {
				t.Fatalf("outage forgot revocation: %v", err)
			}
			p.Publish(t, 1)
			m.Refresh(context.Background())
			if err := m.CheckPath(path, now, Session); !errors.Is(err, ErrRevoked) {
				t.Fatalf("rollback restored revoked peer: %v", err)
			}
		})
	}
}

func TestRejectInvalidCRLs(t *testing.T) {
	p := crltest.New(t, true)
	other := crltest.New(t, true)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	now := time.Now()
	cases := []struct {
		name    string
		modify  func(*x509.RevocationList)
		signer  *crltest.PKI
		corrupt bool
	}{
		{name: "wrong issuer", signer: other},
		{name: "forged signature", corrupt: true},
		{name: "delta", modify: func(c *x509.RevocationList) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 27}, Value: []byte{2, 1, 1}}}
		}},
		{name: "scoped", modify: func(c *x509.RevocationList) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 28}, Value: []byte{0x30, 0}}}
		}},
		{name: "unknown critical", modify: func(c *x509.RevocationList) {
			c.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}}
		}},
		{name: "indirect entry", modify: func(c *x509.RevocationList) {
			c.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 29}, Value: []byte{0x30, 0}}}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			template := &x509.RevocationList{Number: big.NewInt(3), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour)}
			if tc.modify != nil {
				tc.modify(template)
			}
			signer := tc.signer
			if signer == nil {
				signer = p
			}
			raw := signer.Sign(t, template)
			if tc.corrupt {
				raw[len(raw)-1] ^= 1
			}
			p.Respond(raw, 200)
			if err := strict(t, p.Client).CheckPath([]*x509.Certificate{leaf.Cert, p.CA}, now, Session); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("accepted invalid CRL: %v", err)
			}
		})
	}
}

func offline() *http.Client {
	return &http.Client{Transport: transportFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })}
}

func strict(t testing.TB, client *http.Client) *Manager {
	return configured(t, client, Config{})
}

func configured(t testing.TB, client *http.Client, c Config) *Manager {
	t.Helper()
	m := New(client)
	if err := m.Configure(c); err != nil {
		t.Fatal(err)
	}
	return m
}

// The failure policy is graded: what is refused depends on the decision, not
// on one global switch.
func TestEvidenceLevelsOfflineAndLegacy(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	path := []*x509.Certificate{leaf.Cert, p.CA}
	now := time.Now()
	expired := now.Add(90 * time.Minute) // The fixture CRL lasts one hour.

	m := configured(t, p.Client, Config{Grace: time.Hour})
	if err := m.CheckPath(path, now, Fresh); err != nil {
		t.Fatal(err)
	}
	p.Respond(nil, 503)
	m.Refresh(context.Background())
	if err := m.CheckPath(path, now, Fresh); err != nil {
		t.Fatalf("fresh cache unavailable offline: %v", err)
	}
	// Stale within grace: sessions continue, destructive operations do not.
	if err := m.CheckPath(path, expired, Session); err != nil {
		t.Fatalf("stale evidence within grace refused a session: %v", err)
	}
	if err := m.CheckPath(path, expired, Fresh); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stale evidence allowed a destructive operation: %v", err)
	}
	// Beyond grace: no new operator session, but nothing established closes.
	if err := m.CheckPath(path, expired.Add(2*time.Hour), Session); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("evidence beyond grace admitted a session: %v", err)
	}
	if err := m.CheckPath(path, expired.Add(100*24*time.Hour), RevokedOnly); err != nil {
		t.Fatalf("aged evidence closed an established peer: %v", err)
	}

	// No evidence at all: only a decision that needs evidence fails.
	cold := strict(t, offline())
	if err := cold.CheckPath(path, now, RevokedOnly); err != nil {
		t.Fatalf("missing evidence refused a device or established peer: %v", err)
	}
	if err := cold.CheckPath(path, now, Session); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cold offline cache admitted a session: %v", err)
	}
	// A fleet that sets no grace never refuses on age or absence.
	var warnings int
	open := configured(t, offline(), Config{Grace: -1, Logf: func(string, ...any) { warnings++ }})
	for _, level := range []Level{RevokedOnly, Session, Fresh} {
		if err := open.CheckPath(path, now, level); err != nil {
			t.Fatalf("level %d refused without a configured grace: %v", level, err)
		}
	}
	if warnings != 1 {
		t.Fatalf("missing evidence logged %d times, want one rate-limited warning", warnings)
	}

	legacy := *leaf.Cert
	legacy.CRLDistributionPoints = nil
	if err := cold.CheckPath([]*x509.Certificate{&legacy, p.CA}, now, Fresh); err != nil {
		t.Fatal(err)
	}
}

// A device whose clock restarted behind real time must still use a CRL issued
// "in its future", and an expired CRL still carries its revocations.
func TestClockSkewAndExpiredEvidenceKeepRevocations(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	path := []*x509.Certificate{leaf.Cert, p.CA}
	now := time.Now()
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(3), ThisUpdate: now.Add(30 * 24 * time.Hour), NextUpdate: now.Add(31 * 24 * time.Hour)}), 200)
	if err := strict(t, p.Client).CheckPath(path, now, Fresh); err != nil {
		t.Fatalf("CRL issued after a lagging local clock refused: %v", err)
	}
	p.Respond(p.Sign(t, &x509.RevocationList{Number: big.NewInt(3), ThisUpdate: now.Add(-2 * time.Hour), NextUpdate: now.Add(-time.Hour),
		RevokedCertificateEntries: []x509.RevocationListEntry{{SerialNumber: big.NewInt(42), RevocationTime: now.Add(-2 * time.Hour)}}}), 200)
	// A check that cannot be refused for lack of evidence never waits on the
	// network; it registers the endpoint for the background refresh instead.
	m := strict(t, p.Client)
	if err := m.CheckPath(path, now, RevokedOnly); err != nil || p.Hits.Load() != 1 {
		t.Fatalf("revoked-only check fetched inline or failed: hits=%d err=%v", p.Hits.Load(), err)
	}
	m.Refresh(context.Background())
	if err := m.CheckPath(path, now, RevokedOnly); !errors.Is(err, ErrRevoked) {
		t.Fatalf("expired CRL lost its revocation: %v", err)
	}
}

// A restart must not reset a device to "no evidence", nor lower the floor.
func TestPersistedEvidenceSurvivesRestart(t *testing.T) {
	p := crltest.New(t, true)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	path := []*x509.Certificate{leaf.Cert, p.CA}
	dir := t.TempDir()
	p.Publish(t, 5, 42)
	if err := configured(t, p.Client, Config{Dir: dir}).CheckPath(path, time.Now(), Session); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
	restarted := configured(t, offline(), Config{Dir: dir})
	if err := restarted.CheckPath(path, time.Now(), RevokedOnly); !errors.Is(err, ErrRevoked) {
		t.Fatalf("restart forgot a revocation: %v", err)
	}
	other := p.Issue(t, 43, "spiffe://wendy.sh/tenant/t/device/e")
	if err := restarted.CheckPath([]*x509.Certificate{other.Cert, p.CA}, time.Now(), Fresh); err != nil {
		t.Fatalf("persisted fresh evidence not used offline: %v", err)
	}
	// A valid but older CRL from the network cannot undo the persisted floor.
	online := configured(t, p.Client, Config{Dir: dir})
	p.Publish(t, 4)
	if err := online.CheckPath(path, time.Now(), RevokedOnly); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
	online.Refresh(context.Background())
	if err := online.CheckPath(path, time.Now(), RevokedOnly); !errors.Is(err, ErrRevoked) {
		t.Fatalf("rollback after restart restored a revoked certificate: %v", err)
	}
}

// The operator's machine brings the evidence, so the device needs no network.
func TestStapledEvidenceAdmitsOfflineDevice(t *testing.T) {
	p := crltest.New(t, true)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/user/u")
	chain := []*x509.Certificate{leaf.Cert, p.CA}
	operator := strict(t, p.Client)
	operator.Staple(chain) // Registers the operator's own issuer.
	operator.Refresh(context.Background())
	staple := operator.Staple(chain)
	if len(staple) == 0 {
		t.Fatal("online operator has nothing to staple")
	}

	device := strict(t, offline())
	if err := device.CheckPath(chain, time.Now(), Session); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("offline device admitted an operator without evidence: %v", err)
	}
	// Evidence for the right name from the wrong key vouches for nothing.
	forger := crltest.New(t, true)
	forged, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: big.NewInt(9), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour)}, p.CA, forger.Key)
	if err == nil {
		device.Offer(forged)
		if err := device.CheckPath(chain, time.Now(), Session); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("forged staple admitted an operator: %v", err)
		}
	}
	device.Offer(append(append([]byte(nil), staple...), staple...))
	if err := device.CheckPath(chain, time.Now(), Fresh); err != nil {
		t.Fatalf("stapled evidence refused: %v", err)
	}
	// A staple also delivers bad news, and an older one cannot take it back.
	p.Publish(t, 2, 42)
	operator.Refresh(context.Background())
	device.Offer(operator.Staple(chain))
	if err := device.CheckPath(chain, time.Now(), RevokedOnly); !errors.Is(err, ErrRevoked) {
		t.Fatalf("stapled revocation ignored: %v", err)
	}
	device.Offer(staple)
	if err := device.CheckPath(chain, time.Now(), RevokedOnly); !errors.Is(err, ErrRevoked) {
		t.Fatalf("older staple restored a revoked operator: %v", err)
	}
}

func TestConfiguredMirrorReplacesCertificateSource(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	var asked string
	original := p.Client.Transport
	p.Client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		asked = r.URL.String()
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host, u.Path = "https", "crl.pki.wendy.test", "/crl/tenant/issuer"
		copy.URL = &u
		return original.RoundTrip(copy)
	})
	m := configured(t, p.Client, Config{Mirror: "http://10.0.0.5:8080/pki/"})
	if err := m.CheckPath([]*x509.Certificate{leaf.Cert, p.CA}, time.Now(), Fresh); err != nil {
		t.Fatal(err)
	}
	if asked != "http://10.0.0.5:8080/pki/crl/tenant/issuer" {
		t.Fatalf("fetched %q, want the configured mirror", asked)
	}
	if New(nil).Configure(Config{Mirror: "ftp://mirror"}) == nil {
		t.Fatal("accepted a non-HTTP mirror")
	}
}

func TestPeriodicRefreshAndConcurrentChecks(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	m := strict(t, p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.CheckPath(path, time.Now(), Session); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p.Hits.Load() != 1 {
		t.Fatalf("cold fetch stampede: %d", p.Hits.Load())
	}
	p.Publish(t, 2, 42)
	for _, e := range m.entries {
		e.mu.Lock()
		e.nextAttempt = time.Time{}
		e.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(4 * time.Second)
	for p.Hits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.CheckPath(path, time.Now(), Session); !errors.Is(err, ErrRevoked) {
		t.Fatalf("periodic refresh did not revoke: %v", err)
	}
}

func TestPublicTransportBoundaries(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "fc00::1", "64:ff9b::7f00:1", "2002:7f00:1::", "198.18.0.1"} {
		if publicIP(netip.MustParseAddr(s)) {
			t.Errorf("accepted %s", s)
		}
	}
	for _, s := range []string{"https://user:pass@example.com/crl", "file:///crl", "http://example.com:8080/crl", "https://example.com/crl#x"} {
		if validateURL(s) == nil {
			t.Errorf("accepted %s", s)
		}
	}
	client := publicClient()
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil {
		t.Fatal("environment proxy enabled")
	}
	if client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("redirect enabled")
	}
	if _, err := dialPublic(context.Background(), "tcp", "127.0.0.1:80"); err == nil {
		t.Fatal("private address dialed")
	}
}

func TestAlternateURLCannotRollBackIssuerEvidence(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	// Both URLs name the same full-CRL issuer. A signed older answer from a
	// different mirror must not restore a serial already known to be revoked.
	original := p.Client.Transport
	p.Client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		u := *r.URL
		u.Path = "/crl/tenant/issuer"
		copy.URL = &u
		return original.RoundTrip(copy)
	})
	m := strict(t, p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	p.Publish(t, 2, 42)
	if err := m.CheckPath(path, time.Now(), Session); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
	alternate := *leaf.Cert
	alternate.CRLDistributionPoints = []string{"https://crl.pki.wendy.test/mirror"}
	p.Publish(t, 1)
	if err := m.CheckPath([]*x509.Certificate{&alternate, p.CA}, time.Now(), Session); !errors.Is(err, ErrRevoked) {
		t.Fatalf("alternate URL restored revoked certificate: %v", err)
	}
	p.Publish(t, 2) // Same number, conflicting signed content.
	m.Refresh(context.Background())
	if err := m.CheckPath(path, time.Now(), Session); !errors.Is(err, ErrRevoked) {
		t.Fatalf("conflicting number restored certificate: %v", err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBackgroundRefreshDoesNotBlockFreshChecks(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	m := strict(t, p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	if err := m.CheckPath(path, time.Now(), Session); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	original := p.Client.Transport
	p.Client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		close(started)
		select {
		case <-release:
			return original.RoundTrip(r)
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	done := make(chan struct{})
	go func() { m.Refresh(context.Background()); close(done) }()
	<-started
	checked := make(chan error, 1)
	go func() { err := m.CheckPath(path, time.Now(), Session); checked <- err }()
	select {
	case err := <-checked:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("fresh cache blocked on background HTTP")
	}
	close(release)
	<-done
}

func TestResponseSizeAndMissingSKI(t *testing.T) {
	p := crltest.New(t, true)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	p.Respond(make([]byte, maxCRLBytes+1), 200)
	if err := strict(t, p.Client).CheckPath([]*x509.Certificate{leaf.Cert, p.CA}, time.Now(), Session); !errors.Is(err, ErrUnavailable) {
		t.Fatal("oversized response accepted")
	}
	issuer := *p.CA
	issuer.SubjectKeyId = nil
	aki, err := asn1.Marshal(struct {
		ID []byte `asn1:"optional,tag:0"`
	}{issuerKeyID(&issuer)})
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.RevocationList{Number: big.NewInt(2), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour), ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 35}, Value: aki}}}
	crl, err := x509.ParseRevocationList(p.Sign(t, template))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCRL(crl, &issuer); err != nil {
		t.Fatalf("PKI legacy issuer key ID rejected: %v", err)
	}
}

func TestIssuerHistoryUsesSigningIdentity(t *testing.T) {
	p := crltest.New(t, false)
	wrapper := *p.CA
	wrapper.Raw = []byte("different cross-signed certificate wrapper")
	if issuerCacheKey(&wrapper) != issuerCacheKey(p.CA) {
		t.Fatal("certificate wrapper reset issuer revocation history")
	}
	other := crltest.New(t, false)
	if issuerCacheKey(other.CA) == issuerCacheKey(p.CA) {
		t.Fatal("different issuer keys share history")
	}
}
