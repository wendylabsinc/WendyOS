package services

import (
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// uplinkProbeTimeout bounds the NetworkManager queries behind device info, so
// a stuck NetworkManager delays the reply by at most this much.
const uplinkProbeTimeout = 2 * time.Second

// uplinkProbeTargets are public addresses the kernel is asked to route to,
// IPv4 first. preferredSource connects a UDP socket, which sends nothing.
var uplinkProbeTargets = []*net.TCPAddr{
	{IP: net.ParseIP("8.8.8.8"), Port: 53},
	{IP: net.ParseIP("2001:4860:4860::8888"), Port: 53},
}

// addUplinkInfo marks which interface carries internet traffic, with each
// interface's default-route metric and NetworkManager connectivity. Every
// source is best effort: what cannot be read is left unset.
func addUplinkInfo(ctx context.Context, ifaces []*agentpb.NetworkInterface) {
	if len(ifaces) == 0 {
		return
	}
	var sources []net.IP
	for _, target := range uplinkProbeTargets {
		if src, err := preferredSource(target); err == nil {
			sources = append(sources, src)
		}
	}
	ipv4, _ := os.ReadFile("/proc/net/route")
	ipv6, _ := os.ReadFile("/proc/net/ipv6_route")
	ctx, cancel := context.WithTimeout(ctx, uplinkProbeTimeout)
	defer cancel()
	annotateUplinks(ifaces, sources, defaultRouteMetrics(string(ipv4), string(ipv6)), nmConnectivity(ctx))
}

// annotateUplinks marks the first interface holding one of sources, the
// addresses the kernel would send from toward the internet in preference
// order, and copies in each interface's metric and connectivity.
func annotateUplinks(ifaces []*agentpb.NetworkInterface, sources []net.IP, metrics map[string]uint32, connectivity map[string]agentpb.NetworkConnectivity) {
	marked := false
	for _, src := range sources {
		for _, iface := range ifaces {
			if !marked && ifaceHasIP(iface, src) {
				iface.IsDefaultRoute = true
				marked = true
			}
		}
	}
	for _, iface := range ifaces {
		if m, ok := metrics[iface.GetName()]; ok {
			iface.RouteMetric = &m
		}
		iface.Connectivity = connectivity[iface.GetName()]
	}
}

func ifaceHasIP(iface *agentpb.NetworkInterface, ip net.IP) bool {
	for _, a := range iface.GetIpAddresses() {
		if net.ParseIP(a).Equal(ip) {
			return true
		}
	}
	return false
}

// defaultRouteMetrics returns the lowest default-route metric per interface
// from the contents of /proc/net/route and /proc/net/ipv6_route.
func defaultRouteMetrics(ipv4, ipv6 string) map[string]uint32 {
	metrics := map[string]uint32{}
	keep := func(iface, raw string, base int) {
		v, err := strconv.ParseUint(raw, base, 32)
		if err != nil {
			return
		}
		if old, ok := metrics[iface]; !ok || uint32(v) < old {
			metrics[iface] = uint32(v)
		}
	}
	for _, line := range strings.Split(ipv4, "\n") {
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		f := strings.Fields(line)
		if len(f) >= 8 && f[1] == "00000000" && f[7] == "00000000" {
			keep(f[0], f[6], 10)
		}
	}
	for _, line := range strings.Split(ipv6, "\n") {
		// Destination PrefixLen Source SourcePrefixLen NextHop Metric RefCnt Use Flags Iface
		// The kernel's unreachable placeholder default sits on lo.
		f := strings.Fields(line)
		if len(f) >= 10 && f[0] == strings.Repeat("0", 32) && f[1] == "00" && f[9] != "lo" {
			keep(f[9], f[5], 16)
		}
	}
	return metrics
}

// nmConnectivity returns NetworkManager's per-device connectivity, or nil
// when NetworkManager runs no check. Without a check (no URI, or NM built
// without concheck, as on current images) NM reports a guess, "full" for any
// device holding a default route, so its answer is only used when the check
// is enabled. nmcli does not expose that flag; NM's D-Bus property does.
func nmConnectivity(ctx context.Context) map[string]agentpb.NetworkConnectivity {
	conn, err := dbus.ConnectSystemBus(dbus.WithContext(ctx))
	if err != nil {
		return nil
	}
	defer conn.Close()
	var enabled dbus.Variant
	err = conn.Object("org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager").
		CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
			"org.freedesktop.NetworkManager", "ConnectivityCheckEnabled").Store(&enabled)
	if on, _ := enabled.Value().(bool); err != nil || !on {
		return nil
	}
	cmd := exec.CommandContext(ctx, "nmcli", "-t", "-f", "DEVICE,IP4-CONNECTIVITY,IP6-CONNECTIVITY", "device", "status")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return parseNMConnectivity(string(out))
}

var nmConnectivityStates = map[string]agentpb.NetworkConnectivity{
	"none":    agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_NONE,
	"portal":  agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_PORTAL,
	"limited": agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_LIMITED,
	"full":    agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_FULL,
}

// parseNMConnectivity parses `nmcli -t -f DEVICE,IP4-CONNECTIVITY,
// IP6-CONNECTIVITY device status`, keeping the better of the two families
// per device. The enum orders states as NetworkManager does, worst first.
func parseNMConnectivity(out string) map[string]agentpb.NetworkConnectivity {
	states := map[string]agentpb.NetworkConnectivity{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, ":")
		if len(f) != 3 || f[0] == "" {
			continue
		}
		states[f[0]] = max(nmConnectivityStates[f[1]], nmConnectivityStates[f[2]])
	}
	return states
}
