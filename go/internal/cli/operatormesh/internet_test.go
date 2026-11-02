package operatormesh

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestInternetGatewayRequiresLiveAuthorizedOffer(t *testing.T) {
	tenant, _ := meshid.ParseTenant("11111111-1111-4111-8111-111111111111")
	self, _ := meshid.ParseDevice(tenant, "22222222-2222-4222-8222-222222222222")
	gateway, _ := meshid.ParseDevice(tenant, "33333333-3333-4333-8333-333333333333")
	now := time.Now()
	origin := localmesh.RouteOrigin{Prefix: netip.PrefixFrom(gateway.MeshAddress(), 128), RouterID: 42, Seqno: 3}
	fixture := func() localmesh.NodeSnapshot {
		return localmesh.NodeSnapshot{Self: self, Devices: []localmesh.Manifest{{Device: gateway.UUID(), Issued: now.Add(-time.Second).UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), Route: &origin, Services: []localmesh.ManifestService{{AppID: meshcatalog.GatewayAppID, ServiceID: meshcatalog.GatewayServiceID}}}}, Routes: []babel.Route{{Prefix: origin.Prefix, RouterID: origin.RouterID, Seqno: origin.Seqno, Link: 1}}, RouteAuthorizations: map[localmesh.RouteOrigin]bool{origin: true}}
	}
	if !InternetGateway(fixture(), gateway, now) {
		t.Fatal("live authorized gateway rejected")
	}
	for name, change := range map[string]func(*localmesh.NodeSnapshot){
		"expired":          func(v *localmesh.NodeSnapshot) { v.Devices[0].Expires = now.UnixMilli() },
		"withdrawn":        func(v *localmesh.NodeSnapshot) { v.Devices[0].Withdraw = true },
		"no offer":         func(v *localmesh.NodeSnapshot) { v.Devices[0].Services = nil },
		"unverified route": func(v *localmesh.NodeSnapshot) { v.RouteAuthorizations = nil },
		"stale route":      func(v *localmesh.NodeSnapshot) { v.Routes[0].Seqno++ },
		"unreachable":      func(v *localmesh.NodeSnapshot) { v.Routes[0].Unreachable = true },
		"no carrier":       func(v *localmesh.NodeSnapshot) { v.Routes[0].Link = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			view := fixture()
			change(&view)
			if InternetGateway(view, gateway, now) {
				t.Fatal("ineligible gateway accepted")
			}
		})
	}
}
