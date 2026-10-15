// Package meshsharing controls opt-in local-mesh Internet sharing. It consumes
// signed, validated Node manifests and the selected Babel route; host policy
// owns only its NAT, DNS and resolver changes.
package meshsharing

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
)

type Config struct {
	Participate        bool     `json:"participate"`
	Roam               bool     `json:"roam"`
	ShareUplink        bool     `json:"shareUplink"`
	UplinkInterfaces   []string `json:"uplinkInterfaces,omitempty"`
	ExcludedInterfaces []string `json:"excludedInterfaces,omitempty"`
}

func (c Config) Validate() error {
	if !c.Participate && (c.Roam || c.ShareUplink) {
		return errors.New("roam and shareUplink require participate")
	}
	seen := map[string]bool{}
	for _, names := range [][]string{c.UplinkInterfaces, c.ExcludedInterfaces} {
		for _, name := range names {
			if !validPhysicalName(name) || seen[name] {
				return errors.New("invalid or repeated uplink interface")
			}
			seen[name] = true
		}
	}
	return nil
}

func validPhysicalName(name string) bool {
	if name == "" || len(name) > 15 || name == "." || name == ".." || strings.HasPrefix(name, "wlm") || strings.HasPrefix(name, "wnan") || strings.HasPrefix(name, "wnp") {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

type Uplink struct {
	Interface, DNS       string
	Healthy, Independent bool
}

func (c Config) EligibleUplink(u Uplink) bool {
	return validPhysicalName(u.Interface) && u.Healthy && u.Independent &&
		!slices.Contains(c.ExcludedInterfaces, u.Interface) &&
		(len(c.UplinkInterfaces) == 0 || slices.Contains(c.UplinkInterfaces, u.Interface))
}

// Gateway is derived from the Node's signature-validated and unexpired
// manifest plus its selected Babel default and host route. It is never a raw
// remote claim. Reachable is a separately debounced on-mesh health probe.
type Gateway struct {
	Asset     int32
	Address   netip.Addr
	Expires   time.Time
	Link      babel.LinkID
	Verified  bool
	Reachable bool
}

type Decision struct {
	Mode           string
	ShareInterface string
	ShareDNS       string
	GatewayAsset   int32
	GatewayAddress netip.Addr
	DNSLink        babel.LinkID
	Reason         string
}

func Decide(c Config, self int32, uplinks []Uplink, gateway Gateway, now time.Time) Decision {
	if c.Validate() != nil || !c.Participate {
		return Decision{Mode: "disabled", Reason: "participation disabled or invalid"}
	}
	var own Uplink
	for _, u := range uplinks {
		if c.EligibleUplink(u) && (own.Interface == "" || u.Interface < own.Interface) {
			own = u
		}
	}
	if own.Interface != "" {
		d := Decision{Mode: "local", Reason: "healthy independent uplink"}
		if c.ShareUplink {
			d.ShareInterface, d.ShareDNS = own.Interface, own.DNS
		}
		return d
	}
	if !c.Roam {
		return Decision{Mode: "relay", Reason: "roaming disabled"}
	}
	if !gateway.Verified || !gateway.Reachable || gateway.Asset <= 0 || gateway.Asset == self ||
		!now.Before(gateway.Expires) || !gateway.Address.Is4() || !gateway.Address.IsPrivate() || gateway.Link == 0 {
		return Decision{Mode: "offline", Reason: "no verified reachable gateway"}
	}
	return Decision{Mode: "roaming", GatewayAsset: gateway.Asset, GatewayAddress: gateway.Address, DNSLink: gateway.Link, Reason: "using signed mesh gateway"}
}

// SignedGateway binds a verified catalog capability and device manifest to
// Babel's selected default and a route to the donor's own mesh IP. Missing,
// stale or withdrawn records, wrong scope or route origin make it ineligible.
func SignedGateway(selfOrg, selfAsset int32, view localmesh.NodeSnapshot, offers []meshcatalog.Record, now time.Time) Gateway {
	var selected babel.Route
	for _, route := range view.Routes {
		if route.Prefix == netip.MustParsePrefix("0.0.0.0/0") && !route.Local && !route.Unreachable && route.Link != 0 {
			selected = route
			break
		}
	}
	if selected.Link == 0 || int32(uint64(selected.RouterID)>>32) != selfOrg {
		return Gateway{}
	}
	asset := int32(uint32(selected.RouterID))
	if asset <= 0 || asset == selfAsset {
		return Gateway{}
	}
	address, _, err := localmesh.Addresses(selfOrg, asset)
	if err != nil {
		return Gateway{}
	}
	var host babel.Route
	for _, route := range view.Routes {
		if route.Prefix == netip.PrefixFrom(address, 32) && !route.Local && !route.Unreachable && route.Link != 0 && route.RouterID == selected.RouterID {
			host = route
			break
		}
	}
	// The reachability probe targets the donor's /32. A different first hop
	// could make that probe pass while the selected default goes elsewhere.
	if host.Link == 0 || host.Link != selected.Link {
		return Gateway{}
	}
	var offerExpiry time.Time
	for _, offer := range offers {
		if meshcatalog.IsGatewayOffer(offer) && !offer.Withdraw && offer.Key.Mesh == "default" &&
			offer.Key.Org == selfOrg && offer.Key.Asset == asset && time.UnixMilli(offer.Expires).After(now) {
			expires := time.UnixMilli(offer.Expires)
			if expires.After(offerExpiry) {
				offerExpiry = expires
			}
		}
	}
	if offerExpiry.IsZero() {
		return Gateway{}
	}
	for _, manifest := range view.Devices {
		if manifest.Org == selfOrg && manifest.Asset == asset && manifest.Internet && !manifest.Withdraw &&
			manifest.Expires > now.UnixMilli() && manifest.Issued <= now.Add(5*time.Second).UnixMilli() {
			expires := time.UnixMilli(manifest.Expires)
			if offerExpiry.Before(expires) {
				expires = offerExpiry
			}
			return Gateway{Asset: asset, Address: address, Expires: expires, Link: host.Link, Verified: true}
		}
	}
	return Gateway{}
}

// HealthTracker avoids a single failed probe flapping a healthy uplink or
// gateway. Disappearing candidates and signed offers bypass hysteresis.
type HealthTracker struct {
	Healthy             bool
	successes, failures int
}

func (h *HealthTracker) Observe(success bool) bool {
	if success {
		h.failures = 0
		if h.successes < 2 {
			h.successes++
		}
		if h.successes == 2 {
			h.Healthy = true
		}
	} else {
		h.successes = 0
		if h.failures < 3 {
			h.failures++
		}
		if h.failures == 3 {
			h.Healthy = false
		}
	}
	return h.Healthy
}
