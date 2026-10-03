package mtls

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type crlFixture struct {
	server *httptest.Server
	body   atomic.Pointer[[]byte]
}

func serveTestCRL(t *testing.T, ca *x509.Certificate, key *ecdsa.PrivateKey) *crlFixture {
	t.Helper()
	f := &crlFixture{}
	f.publish(t, ca, key, 1, time.Now().Add(-time.Minute), time.Now().Add(time.Hour))
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(*f.body.Load()) }))
	t.Cleanup(f.server.Close)
	return f
}
func (f *crlFixture) publish(t *testing.T, ca *x509.Certificate, key *ecdsa.PrivateKey, number int64, start, end time.Time, serials ...*big.Int) {
	t.Helper()
	list := &x509.RevocationList{Number: big.NewInt(number), ThisUpdate: start, NextUpdate: end}
	for _, serial := range serials {
		list.RevokedCertificateEntries = append(list.RevokedCertificateEntries, x509.RevocationListEntry{SerialNumber: serial, RevocationTime: start})
	}
	der, err := x509.CreateRevocationList(rand.Reader, list, ca, key)
	if err != nil {
		t.Fatal(err)
	}
	f.body.Store(&der)
}
func revocationPeer(t *testing.T, ca *x509.Certificate, key *ecdsa.PrivateKey, endpoint string) *x509.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(testPeerLeafRaw(t, ca, key, "urn:wendy:org:7:user:alice"))
	if err != nil {
		t.Fatal(err)
	}
	leaf.CRLDistributionPoints = []string{endpoint}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}
func TestRevocationRefreshRejectsRevokedCertAndUnavailableStatus(t *testing.T) {
	ca, key, _ := testCAKeyPair(t)
	f := serveTestCRL(t, ca, key)
	leaf := revocationPeer(t, ca, key, f.server.URL)
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	checker := newRevocationChecker([]*x509.Certificate{ca})
	now := time.Now()
	checker.now = func() time.Time { return now }
	if err := checker.check(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	f.publish(t, ca, key, 2, now, now.Add(time.Hour), leaf.SerialNumber)
	now = now.Add(revocationRefresh)
	if err := checker.check(context.Background(), state); err == nil {
		t.Fatal("revoked certificate accepted on existing TLS connection")
	}
	f.server.Close()
	now = now.Add(revocationRefresh)
	if err := checker.check(context.Background(), state); err == nil {
		t.Fatal("unavailable fresh CRL accepted")
	}
}
func TestRevocationRejectsMissingCDP(t *testing.T) {
	ca, key, _ := testCAKeyPair(t)
	leaf, _ := x509.ParseCertificate(testPeerLeafRaw(t, ca, key, "urn:wendy:org:7:user:alice"))
	if err := newRevocationChecker([]*x509.Certificate{ca}).check(context.Background(), tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
		t.Fatal("no-CDP certificate accepted")
	}
}
func TestCRLVerificationRejectsInvalidEvidence(t *testing.T) {
	ca, key, _ := testCAKeyPair(t)
	f := serveTestCRL(t, ca, key)
	der := append([]byte(nil), (*f.body.Load())...)
	for _, tc := range []struct {
		name   string
		mutate func(*x509.RevocationList)
	}{
		{"wrong issuer", func(l *x509.RevocationList) { l.RawIssuer = []byte("other") }},
		{"wrong key ID", func(l *x509.RevocationList) { l.AuthorityKeyId = []byte("other") }},
		{"expired", func(l *x509.RevocationList) { l.NextUpdate = time.Now().Add(-time.Second) }},
		{"future", func(l *x509.RevocationList) { l.ThisUpdate = time.Now().Add(time.Hour) }},
		{"missing next update", func(l *x509.RevocationList) { l.NextUpdate = time.Time{} }},
		{"missing number", func(l *x509.RevocationList) { l.Number = nil }},
		{"bad signature", func(l *x509.RevocationList) { l.Signature[0] ^= 1 }},
		{"delta", func(l *x509.RevocationList) {
			l.Extensions = append(l.Extensions, pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 27}})
		}},
		{"partitioned", func(l *x509.RevocationList) {
			l.Extensions = append(l.Extensions, pkix.Extension{Id: asn1.ObjectIdentifier{2, 5, 29, 28}})
		}},
		{"unknown critical", func(l *x509.RevocationList) {
			l.Extensions = append(l.Extensions, pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3}, Critical: true})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			list, err := x509.ParseRevocationList(append([]byte(nil), der...))
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(list)
			if err := verifyCRL(list, ca, time.Now()); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
}
func TestRevocationRejectsCRLRollback(t *testing.T) {
	ca, key, _ := testCAKeyPair(t)
	f := serveTestCRL(t, ca, key)
	checker := newRevocationChecker([]*x509.Certificate{ca})
	now := time.Now()
	checker.now = func() time.Time { return now }
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{revocationPeer(t, ca, key, f.server.URL)}}
	if err := checker.check(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	f.publish(t, ca, key, 0, now, now.Add(time.Hour))
	now = now.Add(revocationRefresh)
	if err := checker.check(context.Background(), state); err == nil {
		t.Fatal("rollback accepted")
	}
}

func TestMLDSACRLVerification(t *testing.T) {
	ca, key := buildMLDSACACert(t, pkix.Name{CommonName: "ML-DSA CRL CA"}, true)
	// The fixture's authenticated CA values; the test below exercises CRL DER
	// parsing and the actual ML-DSA signing algorithm used by pki-core.
	ca.KeyUsage |= x509.KeyUsageCRLSign
	ca.SubjectKeyId = []byte{1, 2, 3, 4}
	aki, _ := asn1.Marshal(struct {
		ID []byte `asn1:"optional,tag:0"`
	}{ca.SubjectKeyId})
	number, _ := asn1.Marshal(big.NewInt(1))
	tbs := pkix.TBSCertificateList{Version: 1, Signature: pkix.AlgorithmIdentifier{Algorithm: oidMLDSA65}, Issuer: ca.Subject.ToRDNSequence(), ThisUpdate: time.Now().Add(-time.Minute), NextUpdate: time.Now().Add(time.Hour), Extensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{2, 5, 29, 35}, Value: aki}, {Id: asn1.ObjectIdentifier{2, 5, 29, 20}, Value: number}}}
	raw, err := asn1.Marshal(tbs)
	if err != nil {
		t.Fatal(err)
	}
	scheme, _ := mldsaScheme(oidMLDSA65)
	sig := scheme.Sign(key, raw, nil)
	der, err := asn1.Marshal(certOuter{TBSCertificate: asn1.RawValue{FullBytes: raw}, SignatureAlgorithm: algID{Algorithm: oidMLDSA65}, Signature: asn1.BitString{Bytes: sig, BitLength: len(sig) * 8}})
	if err != nil {
		t.Fatal(err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyCRL(list, ca, time.Now()); err != nil {
		t.Fatal(err)
	}
	list.Signature[0] ^= 1
	if err := verifyCRL(list, ca, time.Now()); err == nil {
		t.Fatal("tampered ML-DSA signature accepted")
	}
}

func TestCRLFetchBoundsAndRedirects(t *testing.T) {
	ca, key, _ := testCAKeyPair(t)
	valid := serveTestCRL(t, ca, key)
	checker := newRevocationChecker([]*x509.Certificate{ca})
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, valid.server.URL, http.StatusFound) }},
		{"oversized", func(w http.ResponseWriter, _ *http.Request) { w.Write(make([]byte, maxCRLBytes+1)) }},
		{"bad status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			if _, err := checker.fetch(context.Background(), server.URL, ca, time.Now()); err == nil {
				t.Fatal("unsafe CRL response accepted")
			}
		})
	}
}
