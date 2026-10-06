// Package delegation enforces the signed upper bound of an MCP operator grant.
// Live user permissions and grant revocation remain the control plane's responsibility.
package delegation

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

var appIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

var ScopeOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1, 4}
var EntitlementsOID = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1, 1}

type wireScope struct {
	Version          int
	DelegationID     string `asn1:"utf8"`
	OwnerPrincipal   string `asn1:"utf8"`
	DevicePrincipals []asn1.RawValue
	AppIDs           []asn1.RawValue
	Audience         string `asn1:"optional,utf8"`
	Gateway          string `asn1:"optional,utf8"`
	AllApps          bool   `asn1:"optional"`
}

type Scope struct {
	AllApps  bool
	Version  int
	Audience string
	Gateway  string
	ID       string
	Owner    string
	Devices  []string
	Apps     []string
	rules    []string
}

func values(raw []asn1.RawValue) ([]string, error) {
	if len(raw) > 128 {
		return nil, fmt.Errorf("too many scope entries")
	}
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, v := range raw {
		s := string(v.Bytes)
		if v.Class != 0 || v.Tag != asn1.TagUTF8String || v.IsCompound || !utf8.Valid(v.Bytes) || s == "" || len(s) > 512 || strings.ContainsAny(s, "*\x00\r\n") || seen[s] {
			return nil, fmt.Errorf("invalid or duplicate scope entry")
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// Parse rejects malformed constraints even when an extension is noncritical.
// A nil result means an ordinary, nondelegated certificate.
func Parse(leaf *x509.Certificate) (*Scope, error) {
	var scopeDER, entDER []byte
	for _, e := range leaf.Extensions {
		if e.Id.Equal(ScopeOID) {
			if scopeDER != nil || !e.Critical || len(e.Value) == 0 {
				return nil, fmt.Errorf("delegation scope must be unique and critical")
			}
			scopeDER = e.Value
		}
		if e.Id.Equal(EntitlementsOID) {
			if entDER != nil || len(e.Value) == 0 {
				return nil, fmt.Errorf("invalid entitlement extension")
			}
			entDER = e.Value
		}
	}
	if scopeDER == nil {
		return nil, nil
	}
	if len(scopeDER) > 16384 || len(entDER) == 0 || len(entDER) > 16384 {
		return nil, fmt.Errorf("missing or oversized delegation constraints")
	}
	var w wireScope
	rest, err := asn1.Unmarshal(scopeDER, &w)
	if err != nil || len(rest) != 0 || (w.Version != 1 && w.Version != 2 && w.Version != 3) {
		return nil, fmt.Errorf("unsupported delegation scope")
	}
	canonical, marshalErr := asn1.Marshal(w)
	if marshalErr != nil || !bytes.Equal(canonical, scopeDER) {
		return nil, fmt.Errorf("noncanonical delegation scope")
	}
	id, err := uuid.Parse(w.DelegationID)
	if err != nil || id.String() != w.DelegationID || id == uuid.Nil {
		return nil, fmt.Errorf("invalid delegation ID")
	}
	principal, ok := certs.TenantPrincipalFromCert(leaf)
	ownerID, ownerErr := certs.ParsePrincipal(principal)
	parts := strings.Split(principal, "/")
	if ownerErr != nil || !ok || len(parts) < 7 || parts[5] != "operator" || principal != w.OwnerPrincipal {
		return nil, fmt.Errorf("delegation owner does not match operator identity")
	}
	if w.Version >= 2 {
		audience, err := url.Parse(w.Audience)
		gateway, gatewayErr := certs.ParsePrincipal(w.Gateway)
		if err != nil || audience.Scheme != "https" || audience.Host == "" || audience.User != nil || audience.RawQuery != "" || audience.Fragment != "" || gatewayErr != nil || gateway.TenantUUID != ownerID.TenantUUID || !strings.HasPrefix(w.Gateway, strings.Join(parts[:5], "/")+"/service/") {
			return nil, fmt.Errorf("invalid MCP audience or gateway")
		}
	} else if w.Audience != "" || w.Gateway != "" {
		return nil, fmt.Errorf("v1 scope cannot carry v2 bindings")
	}
	if (w.Version == 3) != w.AllApps || (w.AllApps && len(w.AppIDs) != 0) {
		return nil, fmt.Errorf("all-app scope requires v3 and no explicit app IDs")
	}
	devices, err := values(w.DevicePrincipals)
	if err != nil || len(devices) == 0 {
		return nil, fmt.Errorf("invalid delegated devices")
	}
	prefix := strings.Join(parts[:5], "/") + "/device/"
	for _, d := range devices {
		deviceID, parseErr := certs.ParsePrincipal(d)
		if parseErr != nil || deviceID.TenantUUID != ownerID.TenantUUID || !strings.HasPrefix(d, prefix) || len(d) == len(prefix) {
			return nil, fmt.Errorf("delegated device must belong to operator tenant")
		}
	}
	apps, err := values(w.AppIDs)
	if err != nil {
		return nil, err
	}
	for _, a := range apps {
		if !appIDPattern.MatchString(a) {
			return nil, fmt.Errorf("invalid app ID")
		}
	}
	var raw []asn1.RawValue
	rest, err = asn1.Unmarshal(entDER, &raw)
	if err != nil || len(rest) != 0 {
		return nil, fmt.Errorf("invalid delegated entitlements")
	}
	rules, err := values(raw)
	if err != nil || len(rules) == 0 {
		return nil, fmt.Errorf("invalid delegated entitlements")
	}
	for _, r := range rules {
		p := strings.Split(r, ":")
		if len(p) != 4 || p[0] != "entitlement" || p[1] == "" || p[2] == "" || (p[3] != "allow" && p[3] != "deny") {
			return nil, fmt.Errorf("invalid delegated entitlement")
		}
	}
	return &Scope{AllApps: w.AllApps, Version: w.Version, Audience: w.Audience, Gateway: w.Gateway, ID: w.DelegationID, Owner: principal, Devices: devices, Apps: apps, rules: rules}, nil
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// Authorize admits only reviewed RPCs with explicit method and device grants.
// Unknown methods, shell, deployment, tunnels and unfiltered listings fail closed.
func (s *Scope) Authorize(device, method, app string) error {
	if !contains(s.Devices, device) {
		return fmt.Errorf("device is outside delegation")
	}
	p := strings.Split(strings.TrimPrefix(method, "/"), "/")
	if len(p) != 2 || !strings.HasPrefix(method, "/") {
		return fmt.Errorf("invalid method")
	}
	rule := "entitlement:" + p[0] + ":" + p[1] + ":"
	if contains(s.rules, rule+"deny") || !contains(s.rules, rule+"allow") {
		return fmt.Errorf("operation is outside delegation")
	}
	switch method {
	case "/wendy.agent.services.v2.WendyDeviceInfoService/GetDeviceInfo", "/wendy.agent.services.v2.WendyDeviceInfoService/ListHardwareCapabilities", "/wendy.agent.services.v1.WendyAgentService/ListHardwareCapabilities":
		return nil
	case "/wendy.agent.services.v2.WendyContainerService/StartContainer", "/wendy.agent.services.v2.WendyContainerService/StopContainer", "/wendy.agent.services.v1.WendyContainerService/StartContainer", "/wendy.agent.services.v1.WendyContainerService/StopContainer":
		if appIDPattern.MatchString(app) && ((s.Version == 3 && s.AllApps) || contains(s.Apps, app)) {
			return nil
		}
		return fmt.Errorf("app is outside delegation")
	default:
		return fmt.Errorf("operation has no delegated resource policy")
	}
}

func (s *Scope) Rules() []string { return append([]string(nil), s.rules...) }
