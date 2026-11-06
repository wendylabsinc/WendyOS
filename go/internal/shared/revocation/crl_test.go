package revocation

import (
	"context"
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
			m := New(p.Client)
			path := []*x509.Certificate{leaf.Cert, p.CA}
			now := time.Now()
			for range 2 {
				if _, err := m.CheckPath(path, now); err != nil {
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
			if _, err := m.CheckPath(path, now); !errors.Is(err, ErrRevoked) {
				t.Fatalf("revoked peer accepted: %v", err)
			}
			p.Respond(nil, 503)
			m.Refresh(context.Background())
			if _, err := m.CheckPath(path, now.Add(2*time.Hour)); !errors.Is(err, ErrRevoked) {
				t.Fatalf("outage forgot revocation: %v", err)
			}
			p.Publish(t, 1)
			m.Refresh(context.Background())
			if _, err := m.CheckPath(path, now); !errors.Is(err, ErrRevoked) {
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
		{name: "expired", modify: func(c *x509.RevocationList) {
			c.ThisUpdate = now.Add(-2 * time.Hour)
			c.NextUpdate = now.Add(-time.Hour)
		}},
		{name: "future", modify: func(c *x509.RevocationList) { c.ThisUpdate = now.Add(time.Minute) }},
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
			if _, err := New(p.Client).CheckPath([]*x509.Certificate{leaf.Cert, p.CA}, now); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("accepted invalid CRL: %v", err)
			}
		})
	}
}

func TestExpiryOfflineAndLegacy(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	m := New(p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	now := time.Now()
	until, err := m.CheckPath(path, now)
	if err != nil {
		t.Fatal(err)
	}
	p.Respond(nil, 503)
	m.Refresh(context.Background())
	if _, err := m.CheckPath(path, now); err != nil {
		t.Fatalf("fresh cache unavailable offline: %v", err)
	}
	if _, err := m.CheckPath(path, until); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("accepted expired evidence: %v", err)
	}
	if _, err := New(p.Client).CheckPath(path, now); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cold offline cache accepted: %v", err)
	}
	legacy := *leaf.Cert
	legacy.CRLDistributionPoints = nil
	if _, err := m.CheckPath([]*x509.Certificate{&legacy, p.CA}, now); err != nil {
		t.Fatal(err)
	}
}

func TestPeriodicRefreshAndConcurrentChecks(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	m := New(p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := m.CheckPath(path, time.Now()); err != nil {
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
	if _, err := m.CheckPath(path, time.Now()); !errors.Is(err, ErrRevoked) {
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
	m := New(p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	p.Publish(t, 2, 42)
	if _, err := m.CheckPath(path, time.Now()); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
	alternate := *leaf.Cert
	alternate.CRLDistributionPoints = []string{"https://crl.pki.wendy.test/mirror"}
	p.Publish(t, 1)
	if _, err := m.CheckPath([]*x509.Certificate{&alternate, p.CA}, time.Now()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("alternate URL restored revoked certificate: %v", err)
	}
	p.Publish(t, 2) // Same number, conflicting signed content.
	m.Refresh(context.Background())
	if _, err := m.CheckPath(path, time.Now()); !errors.Is(err, ErrRevoked) {
		t.Fatalf("conflicting number restored certificate: %v", err)
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBackgroundRefreshDoesNotBlockFreshChecks(t *testing.T) {
	p := crltest.New(t, false)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/t/device/d")
	m := New(p.Client)
	path := []*x509.Certificate{leaf.Cert, p.CA}
	if _, err := m.CheckPath(path, time.Now()); err != nil {
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
	go func() { _, err := m.CheckPath(path, time.Now()); checked <- err }()
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
	if _, err := New(p.Client).CheckPath([]*x509.Certificate{leaf.Cert, p.CA}, time.Now()); !errors.Is(err, ErrUnavailable) {
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
	if err := validateCRL(crl, &issuer, time.Now()); err != nil {
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
