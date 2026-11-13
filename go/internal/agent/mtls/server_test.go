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
	"net/url"
	"testing"

	"github.com/google/uuid"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestPKIDeviceTLSRequiresSameTenantOnFullAndResumedSessions(t *testing.T) {
	for _, deviceName := range []string{"sim", "fleet/sim"} {
		t.Run(deviceName, func(t *testing.T) {
			const tenant = "11111111-1111-4111-8111-111111111111"
			certPEM, keyPEM := testLeafCertificate(t, "device")
			block, _ := pem.Decode([]byte(certPEM))
			leaf, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			keyBlock, _ := pem.Decode([]byte(keyPEM))
			key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			principal, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/device/" + deviceName)
			leaf.URIs = []*url.URL{principal}
			der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, &key.PublicKey, key)
			if err != nil {
				t.Fatal(err)
			}
			certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
			caPEM, _ := testCACertificate(t, "root")
			cfg, err := NewTLSConfig(certPEM, caPEM, keyPEM, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.VerifyConnection == nil {
				t.Fatal("PKI tenant gate was not installed")
			}
			for _, resumed := range []bool{false, true} {
				for _, tc := range []struct {
					principal string
					wantErr   bool
				}{
					{"spiffe://wendy.sh/tenant/" + tenant + "/operator/alice", false},
					{"spiffe://wendy.sh/tenant/" + tenant + "/device/peer", false},
					{"spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/operator/alice", true},
					{"urn:wendy:org:7:user:alice", true},
				} {
					u, _ := url.Parse(tc.principal)
					cs := tls.ConnectionState{DidResume: resumed, PeerCertificates: []*x509.Certificate{{URIs: []*url.URL{u}}}}
					if err := cfg.VerifyConnection(cs); (err != nil) != tc.wantErr {
						t.Errorf("principal=%s resumed=%v error=%v", tc.principal, resumed, err)
					}
				}
			}
		})
	}
}

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
func testPeerLeafRaw(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, urn string, extra ...string) []byte {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating leaf key: %v", err)
	}
	var uris []*url.URL
	for _, raw := range append([]string{urn}, extra...) {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing urn %q: %v", raw, err)
		}
		uris = append(uris, u)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "peer"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("creating leaf cert: %v", err)
	}
	return der
}

// testDeviceLeafPEM mints a leaf whose URI SAN is a SPIFFE device principal —
// the shape a v2-enrolled agent's credential carries.
func testDeviceLeafPEM(t *testing.T, principal string) (certPEM, keyPEM string) {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatalf("parsing principal %q: %v", principal, err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &leafKey.PublicKey, leafKey)
	if err != nil {
		t.Fatalf("creating leaf: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	return certPEM, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

// TestNewClientTLSConfigExpectingDevicePinsPrincipal covers the LAN-dial peer
// pinning: mDNS advertises a device's SPIFFE principal unauthenticated, so
// meshDialLAN must not trust chain validity alone — any device holding a cert
// signed by the same CA could otherwise impersonate the mDNS-advertised
// target and MITM the connection. VerifyPeerCertificate must accept only a
// peer leaf whose device principal is exactly the dialed device.
func TestNewClientTLSConfigExpectingDevicePinsPrincipal(t *testing.T) {
	const tenant1 = "11111111-1111-4111-8111-111111111111"
	const tenant2 = "22222222-2222-4222-8222-222222222222"
	const device1 = "33333333-3333-4333-8333-333333333333"
	const device2 = "44444444-4444-4444-8444-444444444444"

	ca, caKey, chainPEM := testCAKeyPair(t)
	ownCertPEM, ownKeyPEM := testDeviceLeafPEM(t, "spiffe://wendy.sh/tenant/"+tenant1+"/device/"+device2)
	want, err := meshid.ParseDevice(mustTenant(t, tenant1), device1)
	if err != nil {
		t.Fatalf("parsing want device: %v", err)
	}

	cfg, err := NewClientTLSConfigExpectingDevice(ownCertPEM, chainPEM, ownKeyPEM, nil, want)
	if err != nil {
		t.Fatalf("NewClientTLSConfigExpectingDevice: %v", err)
	}
	if cfg.VerifyPeerCertificate == nil {
		t.Fatal("VerifyPeerCertificate must not be nil (InsecureSkipVerify relies on it)")
	}

	t.Run("matching device principal is accepted", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "spiffe://wendy.sh/tenant/"+tenant1+"/device/"+device1)
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err != nil {
			t.Fatalf("expected acceptance, got: %v", err)
		}
	})

	t.Run("different device UUID is rejected", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "spiffe://wendy.sh/tenant/"+tenant1+"/device/"+device2)
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for wrong device UUID, got nil")
		}
	})

	t.Run("same device UUID in a foreign tenant is rejected", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "spiffe://wendy.sh/tenant/"+tenant2+"/device/"+device1)
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for foreign tenant, got nil")
		}
	})

	t.Run("legacy asset certificate is rejected", func(t *testing.T) {
		leaf := testPeerLeafRaw(t, ca, caKey, "urn:wendy:org:7:asset:215")
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for a legacy asset certificate, got nil")
		}
	})

	t.Run("dialed device outside own tenant is rejected", func(t *testing.T) {
		foreign, err := meshid.ParseDevice(mustTenant(t, tenant2), device1)
		if err != nil {
			t.Fatalf("parsing foreign device: %v", err)
		}
		foreignCfg, err := NewClientTLSConfigExpectingDevice(ownCertPEM, chainPEM, ownKeyPEM, nil, foreign)
		if err != nil {
			t.Fatalf("NewClientTLSConfigExpectingDevice(foreign): %v", err)
		}
		// The peer leaf matches `foreign` exactly; the own-tenant guard must
		// still refuse the dial because this device lives in tenant1.
		leaf := testPeerLeafRaw(t, ca, caKey, "spiffe://wendy.sh/tenant/"+tenant2+"/device/"+device1)
		if err := foreignCfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for a dialed device outside own tenant, got nil")
		}
	})

	t.Run("chain validity is still enforced (untrusted CA rejected)", func(t *testing.T) {
		otherCA, otherKey, _ := testCAKeyPair(t)
		leaf := testPeerLeafRaw(t, otherCA, otherKey, "spiffe://wendy.sh/tenant/"+tenant1+"/device/"+device1)
		if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
			t.Fatal("expected rejection for a leaf signed by an untrusted CA, got nil")
		}
	})
}

func mustTenant(t *testing.T, text string) meshid.Tenant {
	t.Helper()
	tenant, err := meshid.ParseTenant(text)
	if err != nil {
		t.Fatalf("parsing tenant %q: %v", text, err)
	}
	return tenant
}

// TestVerifyPeerDevice covers the device-to-device rule (WDY-3919): one id,
// the cert's device UUID, matched against the roster entry for the dialed
// name, in the caller's own tenant.
func TestVerifyPeerDevice(t *testing.T) {
	const (
		tenant      = "2558fd76-afc7-466e-9613-6b715296a526"
		otherTenant = "7d1f3f0e-9c1b-4f43-8f3a-0f1e2d3c4b5a"
		device      = "019535d9-3df7-79fb-b466-fa907fa17f9e"
		otherDevice = "019535d9-3df7-79fb-b466-fa907fa17f9f"
	)
	ca, caKey, _ := testCAKeyPair(t)
	spiffe := func(tenant, kind, id string) string {
		return "spiffe://wendy.sh/tenant/" + tenant + "/" + kind + "/" + id
	}
	for _, tc := range []struct {
		name   string
		uris   []string
		own    string
		accept bool
	}{
		{"the roster's device in the caller's tenant", []string{spiffe(tenant, "device", device)}, tenant, true},
		{"another device UUID", []string{spiffe(tenant, "device", otherDevice)}, tenant, false},
		{"the same UUID in another tenant", []string{spiffe(otherTenant, "device", device)}, tenant, false},
		{"an operator, not a device", []string{spiffe(tenant, "operator", device)}, tenant, false},
		{"two device SANs", []string{spiffe(tenant, "device", device), spiffe(tenant, "device", otherDevice)}, tenant, false},
		{"a legacy asset URN", []string{"urn:wendy:org:7:asset:" + device}, tenant, false},
		{"a caller with no tenant identity", []string{spiffe(tenant, "device", device)}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf, err := x509.ParseCertificate(testPeerLeafRaw(t, ca, caKey, tc.uris[0], tc.uris[1:]...))
			if err != nil {
				t.Fatal(err)
			}
			err = VerifyPeerDevice(leaf, tc.own, device)
			if tc.accept && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.accept && err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A UUID pin takes the device rule, which has no own-tenant grace: a caller
// with a legacy (tenantless) leaf cannot verify a device peer.
func TestNewClientTLSConfigExpectingDeviceRequiresOwnTenant(t *testing.T) {
	ca, caKey, chainPEM := testCAKeyPair(t)
	ownCertPEM, ownKeyPEM := testLeafCertificate(t, "sh/wendy/7/1")
	const device = "019535d9-3df7-79fb-b466-fa907fa17f9e"
	cfg, err := NewClientTLSConfigExpectingDevice(ownCertPEM, chainPEM, ownKeyPEM, nil, meshid.NewDevice(mustTenant(t, "2558fd76-afc7-466e-9613-6b715296a526"), uuid.MustParse(device)))
	if err != nil {
		t.Fatal(err)
	}
	leaf := testPeerLeafRaw(t, ca, caKey, "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/device/"+device)
	if err := cfg.VerifyPeerCertificate([][]byte{leaf}, nil); err == nil {
		t.Fatal("device peer accepted without a tenant to compare")
	}
}

func TestNewTLSConfigServesNormalizedIssuerChain(t *testing.T) {
	leafPEM, keyPEM := testLeafCertificate(t, "leaf")
	ca, caKey, _ := testCAKeyPair(t)
	issuer := *ca
	issuer.Subject = pkix.Name{CommonName: "intermediate"}
	issuer.RawSubject = nil
	issuerDER, err := x509.CreateCertificate(rand.Reader, &issuer, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuerDER}))
	block, _ := pem.Decode([]byte(chainPEM))
	block.Bytes = append(block.Bytes, 0, 0)
	chainPEM = string(pem.EncodeToMemory(block))

	tlsConfig, err := NewTLSConfig(leafPEM+"\n"+chainPEM, chainPEM, keyPEM, nil, time.Time{})
	if err != nil {
		t.Fatalf("NewTLSConfig() error = %v", err)
	}

	if got := len(tlsConfig.Certificates[0].Certificate); got != 2 {
		t.Fatalf("served certificate chain length = %d; want leaf and issuer", got)
	}
	for _, der := range tlsConfig.Certificates[0].Certificate {
		if _, err := x509.ParseCertificate(der); err != nil {
			t.Fatalf("TLS peer cannot parse transmitted certificate: %v", err)
		}
	}
}

func TestDevicePinUsesCallerTenantForOperatorCredentials(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "33333333-3333-4333-8333-333333333333"
	ca, caKey, chainPEM := testCAKeyPair(t)
	ownCert, ownKey := testDeviceLeafPEM(t, "spiffe://wendy.sh/tenant/"+tenant+"/operator/test-operator")
	want, err := meshid.ParseDevice(mustTenant(t, tenant), device)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewClientTLSConfigExpectingDevice(ownCert, chainPEM, ownKey, nil, want)
	if err != nil {
		t.Fatal(err)
	}
	peer := testPeerLeafRaw(t, ca, caKey, "spiffe://wendy.sh/tenant/"+tenant+"/device/"+device)
	if err := cfg.VerifyPeerCertificate([][]byte{peer}, nil); err != nil {
		t.Fatalf("same-tenant operator could not pin a device: %v", err)
	}
}
