package unenrollproof

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ocsp"
)

func fixture(t *testing.T, pq bool) (*x509.Certificate, *x509.Certificate, crypto.Signer, crypto.Signer) {
	t.Helper()
	var caKey crypto.Signer
	if pq {
		k, e := mldsa.GenerateKey(mldsa.MLDSA65())
		if e != nil {
			t.Fatal(e)
		}
		caKey = k
	} else {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		caKey = k
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	raw, e := x509.CreateCertificate(rand.Reader, tmpl, tmpl, caKey.Public(), caKey)
	if e != nil {
		t.Fatal(e)
	}
	issuer, e := x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	var leafKey crypto.Signer
	if pq {
		leafKey, e = mldsa.GenerateKey(mldsa.MLDSA65())
	} else {
		leafKey, e = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	}
	if e != nil {
		t.Fatal(e)
	}
	uri, _ := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	tmpl = &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	raw, e = x509.CreateCertificate(rand.Reader, tmpl, issuer, leafKey.Public(), caKey)
	if e != nil {
		t.Fatal(e)
	}
	leaf, e := x509.ParseCertificate(raw)
	if e != nil {
		t.Fatal(e)
	}
	return leaf, issuer, leafKey, caKey
}
func answer(t *testing.T, leaf, issuer *x509.Certificate, key crypto.Signer, status int) []byte {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	template := ocsp.Response{Status: status, SerialNumber: leaf.SerialNumber, ThisUpdate: now.Add(-time.Minute), NextUpdate: now.Add(time.Hour), RevokedAt: now.Add(-time.Minute)}
	signer := key
	_, pq := key.Public().(*mldsa.PublicKey)
	if pq {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		signer = k
	}
	raw, e := ocsp.CreateResponse(issuer, issuer, template, signer)
	if e != nil {
		t.Fatal(e)
	}
	if pq {
		var outer wireResponse
		_, e = asn1.Unmarshal(raw, &outer)
		if e != nil {
			t.Fatal(e)
		}
		var basic basicResponse
		_, e = asn1.Unmarshal(outer.Bytes.Response, &basic)
		if e != nil {
			t.Fatal(e)
		}
		basic.Algorithm.Algorithm = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 3, 18}
		sig, e := key.Sign(rand.Reader, basic.Data.FullBytes, &mldsa.Options{})
		if e != nil {
			t.Fatal(e)
		}
		basic.Signature = asn1.BitString{Bytes: sig, BitLength: len(sig) * 8}
		outer.Bytes.Response, e = asn1.Marshal(basic)
		if e != nil {
			t.Fatal(e)
		}
		raw, e = asn1.Marshal(outer)
		if e != nil {
			t.Fatal(e)
		}
	}
	return raw
}
func TestFetchRefreshesEvidenceWithoutSavedWorkflowState(t *testing.T) {
	leaf, issuer, _, signer := fixture(t, false)
	var statusValue atomic.Int32
	statusValue.Store(int32(ocsp.Good))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/ocsp-request" {
			t.Error("unexpected OCSP request")
		}
		w.Header().Set("Content-Type", "application/ocsp-response")
		_, _ = w.Write(answer(t, leaf, issuer, signer, int(statusValue.Load())))
	}))
	defer server.Close()
	leaf.OCSPServer = []string{server.URL}
	if _, err := FetchRevocation(context.Background(), leaf, issuer); err == nil {
		t.Fatal("good status supplied reset authority")
	}
	statusValue.Store(int32(ocsp.Revoked))
	proof, err := FetchRevocation(context.Background(), leaf, issuer)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRevocation(proof, leaf, issuer, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRevocationEvidenceAndSignedCompletion(t *testing.T) {
	for _, pq := range []bool{false, true} {
		t.Run(map[bool]string{false: "ECDSA", true: "MLDSA"}[pq], func(t *testing.T) {
			leaf, issuer, key, caKey := fixture(t, pq)
			evidence := answer(t, leaf, issuer, caKey, ocsp.Revoked)
			if e := VerifyRevocation(evidence, leaf, issuer, time.Now()); e != nil {
				t.Fatal(e)
			}
			if e := VerifyRevocation(answer(t, leaf, issuer, caKey, ocsp.Good), leaf, issuer, time.Now()); e == nil {
				t.Fatal("good is not revoked")
			}
			if e := VerifyRevocation(evidence, leaf, issuer, time.Now().Add(2*time.Hour)); e == nil {
				t.Fatal("expired evidence accepted")
			}
			bad := append([]byte(nil), evidence...)
			bad[len(bad)-1] ^= 1
			if e := VerifyRevocation(bad, leaf, issuer, time.Now()); e == nil {
				t.Fatal("forged evidence accepted")
			}
			otherLeaf, otherIssuer, _, _ := fixture(t, false)
			if e := VerifyRevocation(evidence, otherLeaf, otherIssuer, time.Now()); e == nil {
				t.Fatal("different issuer accepted")
			}
			receipt, e := SignCompletion(Completion{Principal: leaf.URIs[0].String(), Cloud: "api.example:443", AssetID: "33333333-3333-4333-8333-333333333333", Fingerprint: Fingerprint(leaf), AuthorizedAt: time.Now().Unix(), Certificate: leaf.Raw, Chain: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: issuer.Raw})), Revocation: evidence}, key)
			if e != nil {
				t.Fatal(e)
			}
			if _, _, e := ReadCompletion(receipt); e != nil {
				t.Fatal(e)
			}
			var signed envelope
			json.Unmarshal(receipt, &signed)
			signed.Body = append(signed.Body, ' ')
			bad, _ = json.Marshal(signed)
			if _, _, e := ReadCompletion(bad); e == nil {
				t.Fatal("tampered/noncanonical receipt accepted")
			}
		})
	}
}
