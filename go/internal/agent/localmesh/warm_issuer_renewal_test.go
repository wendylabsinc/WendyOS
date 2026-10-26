package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// A leaf can retain its exact DER/key while its issuing intermediate is
// renewed with the same issuer key. Current identity lifetime includes every
// presented issuer, so the old and new chains have different valid bounds.
func sameLeafIssuerRenewalFixture(t *testing.T, renewedLifetime time.Duration) warmRenewalFixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	root := newManifestTestAuthority(t, now)
	issuerKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var issuers []*x509.Certificate
	var issuerPEMs []string
	for i, lifetime := range []time.Duration{30 * time.Second, renewedLifetime} {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: "renewed test issuer"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(lifetime), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, root.cert, &issuerKey.PublicKey, root.key)
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
	uri, _ := url.Parse(testPrincipal(testTenantUUID, testDeviceAID))
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
	var credentials []*Credentials
	for _, issuerPEM := range issuerPEMs {
		c, err := NewCredentials(leafPEM, issuerPEM+root.pem, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		credentials = append(credentials, c)
	}
	old, renewed := credentials[0], credentials[1]
	if string(old.Certificate.Certificate[0]) != string(renewed.Certificate.Certificate[0]) || sameChain(old.Certificate.Certificate, renewed.Certificate.Certificate) {
		t.Fatal("fixture did not keep exact leaf while changing chain")
	}
	// Use an independent peer whose configured trust contains only the root.
	// If the renewed intermediate is itself configured as a trust anchor,
	// selected-path verification can legitimately use it for the old leaf too,
	// even before the peer has fetched the advertised replacement issuer.
	verify := root.issue(t, now, nil, now.Add(2*time.Hour)).Verify
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
	return warmRenewalFixture{now: now, old: old, renewed: renewed, verify: verify}
}

func TestWarmSameLeafRenewedIssuerRecoversEffectiveLifetime(t *testing.T) {
	f := sameLeafIssuerRenewalFixture(t, time.Hour)
	receiver, d, _ := f.receiver(t)
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Certificate.Request == nil || requests[0].Certificate.Request.Hash == certificateHash(f.renewed.Certificate.Certificate[0]) {
		t.Fatal("test did not fetch renewed issuer behind already cached leaf")
	}
	w := f.wire(t, f.now)
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil || len(changed) != 0 {
		t.Fatalf("same-leaf renewed issuer closed warm link before chain completed: %v", err)
	}
	assertWarmRouteAuthorities(t, d, f.now)
	changed, err := driveWarmCertificateFetch(f.source(t, f.renewed), receiver, requests, f.now)
	if err != nil || len(changed) != 1 || len(d.Snapshot(f.now)) != 1 {
		t.Fatal("verified renewed issuer did not recover original leaf signature", err)
	}
	assertWarmRouteAuthorities(t, d, f.now, w)
}

func TestWarmSameLeafIssuerDoesNotDeferInvalidSignature(t *testing.T) {
	f := sameLeafIssuerRenewalFixture(t, time.Hour)
	receiver, d, _ := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	m := routeManifest(f.renewed.Device, f.now)
	m.Revision = 2
	m.Expires = f.now.Add(10 * time.Second).UnixMilli()
	w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	w.Signature[0] ^= 1
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); !errors.Is(err, ErrInvalidManifestSignature) || len(changed) != 0 {
		t.Fatal("same-leaf issuer renewal widened signature deferral", err)
	}
	if len(receiver.pendingRenewals) != 0 || len(d.Snapshot(f.now)) != 0 {
		t.Fatal("invalid same-leaf signature gained pending authority")
	}
	assertWarmRouteAuthorities(t, d, f.now)
}

func TestWarmSameLeafRenewedIssuerStillEnforcesVerifiedLifetime(t *testing.T) {
	f := sameLeafIssuerRenewalFixture(t, time.Minute)
	receiver, d, _ := f.receiver(t)
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil || len(changed) != 0 {
		t.Fatal("active changed-chain lifetime was not isolated", err)
	}
	if _, err = driveWarmCertificateFetch(f.source(t, f.renewed), receiver, requests, f.now); !errors.Is(err, ErrManifestOutlivesCertificate) {
		t.Fatal("verified new issuer lifetime was relaxed", err)
	}
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 || len(d.Snapshot(f.now)) != 0 {
		t.Fatal("overlong same-leaf renewed chain gained authority")
	}
	assertWarmRouteAuthorities(t, d, f.now)
}

func TestWarmSameLeafLifetimeWaitStillRejectsCorruptedSignature(t *testing.T) {
	f := sameLeafIssuerRenewalFixture(t, time.Hour)
	receiver, d, _ := f.receiver(t)
	requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
	if err != nil {
		t.Fatal(err)
	}
	w := f.wire(t, f.now)
	w.Signature[0] ^= 1
	// The old30-second issuer bound wins before the signature check. Waiting
	// for its exact replacement must not convert this corrupt wire into trust.
	if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil || len(changed) != 0 {
		t.Fatal("active lifetime mismatch was not isolated", err)
	}
	if _, err = driveWarmCertificateFetch(f.source(t, f.renewed), receiver, requests, f.now); !errors.Is(err, ErrInvalidManifestSignature) {
		t.Fatal("verified issuer completion relaxed signature verification", err)
	}
	if len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 || len(d.Snapshot(f.now)) != 0 {
		t.Fatal("corrupt same-leaf wire gained authority")
	}
	assertWarmRouteAuthorities(t, d, f.now)
}
