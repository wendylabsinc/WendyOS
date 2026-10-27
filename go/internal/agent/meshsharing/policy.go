// Package meshsharing controls opt-in local-wendy net Internet sharing. It consumes
// signed, validated Node manifests and the selected Babel route; host policy
// owns only its NAT, DNS and resolver changes.
package meshsharing

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
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

// Gateway is derived from a signature-validated catalog offer, the donor's
// unexpired manifest, and a live Babel route to the donor's own ULA. It is
// never a raw remote claim. Reachable is a separately debounced on-mesh
// health probe of that ULA.
type Gateway struct {
	Device    meshid.Device
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
	GatewayDevice  meshid.Device
	GatewayAddress netip.Addr
	DNSLink        babel.LinkID
	Reason         string
}

func Decide(c Config, self meshid.Device, uplinks []Uplink, gateway Gateway, now time.Time) Decision {
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
	if !gateway.Verified || !gateway.Reachable || gateway.Device == self ||
		!now.Before(gateway.Expires) || !meshid.InMesh(gateway.Address) || gateway.Link == 0 {
		return Decision{Mode: "offline", Reason: "no verified reachable gateway"}
	}
	return Decision{Mode: "roaming", GatewayDevice: gateway.Device, GatewayAddress: gateway.Address, DNSLink: gateway.Link, Reason: "using signed wendy net gateway"}
}

// SignedGateway binds a verified catalog capability and the donor's manifest
// to a live Babel route to the donor's own ULA. Missing, stale or withdrawn
// records, a foreign tenant scope, or a route that the mesh never installed
// make a donor ineligible. Among eligible donors the latest combined expiry
// wins, with the canonical device order breaking ties. The controller uses
// this order for initial selection and failover, but keeps a healthy selected
// donor sticky so lease refreshes do not churn established tunnel state.
func SignedGateway(self meshid.Device, view localmesh.NodeSnapshot, offers []meshcatalog.Record, now time.Time) Gateway {
	candidates := signedGateways(self, view, offers, now)
	if len(candidates) == 0 {
		return Gateway{}
	}
	return candidates[0]
}

// signedGateways returns every eligible donor in deterministic preference order.
// The controller must evaluate reachability before choosing one: a fresh signed
// offer and a surviving Babel route alone do not prove the donor is reachable.
func signedGateways(self meshid.Device, view localmesh.NodeSnapshot, offers []meshcatalog.Record, now time.Time) []Gateway {
	byDevice := make(map[meshid.Device]Gateway)
	for _, offer := range offers {
		if !meshcatalog.IsGatewayOffer(offer) || offer.Withdraw {
			continue
		}
		device, err := meshid.DeviceFromPrincipal(offer.Key.Device)
		if err != nil || device == self || device.Tenant() != self.Tenant() {
			continue
		}
		expires := time.UnixMilli(offer.Expires)
		if !expires.After(now) {
			continue
		}
		var manifestExpiry time.Time
		for _, manifest := range view.Devices {
			if manifest.Device != device.UUID() || manifest.Withdraw {
				continue
			}
			issued, lease := time.UnixMilli(manifest.Issued), time.UnixMilli(manifest.Expires)
			if issued.After(now.Add(5*time.Second)) || !lease.After(now) {
				continue
			}
			if lease.After(manifestExpiry) {
				manifestExpiry = lease
			}
		}
		if manifestExpiry.IsZero() {
			continue
		}
		ula := netip.PrefixFrom(device.MeshAddress(), 128)
		var link babel.LinkID
		for _, route := range view.Routes {
			if route.Prefix == ula && !route.Local && !route.Unreachable && route.Link != 0 {
				link = route.Link
				break
			}
		}
		if link == 0 {
			continue
		}
		if manifestExpiry.Before(expires) {
			expires = manifestExpiry
		}
		candidate := Gateway{Device: device, Address: device.MeshAddress(), Expires: expires, Link: link, Verified: true}
		if previous, ok := byDevice[device]; !ok || candidate.Expires.After(previous.Expires) {
			byDevice[device] = candidate
		}
	}
	candidates := make([]Gateway, 0, len(byDevice))
	for _, candidate := range byDevice {
		candidates = append(candidates, candidate)
	}
	slices.SortFunc(candidates, func(a, b Gateway) int {
		if a.Expires.After(b.Expires) {
			return -1
		}
		if a.Expires.Before(b.Expires) {
			return 1
		}
		return meshid.Compare(a.Device, b.Device)
	})
	return candidates
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
