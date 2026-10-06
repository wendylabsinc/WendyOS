package cloudmcp

import (
	"context"
	"crypto/mldsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type testMachine struct{ auth *config.AuthConfig }

func (m testMachine) CloudCredentials(context.Context) (*config.AuthConfig, error) {
	return m.auth, nil
}

type backendTransport func(*http.Request) (*http.Response, error)

func (f backendTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestBackendAuthenticatesDiscoveryAndPosts(t *testing.T) {
	private, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(private))
	if err != nil {
		t.Fatal(err)
	}
	auth := &config.AuthConfig{APIKey: "machine-token", DPoPPrivateKey: private}
	calls := 0
	b := &CloudBackend{origin: "https://cloud.example", sessions: map[string]machineCredentials{testOrg: testMachine{auth}}, client: &http.Client{Transport: backendTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("Authorization") != "DPoP machine-token" {
			t.Fatal("missing machine authorization")
		}
		parts := strings.Split(r.Header.Get("DPoP"), ".")
		if len(parts) != 3 {
			t.Fatal("missing DPoP")
		}
		payload, err := base64.RawURLEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatal(err)
		}
		var claims map[string]any
		if err := json.Unmarshal(payload, &claims); err != nil {
			t.Fatal(err)
		}
		if claims["htm"] != r.Method || claims["htu"] != r.URL.String() || claims["ath"] == nil {
			t.Fatal("unbound proof")
		}
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil {
			t.Fatal(err)
		}
		if err := mldsa.Verify(key.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), signature, nil); err != nil {
			t.Fatal(err)
		}
		if calls == 1 && (r.Method != "GET" || r.Body != nil) {
			t.Fatal("discovery is not GET")
		}
		if calls == 2 && (r.Method != "POST" || r.Header.Get("Content-Type") != "application/json") {
			t.Fatal("invalid POST")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})}}
	if _, err := b.Organization(context.Background(), testOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := b.request(context.Background(), testOrg, "authorize", map[string]string{"user_token": "user"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	b.sessions = nil
	if _, err := b.Organization(context.Background(), testOrg); err == nil || calls != 2 {
		t.Fatal("unprovisioned discovery reached Cloud")
	}
}

func TestBackendCannotFallBackToMachineDeviceAuthority(t *testing.T) {
	b := &CloudBackend{}
	if _, err := b.Connect(context.Background(), Access{}, testDevice); err != errUserAuthorityRequired {
		t.Fatal(err)
	}
	for _, service := range []string{"ssh", "wendy-registry", "wendy-registry-darwin"} {
		if _, err := b.OpenService(context.Background(), Access{}, testDevice, service); err != errUserAuthorityRequired {
			t.Fatal(err)
		}
	}
}
