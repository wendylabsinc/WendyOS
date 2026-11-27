package localmesh

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Mesh operator admission (native listener, BLE, gateway control, wendy-net
// app) runs the agent's client-leaf allow-list under pki-core trust. A P-256
// operator leaf is refused for its key, whether or not it carries a marker.
func TestMeshOperatorPathRunsClientLeafRule(t *testing.T) {
	now := time.Now()
	type issuer struct {
		cert *x509.Certificate
		key  crypto.Signer
	}
	var serial int64
	mint := func(parent *issuer, mldsaKey bool, principal string) *issuer {
		t.Helper()
		var key crypto.Signer
		var err error
		if mldsaKey {
			key, err = mldsa.GenerateKey(mldsa.MLDSA65())
		} else {
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		}
		if err != nil {
			t.Fatal(err)
		}
		serial++
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: principal}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
		if principal == "" {
			tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
		} else {
			u, _ := url.Parse(principal)
			tmpl.URIs = []*url.URL{u}
			tmpl.KeyUsage = x509.KeyUsageDigitalSignature
			tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}
		}
		parentCert, signer := tmpl, key
		if parent != nil {
			parentCert, signer = parent.cert, parent.key
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parentCert, key.Public(), signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return &issuer{cert, key}
	}
	encode := func(c *x509.Certificate) string {
		return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	root := mint(nil, true, "")
	inter := mint(root, true, "")
	liteOperatorCA := mint(inter, false, "")
	self := mint(inter, true, testPrincipal(testTenantUUID, testDeviceAID))
	keyDER, err := x509.MarshalPKCS8PrivateKey(self.key)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := newTestCredentials(encode(self.cert), encode(inter.cert)+encode(root.cert), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	if err != nil {
		t.Fatal(err)
	}
	operator := "spiffe://wendy.sh/tenant/" + testTenantUUID + "/operator/op"
	for _, tc := range []struct {
		name   string
		parent *issuer
		mldsa  bool
		accept bool
	}{
		{"P-256 operator leaf without marker", inter, false, false},
		{"Lite operator leaf without marker", liteOperatorCA, false, false},
		{"ML-DSA operator leaf", inter, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain := [][]byte{mint(tc.parent, tc.mldsa, operator).cert.Raw, tc.parent.cert.Raw}
			_, primaryErr := credentials.VerifyPrimaryOperator(chain, time.Now())
			_, peerErr := credentials.Verify(chain, time.Now())
			for _, err := range []error{primaryErr, peerErr} {
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
