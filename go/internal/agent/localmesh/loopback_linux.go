//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"golang.org/x/sys/unix"
)

const loopbackInterfaceName = "lo"

// IFA_PROTO is Linux's per-address owner tag (include/uapi/linux/if_addr.h).
// netlink v1.3.1 does not expose it, so only these address operations use the
// raw attribute. Keep this separate from lo's alias, MTU, flags and sysctls:
// the interface belongs to the host, not to Wendy.
const ifaProto = 11
const meshAddressProtocol = 186

type loopbackAddress struct {
	prefix      netip.Prefix
	protocol    uint8
	scope       uint8
	hasProtocol bool
}

func (a loopbackAddress) owned() bool {
	return a.protocol == meshAddressProtocol && a.scope == unix.RT_SCOPE_UNIVERSE &&
		a.prefix.Bits() == 128 && meshid.InMesh(a.prefix.Addr())
}

func meshLoopback() (netlink.Link, error) {
	link, err := netlink.LinkByName(loopbackInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("find mesh loopback interface: %w", err)
	}
	if link.Attrs().Flags&net.FlagLoopback == 0 || link.Attrs().Flags&net.FlagUp == 0 {
		return nil, errors.New("mesh needs the host loopback interface to be up")
	}
	return link, nil
}

func readLoopbackAddresses(link netlink.Link) ([]loopbackAddress, error) {
	req := nl.NewNetlinkRequest(unix.RTM_GETADDR, unix.NLM_F_DUMP)
	req.AddData(nl.NewIfAddrmsg(unix.AF_INET6))
	messages, err := req.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWADDR)
	if err != nil {
		return nil, err
	}
	return parseLoopbackAddresses(messages, link.Attrs().Index)
}

func parseLoopbackAddresses(messages [][]byte, index int) ([]loopbackAddress, error) {
	var addresses []loopbackAddress
	for _, data := range messages {
		if len(data) < unix.SizeofIfAddrmsg {
			return nil, errors.New("short loopback address message")
		}
		msg := nl.DeserializeIfAddrmsg(data)
		if msg.Family != unix.AF_INET6 || int(msg.Index) != index {
			continue
		}
		attrs, err := nl.ParseRouteAttr(data[msg.Len():])
		if err != nil {
			return nil, err
		}
		var address netip.Addr
		var protocol uint8
		var hasProtocol bool
		for _, attr := range attrs {
			switch attr.Attr.Type {
			case unix.IFA_ADDRESS:
				if len(attr.Value) != 16 {
					return nil, errors.New("invalid IPv6 loopback address")
				}
				address = netip.AddrFrom16([16]byte(attr.Value))
			case ifaProto:
				if len(attr.Value) != 1 {
					return nil, errors.New("invalid loopback address protocol")
				}
				protocol = attr.Value[0]
				hasProtocol = true
			}
		}
		prefix := netip.PrefixFrom(address, int(msg.Prefixlen))
		if !prefix.IsValid() {
			return nil, errors.New("invalid loopback address prefix")
		}
		addresses = append(addresses, loopbackAddress{prefix: prefix, protocol: protocol, scope: msg.Scope, hasProtocol: hasProtocol})
	}
	return addresses, nil
}

// A supported kernel includes IFA_PROTO in dumps even when its value is zero.
// Detect it on an existing address before adding ours: older kernels can ignore
// an unknown attribute, leaving an address we cannot safely reclaim.
func requireLoopbackAddressProtocol(addresses []loopbackAddress) error {
	for _, address := range addresses {
		if address.hasProtocol {
			return nil
		}
	}
	return errors.New("mesh needs IPv6 address protocol tags (Linux IFA_PROTO) and an existing IPv6 address on lo")
}

func inspectLoopbackAddress(link netlink.Link, address netip.Addr) (bool, error) {
	addresses, err := readLoopbackAddresses(link)
	if err != nil {
		return false, err
	}
	for _, a := range addresses {
		if a.prefix.Addr() == address {
			if !a.owned() {
				return false, fmt.Errorf("loopback address %s is not owned by wendy net", a.prefix)
			}
			return true, nil
		}
	}
	return false, nil
}

func addLoopbackAddress(link netlink.Link, address netip.Addr) error {
	// EXCL prevents adoption of an address another owner installed after the
	// caller inspected lo. Callers re-read EEXIST rather than assuming success.
	req := nl.NewNetlinkRequest(unix.RTM_NEWADDR, unix.NLM_F_CREATE|unix.NLM_F_EXCL|unix.NLM_F_ACK)
	msg := nl.NewIfAddrmsg(unix.AF_INET6)
	msg.Index = uint32(link.Attrs().Index)
	msg.Prefixlen = 128
	req.AddData(msg)
	req.AddData(nl.NewRtAttr(unix.IFA_ADDRESS, address.AsSlice()))
	req.AddData(nl.NewRtAttr(ifaProto, []byte{meshAddressProtocol}))
	flags := make([]byte, 4)
	nl.NativeEndian().PutUint32(flags, unix.IFA_F_NOPREFIXROUTE|unix.IFA_F_NODAD)
	req.AddData(nl.NewRtAttr(unix.IFA_FLAGS, flags))
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	return err
}

func removeLoopbackAddress(link netlink.Link, address loopbackAddress) error {
	if !address.owned() {
		return errors.New("refusing to remove an unowned loopback address")
	}
	// Re-read before deletion: an administrator may have replaced the address
	// since the initial cleanup scan. Never remove a matching untagged address.
	present, err := inspectLoopbackAddress(link, address.prefix.Addr())
	if err != nil || !present {
		return err
	}
	err = netlink.AddrDel(link, &netlink.Addr{IPNet: ipNet(address.prefix)})
	if errors.Is(err, unix.EADDRNOTAVAIL) {
		return nil
	}
	return err
}
