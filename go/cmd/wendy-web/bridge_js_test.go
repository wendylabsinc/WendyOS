//go:build js && wasm

package main

import (
	"bytes"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
)

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

type savedOperator struct {
	key   *mldsa.PrivateKey
	leaf  []byte
	token string
}

// saveOperator writes the record browserauth saves after a sign-in: a
// certificate and cloud token for a fresh operator key, issued by authority.
func saveOperator(t *testing.T, authority *mldsa.PrivateKey, issuer, tenant string) savedOperator {
	t.Helper()
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/operator/operator-test"
	uri, _ := url.Parse(principal)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "operator-test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature}
	leaf, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), authority)
	if err != nil {
		t.Fatal(err)
	}
	thumb, err := cloudrequest.OperatorJWKThumbprint(key)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := json.Marshal(map[string]string{"kid": "test", "alg": "ML-DSA-65"})
	body, _ := json.Marshal(map[string]any{"iss": issuer, "aud": browserauth.CloudResource, "sub": "operator-test", "tenant_uuid": tenant, "exp": time.Now().Add(time.Hour).Unix(), "cnf": map[string]string{"jkt": thumb}})
	input := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sig, err := authority.Sign(rand.Reader, []byte(input), crypto.Hash(0))
	if err != nil {
		t.Fatal(err)
	}
	access := input + "." + base64.RawURLEncoding.EncodeToString(sig)
	record, _ := json.Marshal(map[string]any{
		"version":     2,
		"issuer":      issuer,
		"certificate": map[string]string{"pemCertificate": string(pemCertificate(leaf)), "principalUri": principal},
		"tokens":      map[string]any{"access_token": access, "refresh_token": "refresh-" + tenant, "token_type": "DPoP", "expires_in": 3600},
		"profile":     map[string]string{"subject": "operator-test", "tenant": tenant, "issuer": issuer, "principal": principal, "expires": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	})
	if err = (browserCredentialStore{}).SaveWrapped(record, key.Bytes(), "operator-key:"+tenant+":operator-test"); err != nil {
		t.Fatal(err)
	}
	return savedOperator{key, leaf, access}
}

func pemCertificate(der []byte) []byte {
	return []byte("-----BEGIN CERTIFICATE-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END CERTIFICATE-----\n")
}

func TestCredentialAndSignUseEachOrganizationsSavedOperator(t *testing.T) {
	loadCredentialScript(t)
	authority, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	issuer := browserauth.AuthBase + "/realms/acme"
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kid": "test", "alg": "ML-DSA-65", "kty": "AKP", "pub": base64.RawURLEncoding.EncodeToString(authority.Public().(*mldsa.PublicKey).Bytes())}}})
	discovery, _ := json.Marshal(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/oauth2/token", "jwks_uri": issuer + "/.well-known/jwks.json"})
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		body := map[string][]byte{issuer + "/.well-known/openid-configuration": discovery, issuer + "/.well-known/jwks.json": jwks}[r.URL.String()]
		if body == nil {
			t.Fatalf("unexpected request to %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	const tenantA, tenantB = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	a := saveOperator(t, authority, issuer, tenantA)
	b := saveOperator(t, authority, issuer, tenantB)
	br := &bridge{auth: &browserauth.Session{Client: client, Store: browserCredentialStore{}}}
	call := func(method, tenant string, data []byte) (any, error) {
		params, _ := json.Marshal(map[string]string{"tenant": tenant})
		return br.handle(request{Method: method, Params: params, bytes: data})
	}

	value, err := call("credential", tenantA, nil)
	if err != nil {
		t.Fatal(err)
	}
	cred := value.(map[string]any)
	jwk, _, _ := cloudrequest.OperatorPublicJWK(a.key)
	if cred["accessToken"] != a.token || cred["algorithm"] != "ML-DSA-65" || !bytes.Equal(cred["chainDER"].([][]byte)[0], a.leaf) || cred["publicJWK"].(map[string]string)["pub"] != jwk["pub"] || cred["profile"].(browserauth.Profile).Tenant != tenantA {
		t.Fatalf("credential is not organization A's operator: %v", cred)
	}
	if raw, _ := json.Marshal(cred); bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(a.key.Bytes()))) {
		t.Fatal("credential carries the operator key")
	}
	for _, op := range []savedOperator{a, b, a} {
		tenant := tenantA
		if op.token == b.token {
			tenant = tenantB
		}
		message := []byte("header.payload-" + tenant)
		sig, err := call("sign", tenant, message)
		if err != nil {
			t.Fatal(err)
		}
		if err = mldsa.Verify(op.key.Public().(*mldsa.PublicKey), message, sig.(signature), nil); err != nil {
			t.Fatalf("signature for %s is not its operator's: %v", tenant, err)
		}
	}
	if _, err = call("auth-signout", tenantB, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = call("sign", tenantB, []byte("x")); err == nil || err.Error() != "Sign in with Wendy first" {
		t.Fatalf("signed for an organization that signed out: %v", err)
	}
	if _, err = call("sign", tenantA, []byte("x")); err != nil {
		t.Fatalf("signing out of B signed out A: %v", err)
	}
	if _, err = call("auth-signout", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = call("sign", tenantA, []byte("x")); err == nil {
		t.Fatal("signed after signing out of every organization")
	}

	// A stale or tampered slot: B's slot holds A's record and key. A valid
	// sign-in to A must not sign for B.
	a = saveOperator(t, authority, issuer, tenantA)
	if err = selectOrg(org{Tenant: tenantA}); err != nil {
		t.Fatal(err)
	}
	record, err := browserCredentialStore{}.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = (browserCredentialStore{}).SaveWrapped(record, a.key.Bytes(), "operator-key:"+tenantB+":operator-test"); err != nil {
		t.Fatal(err)
	}
	if _, err = call("sign", tenantB, []byte("x")); err == nil || err.Error() != "Sign in with Wendy first" {
		t.Fatalf("signed for B with the record in B's slot that belongs to A: %v", err)
	}
	if _, err = call("credential", tenantB, nil); err == nil {
		t.Fatal("returned A's credential for B")
	}
}

// rotate is registry-key's alone: an unwrap carrying it must not replace the
// registered key.
func TestRegistryUnwrapDoesNotRotate(t *testing.T) {
	loadCredentialScript(t)
	authority, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	issuer := browserauth.AuthBase + "/realms/acme"
	jwks, _ := json.Marshal(map[string]any{"keys": []map[string]string{{"kid": "test", "alg": "ML-DSA-65", "kty": "AKP", "pub": base64.RawURLEncoding.EncodeToString(authority.Public().(*mldsa.PublicKey).Bytes())}}})
	discovery, _ := json.Marshal(map[string]string{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/oauth2/token", "jwks_uri": issuer + "/.well-known/jwks.json"})
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		body := map[string][]byte{issuer + "/.well-known/openid-configuration": discovery, issuer + "/.well-known/jwks.json": jwks}[r.URL.String()]
		if body == nil {
			t.Fatalf("unexpected request to %s", r.URL)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})
	const tenant = "11111111-1111-1111-1111-111111111111"
	saveOperator(t, authority, issuer, tenant)
	br := &bridge{auth: &browserauth.Session{Client: client, Store: browserCredentialStore{}}}
	call := func(method string, rotate bool) (any, error) {
		params, _ := json.Marshal(map[string]any{"tenant": tenant, "rotate": rotate})
		return br.handle(request{Method: method, Params: params})
	}
	keyID := func() any {
		value, err := call("registry-key", false)
		if err != nil {
			t.Fatal(err)
		}
		return value.(map[string]any)["keyId"]
	}
	registered := keyID()
	if _, err := call("registry-unwrap", true); err == nil {
		t.Fatal("unwrap of an empty wrap succeeded")
	}
	if got := keyID(); got != registered {
		t.Fatalf("registry-unwrap with rotate replaced the key: %v, was %v", got, registered)
	}
	if _, err := call("registry-key", true); err != nil {
		t.Fatal(err)
	}
	if got := keyID(); got == registered {
		t.Fatal("registry-key with rotate kept the key")
	}
}
