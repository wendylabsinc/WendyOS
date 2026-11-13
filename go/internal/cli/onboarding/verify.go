package onboarding

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type VerifyOptions struct {
	Address           string
	OSVersion         string
	DeviceType        string
	PublicKey         string
	RequireEnrollment bool
	Timeout           time.Duration
}

type Verification struct {
	Address      string   `json:"address"`
	Reachable    bool     `json:"reachable"`
	Verified     bool     `json:"verified"`
	AgentVersion string   `json:"agent_version,omitempty"`
	OSVersion    string   `json:"os_version,omitempty"`
	DeviceType   string   `json:"device_type,omitempty"`
	PublicKey    string   `json:"public_key,omitempty"`
	Enrollment   string   `json:"enrollment"`
	Application  string   `json:"application"`
	Problems     []string `json:"problems"`
}

// Verify checks an explicit endpoint once with a bounded deadline. Callers may
// repeat it during first boot; it never substitutes a default or scans for a
// different device. An optional public key pins a previously recorded identity.
func Verify(ctx context.Context, opts VerifyOptions, connect func(context.Context, string) (*grpcclient.AgentConnection, error)) (*Verification, error) {
	address, err := verificationAddress(opts.Address)
	if err != nil {
		return nil, err
	}
	if connect == nil {
		return nil, fmt.Errorf("no device connector is available")
	}
	if opts.Timeout <= 0 || opts.Timeout > time.Minute {
		return nil, fmt.Errorf("timeout must be greater than zero and at most one minute")
	}
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	out := &Verification{Address: address, Enrollment: "unknown", Application: "not_checked", Problems: []string{}}
	conn, err := connect(ctx, address)
	if err != nil {
		out.Problems = append(out.Problems, err.Error())
		return out, nil
	}
	defer conn.Close()
	v, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		out.Problems = append(out.Problems, err.Error())
		return out, nil
	}
	out.Reachable = true
	out.AgentVersion, out.OSVersion, out.DeviceType, out.PublicKey = v.GetVersion(), v.GetOsVersion(), v.GetDeviceType(), v.GetPublicKey()
	for _, check := range []struct{ name, want, got string }{
		{"OS version", osVersionNumber(opts.OSVersion), osVersionNumber(out.OSVersion)},
		{"device type", opts.DeviceType, out.DeviceType},
		{"public key", opts.PublicKey, out.PublicKey},
	} {
		if check.want != "" && check.want != check.got {
			out.Problems = append(out.Problems, check.name+" does not match the expected installation")
		}
	}
	// Do not query another service on a device that failed the expected identity.
	if len(out.Problems) > 0 {
		return out, nil
	}
	p, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err == nil {
		switch p.GetResponse().(type) {
		case *agentpb.IsProvisionedResponse_Provisioned:
			out.Enrollment = "enrolled"
		case *agentpb.IsProvisionedResponse_NotProvisioned:
			out.Enrollment = "not_enrolled"
		}
	}
	if opts.RequireEnrollment && out.Enrollment != "enrolled" {
		out.Problems = append(out.Problems, "cloud enrollment is not verified")
	}
	out.Verified = len(out.Problems) == 0
	if out.Verified {
		analytics.TrackStage(ctx, "device_boot_verified", "wendy install verify", "completed", "", map[string]string{"step": "first_boot", "device_reachable": "true"})
	}
	return out, nil
}

func verificationAddress(address string) (string, error) {
	if address == "" || strings.TrimSpace(address) == "" {
		return "", fmt.Errorf("an explicit device address is required")
	}
	if strings.HasPrefix(address, "cloud:") {
		return address, nil
	}
	if strings.ContainsAny(address, " /\\\t\r\n") {
		return "", fmt.Errorf("invalid device address %q", address)
	}
	if host, port, err := net.SplitHostPort(address); err == nil {
		n, err := strconv.Atoi(port)
		if host == "" || err != nil || n < 1 || n > 65535 || (strings.Contains(host, ":") && !ipv6Literal(host)) {
			return "", fmt.Errorf("invalid device host or port")
		}
		return address, nil
	}
	host := address
	bracketed := strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]")
	if bracketed {
		host = host[1 : len(host)-1]
	}
	if (bracketed || strings.Contains(host, ":")) && !ipv6Literal(host) {
		return "", fmt.Errorf("invalid device address; use hostname, IP or host:port")
	}
	return net.JoinHostPort(host, "50051"), nil
}

// ipv6Literal reports whether host is an IPv6 address, including the zone
// wendy discover prints for a USB link-local address (fe80::…%en31), which
// net.ParseIP rejects. Interface names never contain a colon or bracket, so a
// zone with one is a mangled host:port, not an address.
func ipv6Literal(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Is6() && !strings.ContainsAny(ip.Zone(), ":[]")
}

// osVersionPrefix is the display prefix the agent puts on os_version
// (WendyOS-0.19.5); the installer and release manifest name the bare release.
const osVersionPrefix = "WendyOS-"

// osVersionNumber strips osVersionPrefix, in any case, so the version the
// installer prints and the one the agent reports compare equal. A bare prefix
// is left alone so it can never become an empty, unchecked expectation.
func osVersionNumber(v string) string {
	if len(v) > len(osVersionPrefix) && strings.EqualFold(v[:len(osVersionPrefix)], osVersionPrefix) {
		return v[len(osVersionPrefix):]
	}
	return v
}
