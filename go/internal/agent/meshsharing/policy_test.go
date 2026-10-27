package meshsharing

import (
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	policyTenant     = "0b7c1d9e-2a3f-4c5d-8e90-1234567890ab"
	policyOtherTenna = "0b7c1d9e-2a3f-4c5d-8e90-1234567890ac"
	policySelfDevice = "11111111-2222-4333-8444-555555555555"
	policyDonor      = "22222222-3333-4444-8555-666666666666"
	policyDonorLate  = "33333333-4444-4555-8666-777777777777"
)

func mustPolicyDevice(t *testing.T, tenant, device string) meshid.Device {
	t.Helper()
	scope, err := meshid.ParseTenant(tenant)
	if err != nil {
		t.Fatal(err)
	}
	d, err := meshid.ParseDevice(scope, device)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func policySelf(t *testing.T) meshid.Device {
	return mustPolicyDevice(t, policyTenant, policySelfDevice)
}

func policyDonorDevice(t *testing.T) meshid.Device {
	return mustPolicyDevice(t, policyTenant, policyDonor)
}

func gatewayOffer(donor meshid.Device, now time.Time) meshcatalog.Record {
	return meshcatalog.Record{Version: 1, Key: meshcatalog.Key{Mesh: "default", Device: donor.Principal(),
		AppID: meshcatalog.GatewayAppID, ServiceID: meshcatalog.GatewayServiceID}, Generation: 1,
		Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli()}
}

func gatewayView(donor meshid.Device, now time.Time) localmesh.NodeSnapshot {
	return localmesh.NodeSnapshot{
		Devices: []localmesh.Manifest{{Version: 1, Device: donor.UUID(), Revision: 1,
			Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 8443}},
		Routes: []babel.Route{
			{Prefix: netip.PrefixFrom(donor.MeshAddress(), 128), Link: 8, Metric: 768},
		},
	}
}

func TestSignedGatewayRequiresFreshOfferManifestAndRoute(t *testing.T) {
	now := time.Now()
	self, donor := policySelf(t), policyDonorDevice(t)
	view := gatewayView(donor, now)
	got := SignedGateway(self, view, []meshcatalog.Record{gatewayOffer(donor, now)}, now)
	if !got.Verified || got.Device != donor || got.Link != 8 || got.Address != donor.MeshAddress() {
		t.Fatalf("valid signed gateway rejected: %+v", got)
	}
	if !meshid.InMesh(got.Address) {
		t.Fatalf("gateway address %v is outside the managed pool", got.Address)
	}
	checks := []struct {
		name   string
		change func(*localmesh.NodeSnapshot)
	}{
		{"expired manifest", func(v *localmesh.NodeSnapshot) { v.Devices[0].Expires = now.UnixMilli() }},
		{"withdrawn manifest", func(v *localmesh.NodeSnapshot) { v.Devices[0].Withdraw = true }},
		{"no manifest", func(v *localmesh.NodeSnapshot) { v.Devices = nil }},
		{"manifest for another device", func(v *localmesh.NodeSnapshot) { v.Devices[0].Device = uuid.MustParse(policyDonorLate) }},
		{"no donor ULA route", func(v *localmesh.NodeSnapshot) { v.Routes = nil }},
		{"unreachable donor route", func(v *localmesh.NodeSnapshot) { v.Routes[0].Unreachable = true }},
		{"local donor route", func(v *localmesh.NodeSnapshot) { v.Routes[0].Local = true }},
		{"route without a link", func(v *localmesh.NodeSnapshot) { v.Routes[0].Link = 0 }},
	}
	for _, test := range checks {
		t.Run(test.name, func(t *testing.T) {
			v := gatewayView(donor, now)
			test.change(&v)
			if g := SignedGateway(self, v, []meshcatalog.Record{gatewayOffer(donor, now)}, now); g.Verified {
				t.Fatalf("accepted invalid gateway: %+v", g)
			}
		})
	}
	if g := SignedGateway(self, view, nil, now); g.Verified {
		t.Fatalf("accepted gateway without signed service: %+v", g)
	}
	bad := gatewayOffer(donor, now)
	bad.Withdraw = true
	if g := SignedGateway(self, view, []meshcatalog.Record{bad}, now); g.Verified {
		t.Fatalf("accepted withdrawn signed service: %+v", g)
	}
	expired := gatewayOffer(donor, now)
	expired.Expires = now.Add(-time.Second).UnixMilli()
	if g := SignedGateway(self, view, []meshcatalog.Record{expired}, now); g.Verified {
		t.Fatalf("accepted expired signed service: %+v", g)
	}
	// A donor from another tenant is never this mesh's gateway, even with a
	// perfectly shaped offer.
	foreign := gatewayOffer(mustPolicyDevice(t, policyOtherTenna, policyDonor), now)
	if g := SignedGateway(self, view, []meshcatalog.Record{foreign}, now); g.Verified {
		t.Fatalf("accepted gateway from another tenant: %+v", g)
	}
	// This host is never its own gateway.
	if g := SignedGateway(self, gatewayView(self, now), []meshcatalog.Record{gatewayOffer(self, now)}, now); g.Verified {
		t.Fatalf("accepted self as gateway: %+v", g)
	}
}

func TestSignedGatewayConvergesOnOneDonor(t *testing.T) {
	now := time.Now()
	self, donor := policySelf(t), policyDonorDevice(t)
	late := mustPolicyDevice(t, policyTenant, policyDonorLate)
	donorOffer, lateOffer := gatewayOffer(donor, now), gatewayOffer(late, now)
	lateOffer.Expires += (5 * time.Minute).Milliseconds()
	offers := []meshcatalog.Record{donorOffer, lateOffer}
	view := gatewayView(donor, now)
	lateView := gatewayView(late, now)
	lateView.Devices[0].Expires += (5 * time.Minute).Milliseconds()
	view.Devices = append(view.Devices, lateView.Devices...)
	view.Routes = append(view.Routes, lateView.Routes...)
	// The donor with the longer combined lease wins, so every host holding
	// the same records picks the same gateway.
	if got := SignedGateway(self, view, offers, now); got.Device != late {
		t.Fatalf("converged selection picked %+v, want the longer-lived donor", got.Device)
	}
	// Equal expiry must break by canonical device order, not record order.
	same := gatewayOffer(late, now)
	same.Expires = gatewayOffer(donor, now).Expires
	if got := SignedGateway(self, view, []meshcatalog.Record{same, gatewayOffer(donor, now)}, now); got.Device != donor {
		t.Fatalf("equal-expiry donors resolved to %v, want the lower UUID %v", got.Device, donor)
	}
	if got := SignedGateway(self, view, []meshcatalog.Record{gatewayOffer(donor, now), same}, now); got.Device != donor {
		t.Fatalf("record order changed the resolved gateway: %+v", got)
	}
}

func TestPolicyPrefersIndependentLocalUplinkAndCannotReshareBorrowedRoute(t *testing.T) {
	now := time.Now()
	self, donor := policySelf(t), policyDonorDevice(t)
	cfg := Config{Participate: true, Roam: true, ShareUplink: true, ExcludedInterfaces: []string{"eth0"}}
	gateway := Gateway{Device: donor, Address: donor.MeshAddress(), Expires: now.Add(time.Minute), Link: 8, Verified: true, Reachable: true}
	if d := Decide(cfg, self, []Uplink{{Interface: "wlan0", DNS: "1.1.1.1", Healthy: true, Independent: true}}, gateway, now); d.Mode != "local" || d.ShareInterface != "wlan0" {
		t.Fatalf("local uplink not preferred: %+v", d)
	}
	if d := Decide(cfg, self, []Uplink{{Interface: "wlmp8", Healthy: true, Independent: true}, {Interface: "eth0", Healthy: true, Independent: true}, {Interface: "wlan0", Healthy: true, Independent: false}}, gateway, now); d.Mode != "roaming" || d.ShareInterface != "" {
		t.Fatalf("borrowed or excluded route was re-shared: %+v", d)
	}
	if d := Decide(cfg, self, nil, gateway, now); d.Mode != "roaming" || d.GatewayDevice != donor || d.GatewayAddress != donor.MeshAddress() || d.DNSLink != 8 {
		t.Fatalf("roaming decision lost the gateway identity: %+v", d)
	}
	cfg.Roam = false
	if d := Decide(cfg, self, nil, gateway, now); d.Mode != "relay" {
		t.Fatalf("roaming disable ignored: %+v", d)
	}
	cfg.Participate, cfg.ShareUplink = false, false
	if d := Decide(cfg, self, nil, gateway, now); d.Mode != "disabled" {
		t.Fatalf("participation disable ignored: %+v", d)
	}
}

func TestDecideRefusesUnverifiedOrForeignGateways(t *testing.T) {
	now := time.Now()
	self, donor := policySelf(t), policyDonorDevice(t)
	cfg := Config{Participate: true, Roam: true}
	base := Gateway{Device: donor, Address: donor.MeshAddress(), Expires: now.Add(time.Minute), Link: 8, Verified: true, Reachable: true}
	broken := []struct {
		name   string
		change func(*Gateway)
	}{
		{"unverified", func(g *Gateway) { g.Verified = false }},
		{"unreachable", func(g *Gateway) { g.Reachable = false }},
		{"expired", func(g *Gateway) { g.Expires = now }},
		{"self", func(g *Gateway) { g.Device = self }},
		{"no link", func(g *Gateway) { g.Link = 0 }},
		{"address outside the pool", func(g *Gateway) { g.Address = netip.MustParseAddr("10.88.1.204") }},
	}
	for _, test := range broken {
		g := base
		test.change(&g)
		if d := Decide(cfg, self, nil, g, now); d.Mode != "offline" {
			t.Fatalf("%s: accepted gateway decision: %+v", test.name, d)
		}
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
	self, donor := policySelf(t), policyDonorDevice(t)
	v := gatewayView(donor, now)
	if SignedGateway(self, v, []meshcatalog.Record{gatewayOffer(donor, now)}, now.Add(2*time.Minute)).Verified {
		t.Fatal("expired signed offer retained through health hysteresis")
	}
}
