package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const testOperatorTenant = "2558fd76-afc7-466e-9613-6b715296a526"
const testOperatorSubject = "operator-subject"

func TestOIDCLoginUsesDevPKIIdentityEndpointByDefault(t *testing.T) {
	cmd := newAuthLoginCmd()
	flag := cmd.Flags().Lookup("pki-identity-endpoint")
	if flag == nil {
		t.Fatal("pki-identity-endpoint flag is missing")
	}
	if got, want := flag.DefValue, "https://identity.dev.pki.wendy.sh/v1/identity/certificate"; got != want {
		t.Fatalf("pki-identity-endpoint default = %q, want %q", got, want)
	}
}

func testLeafPEM(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	now := time.Now()
	principal, err := url.Parse("spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "operator"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{principal},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func decodeProofPart(t *testing.T, proof string, index int, out any) {
	t.Helper()
	parts := strings.Split(proof, ".")
	if len(parts) != 3 {
		t.Fatalf("DPoP proof has %d parts", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[index])
	if err != nil {
		t.Fatalf("decoding DPoP proof: %v", err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("parsing DPoP proof: %v", err)
	}
}

func base64URLHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestRequestPKIIdentityCertificateUsesBoundCSRFlow(t *testing.T) {
	privateKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseECPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := testLeafPEM(t, key)

	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "DPoP identity-access-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/pkcs10" {
			t.Errorf("Content-Type = %q", got)
		}
		csrBody, readErr := io.ReadAll(r.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		block, _ := pem.Decode(csrBody)
		if block == nil || block.Type != "CERTIFICATE REQUEST" {
			t.Fatalf("request is not a PEM CSR: %q", csrBody)
		}
		csr, parseErr := x509.ParseCertificateRequest(block.Bytes)
		if parseErr != nil || csr.CheckSignature() != nil {
			t.Fatalf("invalid CSR: %v", parseErr)
		}
		if csr.Subject.CommonName != testOperatorSubject {
			t.Errorf("CSR CN = %q", csr.Subject.CommonName)
		}
		csrPublic, _ := x509.MarshalPKIXPublicKey(csr.PublicKey)
		keyPublic, _ := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if string(csrPublic) != string(keyPublic) {
			t.Error("CSR does not use the DPoP key")
		}

		proof := r.Header.Get("DPoP")
		var payload map[string]any
		decodeProofPart(t, proof, 1, &payload)
		if payload["htm"] != "POST" || payload["htu"] != endpoint {
			t.Errorf("DPoP target = %v %v", payload["htm"], payload["htu"])
		}
		if payload["ath"] != base64URLHash("identity-access-token") {
			t.Errorf("ath = %v", payload["ath"])
		}
		var header struct {
			JWK map[string]string `json:"jwk"`
		}
		decodeProofPart(t, proof, 0, &header)
		thumbprint, thumbErr := jwkThumbprint(&key.PublicKey)
		if thumbErr != nil {
			t.Fatal(thumbErr)
		}
		canonical := `{"crv":"` + header.JWK["crv"] + `","kty":"` + header.JWK["kty"] + `","x":"` + header.JWK["x"] + `","y":"` + header.JWK["y"] + `"}`
		if base64URLHash(canonical) != thumbprint {
			t.Error("DPoP proof does not embed the CSR key")
		}

		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_, _ = io.WriteString(w, leafPEM+leafPEM)
	}))
	defer server.Close()
	endpoint = server.URL + "/v1/identity/certificate"

	got, err := requestPKIIdentityCertificate(
		context.Background(), server.Client(), endpoint, privateKeyPEM, key,
		"identity-access-token", testOperatorTenant, testOperatorSubject,
	)
	if err != nil {
		t.Fatalf("requestPKIIdentityCertificate: %v", err)
	}
	if got.PemCertificate != leafPEM || got.PemCertificateChain != leafPEM {
		t.Fatalf("certificate split incorrectly: %+v", got)
	}
	if got.PemPrivateKey != privateKeyPEM {
		t.Fatal("certificate did not retain the DPoP/CSR private key")
	}
	if got.PrincipalURI != "spiffe://wendy.sh/tenant/"+testOperatorTenant+"/operator/"+testOperatorSubject {
		t.Fatalf("principal URI = %q", got.PrincipalURI)
	}
}

func TestRequestPKIIdentityCertificateRejectsUnauthorized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()
	privateKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseECPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	_, err = requestPKIIdentityCertificate(
		context.Background(), server.Client(), server.URL+"/v1/identity/certificate",
		privateKeyPEM, key, "token", testOperatorTenant, testOperatorSubject,
	)
	if err == nil || !strings.Contains(err.Error(), "realm-to-tenant mapping and PKI audience") {
		t.Fatalf("error = %v", err)
	}
}

func TestSplitCertificateChainPEMRejectsNonCertificate(t *testing.T) {
	_, _, _, err := splitCertificateChainPEM([]byte("not a certificate"))
	if err == nil {
		t.Fatal("expected malformed chain error")
	}
}

func TestSplitCertificateChainPEMKeepsUnsupportedChainOpaque(t *testing.T) {
	privateKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := parseECPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := testLeafPEM(t, key)
	// pki-core chains may use algorithms this Go toolchain cannot parse. Only
	// the P-256 leaf must be parsed to check the CSR key; retain later DER
	// blocks verbatim for TLS to send on the wire.
	opaqueChainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte{1, 2, 3}}))
	gotLeaf, gotChain, _, err := splitCertificateChainPEM([]byte(leafPEM + opaqueChainPEM))
	if err != nil {
		t.Fatalf("splitCertificateChainPEM rejected opaque chain: %v", err)
	}
	if gotLeaf != leafPEM || gotChain != opaqueChainPEM {
		t.Fatal("certificate chain was not retained leaf-first")
	}
}

func TestCertXFCCUsesSPIFFEPrincipal(t *testing.T) {
	const principal = "spiffe://wendy.sh/tenant/2558fd76-afc7-466e-9613-6b715296a526/operator/alice"
	got := certXFCC(config.CertificateInfo{
		PrincipalURI:   principal,
		OrganizationID: 7,
		UserID:         "legacy-user",
	})
	if got != "URI="+principal {
		t.Fatalf("certXFCC = %q", got)
	}
}
