package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"testing"
	"time"

	circlSign "github.com/cloudflare/circl/sign"
	"github.com/cloudflare/circl/sign/mldsa/mldsa65"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func signPeerTestCertificate(t *testing.T, child, issuer *x509.Certificate, key circlSign.PrivateKey) *x509.Certificate {
	t.Helper()
	var tbs tbsCertificate
	if _, err := asn1.Unmarshal(child.RawTBSCertificate, &tbs); err != nil {
		t.Fatal(err)
	}
	tbs.Issuer = asn1.RawValue{FullBytes: issuer.RawSubject}
	eku, err := buildEKUExt([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 1}, {1, 3, 6, 1, 5, 5, 7, 3, 2}})
	if err != nil {
		t.Fatal(err)
	}
	for i, ext := range tbs.Extensions {
		if ext.Id.Equal(eku.Id) {
			tbs.Extensions[i] = eku
		}
	}
	der, err := asn1.Marshal(tbs)
	if err != nil {
		t.Fatal(err)
	}
	sig := mldsa65.Scheme().Sign(key, der, &circlSign.SignatureOpts{})
	encoded, err := asn1.Marshal(certOuter{TBSCertificate: asn1.RawValue{FullBytes: der}, SignatureAlgorithm: algID{Algorithm: oidMLDSA65}, Signature: asn1.BitString{Bytes: sig, BitLength: len(sig) * 8}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestPeerMLDSAIntermediateChains(t *testing.T) {
	root, rootKey := buildMLDSACACert(t, pkix.Name{CommonName: "Tenant CA"}, true)
	root = signPeerTestCertificate(t, root, root, rootKey)
	issuer, issuerKey := buildMLDSACACert(t, pkix.Name{CommonName: "Device Authority"}, true)
	issuer = signPeerTestCertificate(t, issuer, root, rootKey)
	leaf := buildMLDSALeafCert(t, issuer, issuerKey)
	leaf = signPeerTestCertificate(t, leaf, issuer, issuerKey)
	now := time.Now()
	rootPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	serverVerify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{ChainPEM: rootPEM})
	if err != nil {
		t.Fatal(err)
	}
	if err := serverVerify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf, issuer}}); err != nil {
		t.Fatalf("server chain rejected: %v", err)
	}
	pool := x509.NewCertPool()
	clientVerify := buildVerifyPeerCertificate(pool, []*x509.Certificate{root}, nil, time.Time{})
	if err := clientVerify([][]byte{leaf.Raw, issuer.Raw}, nil); err != nil {
		t.Fatalf("client chain rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*x509.Certificate, *x509.Certificate, *x509.Certificate)
	}{
		{"expired leaf", func(l, i, r *x509.Certificate) { l.NotAfter = now.Add(-time.Minute) }},
		{"immature leaf", func(l, i, r *x509.Certificate) { l.NotBefore = now.Add(time.Hour) }},
		{"wrong leaf usage", func(l, i, r *x509.Certificate) { l.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning} }},
		{"expired issuer", func(l, i, r *x509.Certificate) { i.NotAfter = now.Add(-time.Minute) }},
		{"non CA issuer", func(l, i, r *x509.Certificate) { i.IsCA = false }},
		{"issuer cannot sign", func(l, i, r *x509.Certificate) { i.KeyUsage = x509.KeyUsageDigitalSignature }},
		{"issuer wrong usage", func(l, i, r *x509.Certificate) { i.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning} }},
		{"path too long", func(l, i, r *x509.Certificate) { r.MaxPathLen = 0; r.MaxPathLenZero = true }},
		{"critical extension", func(l, i, r *x509.Certificate) { i.UnhandledCriticalExtensions = []asn1.ObjectIdentifier{{1, 2, 3, 4}} }},
		{"name constraints", func(l, i, r *x509.Certificate) {
			i.Extensions = append(append([]pkix.Extension{}, i.Extensions...), pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 30}})
		}},
		{"wrong issuer signature", func(l, i, r *x509.Certificate) {
			i.Signature = append([]byte(nil), i.Signature...)
			i.Signature[0] ^= 1
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, i, r := *leaf, *issuer, *root
			tc.change(&l, &i, &r)
			if err := certs.VerifyPeerCertificateChain(&l, []*x509.Certificate{&i}, []*x509.Certificate{&r}, x509.ExtKeyUsageClientAuth, now, now); err == nil {
				t.Fatal("invalid chain accepted")
			}
		})
	}
	otherRoot, _ := buildMLDSACACert(t, pkix.Name{CommonName: "Untrusted"}, true)
	if err := certs.VerifyPeerCertificateChain(leaf, []*x509.Certificate{issuer, root}, []*x509.Certificate{otherRoot}, x509.ExtKeyUsageClientAuth, now, now); err == nil {
		t.Fatal("peer-supplied root became trusted")
	}
	if err := serverVerify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
		t.Fatal("missing issuer accepted")
	}
}
