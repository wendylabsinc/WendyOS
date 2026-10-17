package commands

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
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

// liteLeafPEM self-signs an operator leaf for key; marked adds pki-core's Lite EKU.
func liteLeafPEM(t *testing.T, key crypto.Signer, principal string, notAfter time.Time, marked bool) string {
	t.Helper()
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(7),
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{u},
	}
	if marked {
		template.UnknownExtKeyUsage = []asn1.ObjectIdentifier{oidLiteOperator}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func newP256KeyPEM(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	return keyPEM, key.(*ecdsa.PrivateKey)
}

func jwsHeader(t *testing.T, jws []byte) map[string]any {
	t.Helper()
	part, _, _ := strings.Cut(string(jws), ".")
	raw, err := base64.RawURLEncoding.DecodeString(part)
	if err != nil {
		t.Fatal(err)
	}
	var header map[string]any
	if err := json.Unmarshal(raw, &header); err != nil {
		t.Fatal(err)
	}
	return header
}

func TestLiteRelayOptionsUseTheLiteLeafNotThePrimary(t *testing.T) {
	t.Setenv("WENDY_LITE_OPERATOR_CERT", "")
	t.Setenv("WENDY_LITE_OPERATOR_KEY", "")
	auth := liteTestAuth("cloud.example:443")
	auth.PKIEndpoint = "https://identity.selected.example/v1/identity/certificate"
	auth.Certificates, _ = liteRelayTestPKI(t)
	lite, _ := liteRelayTestPKI(t)
	auth.LiteCertificate = lite[0].PemCertificate + lite[0].PemCertificateChain
	auth.LitePrivateKey = lite[0].PemPrivateKey

	options, err := liteRelayOptions(context.Background(), auth, false)
	if err != nil {
		t.Fatal(err)
	}
	if options.OperatorIdentity.PemCertificate != lite[0].PemCertificate || options.OperatorIdentity.PemCertificateChain != lite[0].PemCertificateChain {
		t.Fatal("Lite lane did not select the session's Lite leaf and chain")
	}
	if options.DeviceTrustChainPEM != auth.Certificates[0].PemCertificateChain {
		t.Fatal("Lite relay did not use the primary session's device trust chain")
	}
	proof, err := options.SignRequest([]byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := pem.Decode([]byte(lite[0].PemCertificate))
	if got := relayRequestCertificate(t, proof); !bytes.Equal(got, leaf.Bytes) {
		t.Fatal("Lite request x5c is not the Lite leaf")
	}
	if alg := jwsHeader(t, proof)["alg"]; alg != "ES256" {
		t.Fatalf("Lite request alg = %v, want ES256", alg)
	}
	if options.DeviceTrustFallback == nil {
		t.Fatal("missing lazy P-256 device authority discovery")
	}
	// A later session switch must not redirect an in-flight device handshake's
	// trust lookup. Cancellation proves the original valid target was selected
	// without making a network request.
	auth.PKIEndpoint = ""
	auth.Certificates[0].PrincipalURI = "invalid"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = options.DeviceTrustFallback(ctx)
	var lookupErr *url.Error
	wantEndpoint := "https://est.selected.example/.well-known/est/" + liteTestTenant + "/cacerts?chain_algorithm=ECDSA_P256"
	if !errors.Is(err, context.Canceled) || !errors.As(err, &lookupErr) || lookupErr.URL != wantEndpoint {
		t.Fatalf("device trust discovery did not retain its selected PKI and tenant: %v", err)
	}
}

// Without a usable Lite leaf the CLI requests one; it never presents the
// primary. A non-OIDC session cannot request, so the request path errors.
func TestLiteRelayOptionsRequestInsteadOfFallingBack(t *testing.T) {
	t.Setenv("WENDY_LITE_OPERATOR_CERT", "")
	t.Setenv("WENDY_LITE_OPERATOR_KEY", "")
	principal := "spiffe://wendy.sh/tenant/" + liteTestTenant + "/operator/test"
	keyPEM, key := newP256KeyPEM(t)
	for _, tc := range []struct {
		name  string
		leaf  string
		renew bool
	}{
		{name: "no Lite leaf"},
		{name: "Lite leaf expires within a session", leaf: liteLeafPEM(t, key, principal, time.Now().Add(10*time.Minute), true)},
		{name: "Lite leaf of another operator", leaf: liteLeafPEM(t, key, principal+"-other", time.Now().Add(time.Hour), true)},
		{name: "Cloud refused the stored leaf", leaf: liteLeafPEM(t, key, principal, time.Now().Add(time.Hour), true), renew: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth := liteTestAuth("cloud.example:443")
			auth.Certificates, _ = liteRelayTestPKI(t)
			if tc.leaf != "" {
				auth.LiteCertificate, auth.LitePrivateKey = tc.leaf, keyPEM
			}
			_, err := liteRelayOptions(context.Background(), auth, tc.renew)
			if err == nil || !strings.Contains(err.Error(), "needs a Lite operator certificate") {
				t.Fatalf("expected a Lite leaf request, got %v", err)
			}
		})
	}
}

func TestTunnelV2SignerKeepsTheMLDSAPrimary(t *testing.T) {
	t.Setenv("WENDY_TUNNEL_SIGNING_CERT", "")
	t.Setenv("WENDY_TUNNEL_SIGNING_KEY", "")
	primaryKeyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	primaryKey, err := certs.ParseSigningPrivateKeyPEM([]byte(primaryKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject
	primaryPEM := testLeafPEM(t, primaryKey)
	liteKeyPEM, liteKey := newP256KeyPEM(t)
	auth := &config.AuthConfig{
		Certificates:    []config.CertificateInfo{{PemCertificate: primaryPEM, PemPrivateKey: primaryKeyPEM, PrincipalURI: principal}},
		LiteCertificate: liteLeafPEM(t, liteKey, principal, time.Now().Add(time.Hour), true),
		LitePrivateKey:  liteKeyPEM,
	}
	sign, err := tunnelPrincipalSigner(auth)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := sign([]byte("request"))
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := pem.Decode([]byte(primaryPEM))
	if got := relayRequestCertificate(t, proof); !bytes.Equal(got, leaf.Bytes) {
		t.Fatal("tunnel v2 request is not signed by the ML-DSA primary")
	}
	if alg := jwsHeader(t, proof)["alg"]; alg != "ML-DSA-65" {
		t.Fatalf("tunnel v2 alg = %v, want ML-DSA-65", alg)
	}
}

// The Lite leaf is requested with a P-256 CSR, proved by the ML-DSA DPoP key.
func TestRequestLiteLeafUsesP256CSROverMLDSASession(t *testing.T) {
	sessionKeyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	sessionKey, err := certs.ParseSigningPrivateKeyPEM([]byte(sessionKeyPEM))
	if err != nil {
		t.Fatal(err)
	}
	liteKeyPEM, liteKey := newP256KeyPEM(t)
	principal := "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject
	for _, marked := range []bool{true, false} {
		leafPEM := liteLeafPEM(t, liteKey, principal, time.Now().Add(time.Hour), marked)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			block, _ := pem.Decode(body)
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil || csr.CheckSignature() != nil {
				t.Fatalf("invalid CSR: %v", err)
			}
			if key, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok || key.Curve != elliptic.P256() || !key.Equal(liteKey.Public()) {
				t.Error("CSR does not carry the Lite P-256 key")
			}
			var header struct {
				Alg string `json:"alg"`
			}
			decodeProofPart(t, r.Header.Get("DPoP"), 0, &header)
			if header.Alg != "ML-DSA-65" {
				t.Errorf("DPoP alg = %q, want the ML-DSA session key", header.Alg)
			}
			_, _ = io.WriteString(w, leafPEM)
		}))
		got, err := requestPKIIdentityCertificate(
			context.Background(), server.Client(), server.URL+"/v1/identity/certificate", liteKeyPEM, sessionKey,
			"identity-access-token", testOperatorTenant, testOperatorSubject,
		)
		server.Close()
		if err != nil {
			t.Fatalf("requestPKIIdentityCertificate: %v", err)
		}
		if err := checkLiteOperatorLeaf(got.PemCertificate); (err == nil) != marked {
			t.Fatalf("marked=%v: checkLiteOperatorLeaf = %v", marked, err)
		}
	}
}

// liteIssuingSession is an OIDC session whose realm rotates refresh tokens and
// whose pki-core issues a marked Lite leaf for a P-256 CSR (or refuses, 403).
func liteIssuingSession(t *testing.T, refuse bool) (*config.AuthConfig, func() *config.Config, *int) {
	t.Helper()
	t.Setenv("WENDY_SECRET_STORE", "file")
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := operatorJWKThumbprint(key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject
	issued := new(int)
	var issuer string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(oidcProviderMetadata{Issuer: issuer, AuthorizationEndpoint: issuer + "/authorize", TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []oidcJWK{testES256JWK(authority, "authority")}})
		case "/token":
			_ = r.ParseForm()
			next := map[string]string{"refresh-1": "refresh-2", "refresh-2": "refresh-3"}[r.Form.Get("refresh_token")]
			claims := map[string]any{"iss": issuer, "aud": r.Form.Get("resource"), "sub": testOperatorSubject, "exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": thumbprint}}
			_ = json.NewEncoder(w).Encode(oidcTokenResponse{AccessToken: testES256Token(t, authority, "authority", claims), RefreshToken: next, ExpiresIn: 3600, TokenType: "DPoP"})
		case "/v1/identity/certificate":
			if refuse {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			body, _ := io.ReadAll(r.Body)
			block, _ := pem.Decode(body)
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			leafKey, ok := csr.PublicKey.(*ecdsa.PublicKey)
			if !ok {
				t.Fatal("Lite CSR is not P-256")
			}
			*issued++
			// Self-signed by the CA stand-in, for the CSR's key.
			tmpl := &x509.Certificate{SerialNumber: big.NewInt(9), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, UnknownExtKeyUsage: []asn1.ObjectIdentifier{oidLiteOperator}}
			u, _ := url.Parse(principal)
			tmpl.URIs = []*url.URL{u}
			der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, leafKey, authority)
			if err != nil {
				t.Fatal(err)
			}
			_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: der})
		default:
			http.NotFound(w, r)
		}
	}))
	issuer = server.URL
	t.Cleanup(server.Close)
	primary := config.CertificateInfo{PemCertificate: testLeafPEM(t, key), PemPrivateKey: keyPEM, PrincipalURI: principal}
	auth := &config.AuthConfig{
		CloudDashboard: "https://cloud.example", CloudGRPC: "cloud.example:443",
		OAuthIssuer: issuer, OAuthClientID: "wendy-cli", OAuthResource: "https://cloud.example/api",
		PKIResource: "https://pki.example/identity", PKIEndpoint: issuer + "/v1/identity/certificate",
		OAuthExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		APIKey:         "api-token", RefreshToken: "refresh-1", DPoPPrivateKey: keyPEM,
		Certificates: []config.CertificateInfo{primary},
	}
	return auth, seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}}), issued
}

func TestLiteOperatorCertificateIssuesAndPersists(t *testing.T) {
	auth, reload, issued := liteIssuingSession(t, false)
	primary := auth.Certificates[0]
	cert, err := liteOperatorCertificate(context.Background(), auth, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLiteOperatorLeaf(cert.PemCertificate); err != nil {
		t.Fatal(err)
	}
	saved := reload().Auth[0]
	if saved.LiteCertificate == "" || saved.LitePrivateKey == "" || saved.RefreshToken != "refresh-3" {
		t.Fatalf("Lite leaf or rotated refresh token not persisted: %+v", saved)
	}
	if saved.Certificates[0].PemCertificate != primary.PemCertificate {
		t.Fatal("Lite issuance replaced the primary")
	}
	// A fresh stored leaf is reused; renew asks pki-core again.
	if _, err := liteOperatorCertificate(context.Background(), auth, false); err != nil || *issued != 1 {
		t.Fatalf("stored leaf not reused: issued=%d err=%v", *issued, err)
	}
}

func TestLiteOperatorCertificateKeepsRotatedTokenOnRefusal(t *testing.T) {
	auth, reload, _ := liteIssuingSession(t, true)
	if _, err := liteOperatorCertificate(context.Background(), auth, false); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("expected pki-core's refusal, got %v", err)
	}
	if saved := reload().Auth[0]; saved.RefreshToken != "refresh-2" || saved.LiteCertificate != "" {
		t.Fatalf("refusal lost the rotated refresh token or stored a leaf: %+v", saved)
	}
}

// pki-core caps the leaf at the primary's notAfter; a leaf already at that cap
// is as long as a new one would be, so it is reused rather than re-requested.
func TestSessionLiteCertificateReusesLeafCappedByPrimary(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/" + liteTestTenant + "/operator/test"
	keyPEM, key := newP256KeyPEM(t)
	expiry := time.Now().Add(10 * time.Minute).Truncate(time.Second)
	auth := liteTestAuth("cloud.example:443")
	auth.Certificates[0].PemCertificate = liteLeafPEM(t, key, principal, expiry, false)
	auth.LiteCertificate, auth.LitePrivateKey = liteLeafPEM(t, key, principal, expiry, true), keyPEM
	if _, ok := sessionLiteCertificate(auth, liteRenewMargin); !ok {
		t.Fatal("leaf at the primary's notAfter was treated as stale")
	}
	auth.Certificates[0].PemCertificate = liteLeafPEM(t, key, principal, expiry.Add(time.Hour), false)
	if _, ok := sessionLiteCertificate(auth, liteRenewMargin); ok {
		t.Fatal("short leaf under a longer primary was reused")
	}
}
