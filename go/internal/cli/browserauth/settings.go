package browserauth

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// Settings pins the identity, PKI and Cloud services for a deployment. Tool
// arguments and OAuth redirect parameters cannot change these destinations.
type Settings struct {
	AuthBase         string `json:"auth_base"`
	ClientID         string `json:"client_id"`
	IdentityResource string `json:"identity_resource"`
	IdentityEndpoint string `json:"identity_endpoint"`
	CloudResource    string `json:"cloud_resource"`
	CloudGRPC        string `json:"cloud_grpc"`
	RelayIssuer      string `json:"relay_issuer"`
}

func (s *Session) settings() Settings {
	if s.Settings != nil {
		return *s.Settings
	}
	return Settings{AuthBase, ClientID, IdentityResource, IdentityEndpoint, CloudResource, "api.dev.wendy.sh:443", "https://api.dev.wendy.sh"}
}

func (c Settings) Validate() error {
	for _, raw := range []string{c.AuthBase, c.IdentityResource, c.IdentityEndpoint, c.CloudResource, c.RelayIssuer} {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return fmt.Errorf("Cloud login services require explicit HTTPS URLs")
		}
	}
	auth, _ := url.Parse(c.AuthBase)
	relay, _ := url.Parse(c.RelayIssuer)
	identity, _ := url.Parse(c.IdentityEndpoint)
	host, port, err := net.SplitHostPort(c.CloudGRPC)
	if c.ClientID == "" || auth.Path != "" || relay.Path != "" || identity.Path != "/v1/identity/certificate" || err != nil || host == "" || port != "443" || strings.ContainsAny(c.CloudGRPC, "/?#@") {
		return fmt.Errorf("invalid Cloud login client or service authority")
	}
	return nil
}

func (s *Session) validIssuer(raw string) bool {
	u, err := url.Parse(raw)
	base, _ := url.Parse(s.settings().AuthBase)
	if err != nil || base == nil || u.Scheme != "https" || u.Host != base.Host || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 3 || parts[1] != "realms" || parts[2] == "" {
		return false
	}
	for _, c := range parts[2] {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
