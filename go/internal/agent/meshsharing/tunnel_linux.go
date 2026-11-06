//go:build linux

package meshsharing

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

const tunnelPrefix = "wlmi"
const tunnelOwner = "wendy:mesh-internet"
const tunnelProtocol = 187

// internetTunnel carries both IP families in IPv6. Its outer addresses are
// ordinary device ULAs, so Babel chooses and repairs every transit hop without
// learning Internet defaults or borrower lease addresses.
type internetTunnel struct {
	link *netlink.Ip6tnl
}

func newInternetTunnel(name string, local, remote meshid.Device) (*internetTunnel, error) {
	if !local.IsValid() || !remote.IsValid() || local == remote || local.Tenant() != remote.Tenant() {
		return nil, errors.New("invalid Internet tunnel peer")
	}
	link := &netlink.Ip6tnl{LinkAttrs: netlink.LinkAttrs{Name: name, Alias: tunnelOwner, MTU: 1280},
		Local: net.IP(local.MeshAddress().AsSlice()), Remote: net.IP(remote.MeshAddress().AsSlice()),
		Ttl: 64, Proto: 0, Flags: uint32(netlink.IP6_TNL_F_IGN_ENCAP_LIMIT)}
	if err := netlink.LinkAdd(link); err != nil {
		return nil, fmt.Errorf("create IPv6 Internet tunnel (kernel CONFIG_IPV6_TUNNEL required): %w", err)
	}
	// ip6tnl creation can discard IFLA_IFALIAS from the initial request.
	// Set the ownership marker on the created interface explicitly.
	if err := netlink.LinkSetAlias(link, tunnelOwner); err != nil {
		return nil, errors.Join(err, netlink.LinkDel(link))
	}
	t := &internetTunnel{link: link}
	if err := netlink.LinkSetUp(link); err != nil {
		return nil, errors.Join(err, t.close())
	}
	return t, nil
}

func tunnelIPNet(address netip.Addr) *net.IPNet {
	bits := address.BitLen()
	return &net.IPNet{IP: net.IP(address.AsSlice()), Mask: net.CIDRMask(bits, bits)}
}

func (t *internetTunnel) assign(address netip.Addr) error {
	return netlink.AddrAdd(t.link, &netlink.Addr{IPNet: tunnelIPNet(address), Flags: unix.IFA_F_NOPREFIXROUTE | unix.IFA_F_NODAD})
}

func (t *internetTunnel) route(address netip.Addr, defaultRoute bool) error {
	family := netlink.FAMILY_V4
	if address.Is6() {
		family = netlink.FAMILY_V6
	}
	route := &netlink.Route{LinkIndex: t.link.Index, Table: unix.RT_TABLE_MAIN, Protocol: tunnelProtocol, Family: family, Dst: tunnelIPNet(address)}
	if defaultRoute {
		route.Dst = nil
		route.Src = net.IP(address.AsSlice())
		route.Priority = 5
	}
	// Never replace a foreign route, even when its metric happens to match.
	return netlink.RouteAdd(route)
}

func (t *internetTunnel) close() error {
	live, err := netlink.LinkByIndex(t.link.Index)
	if err != nil {
		var missing netlink.LinkNotFoundError
		if errors.As(err, &missing) {
			return nil
		}
		return err
	}
	if live.Attrs().Alias != tunnelOwner || live.Attrs().Name != t.link.Name || live.Type() != "ip6tnl" {
		return fmt.Errorf("Internet tunnel ownership changed: name=%q type=%q alias=%q", live.Attrs().Name, live.Type(), live.Attrs().Alias)
	}
	return netlink.LinkDel(live)
}

func reclaimInternetTunnels() error {
	links, err := netlink.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.Attrs().Alias != tunnelOwner {
			continue
		}
		if link.Type() != "ip6tnl" {
			return errors.New("Internet tunnel alias belongs to a different link type")
		}
		if err := netlink.LinkDel(link); err != nil {
			return err
		}
	}
	return nil
}

// Private lease pools must not overlap an existing LAN or another overlay.
// This check happens before conntrack reclamation and before granting a lease.
func checkLeasePoolsAvailable() error {
	for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
		pool := netip.MustParsePrefix(leaseIPv4Pool)
		if family == netlink.FAMILY_V6 {
			pool = netip.MustParsePrefix(leaseIPv6Pool)
		}
		routes, err := netlink.RouteList(nil, family)
		if err != nil {
			return err
		}
		for _, route := range routes {
			if route.Dst == nil {
				continue
			}
			ones, _ := route.Dst.Mask.Size()
			if ones == 0 {
				continue
			}
			address, ok := netip.AddrFromSlice(route.Dst.IP)
			if !ok {
				continue
			}
			prefix := netip.PrefixFrom(address.Unmap(), ones)
			if prefix.Overlaps(pool) {
				return fmt.Errorf("Internet lease pool %s conflicts with existing route %s", pool, prefix)
			}
		}
		addresses, err := netlink.AddrList(nil, family)
		if err != nil {
			return err
		}
		for _, address := range addresses {
			ip, ok := netip.AddrFromSlice(address.IP)
			if ok && pool.Contains(ip.Unmap()) {
				return fmt.Errorf("Internet lease pool %s conflicts with existing address %s", pool, ip)
			}
		}
	}
	return nil
}
