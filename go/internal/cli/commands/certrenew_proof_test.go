package commands

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// proofTestCert self-signs a leaf for key and stores it the way a session does.
func proofTestCert(t *testing.T, key crypto.Signer) config.CertificateInfo {
	t.Helper()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "renew-proof-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	return config.CertificateInfo{
		PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}
}

func proofTestCSR(t *testing.T) (csrPEM string, csrDER []byte) {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	csrPEM, err = certs.GenerateCSR([]byte(keyPEM), "renew-proof-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(csrPEM))
	return csrPEM, block.Bytes
}

func b64Sum(b []byte) string {
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// verifyProofSig checks the JWS signature the way pki-core's reqsig does:
// pure ML-DSA with an empty context, ECDSA as fixed-width r||s, Ed25519 raw.
func verifyProofSig(t *testing.T, pub crypto.PublicKey, alg, signingInput string, sig []byte) {
	t.Helper()
	ok := false
	switch k := pub.(type) {
	case *mldsa.PublicKey:
		ok = mldsa.Verify(k, []byte(signingInput), sig, nil) == nil
	case *ecdsa.PublicKey:
		n := len(sig) / 2
		digest := sha256.Sum256([]byte(signingInput))
		h := digest[:]
		if alg == "ES384" {
			d := crypto.SHA384.New()
			d.Write([]byte(signingInput))
			h = d.Sum(nil)
		}
		ok = ecdsa.Verify(k, h, new(big.Int).SetBytes(sig[:n]), new(big.Int).SetBytes(sig[n:]))
	case ed25519.PublicKey:
		ok = ed25519.Verify(k, []byte(signingInput), sig)
	}
	if !ok {
		t.Fatalf("%s signature does not verify under the presented certificate's key", alg)
	}
}

// TestRenewalPossessionProofAlgorithmFollowsTheKey is the algorithm-in-use
// assertion: the proof is signed with the presented key's own algorithm, and a
// key outside pki-core's set is refused rather than swapped for another.
func TestRenewalPossessionProofAlgorithmFollowsTheKey(t *testing.T) {
	gen := func(f func() (crypto.Signer, error)) crypto.Signer {
		k, err := f()
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	mld := func(p mldsa.Parameters) crypto.Signer {
		return gen(func() (crypto.Signer, error) { return mldsa.GenerateKey(p) })
	}
	ec := func(c elliptic.Curve) crypto.Signer {
		return gen(func() (crypto.Signer, error) { return ecdsa.GenerateKey(c, rand.Reader) })
	}
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)

	tests := []struct {
		key     crypto.Signer
		wantAlg string
		sigLen  int
	}{
		{mld(mldsa.MLDSA65()), "ML-DSA-65", mldsa.MLDSA65().SignatureSize()},
		{mld(mldsa.MLDSA44()), "ML-DSA-44", mldsa.MLDSA44().SignatureSize()},
		{mld(mldsa.MLDSA87()), "ML-DSA-87", mldsa.MLDSA87().SignatureSize()},
		{ec(elliptic.P256()), "ES256", 64},
		{ec(elliptic.P384()), "ES384", 96},
		{edKey, "EdDSA", ed25519.SignatureSize},
	}
	csrPEM, csrDER := proofTestCSR(t)
	for _, tc := range tests {
		t.Run(tc.wantAlg, func(t *testing.T) {
			cert := proofTestCert(t, tc.key)
			now := time.Unix(1_800_000_000, 0)
			proof, err := renewalPossessionProof(cert, csrPEM, now)
			if err != nil {
				t.Fatalf("renewalPossessionProof: %v", err)
			}
			parts := strings.Split(proof, ".")
			if len(parts) != 3 {
				t.Fatalf("proof is not JWS Compact: %d segments", len(parts))
			}
			hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
			if want := `{"alg":"` + tc.wantAlg + `"}`; string(hdr) != want {
				t.Fatalf("header = %s, want %s", hdr, want)
			}
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil || len(sig) != tc.sigLen {
				t.Fatalf("signature: %d bytes (err %v), want %d", len(sig), err, tc.sigLen)
			}
			verifyProofSig(t, tc.key.Public(), tc.wantAlg, parts[0]+"."+parts[1], sig)

			// Payload: exactly the five members pki-core accepts, bound to the
			// presented cert's DER and the CSR's DER.
			raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
			var p map[string]any
			if err := json.Unmarshal(raw, &p); err != nil {
				t.Fatalf("payload: %v", err)
			}
			block, _ := pem.Decode([]byte(cert.PemCertificate))
			want := map[string]any{
				"op":          "renew",
				"cert_sha256": b64Sum(block.Bytes),
				"csr_sha256":  b64Sum(csrDER),
				"jti":         p["jti"],
				"iat":         float64(now.Unix()),
			}
			if len(p) != len(want) {
				t.Fatalf("payload members = %v, want exactly %v", p, want)
			}
			for k, v := range want {
				if p[k] != v {
					t.Errorf("payload[%q] = %v, want %v", k, p[k], v)
				}
			}
			if jti, _ := p["jti"].(string); jti == "" || len(jti) > 128 {
				t.Errorf("jti = %q, want 1..128 bytes", jti)
			}
		})
	}

	refused := map[string]crypto.Signer{
		"P-521":    ec(elliptic.P521()),
		"RSA-2048": gen(func() (crypto.Signer, error) { return rsa.GenerateKey(rand.Reader, 2048) }),
	}
	for name, key := range refused {
		t.Run("refuses "+name, func(t *testing.T) {
			_, err := renewalPossessionProof(proofTestCert(t, key), csrPEM, time.Now())
			var ru renewUnavailableError
			if !errors.As(err, &ru) {
				t.Fatalf("a %s key: err = %v; want a reported refusal, never a substitute algorithm", name, err)
			}
		})
	}
}

// TestRenewalPossessionProofIsFreshPerRequest: the jti is single-use at
// pki-core, so a reused proof would turn the second renewal into a 401.
func TestRenewalPossessionProofIsFreshPerRequest(t *testing.T) {
	key, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	cert := proofTestCert(t, key)
	csrPEM, _ := proofTestCSR(t)
	now := time.Now()
	jtis := map[string]bool{}
	for range 3 {
		proof, err := renewalPossessionProof(cert, csrPEM, now)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[1])
		var p struct{ JTI string }
		_ = json.Unmarshal(raw, &p)
		if jtis[p.JTI] {
			t.Fatalf("jti %q repeated", p.JTI)
		}
		jtis[p.JTI] = true
	}
}

// TestRenewSendsPossessionProofForThePresentedCert drives the real request
// path: the body carries a proof signed by the key the mTLS client presents,
// bound to that client certificate and to the CSR in the same body.
func TestRenewSendsPossessionProofForThePresentedCert(t *testing.T) {
	key, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	current := proofTestCert(t, key)
	csrPEM, csrDER := proofTestCSR(t)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		if len(r.TLS.PeerCertificates) == 0 {
			t.Error("no client certificate presented")
			return
		}
		presented := r.TLS.PeerCertificates[0].Raw
		var body renewRequestBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		parts := strings.Split(body.PossessionProof, ".")
		if len(parts) != 3 {
			t.Errorf("possession_proof = %q, want JWS Compact", body.PossessionProof)
			return
		}
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		var p struct {
			CertSHA256 string `json:"cert_sha256"`
			CSRSHA256  string `json:"csr_sha256"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.CertSHA256 != b64Sum(presented) || p.CSRSHA256 != b64Sum(csrDER) || body.CSR != csrPEM {
			t.Errorf("proof not bound to the presented cert and this CSR: %+v", p)
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		verifyProofSig(t, key.Public(), "ML-DSA-65", parts[0]+"."+parts[1], sig)
	}))
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	// The production client config, trusting only the test server.
	serverPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
	tlsCfg, err := certs.LoadTLSConfig(current.PemCertificate, "", current.PemPrivateKey, serverPEM)
	if err != nil {
		t.Fatal(err)
	}
	origClient, origCSR := renewHTTPClientForFn, renewCSRForFn
	renewHTTPClientForFn = func(config.CertificateInfo) (*http.Client, error) {
		return &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}}, nil
	}
	renewCSRForFn = func(config.CertificateInfo) (string, string, error) { return csrPEM, "new-key", nil }
	t.Cleanup(func() { renewHTTPClientForFn, renewCSRForFn = origClient, origCSR })

	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{current}}
	if _, _, _, err := renewViaPKICoreImpl(context.Background(), srv.URL+"/v1/renew", auth); err == nil {
		t.Fatal("want the stub's 401 reported")
	}
}

// TestRenewCSRKeepsTheCurrentKeyAlgorithm: a renewal re-keys, and the new key
// must not be weaker than the one it replaces (no silent ML-DSA -> P-256).
func TestRenewCSRKeepsTheCurrentKeyAlgorithm(t *testing.T) {
	mlKey, _ := mldsa.GenerateKey(mldsa.MLDSA65())
	ecKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	_, edKey, _ := ed25519.GenerateKey(rand.Reader)
	for name, key := range map[string]crypto.Signer{"ML-DSA-65": mlKey, "P-256": ecKey, "Ed25519": edKey} {
		t.Run(name, func(t *testing.T) {
			cur := proofTestCert(t, key)
			_, newKeyPEM, err := renewCSRFor(cur)
			if err != nil {
				t.Fatalf("renewCSRFor: %v", err)
			}
			newKey, err := certs.ParseSigningPrivateKeyPEM([]byte(newKeyPEM))
			if err != nil {
				t.Fatal(err)
			}
			if got, want := keyAlgName(newKey), keyAlgName(key); got != want {
				t.Fatalf("renewal key = %s, current key = %s", got, want)
			}
		})
	}
}

func keyAlgName(k crypto.Signer) string {
	switch pub := k.Public().(type) {
	case *mldsa.PublicKey:
		return pub.Parameters().String()
	case *ecdsa.PublicKey:
		return pub.Curve.Params().Name
	case ed25519.PublicKey:
		return "Ed25519"
	}
	return "other"
}
