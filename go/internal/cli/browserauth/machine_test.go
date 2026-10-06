package browserauth

import (
	"bytes"
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func machineFixture(t *testing.T, scenario string) (*MachineSession, *int) {
	t.Helper()
	settings := (&Session{}).settings()
	issuer := settings.AuthBase + "/realms/example"
	tenant := "12345678-1234-1234-1234-123456789abc"
	private, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(private))
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := cloudrequest.OperatorJWKThumbprint(key)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	seen := map[string]bool{}
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.String() {
		case issuer + "/.well-known/openid-configuration":
			return reply(200, metadata{issuer, issuer + "/authorize", issuer + "/oauth2/token", issuer + "/.well-known/jwks.json"}), nil
		case issuer + "/.well-known/jwks.json":
			return reply(200, map[string]any{"keys": []any{map[string]string{"kid": "authority", "alg": "ML-DSA-65", "kty": "AKP", "pub": base64.RawURLEncoding.EncodeToString(authority.PublicKey().Bytes())}}}), nil
		case issuer + "/oauth2/token":
			calls++
			if scenario == "disabled" {
				return reply(400, map[string]string{"error": "invalid_grant"}), nil
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || r.Form.Get("refresh_token") != "" {
				t.Fatal("not a machine grant")
			}
			parts := strings.Split(r.Form.Get("assertion"), ".")
			if len(parts) != 3 {
				t.Fatal("missing assertion")
			}
			decode := base64.RawURLEncoding.DecodeString
			header, _ := decode(parts[0])
			payload, _ := decode(parts[1])
			sig, _ := decode(parts[2])
			var h map[string]string
			var p map[string]any
			if json.Unmarshal(header, &h) != nil || json.Unmarshal(payload, &p) != nil {
				t.Fatal("invalid assertion")
			}
			if h["kid"] != thumb || h["alg"] != "ML-DSA-65" || p["iss"] != "mcp-service" || p["sub"] != "mcp-service" || p["aud"] != r.URL.String() {
				t.Fatal("incorrect assertion binding")
			}
			if err := mldsa.Verify(key.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
				t.Fatal(err)
			}
			jti, _ := p["jti"].(string)
			if jti == "" || seen[jti] {
				t.Fatal("assertion replay")
			}
			seen[jti] = true
			claims := map[string]any{"iss": issuer, "sub": "mcp-service", "tenant_uuid": tenant, "principal_kind": "service", "aud": r.Form.Get("resource"), "exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": thumb}}
			switch scenario {
			case "human":
				claims["principal_kind"] = "user"
			case "tenant":
				claims["tenant_uuid"] = "aaaaaaaa-1234-1234-1234-123456789abc"
			case "subject":
				claims["sub"] = "other-service"
			case "audience":
				claims["aud"] = "https://other.test"
			case "binding":
				claims["cnf"] = map[string]string{"jkt": "other-key"}
			case "expired":
				claims["exp"] = time.Now().Add(-time.Minute).Unix()
			}
			b, _ := json.Marshal(claims)
			hb, _ := json.Marshal(map[string]string{"kid": "authority", "alg": "ML-DSA-65", "typ": "at+jwt"})
			input := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(b)
			signature, err := authority.Sign(rand.Reader, []byte(input), &mldsa.Options{})
			if err != nil {
				t.Fatal(err)
			}
			tokenType := "DPoP"
			if scenario == "bearer" {
				tokenType = "Bearer"
			}
			return reply(200, map[string]any{"access_token": input + "." + base64.RawURLEncoding.EncodeToString(signature), "token_type": tokenType, "expires_in": 3600}), nil
		case settings.IdentityEndpoint:
			if !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP ") || r.Header.Get("DPoP") == "" {
				t.Fatal("missing certificate PoP")
			}
			body, _ := io.ReadAll(r.Body)
			block, _ := pem.Decode(body)
			if block == nil {
				t.Fatal("missing CSR")
			}
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				t.Fatal(err)
			}
			if err := csr.CheckSignature(); err != nil {
				t.Fatal(err)
			}
			kind := "service"
			if scenario == "operator-cert" {
				kind = "operator"
			}
			uri, _ := url.Parse(fmt.Sprintf("spiffe://wendy.sh/tenant/%s/%s/mcp-service", tenant, kind))
			leaf := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature}
			if scenario == "expired-cert" {
				leaf.NotAfter = time.Now().Add(-time.Second)
			}
			public := csr.PublicKey
			if scenario == "wrong-cert-key" {
				public = authority.Public()
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, public, authority)
			if err != nil {
				t.Fatal(err)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))}, nil
		}
		t.Fatalf("unexpected endpoint %s", r.URL)
		return nil, nil
	})
	m, err := NewMachineSession(settings, issuer, tenant, "mcp-service", private, client)
	if err != nil {
		t.Fatal(err)
	}
	return m, &calls
}

func TestMachineCredentialsAndRefresh(t *testing.T) {
	m, calls := machineFixture(t, "")
	auth, err := m.Credentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 2 || len(auth.Certificates) != 1 || !strings.Contains(auth.Certificates[0].PrincipalURI, "/service/mcp-service") || auth.RefreshToken != "" {
		t.Fatal("incorrect machine session")
	}
	auth.Certificates[0].PrincipalURI = "modified"
	second, err := m.Credentials(context.Background())
	if err != nil || *calls != 2 || second.Certificates[0].PrincipalURI == "modified" {
		t.Fatal("cached credential copy", err)
	}
	m.expires = time.Now()
	if _, err := m.Credentials(context.Background()); err != nil || *calls != 3 {
		t.Fatal("token refresh", err)
	}
	m.certificateExpires = time.Now()
	if _, err := m.Credentials(context.Background()); err != nil || *calls != 4 {
		t.Fatal("certificate reissuance", err)
	}
}

func TestMachineCredentialsRejectWrongAuthority(t *testing.T) {
	for _, scenario := range []string{"disabled", "human", "tenant", "subject", "audience", "binding", "expired", "bearer", "operator-cert", "expired-cert", "wrong-cert-key"} {
		t.Run(scenario, func(t *testing.T) {
			m, _ := machineFixture(t, scenario)
			if _, err := m.Credentials(context.Background()); err == nil {
				t.Fatal("accepted invalid machine credential")
			}
		})
	}
}

func TestMachineCloudCredentialsDoNotEnrollCertificate(t *testing.T) {
	m, calls := machineFixture(t, "")
	auth, err := m.CloudCredentials(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *calls != 1 || len(auth.Certificates) != 0 || !m.certificateExpires.IsZero() {
		t.Fatal("Cloud authentication enrolled a device credential")
	}
}
