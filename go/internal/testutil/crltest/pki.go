// Package crltest supplies signed PKI fixtures with an in-process CRL responder.
package crltest

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

const Endpoint = "https://crl.pki.wendy.test/crl/tenant/issuer"

type response struct {
	body   []byte
	status int
}
type PKI struct {
	CA     *x509.Certificate
	Key    crypto.Signer
	PEM    string
	Client *http.Client
	Hits   atomic.Int64
	reply  atomic.Pointer[response]
}
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type Leaf struct {
	Cert        *x509.Certificate
	PEM, KeyPEM string
}

func New(t testing.TB, pq bool) *PKI {
	t.Helper()
	var key crypto.Signer
	var err error
	if pq {
		key, err = mldsa.GenerateKey(mldsa.MLDSA65())
	} else {
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "CRL test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	p := &PKI{CA: ca, Key: key, PEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw}))}
	p.Client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		p.Hits.Add(1)
		if r.URL.String() != Endpoint {
			t.Errorf("unexpected endpoint %s", r.URL)
		}
		if r.Header.Get("Cache-Control") != "no-cache" {
			t.Error("CRL refresh may use stale CDN response")
		}
		reply := p.reply.Load()
		return &http.Response{StatusCode: reply.status, Body: io.NopCloser(bytes.NewReader(reply.body)), Header: make(http.Header)}, nil
	})}
	p.Publish(t, 1)
	return p
}

func (p *PKI) Issue(t testing.TB, serial int64, principal string) Leaf {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{u}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, CRLDistributionPoints: []string{Endpoint}}
	raw, err := x509.CreateCertificate(rand.Reader, template, p.CA, key.Public(), p.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	keyRaw, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return Leaf{cert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: raw})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyRaw}))}
}

func (p *PKI) Sign(t testing.TB, template *x509.RevocationList) []byte {
	t.Helper()
	raw, err := x509.CreateRevocationList(rand.Reader, template, p.CA, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func (p *PKI) Respond(raw []byte, status int) {
	p.reply.Store(&response{append([]byte(nil), raw...), status})
}
func (p *PKI) Publish(t testing.TB, number int64, serials ...int64) {
	t.Helper()
	now := time.Now()
	template := &x509.RevocationList{Number: big.NewInt(number), ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour)}
	for _, s := range serials {
		template.RevokedCertificateEntries = append(template.RevokedCertificateEntries, x509.RevocationListEntry{SerialNumber: big.NewInt(s), RevocationTime: now.Add(-time.Minute), ReasonCode: 1})
	}
	p.Respond(p.Sign(t, template), 200)
}
