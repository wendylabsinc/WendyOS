package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// testLeafCertificate generates a proper leaf (end-entity) certificate for testing.
func testLeafCertificate(t *testing.T, commonName string) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// testCACertificate generates a self-signed CA certificate for testing.
func testCACertificate(t *testing.T, commonName string) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}))

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// TestBuildVerifyPeerCertificate_StandardPathHonorsNotBeforeFloor verifies that
// the NotBefore floor is applied to the standard (RSA/ECDSA) x509 verification
// path through VerifyOptions.CurrentTime — not only the ML-DSA path. A leaf
// whose NotBefore is in the future relative to the real clock is rejected
// without a floor but accepted once the floor advances the effective time to its
// NotBefore. Guards against a regression where the floor is dropped from the
// standard path and devices with stale clocks reject valid client certs again.
func TestBuildVerifyPeerCertificate_StandardPathHonorsNotBeforeFloor(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Floor Test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing CA cert: %v", err)
	}

	// Leaf NotBefore is one hour in the future — "not yet valid" against the real clock.
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	notBefore := time.Now().Add(time.Hour)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "future-leaf"},
		NotBefore:    notBefore,
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating leaf cert: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	caCerts := []*x509.Certificate{caCert}
	rawCerts := [][]byte{leafDER}

	// Without a floor the real clock precedes the leaf's NotBefore → rejected.
	if err := buildVerifyPeerCertificate(caPool, caCerts, nil, time.Time{})(rawCerts, nil); err == nil {
		t.Fatal("expected rejection without floor (leaf not yet valid), got nil")
	}

	// With the floor at the leaf's NotBefore the effective time advances to it → accepted.
	if err := buildVerifyPeerCertificate(caPool, caCerts, nil, notBefore)(rawCerts, nil); err != nil {
		t.Errorf("expected acceptance with floor=%v, got: %v", notBefore, err)
	}
}

// TestBuildVerifyPeerCertificate_ClientCertIssuedAfterFloor covers the case
// where a device's RTC is stuck at epoch (pre-NTP), its notBeforeFloor is set
// to the device cert's NotBefore (e.g. June 17), but the connecting CLI user
// cert was issued after that floor (e.g. June 23). Without the fix, effectiveNow
// = floor (June 17) < client cert NotBefore (June 23), which causes a spurious
// "certificate not yet valid" rejection.
func TestBuildVerifyPeerCertificate_ClientCertIssuedAfterFloor(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Cloud Root CA"},
		NotBefore:             time.Now().Add(-30 * 24 * time.Hour),
		NotAfter:              time.Now().Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parsing CA cert: %v", err)
	}

	// Simulate device provisioned 6 days ago → floor = 6 days ago.
	deviceProvisionedAt := time.Now().Add(-6 * 24 * time.Hour)
	notBeforeFloor := deviceProvisionedAt

	// Client cert issued today (after the floor). NotBefore = now (within the last minute).
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	clientNotBefore := time.Now().Add(-time.Minute) // issued just now
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "wendy/user/testuser"},
		NotBefore:    clientNotBefore,
		NotAfter:     clientNotBefore.Add(365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating leaf cert: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	caCerts := []*x509.Certificate{caCert}
	rawCerts := [][]byte{leafDER}

	// Simulate device clock stuck at epoch — real clock is far behind the floor.
	// (We can't override time.Now, but we can verify that with the floor at
	// notBeforeFloor and the real clock as-is the cert is accepted. The test
	// exercises the branch where realNow < notBeforeFloor is NOT true since we
	// can't freeze time.Now in unit tests, so we confirm the cert is accepted
	// when the floor is set to a time before the cert's NotBefore — the normal
	// post-fix path — and rejected when the floor is behind the NotBefore without
	// the fix, which the preceding TestBuildVerifyPeerCertificate_StandardPathHonorsNotBeforeFloor
	// already covers. What this test adds: the floor is EARLIER than the client
	// cert's NotBefore, confirming the cert is accepted regardless.)
	if err := buildVerifyPeerCertificate(caPool, caCerts, nil, notBeforeFloor)(rawCerts, nil); err != nil {
		t.Errorf("client cert issued after floor should be accepted, got: %v", err)
	}
}

func TestNewTLSConfigEmptyChainReturnsError(t *testing.T) {
	certPEM, keyPEM := testLeafCertificate(t, "leaf")

	_, err := NewTLSConfig(certPEM, "", keyPEM, nil, time.Time{})
	if err == nil {
		t.Fatal("NewTLSConfig() expected error for empty chainPEM, got nil")
	}
}

// testCAKeyPair generates a self-signed CA certificate/key and also returns
// its PEM encoding, so callers can both sign peer leaves with it (needs the
// *x509.Certificate + key) and use it as a chainPEM (needs the PEM).
func testCAKeyPair(t *testing.T) (cert *x509.Certificate, key *ecdsa.PrivateKey, certPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating CA key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: "Test Peer-Pinning CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating CA cert: %v", err)
	}
	cert, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing CA cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	return cert, key, certPEM
}

// testPeerLeafRaw signs a leaf certificate with ca/caKey carrying urn as a SAN
// URI (the wendy org/entity identity format certs.IdentityFromCert parses),
// returning the raw DER bytes as they would appear in tls.ConnectionState's
// PeerCertificates during a real handshake.
func testPeerLeafRaw(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, urn string) []byte {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	u, err := url.Parse(urn)
	if err != nil {
		t.Fatalf("parsing urn %q: %v", urn, err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating leaf cert: %v", err)
	}
	return der
}

// TestNewClientTLSConfigExpectingPeerPinsIdentity covers the LAN-dial peer
// pinning fix (C-final-review Fix 2): mDNS advertises a device's cloud asset
// ID unauthenticated, so meshDialLAN must not trust chain validity alone —
// any device holding a cert signed by the same CA (a different asset in the
// same org, or a user cert) could otherwise impersonate the mDNS-advertised
// target and MITM the connection. NewClientTLSConfigExpectingPeer's
// VerifyPeerCertificate must accept only a peer leaf whose wendy identity is
// exactly the expected asset in the expected org.
func TestNewClientTLSConfigExpectingPeerPinsIdentity(t *testing.T) {
	ca, caKey, chainPEM := testCAKeyPair(t)
	// The config's own identity supplies the expected tenant: the peer's scope
	// is compared against the scope of the certificate we ourselves present, so
	// the dialer has to be an org-7 asset for the org half of the pin to bite.
	ownCertPEM, ownKeyPEM := testLeafCertificate(t, "sh/wendy/7/1")

	cfg, err := NewClientTLSConfigExpectingPeer(ownCertPEM, chainPEM, ownKeyPEM, nil, "215")
	if err != nil {
		t.Fatalf("NewClientTLSConfigExpectingPeer: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("VerifyPeerCertificate must not be nil (InsecureSkipVerify relies on it)")
	}

	t.Run("matching asset in expected org is accepted", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "urn:wendy:org:7:asset:215")
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err != nil {
			t.Fatalf("expected acceptance, got: %v", err)
		}
	})

	t.Run("wrong asset ID in the right org is rejected", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "urn:wendy:org:7:asset:999")
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for wrong asset ID, got nil")
		}
	})

	t.Run("right asset ID in the wrong org is rejected", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "urn:wendy:org:9:asset:215")
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for wrong org, got nil")
		}
	})

	t.Run("user certificate for the expected identity's org is rejected", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "urn:wendy:org:7:user:215")
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for a user certificate, got nil")
		}
	})

	t.Run("chain validity is still enforced (untrusted CA rejected)", func(t *testing.T) {
		otherCA, otherKey, _ := testCAKeyPair(t)
		leaf := testPeerLeafRaw(t, otherCA, otherKey, "urn:wendy:org:7:asset:215")
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for a leaf signed by an untrusted CA, got nil")
		}
	})
}

func TestNewTLSConfigServesOnlyLeafCertificate(t *testing.T) {
	leafPEM, keyPEM := testLeafCertificate(t, "leaf")
	chainPEM, _ := testCACertificate(t, "chain")

	tlsConfig, err := NewTLSConfig(leafPEM+"\n"+chainPEM, chainPEM, keyPEM, nil, time.Time{})
	if err != nil {
		t.Fatalf("NewTLSConfig() error = %v", err)
	}

	if got := len(tlsConfig.Certificates[0].Certificate); got != 1 {
		t.Fatalf("served certificate chain length = %d; want 1", got)
	}
}
