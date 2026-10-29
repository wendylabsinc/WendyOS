package meshcatalog

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// A leaf can retain its exact DER/key while its issuing intermediate is
// renewed with the same issuer key. Current identity lifetime includes every
// presented issuer, so the old and new chains have different valid bounds.
func serviceIssuerRenewalFixture(t *testing.T, renewedLifetime time.Duration) (time.Time, *localmesh.Credentials, *localmesh.Credentials, localmesh.IdentityVerifier) {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "service root"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, &rootKey.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	rootCert, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER}))
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var issuers []*x509.Certificate
	var issuerPEMs []string
	for i, lifetime := range []time.Duration{30 * time.Second, renewedLifetime} {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: "renewed test issuer"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(lifetime), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, rootCert, &issuerKey.PublicKey, rootKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		issuers = append(issuers, cert)
		issuerPEMs = append(issuerPEMs, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse(p533)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(4), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	leaf, err := x509.CreateCertificate(rand.Reader, tmpl, issuers[0], &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	var credentials []*localmesh.Credentials
	for i, issuerPEM := range issuerPEMs {
		trust := issuerPEM + rootPEM
		if i == 0 {
			// The own-trust-bound negative pins only the old issuer. Including
			// the root would legitimately permit the presented renewed issuer.
			trust = issuerPEM
		}
		c, err := localmesh.NewCredentials(leafPEM, trust, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, c)
	}
	old, renewed := credentials[0], credentials[1]
	if string(old.Certificate.Certificate[0]) != string(renewed.Certificate.Certificate[0]) || bytes.Equal(old.Certificate.Certificate[1], renewed.Certificate.Certificate[1]) {
		t.Fatal("fixture did not keep exact leaf while changing chain")
	}
	// An independent root-only peer must fetch each presented intermediate.
	// Configuring the renewed same-key issuer would already provide a valid
	// alternate path for the old leaf, defeating the warm-fetch precondition.
	observerTemplate := *tmpl
	observerTemplate.SerialNumber = big.NewInt(5)
	observerDER, err := x509.CreateCertificate(rand.Reader, &observerTemplate, rootCert, &key.PublicKey, rootKey)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := localmesh.NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: observerDER})), rootPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	verify := observer.Verify
	oldID, err := verify(old.Certificate.Certificate, now)
	if err != nil {
		t.Fatal(err)
	}
	newID, err := verify(renewed.Certificate.Certificate, now)
	if err != nil {
		t.Fatal(err)
	}
	if !oldID.NotAfter.Equal(now.Add(30*time.Second)) || !newID.NotAfter.Equal(now.Add(renewedLifetime)) {
		t.Fatal("fixture did not isolate effective issuer lifetime", oldID.NotAfter, newID.NotAfter)
	}
	return now, old, renewed, verify
}

func TestWarmServiceSameLeafIssuerRenewal(t *testing.T) {
	for _, mode := range []string{"renewed", "still-too-short", "invalid-signature", "missing-descriptor", "same-descriptor", "own-trust-bound"} {
		t.Run(mode, func(t *testing.T) {
			lifetime := time.Hour
			if mode == "still-too-short" {
				lifetime = time.Minute
			}
			now, old, current, verify := serviceIssuerRenewalFixture(t, lifetime)
			if mode == "own-trust-bound" {
				verify = old.Verify
			}
			cache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), verify, now)
			if err != nil {
				t.Fatal(err)
			}
			dest, err := NewCatalog("default", old.Device, 32, old, cache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, now)
			if err != nil {
				t.Fatal(err)
			}
			sourceCache, err := localmesh.OpenIdentityCache("", localmesh.DefaultCacheLimits(), current.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			source, err := NewCatalog("default", current.Device, 32, current, sourceCache, func(string, string, uint16) error { return nil }, nil, func([]Receipt) error { return nil }, now)
			if err != nil {
				t.Fatal(err)
			}
			sender, receiver := NewSynchronizer(source, sourceCache), NewSynchronizer(dest, cache)
			defer sender.Close()
			defer receiver.Close()
			descriptor := localmesh.DescribeCertificates(p533, current.Certificate.Certificate)
			if mode == "same-descriptor" {
				descriptor = localmesh.DescribeCertificates(p533, old.Certificate.Certificate)
			}
			var requests []Message
			if mode != "missing-descriptor" {
				requests, _, err = receiver.Receive(Message{Kind: "certificate", Certificate: descriptor}, now)
				if err != nil {
					t.Fatal(err)
				}
			}
			spec := testSpec()
			key := Key{"default", p533, spec.AppID, spec.ServiceID}
			record, err := spec.record(key, 1, now)
			if err != nil {
				t.Fatal(err)
			}
			wire, err := Sign(record, current.Certificate.Certificate, current.Signer, now)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "invalid-signature" {
				wire.Signature[0] ^= 1
			}
			_, changed, err := receiver.Receive(Message{Kind: "record", Record: &wire}, now)
			if mode == "invalid-signature" || mode == "missing-descriptor" || mode == "same-descriptor" {
				if err == nil || receiver.PendingIdentity() || len(changed) != 0 {
					t.Fatal("same-leaf issuer widened ordinary failure", err)
				}
				return
			}
			if err != nil || len(changed) != 0 || !receiver.PendingIdentity() {
				t.Fatal("different-chain lifetime was not deferred", err)
			}
			if len(requests) != 1 || requests[0].Certificate.Request == nil || requests[0].Certificate.Request.Hash == wire.Fingerprint {
				t.Fatal("fixture did not fetch only renewed issuer")
			}
			var initial []Message
			for _, request := range requests {
				out, _, err := sender.Receive(request, now)
				if err != nil {
					t.Fatal(err)
				}
				initial = append(initial, out...)
			}
			changed, err = exchangeWarmServiceMessages(sender, receiver, initial, now)
			if mode == "still-too-short" || mode == "own-trust-bound" {
				if err == nil || len(changed) != 0 || len(dest.Snapshot(now)) != 0 || len(dest.history) != 0 {
					t.Fatal("peer hint relaxed current certificate/trust lifetime", err)
				}
				return
			}
			if err != nil || len(changed) != 1 || receiver.PendingIdentity() || len(dest.Snapshot(now)) != 1 {
				t.Fatal("verified renewed issuer did not recover same-leaf service", err)
			}
			if len(dest.Snapshot(now.Add(spec.Lease+time.Second))) != 0 || dest.history[key].Generation != 1 {
				t.Fatal("issuer renewal extended signed lease or lost receipt")
			}
		})
	}
}
