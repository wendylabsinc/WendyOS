package commands

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// formatNetworkInterfaces renders the device's network interfaces and their IP
// addresses as an aligned block suitable for printing under `wendy device info`,
// followed by the interface's uplink notes when the agent reports them. The
// returned string ends with a trailing newline. Interfaces with no addresses
// are skipped.
func formatNetworkInterfaces(ifaces []*agentpb.NetworkInterface) string {
	var rows strings.Builder
	tw := tabwriter.NewWriter(&rows, 0, 0, 2, ' ', 0)
	for _, iface := range ifaces {
		if len(iface.GetIpAddresses()) == 0 {
			continue
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", iface.GetName(), strings.Join(iface.GetIpAddresses(), ", "), uplinkNotes(iface))
	}
	tw.Flush()

	var b strings.Builder
	b.WriteString("Network:\n")
	// Rows without notes would otherwise end in column padding.
	for _, line := range strings.SplitAfter(rows.String(), "\n") {
		if line != "" {
			b.WriteString(strings.TrimRight(line, " \n") + "\n")
		}
	}
	return b.String()
}

// uplinkNotes describes an interface's role in reaching the internet, e.g.
// "default route, metric 100, internet: full". Empty for agents that report
// none of it.
func uplinkNotes(iface *agentpb.NetworkInterface) string {
	var notes []string
	if iface.GetIsDefaultRoute() {
		notes = append(notes, "default route")
	}
	if iface.RouteMetric != nil {
		notes = append(notes, fmt.Sprintf("metric %d", iface.GetRouteMetric()))
	}
	if c := connectivityLabel(iface.GetConnectivity()); c != "" {
		notes = append(notes, "internet: "+c)
	}
	return strings.Join(notes, ", ")
}

// connectivityLabel is the lowercase connectivity state ("full", "limited",
// "portal", "none"), or "" when unknown.
func connectivityLabel(c agentpb.NetworkConnectivity) string {
	if c == agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_UNKNOWN {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(c.String(), "NETWORK_CONNECTIVITY_"))
}

// bestReachableIP picks the IP address most likely to be reachable from a
// developer's machine (e.g. to open an app in a browser). IPv4 addresses are
// preferred over IPv6, and interfaces are considered in the order the agent
// reported them. Returns "" when no address is available.
func bestReachableIP(ifaces []*agentpb.NetworkInterface) string {
	var firstAny string
	for _, iface := range ifaces {
		for _, addr := range iface.GetIpAddresses() {
			ip := net.ParseIP(addr)
			if ip == nil {
				continue
			}
			if ip.To4() != nil {
				return addr
			}
			if firstAny == "" {
				firstAny = addr
			}
		}
	}
	return firstAny
}

// isIPv6Literal reports whether host parses as a bare (unbracketed) IPv6
// literal, zone ID included. Hostnames and IPv4 are false.
func isIPv6Literal(host string) bool {
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Is6()
}

// urlSafeHost returns host formatted for the authority part of a URL: IPv6
// literals are bracketed, with any zone ID percent-escaped per RFC 6874. IPv4
// and hostnames pass through unchanged. IPv4-mapped IPv6 literals are unmapped
// to plain IPv4 so the result remains URL-safe.
func urlSafeHost(host string) string {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if addr.Is4In6() {
		return addr.Unmap().String()
	}
	if !addr.Is6() {
		return host
	}
	return "[" + strings.ReplaceAll(host, "%", "%25") + "]"
}

// reachableAppURL builds a browser-openable URL for a freshly started app,
// using a routable device IP instead of the (often unresolvable) .local
// hostname that `wendy run` otherwise reports.
//
// When the app configures a postStart openURL that references the device
// hostname, that template is reused verbatim (scheme, port, and path) with the
// IP swapped in, so the printed URL matches what the browser hook opens.
// Otherwise, an HTTP entitlement's presentation port is preferred over the
// readiness TCP port. This keeps the URL users see/open independent from the
// port the CLI probes (for example, probe an admin port on 9000 but present
// the public UI on 8080). Returns "" when no reachable URL can be derived.
func reachableAppURL(hookURL, appID, serviceName, deviceIP string, httpPort int, readiness *appconfig.ReadinessConfig) string {
	if deviceIP == "" {
		return ""
	}
	if hookURL != "" && strings.Contains(hookURL, "WENDY_HOSTNAME") {
		return expandHookEnv(hookURL, urlSafeHost(deviceIP), appID, serviceName)
	}
	if httpPort != 0 {
		return "http://" + net.JoinHostPort(deviceIP, strconv.Itoa(httpPort))
	}
	if readiness != nil && readiness.TCPSocket != nil && readiness.TCPSocket.Port != 0 {
		return "http://" + net.JoinHostPort(deviceIP, strconv.Itoa(readiness.TCPSocket.Port))
	}
	return ""
}

// httpEntitlementPort returns the port declared by an `http` entitlement, if
// the app has one.
func httpEntitlementPort(entitlements []appconfig.Entitlement) (int, bool) {
	for _, e := range entitlements {
		if e.Type == appconfig.EntitlementHTTP && e.Port > 0 {
			return e.Port, true
		}
	}
	return 0, false
}

// effectiveReadiness returns appCfg.Readiness unchanged when it already
// declares a TCP probe. Otherwise, when the app declares an `http`
// entitlement, it synthesizes a readiness probe on that port (preserving any
// explicit timeoutSeconds) so `wendy run` waits for the declared HTTP port to
// come up before announcing or opening it — an `http` entitlement gets this
// gating without a separate readiness.tcpSocket config.
func effectiveReadiness(appCfg *appconfig.AppConfig) *appconfig.ReadinessConfig {
	if appCfg.Readiness != nil && appCfg.Readiness.TCPSocket != nil {
		return appCfg.Readiness
	}
	port, ok := httpEntitlementPort(appCfg.Entitlements)
	if !ok {
		return appCfg.Readiness
	}
	timeout := 0
	if appCfg.Readiness != nil {
		timeout = appCfg.Readiness.TimeoutSeconds
	}
	return &appconfig.ReadinessConfig{
		TCPSocket:      &appconfig.TCPSocketProbe{Port: port},
		TimeoutSeconds: timeout,
	}
}
