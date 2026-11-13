//go:build linux

package meshsharing

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

type linuxProbe struct{}

var meshIPv4 = []netip.Prefix{
	netip.MustParsePrefix("10.88.0.0/16"),
	netip.MustParsePrefix("10.99.0.0/16"),
}

func independentIPv4(raw string) bool {
	ip, err := netip.ParseAddr(raw)
	if err != nil || !ip.Is4() || ip.IsLoopback() || ip.IsMulticast() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range meshIPv4 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func hasPhysicalDefault(routes, iface string) bool {
	for _, line := range strings.Split(routes, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "default" || fields[1] != "via" || !independentIPv4(fields[2]) {
			continue
		}
		for i := 3; i+1 < len(fields); i++ {
			if fields[i] == "dev" && fields[i+1] == iface {
				return true
			}
		}
	}
	return false
}

// NewLinux may return a nonnil cleanup-only controller on failure. The caller
// must retry its Close before creating another policy, even if participation is
// disabled after the failed initialization.
func NewLinux(ctx context.Context, credentials *localmesh.Credentials, node Node) (*Controller, error) {
	policy, err := newLinuxTunnelPolicy(ctx, credentials)
	if err != nil {
		if policy != nil {
			return cleanupController(policy), err
		}
		return nil, err
	}
	c, err := NewWithDeps(credentials.Device, node, policy, linuxProbe{})
	if err != nil {
		return cleanupController(policy), errors.Join(err, policy.Close())
	}
	return c, nil
}

func command(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %v: %w: %s", name, args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (linuxProbe) Candidates(ctx context.Context, cfg Config) (map[string]string, error) {
	found := map[string]string{}
	out, err := command(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE", "device", "status")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Split(line, ":")
		if len(fields) != 3 || fields[2] != "connected" || (fields[1] != "wifi" && fields[1] != "ethernet") {
			continue
		}
		iface := fields[0]
		if !validPhysicalName(iface) || slices.Contains(cfg.ExcludedInterfaces, iface) ||
			(len(cfg.UplinkInterfaces) > 0 && !slices.Contains(cfg.UplinkInterfaces, iface)) {
			continue
		}
		// The physical default must exist on this device independently of the
		// borrowed wlmp* default. NM's device type alone cannot prove egress.
		// Query unfiltered routes: iproute2 may omit the dev token from
		// output when a dev selector was supplied, hiding a valid default.
		routes, err := command(ctx, "ip", "-4", "route", "show", "default")
		if err != nil || routes == "" {
			continue
		}
		if !hasPhysicalDefault(routes, iface) {
			continue
		}
		dns, err := command(ctx, "nmcli", "-g", "IP4.DNS", "device", "show", iface)
		if err != nil {
			continue
		}
		for _, raw := range strings.Fields(dns) {
			if independentIPv4(raw) {
				found[iface] = raw
				break
			}
		}
	}
	return found, nil
}

func bindDevice(iface string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var inner error
		err := raw.Control(func(fd uintptr) {
			inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, iface)
		})
		if err != nil {
			return err
		}
		return inner
	}
}

func (linuxProbe) Uplink(parent context.Context, iface, dns string) bool {
	// Every lease grants IPv4. An IPv6-only HTTPS success must not establish
	// that mandatory uplink; IPv6 has its own optional-family health probe.
	return probeUplinkFamily(parent, iface, dns, "tcp4")
}

func probeUplinkIPv6(ctx context.Context, iface, dns string) bool {
	return probeUplinkFamily(ctx, iface, dns, "tcp6")
}

func probeUplinkFamily(parent context.Context, iface, dns, family string) bool {
	if !validPhysicalName(iface) {
		return false
	}
	if !independentIPv4(dns) {
		return false
	}
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		dialer := net.Dialer{Control: bindDevice(iface)}
		return dialer.DialContext(ctx, network, net.JoinHostPort(dns, "53"))
	}}
	dialer := &net.Dialer{Control: bindDevice(iface), Resolver: resolver}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if family != "" {
			network = family
		}
		return dialer.DialContext(ctx, network, address)
	}, DisableKeepAlives: true, Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://connectivitycheck.gstatic.com/generate_204", nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusNoContent
}

// Gateway probes the donor's ULA over the mesh route with an ICMPv6 echo
// bound to this host's own ULA: a reply proves the route the decision names.
// A BLE hop regularly answers in over a second, so wait two; the caller's
// three-second bound still applies.
func (linuxProbe) Gateway(ctx context.Context, local, remote netip.Addr) bool {
	if !meshid.InMesh(local) || !meshid.InMesh(remote) {
		return false
	}
	_, err := command(ctx, "ping", "-n", "-6", "-I", local.String(), "-c", "1", "-W", "2", remote.String())
	return err == nil
}
