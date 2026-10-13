package localmesh

import (
	"net/netip"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
)

// routeAuthorities converts only live, verified directory manifests and
// catalog gateway offers into Babel admission policy. Callers obtain manifests
// from Directory.Snapshot and offer assets from a verified catalog, never from
// an adjacent link's unsupported route claims.
func routeAuthorities(org int32, manifests []Manifest, offerAssets []int32, now time.Time) (map[babel.RouterID]bool, map[babel.RouterID]bool) {
	availableOffers := make(map[int32]bool, len(offerAssets))
	for _, asset := range offerAssets {
		availableOffers[asset] = true
	}
	hosts := make(map[babel.RouterID]bool, len(manifests))
	gateways := make(map[babel.RouterID]bool)
	for _, manifest := range manifests {
		if manifest.Org != org || manifest.Withdraw || !time.UnixMilli(manifest.Expires).After(now) {
			continue
		}
		id, err := RouterID(org, manifest.Asset)
		if err != nil {
			continue
		}
		hosts[id] = true
		if manifest.Internet && availableOffers[manifest.Asset] {
			gateways[id] = true
		}
	}
	return hosts, gateways
}

func routeAuthorized(org int32, hosts, gateways map[babel.RouterID]bool, origin babel.RouterID, prefix netip.Prefix) bool {
	if !prefix.Addr().Is4() {
		return false
	}
	if prefix == netip.MustParsePrefix("0.0.0.0/0") {
		return gateways[origin]
	}
	return hosts[origin] && OwnedPrefix(org, origin, prefix)
}
