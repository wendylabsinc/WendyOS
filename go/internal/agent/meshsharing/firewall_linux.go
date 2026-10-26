//go:build linux

package meshsharing

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

const internetMarker = "wendy-mesh-internet"

type internetRule struct {
	family, table, chain string
	args                 []string
}

func addInternetRule(ctx context.Context, rules *[]internetRule, family, table, chain string, args ...string) error {
	args = append(args, "-m", "comment", "--comment", internetMarker)
	_, err := command(ctx, family, append([]string{"-w", "3", "-t", table, "-I", chain, "1"}, args...)...)
	if err == nil {
		*rules = append(*rules, internetRule{family, table, chain, args})
	}
	return err
}

func removeInternetRules(ctx context.Context, rules *[]internetRule) error {
	var errs []error
	var remaining []internetRule
	for i := len(*rules) - 1; i >= 0; i-- {
		r := (*rules)[i]
		_, err := command(ctx, r.family, append([]string{"-w", "3", "-t", r.table, "-D", r.chain}, r.args...)...)
		if err != nil {
			errs = append(errs, err)
			remaining = append(remaining, r)
		}
	}
	slices.Reverse(remaining)
	*rules = remaining
	return errors.Join(errs...)
}

func reclaimInternetRules(ctx context.Context) error {
	for _, family := range []string{"iptables", "ip6tables"} {
		for _, target := range [][2]string{{"filter", "INPUT"}, {"filter", "FORWARD"}, {"nat", "POSTROUTING"}, {"mangle", "FORWARD"}} {
			out, err := command(ctx, family, "-w", "3", "-t", target[0], "-S", target[1])
			if err != nil {
				return err
			}
			for _, line := range strings.Split(out, "\n") {
				fields := strings.Fields(line)
				if len(fields) < 3 || fields[0] != "-A" {
					continue
				}
				owned := false
				for i, field := range fields {
					if i > 0 && fields[i-1] == "--comment" && strings.Trim(field, "\"") == internetMarker {
						owned = true
					}
				}
				if !owned {
					continue
				}
				for i := range fields {
					fields[i] = strings.Trim(fields[i], "\"")
				}
				if _, err := command(ctx, family, append([]string{"-w", "3", "-t", target[0], "-D"}, fields[1:]...)...); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func flushTunnelConntrack(prefix netip.Prefix) error {
	var filters []netlink.CustomConntrackFilter
	for _, kind := range []netlink.ConntrackFilterType{netlink.ConntrackOrigSrcIP, netlink.ConntrackOrigDstIP, netlink.ConntrackReplySrcIP, netlink.ConntrackReplyDstIP} {
		filter := &netlink.ConntrackFilter{}
		if err := filter.AddIPNet(kind, &net.IPNet{IP: net.IP(prefix.Addr().AsSlice()), Mask: net.CIDRMask(prefix.Bits(), prefix.Addr().BitLen())}); err != nil {
			return err
		}
		filters = append(filters, filter)
	}
	family := netlink.InetFamily(unix.AF_INET)
	if prefix.Addr().Is6() {
		family = unix.AF_INET6
	}
	_, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, family, filters...)
	return err
}

func flushLease(lease tunnelLease) error {
	return errors.Join(flushTunnelConntrack(netip.PrefixFrom(lease.address4(), 32)), flushTunnelConntrack(netip.PrefixFrom(lease.address6(), 128)))
}

var forbiddenTunnel4 = []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "240.0.0.0/4", leaseIPv4Pool, "10.99.0.0/16"}
var forbiddenTunnel6 = []string{"::/128", "::1/128", "fe80::/10", "ff00::/8", "fc00::/7"}

func grantTunnelEgress(ctx context.Context, rules *[]internetRule, tunnel, uplink string, lease tunnelLease) error {
	for _, family := range []struct {
		tool, source string
		forbidden    []string
	}{{"iptables", lease.address4().String(), forbiddenTunnel4}, {"ip6tables", lease.address6().String(), forbiddenTunnel6}} {
		if family.tool == "ip6tables" && !lease.IPv6 {
			continue
		}
		add := func(table, chain string, args ...string) error {
			return addInternetRule(ctx, rules, family.tool, table, chain, args...)
		}
		for _, direction := range []string{"-i", "-o"} {
			if err := add("mangle", "FORWARD", direction, tunnel, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"); err != nil {
				return err
			}
		}
		if err := add("nat", "POSTROUTING", "-s", family.source, "-o", uplink, "-j", "MASQUERADE"); err != nil {
			return err
		}
		if err := add("filter", "FORWARD", "-i", tunnel, "-o", uplink, "-s", family.source, "-j", "ACCEPT"); err != nil {
			return err
		}
		for _, prefix := range family.forbidden {
			if err := add("filter", "FORWARD", "-i", tunnel, "-o", uplink, "-d", prefix, "-j", "REJECT"); err != nil {
				return err
			}
		}
		if err := add("filter", "FORWARD", "-i", uplink, "-o", tunnel, "-d", family.source, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
			return err
		}
	}
	return nil
}
