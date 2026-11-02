package meshcatalog

import (
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net/netip"
	"time"
)

func eligibleSnapshot(view localmesh.NodeSnapshot, device meshid.Device, now time.Time) bool {
	manifestOK := false
	for _, m := range view.Devices {
		if m.Device == device.UUID() && !m.Withdraw && time.UnixMilli(m.Expires).After(now) {
			manifestOK = true
			break
		}
	}
	if !manifestOK {
		return false
	}
	prefix := netip.PrefixFrom(device.MeshAddress(), 128)
	for _, route := range view.Routes {
		if route.Prefix == prefix && !route.Local && !route.Unreachable {
			return true
		}
	}
	return false
}
