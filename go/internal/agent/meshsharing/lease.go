package meshsharing

import (
	"errors"
	"net/netip"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Leases belong to a gateway control connection, not to a truncated identity.
// Neither pool is advertised in Babel. The second ULA is private tunnel
// plumbing, deliberately outside the device identity /48. Separate gateways
// may reuse these addresses because a borrower uses only one gateway at once.
const leaseIPv4Pool = "10.88.0.0/16"
const leaseIPv6Pool = "fd2b:677a:9d11::/112"

type tunnelLease struct {
	Slot       uint16 `json:"slot"`
	IPv6       bool   `json:"ipv6"`
	generation uint64
}

func (l tunnelLease) address4() netip.Addr {
	return netip.AddrFrom4([4]byte{10, 88, byte(l.Slot >> 8), byte(l.Slot)})
}

func (l tunnelLease) address6() netip.Addr {
	b := netip.MustParsePrefix(leaseIPv6Pool).Addr().As16()
	b[14], b[15] = byte(l.Slot>>8), byte(l.Slot)
	return netip.AddrFrom16(b)
}

func (l tunnelLease) valid() bool { return l.Slot != 0 && l.Slot != 65535 }

// The caller serializes allocation and releases only after its tunnel and
// conntrack state have been removed. A failed cleanup keeps the slot reserved.
type leasePool struct {
	used       map[uint16]leaseAllocation
	next       uint16
	generation uint64
}

type leaseAllocation struct {
	device     meshid.Device
	generation uint64
}

func (p *leasePool) allocate(device meshid.Device) (tunnelLease, error) {
	if p.used == nil {
		p.used = make(map[uint16]leaseAllocation)
	}
	for _, owner := range p.used {
		if owner.device == device {
			return tunnelLease{}, errors.New("device already has a gateway lease")
		}
	}
	for range 65534 {
		p.next++
		if p.next == 0 || p.next == 65535 {
			p.next = 1
		}
		if _, occupied := p.used[p.next]; !occupied {
			p.generation++
			p.used[p.next] = leaseAllocation{device, p.generation}
			return tunnelLease{Slot: p.next, generation: p.generation}, nil
		}
	}
	return tunnelLease{}, errors.New("gateway tunnel address pool exhausted")
}

func (p *leasePool) release(device meshid.Device, lease tunnelLease) {
	if p.used[lease.Slot] == (leaseAllocation{device, lease.generation}) {
		delete(p.used, lease.Slot)
	}
}
