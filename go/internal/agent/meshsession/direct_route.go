package meshsession

import (
	"net/netip"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// ResolvePromotion is the route predicate for the automatic promotion owner.
// bleAvailable and bleCost describe the retained BLE lane. Live authenticated
// snapshot links also count as physical neighbors after that lane is retired.
// A cheaper installed direct carrier triggers promotion even while
// BLE still works. Multihop is eligible only while the direct attachment is lost.
//
// This proves local forwarding eligibility, not end-to-end reachability. Babel
// feasibility excludes reflected candidates; a signed origin alone does not.
// The upper peer handshake must still succeed before the actor fences BLE, and
// callers must re-evaluate this predicate after setup before requesting cutover.
func ResolvePromotion(snapshot localmesh.NodeSnapshot, peer meshid.Device, bleAvailable bool, bleCost uint16, selected uint32, now time.Time) (netip.AddrPort, error) {
	if !peer.IsValid() || !snapshot.Self.IsValid() || peer == snapshot.Self || peer.Tenant() != snapshot.Self.Tenant() || bleCost == 0 || bleCost == babel.Infinity {
		return netip.AddrPort{}, ErrNoRoute
	}
	port := uint16(0)
	switch directstream.SelectedTransport(selected) {
	case directstream.CapabilityVersion | directstream.CapabilityTLS:
		port = TLSPort
	case directstream.CapabilityVersion | directstream.CapabilityQUIC:
		port = Port
	default:
		return netip.AddrPort{}, ErrNoRoute
	}
	physicalDirect := bleAvailable
	for _, link := range snapshot.Links {
		// ForwardingReady is deliberately not required: a live direct carrier may
		// lose route selection to a cheaper relay without losing the neighbor.
		if link.ID != 0 && link.Device == peer && !link.Retiring {
			physicalDirect = true
			break
		}
	}
	prefix := netip.PrefixFrom(peer.MeshAddress(), 128)
	for _, route := range snapshot.ForwardingRoutes {
		if route.Prefix != prefix || route.Local || route.Unreachable || route.Metric == babel.Infinity || route.Link == 0 || route.RouterID == 0 || !route.NextHop.IsValid() || route.NextHop == snapshot.Self.MeshAddress() {
			continue
		}
		current := false
		for _, candidate := range snapshot.Routes {
			if candidate == route {
				current = true
				break
			}
		}
		if !current {
			continue
		}
		origin := localmesh.RouteOrigin{Prefix: route.Prefix, RouterID: route.RouterID, Seqno: route.Seqno}
		proof, exists := snapshot.RouteProofs[origin]
		if !exists || !snapshot.RouteAuthorizations[origin] || proof.Principal != peer.Principal() || !now.Before(proof.Deadline) {
			continue
		}
		for _, link := range snapshot.Links {
			if link.ID != route.Link || link.Cost == 0 || link.Cost == babel.Infinity || !link.ForwardingReady || link.Retiring || !link.Device.IsValid() || link.Device == snapshot.Self || link.Device.Tenant() != peer.Tenant() {
				continue
			}
			if link.Device == peer {
				// A restored BLE direct carrier should recover the retained session. It
				// must not accidentally promote merely because its /128 became usable.
				switch link.Transport {
				case "ethernet", "infrastructure-wifi", "tcp", "nan", "quic", "tls-stream":
				default:
					continue
				}
				if bleAvailable && link.Cost >= bleCost {
					continue
				}
			} else if physicalDirect {
				continue
			}
			return netip.AddrPortFrom(peer.MeshAddress(), port), nil
		}
	}
	return netip.AddrPort{}, ErrNoRoute
}
