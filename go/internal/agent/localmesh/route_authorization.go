package localmesh

import (
	"errors"
	"net/netip"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// RouteOrigin is the exact Babel update a device authorizes in its signed
// manifest. The router ID and sequence number are origin fields; a relay's next
// hop and metric remain path-dependent and are not claims by the destination.
type RouteOrigin struct {
	Prefix   netip.Prefix   `json:"prefix" cbor:"p"`
	RouterID babel.RouterID `json:"routerId" cbor:"r"`
	Seqno    uint16         `json:"seqno" cbor:"s"`
}

func (m Manifest) validateRoute(device meshid.Device) error {
	if m.Route == nil {
		return nil
	}
	if m.Route.Prefix != netip.PrefixFrom(device.MeshAddress(), 128) {
		return errors.New("route authorization must name the signer's exact net /128")
	}
	if m.Route.RouterID == 0 || m.Route.RouterID == ^babel.RouterID(0) {
		return errors.New("invalid authorized router ID")
	}
	return nil
}

// RouteAuthorizations returns a detached snapshot for route admission. It checks
// current trust and the exact record's signature against the current key, and
// requires a live version-4 lease. Conflicting address claims by different
// principals authorize neither origin. A signed origin proves address ownership,
// not the honesty or reachability of its relays.
func (d *Directory) RouteAuthorizations(now time.Time) map[RouteOrigin]bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.expireLocked(now)
	verified := make([]Manifest, 0, len(d.entries))
	for principal, e := range d.entries {
		m := e.manifest
		if e.expired || m.Withdraw || m.Version != ManifestVersionRoute || m.Route == nil {
			continue
		}
		// A renewed principal cannot lend its new key's authority to an old
		// signature, including when collecting conflicting address claims.
		if _, ok := d.currentEntryChain(principal, e, now); !ok {
			continue
		}
		verified = append(verified, m)
	}
	return authorizedOrigins(verified, d.tenant)
}

// authorizedOrigins derives route ownership from a freshly verified Directory
// snapshot. The caller must have checked current identity trust and lease expiry
// for every manifest. Keeping this projection pure lets node diagnostics reuse
// that same verification without another full certificate-chain pass.
func authorizedOrigins(verified []Manifest, tenant meshid.Tenant) map[RouteOrigin]bool {
	routes := make(map[RouteOrigin]bool)
	owners := make(map[RouteOrigin]string)
	prefixOwners := make(map[netip.Prefix]string)
	conflicts := make(map[string]bool)
	for _, m := range verified {
		if m.Withdraw || m.Version != ManifestVersionRoute || m.Route == nil {
			continue
		}
		device, err := m.Peer(tenant)
		if err != nil || !device.IsValid() || m.validateRoute(device) != nil {
			continue
		}
		principal := device.Key()
		route := *m.Route
		if owner, ok := prefixOwners[route.Prefix]; ok && owner != principal {
			conflicts[owner], conflicts[principal] = true, true
		}
		prefixOwners[route.Prefix] = principal
		owners[route] = principal
	}
	for route, principal := range owners {
		if !conflicts[principal] {
			routes[route] = true
		}
	}
	return routes
}
