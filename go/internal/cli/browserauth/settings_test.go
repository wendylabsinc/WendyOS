package browserauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestConfiguredServicesAndRealmPinning(t *testing.T) {
	settings := (&Session{}).settings()
	settings.AuthBase = "https://auth.production.example"
	settings.ClientID = "hosted-login"
	settings.IdentityResource = "https://pki.production.example/identity"
	s := &Session{Settings: &settings}
	calls := 0
	s.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() == settings.AuthBase+"/api/login/realm" {
			return reply(200, map[string]string{"loginURL": "/realms/acme/account/login"}), nil
		}
		issuer := settings.AuthBase + "/realms/acme"
		if r.URL.String() != issuer+"/.well-known/openid-configuration" {
			t.Fatalf("unexpected destination %s", r.URL)
		}
		return reply(200, metadata{Issuer: issuer, Authorize: issuer + "/authorize", Token: issuer + "/oauth2/token", JWKS: issuer + "/.well-known/jwks.json"}), nil
	})
	login, err := s.Begin(context.Background(), "alice@example.com", "https://gateway.example/auth/callback")
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(login)
	if u.Query().Get("client_id") != "hosted-login" || u.Query().Get("resource") != settings.IdentityResource || !strings.HasPrefix(login, settings.AuthBase+"/realms/acme/authorize?") {
		t.Fatal("configured services not used", login)
	}
	before := calls
	if _, err := s.Complete(context.Background(), "code", s.state, settings.AuthBase+"/realms/other"); err == nil {
		t.Fatal("callback substituted another trusted realm")
	}
	if calls != before {
		t.Fatal("mixed-up callback made a token request")
	}
	if s.validIssuer(AuthBase + "/realms/system") {
		t.Fatal("production session accepts development issuer")
	}
}

func TestSettingsRejectAmbiguousEndpoints(t *testing.T) {
	for _, alter := range []func(*Settings){
		func(s *Settings) { s.AuthBase = "http://auth.example" },
		func(s *Settings) { s.AuthBase += "/" },
		func(s *Settings) { s.IdentityEndpoint += "?destination=other" },
		func(s *Settings) { s.IdentityEndpoint = "https://identity.example/other" },
		func(s *Settings) { s.CloudResource = "https://user:secret@cloud.example/api" },
		func(s *Settings) { s.CloudGRPC = "api.example:80" },
		func(s *Settings) { s.CloudGRPC = "api.example:443/other" },
		func(s *Settings) { s.RelayIssuer += "/grants" },
	} {
		settings := (&Session{}).settings()
		alter(&settings)
		if err := settings.Validate(); err == nil {
			t.Fatal("ambiguous service configuration accepted", settings)
		}
	}
}
