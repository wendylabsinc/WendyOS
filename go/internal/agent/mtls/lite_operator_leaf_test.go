package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// A pki-core-shaped Lite operator leaf (P-256, marker EKU, issued by the
// Lite Operator Authority that the tenant Intermediate signs) must not pass the
// agent's client-certificate verifier (lite-relay-v1 § Operator dual-key).
func TestLiteOperatorLeafRefusedByAgentVerifier(t *testing.T) {
	root, rootKey := buildMLDSACACert(t, pkix.Name{CommonName: "Tenant Root"}, true)
	root = signPeerTestCertificate(t, root, root, rootKey, func(tbs *tbsCertificate) { tbs.Subject = asn1.RawValue{FullBytes: root.RawSubject} })
	inter, interKey := buildMLDSACACert(t, pkix.Name{CommonName: "Tenant Intermediate"}, true)
	inter = signPeerTestCertificate(t, inter, root, rootKey, func(tbs *tbsCertificate) { tbs.Subject = asn1.RawValue{FullBytes: inter.RawSubject} })

	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	twinTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Tenant Lite Operator CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true}
	twinDER, err := x509.CreateCertificate(rand.Reader, twinTmpl, twinTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	twin, _ := x509.ParseCertificate(twinDER)
	signed := signPeerTestCertificate(t, twin, inter, interKey, func(tbs *tbsCertificate) {
		tbs.Subject = asn1.RawValue{FullBytes: twin.RawSubject}
		tbs.Signature = algID{Algorithm: oidMLDSA65}
	})

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	spiffe, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/user/op")
	leafDER, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 65441, 1, 9}}, URIs: []*url.URL{spiffe}}, twin, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}

	chainPEM := ""
	for _, c := range []*x509.Certificate{inter, root} {
		chainPEM += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw}))
	}
	mesh, err := NewPeerCertificateVerifier(chainPEM, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	pool, cas, err := peerCertificateTrust(chainPEM)
	if err != nil {
		t.Fatal(err)
	}
	general := buildVerifyPeerCertificate(pool, cas, nil, time.Time{})
	for name, chain := range map[string][][]byte{
		"leaf+intermediate-signed Lite CA": {leafDER, signed.Raw},
		"leaf+self-signed twin":            {leafDER, twin.Raw},
		"leaf+both":                        {leafDER, signed.Raw, twin.Raw},
		"leaf alone":                       {leafDER},
	} {
		_, meshErr := mesh(chain, now)
		genErr := general(chain, nil)
		if meshErr == nil || genErr == nil {
			t.Errorf("%s: Lite operator leaf accepted (mesh err=%v, general err=%v)", name, meshErr, genErr)
		}
	}
}
