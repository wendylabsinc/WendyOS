package meshcatalog

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net/netip"
	"time"
)

// manifestRouteDeadline uses the same exact verified route proof as Node/FIB
// admission. A retained old tuple may carry unchanged current service metadata,
// but its original deadline must also bound discovery and DNS TTLs.
func manifestRouteDeadline(view localmesh.NodeSnapshot, m localmesh.Manifest, device meshid.Device, now time.Time) time.Time {
	until := m.LeaseDeadline()
	if m.Withdraw || !until.After(now) {
		return time.Time{}
	}
	prefix := netip.PrefixFrom(device.MeshAddress(), 128)
	for _, route := range view.Routes {
		if route.Prefix != prefix || route.Local || route.Unreachable || route.Metric == 65535 {
			continue
		}
		if view.RouteProofs != nil {
			origin := localmesh.RouteOrigin{Prefix: route.Prefix, RouterID: route.RouterID, Seqno: route.Seqno}
			proof, ok := view.RouteProofs[origin]
			if !ok || proof.Principal != device.Principal() || !view.RouteAuthorizations[origin] || !proof.Deadline.After(now) {
				continue
			}
			if proof.Deadline.Before(until) {
				until = proof.Deadline
			}
		}
		return until
	}
	return time.Time{}
}

func eligibleSnapshot(view localmesh.NodeSnapshot, device meshid.Device, now time.Time) bool {
	for _, m := range view.Devices {
		if m.Device == device.UUID() && !manifestRouteDeadline(view, m, device, now).IsZero() {
			return true
		}
	}
	return false
}
