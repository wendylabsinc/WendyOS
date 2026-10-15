package meshsharing

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
)

func gatewayOffer(asset int32, now time.Time) meshcatalog.Record {
	return meshcatalog.Record{Version: 1, Key: meshcatalog.Key{Mesh: "default", Org: 64, Asset: asset,
		AppID: meshcatalog.GatewayAppID, ServiceID: meshcatalog.GatewayServiceID}, Generation: 1,
		Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli()}
}

func gatewayView(now time.Time) localmesh.NodeSnapshot {
	id, _ := localmesh.RouterID(64, 460)
	ip, _, _ := localmesh.Addresses(64, 460)
	return localmesh.NodeSnapshot{
		Devices: []localmesh.Manifest{{Version: 1, Org: 64, Asset: 460, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Internet: true}},
		Routes: []babel.Route{
			{Prefix: netip.MustParsePrefix("0.0.0.0/0"), RouterID: id, Link: 8, Metric: 512},
			{Prefix: netip.PrefixFrom(ip, 32), RouterID: id, Link: 8, Metric: 768},
		},
	}
}

func TestSignedGatewayRequiresFreshOfferAndSelectedRoutes(t *testing.T) {
	now := time.Now()
	view := gatewayView(now)
	got := SignedGateway(64, 445, view, []meshcatalog.Record{gatewayOffer(460, now)}, now)
	if !got.Verified || got.Asset != 460 || got.Link != 8 || got.Address.String() != "10.88.1.204" {
		t.Fatalf("valid signed gateway rejected: %+v", got)
	}
	checks := []struct {
		name   string
		change func(*localmesh.NodeSnapshot)
	}{
		{"expired offer", func(v *localmesh.NodeSnapshot) { v.Devices[0].Expires = now.UnixMilli() }},
		{"withdrawn offer", func(v *localmesh.NodeSnapshot) { v.Devices[0].Withdraw = true }},
		{"no internet capability", func(v *localmesh.NodeSnapshot) { v.Devices[0].Internet = false }},
		{"other organization", func(v *localmesh.NodeSnapshot) { v.Devices[0].Org = 65 }},
		{"no donor host route", func(v *localmesh.NodeSnapshot) { v.Routes = v.Routes[:1] }},
		{"wrong default origin", func(v *localmesh.NodeSnapshot) { v.Routes[0].RouterID++ }},
		{"different default first hop", func(v *localmesh.NodeSnapshot) { v.Routes[0].Link = 7 }},
		{"unreachable default", func(v *localmesh.NodeSnapshot) { v.Routes[0].Unreachable = true }},
	}
	for _, test := range checks {
		t.Run(test.name, func(t *testing.T) {
			v := gatewayView(now)
			test.change(&v)
			if g := SignedGateway(64, 445, v, []meshcatalog.Record{gatewayOffer(460, now)}, now); g.Verified {
				t.Fatalf("accepted invalid gateway: %+v", g)
			}
		})
	}
	if g := SignedGateway(64, 445, view, nil, now); g.Verified {
		t.Fatalf("accepted gateway without signed service: %+v", g)
	}
	bad := gatewayOffer(460, now)
	bad.Withdraw = true
	if g := SignedGateway(64, 445, view, []meshcatalog.Record{bad}, now); g.Verified {
		t.Fatalf("accepted withdrawn signed service: %+v", g)
	}
}

func TestSignedGatewayAllowsConvergedTwoHopDonor(t *testing.T) {
	now := time.Now()
	view := gatewayView(now)
	view.Routes[0].Link = 7
	if SignedGateway(64, 445, view, []meshcatalog.Record{gatewayOffer(460, now)}, now).Verified {
		t.Fatal("probed donor through a different link than selected default")
	}
	// A route change may briefly give the donor /32 and default different
	// first hops. Once Babel converges on the same relay, it is usable.
	view.Routes[1].Link = 7
	if got := SignedGateway(64, 445, view, []meshcatalog.Record{gatewayOffer(460, now)}, now); !got.Verified || got.Link != 7 {
		t.Fatalf("converged two-hop donor rejected: %+v", got)
	}
}

func TestPolicyPrefersIndependentLocalUplinkAndCannotReshareBorrowedRoute(t *testing.T) {
	now := time.Now()
	cfg := Config{Participate: true, Roam: true, ShareUplink: true, ExcludedInterfaces: []string{"eth0"}}
	gateway := Gateway{Asset: 460, Address: netip.MustParseAddr("10.88.1.204"), Expires: now.Add(time.Minute), Link: 8, Verified: true, Reachable: true}
	if d := Decide(cfg, 445, []Uplink{{Interface: "wlan0", DNS: "1.1.1.1", Healthy: true, Independent: true}}, gateway, now); d.Mode != "local" || d.ShareInterface != "wlan0" {
		t.Fatalf("local uplink not preferred: %+v", d)
	}
	if d := Decide(cfg, 445, []Uplink{{Interface: "wlmp8", Healthy: true, Independent: true}, {Interface: "eth0", Healthy: true, Independent: true}, {Interface: "wlan0", Healthy: true, Independent: false}}, gateway, now); d.Mode != "roaming" || d.ShareInterface != "" {
		t.Fatalf("borrowed or excluded route was re-shared: %+v", d)
	}
	cfg.Roam = false
	if d := Decide(cfg, 445, nil, gateway, now); d.Mode != "relay" {
		t.Fatalf("roaming disable ignored: %+v", d)
	}
	cfg.Participate, cfg.ShareUplink = false, false
	if d := Decide(cfg, 445, nil, gateway, now); d.Mode != "disabled" {
		t.Fatalf("participation disable ignored: %+v", d)
	}
}

func TestHealthDebounceAndImmediateOfferExpiry(t *testing.T) {
	h := &HealthTracker{}
	for i, sample := range []struct{ good, expected bool }{{true, false}, {true, true}, {false, true}, {false, true}, {false, false}, {true, false}, {true, true}} {
		if got := h.Observe(sample.good); got != sample.expected {
			t.Fatalf("sample %d: got %v want %v", i, got, sample.expected)
		}
	}
	now := time.Now()
	v := gatewayView(now)
	if SignedGateway(64, 445, v, []meshcatalog.Record{gatewayOffer(460, now)}, now.Add(2*time.Minute)).Verified {
		t.Fatal("expired signed offer retained through health hysteresis")
	}
}
