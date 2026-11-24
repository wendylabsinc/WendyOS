package mtls

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

const testTenant = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/"

type testIssuer struct {
	cert *x509.Certificate
	key  crypto.Signer
}

func issueTestCert(t *testing.T, tmpl *x509.Certificate, parent *testIssuer, pub any, self crypto.Signer) *testIssuer {
	t.Helper()
	tmpl.SerialNumber = big.NewInt(time.Now().UnixNano())
	tmpl.NotBefore, tmpl.NotAfter = time.Now().Add(-time.Minute), time.Now().Add(time.Hour)
	parentCert, signer := tmpl, self
	if parent != nil {
		parentCert, signer = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, pub, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testIssuer{cert: cert, key: self}
}

func testCA(t *testing.T, name string, parent *testIssuer, mldsaKey bool) *testIssuer {
	tmpl := &x509.Certificate{Subject: pkix.Name{CommonName: name}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	if mldsaKey {
		key, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		return issueTestCert(t, tmpl, parent, key.PublicKey(), key)
	}
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return issueTestCert(t, tmpl, parent, &key.PublicKey, key)
}

func testLeaf(t *testing.T, parent *testIssuer, mldsaKey bool, uri string, edit func(*x509.Certificate)) [][]byte {
	tmpl := &x509.Certificate{KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if uri != "" {
		u, _ := url.Parse(uri)
		tmpl.URIs = []*url.URL{u}
	}
	if edit != nil {
		edit(tmpl)
	}
	var pub any
	if mldsaKey {
		key, _ := mldsa.GenerateKey(mldsa.MLDSA65())
		pub = key.PublicKey()
	} else {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		pub = &key.PublicKey
	}
	leaf := issueTestCert(t, tmpl, parent, pub, nil)
	return [][]byte{leaf.cert.Raw, parent.cert.Raw}
}

func testChainPEM(cas ...*testIssuer) string {
	var out string
	for _, ca := range cas {
		out += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw}))
	}
	return out
}

// verifyBoth runs a chain through the mesh verifier and the general listener's
// verifier, which share buildPeerCertificateWindowVerifier.
func verifyBoth(t *testing.T, chainPEM string, chain [][]byte) (meshErr, generalErr error) {
	t.Helper()
	mesh, err := NewPeerCertificateVerifier(chainPEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	_, meshErr = mesh(chain, time.Now())
	pool, cas, err := peerCertificateTrust(chainPEM)
	if err != nil {
		t.Fatal(err)
	}
	return meshErr, buildVerifyPeerCertificate(pool, cas, nil, time.Time{})(chain, nil)
}

func tierExt(t *testing.T, tier string) func(*x509.Certificate) {
	value, err := asn1.MarshalWithParams(tier, "utf8")
	if err != nil {
		t.Fatal(err)
	}
	return func(c *x509.Certificate) {
		c.ExtraExtensions = append(c.ExtraExtensions, pkix.Extension{Id: oidDeviceTrustTier, Value: value})
	}
}

func parseLeaf(t *testing.T, chain [][]byte) *x509.Certificate {
	leaf, err := x509.ParseCertificate(chain[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// Under pki-core trust a client leaf that is not ML-DSA is accepted only as a
// Wendy Lite board, or through the named Tier A/B fallback exception. The
// P-256 Lite operator leaf (lite-relay-v1 § Operator dual-key) chains to the
// same tenant Intermediate and is refused for what it lacks, not its marker.
func TestPKICoreTrustAllowsOnlyMLDSAOrLiteBoard(t *testing.T) {
	root := testCA(t, "Tenant Root", nil, true)
	inter := testCA(t, "Tenant Intermediate", root, true)
	chainPEM := testChainPEM(inter, root)
	liteOperatorCA := testCA(t, "Tenant Lite Operator CA", inter, false)
	liteDeviceCA := testCA(t, "Tenant Lite Device CA", inter, false)
	marker := func(c *x509.Certificate) {
		c.UnknownExtKeyUsage = []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 65441, 1, 9}}
	}

	for _, tc := range []struct {
		name   string
		chain  [][]byte
		accept bool
	}{
		{"Lite operator leaf", testLeaf(t, liteOperatorCA, false, testTenant+"operator/op", marker), false},
		{"P-256 operator leaf without marker", testLeaf(t, inter, false, testTenant+"operator/op", nil), false},
		{"P-256 service leaf", testLeaf(t, liteOperatorCA, false, testTenant+"service/svc", nil), false},
		{"P-256 leaf without identity", testLeaf(t, inter, false, "", nil), false},
		{"P-256 device leaf under Lite CA without tier", testLeaf(t, liteDeviceCA, false, testTenant+"device/board", nil), false},
		{"P-256 device leaf under Lite CA with tier B", testLeaf(t, liteDeviceCA, false, testTenant+"device/board", tierExt(t, "B")), false},
		{"P-256 operator leaf with tier C", testLeaf(t, liteDeviceCA, false, testTenant+"operator/op", tierExt(t, "C")), false},
		{"ML-DSA operator leaf", testLeaf(t, inter, true, testTenant+"operator/op", nil), true},
		{"ML-DSA device leaf", testLeaf(t, inter, true, testTenant+"device/d1", nil), true},
		{"Lite board leaf", testLeaf(t, liteDeviceCA, false, testTenant+"device/board", tierExt(t, "C")), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meshErr, generalErr := verifyBoth(t, chainPEM, tc.chain)
			for _, err := range []error{meshErr, generalErr} {
				if tc.accept && err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !tc.accept && (err == nil || !strings.Contains(err.Error(), "wendy auth login")) {
					t.Fatalf("want key-type refusal, got %v", err)
				}
			}
		})
	}
}

// pki-core's P-256 Tier A/B fallback device leaf (WDY-3620) passes only
// through tierABFallbackException.
func TestTierABFallbackException(t *testing.T) {
	root := testCA(t, "Tenant Root", nil, true)
	inter := testCA(t, "Tenant Intermediate", root, true)
	chain := testLeaf(t, inter, false, testTenant+"device/d2", nil)
	leaf := parseLeaf(t, chain)
	if isMLDSACert(leaf) || isLiteBoard(leaf) || !tierABFallbackException(leaf) {
		t.Fatal("fallback leaf must be admitted by its exception alone")
	}
	if meshErr, generalErr := verifyBoth(t, testChainPEM(inter, root), chain); meshErr != nil || generalErr != nil {
		t.Fatalf("fallback leaf refused: mesh=%v general=%v", meshErr, generalErr)
	}
	operator := parseLeaf(t, testLeaf(t, inter, false, testTenant+"operator/op", nil))
	if tierABFallbackException(operator) {
		t.Fatal("fallback exception admitted an operator leaf")
	}
}

// Legacy Cloud trust (classical anchors) keeps accepting its P-256 operator
// and device leaves, and only through legacyCloudException.
func TestLegacyCloudException(t *testing.T) {
	root := testCA(t, "Legacy Cloud CA", nil, false)
	pkiRoot := testCA(t, "Tenant Root", nil, true)
	if !legacyCloudException([]*x509.Certificate{root.cert}) || legacyCloudException([]*x509.Certificate{root.cert, pkiRoot.cert}) {
		t.Fatal("legacy exception must key on classical-only anchors")
	}
	chainPEM := testChainPEM(root)
	for name, chain := range map[string][][]byte{
		"legacy operator CN": testLeaf(t, root, false, "", func(c *x509.Certificate) { c.Subject.CommonName = "wendy/user/42" }),
		"legacy device CN":   testLeaf(t, root, false, "", func(c *x509.Certificate) { c.Subject.CommonName = "sh/wendy/7/42" }),
		"P-256 operator URI": testLeaf(t, root, false, testTenant+"operator/op", nil),
	} {
		if clientLeafAllowed(parseLeaf(t, chain), false) {
			t.Errorf("%s allowed without the legacy exception", name)
		}
		if meshErr, generalErr := verifyBoth(t, chainPEM, chain); meshErr != nil || generalErr != nil {
			t.Errorf("%s refused: mesh=%v general=%v", name, meshErr, generalErr)
		}
	}
}
