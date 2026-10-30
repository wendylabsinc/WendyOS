//go:build linux

package cloudrelay

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const interfaceFallbackSupported = true

func availableUplinks(ctx context.Context) ([]uplink, error) {
	routes, err := netlink.RouteList(nil, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("listing Cloud uplinks: %w", err)
	}
	var names []string
	for _, route := range routes {
		if route.Type != unix.RTN_UNICAST {
			continue
		}
		if route.Dst != nil {
			ones, _ := route.Dst.Mask.Size()
			if ones != 0 {
				continue
			}
		}
		iface, err := net.InterfaceByIndex(route.LinkIndex)
		if err != nil || iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || slices.Contains(names, iface.Name) {
			continue
		}
		names = append(names, iface.Name)
	}
	slices.Sort(names)
	paths := make([]uplink, 0, len(names))
	for _, name := range names {
		paths = append(paths, uplink{name: name, dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialUplink(ctx, name, network, address)
		}})
	}
	return paths, ctx.Err()
}

func bindUplink(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		if err := raw.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		}); err != nil {
			return err
		}
		return bindErr
	}
}

func dialUplink(ctx context.Context, name, network, address string) (net.Conn, error) {
	d := &net.Dialer{Control: bindUplink(name)}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if net.ParseIP(strings.Split(host, "%")[0]) == nil {
		d.Resolver = uplinkResolver(name, uplinkDNS(ctx, name))
	}
	return d.DialContext(ctx, network, address)
}

func uplinkResolver(name string, servers []string) *net.Resolver {
	var next atomic.Uint64
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		if len(servers) == 0 {
			return nil, fmt.Errorf("no DNS servers for uplink %s", name)
		}
		server := servers[(next.Add(1)-1)%uint64(len(servers))]
		d := net.Dialer{Control: bindUplink(name)}
		return d.DialContext(ctx, network, net.JoinHostPort(server, "53"))
	}}
}

// The system stub may forward DNS through the broken primary. Ask NetworkManager
// for this interface's DNS and bind DNS sockets to it as well as the TCP socket.
func uplinkDNS(ctx context.Context, name string) []string {
	query, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	out, err := exec.CommandContext(query, "nmcli", "--escape", "no", "-g", "IP4.DNS,IP6.DNS", "device", "show", name).Output()
	if err == nil {
		if servers := parseUplinkDNS(string(out), name); len(servers) > 0 {
			return servers
		}
	}
	// Wendy Lite hosts may not use NetworkManager. Use configured upstreams,
	// excluding loopback stubs that cannot be reached through a bound interface.
	for _, path := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var servers []string
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[0] == "nameserver" {
				servers = append(servers, parseUplinkDNS(fields[1], name)...)
			}
		}
		if len(servers) > 0 {
			return servers
		}
	}
	return nil
}

func parseUplinkDNS(value, name string) []string {
	var servers []string
	for _, server := range strings.Fields(value) {
		ip := net.ParseIP(strings.Split(server, "%")[0])
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
			continue
		}
		if ip.To4() == nil && ip.IsLinkLocalUnicast() {
			server = ip.String() + "%" + name
		}
		if !slices.Contains(servers, server) {
			servers = append(servers, server)
		}
	}
	return servers
}
