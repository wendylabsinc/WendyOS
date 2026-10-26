package meshcatalog

import (
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestEligibleSnapshotRequiresSignedPresenceAndDeviceRoute(t *testing.T) {
	now := time.Now()
	device := principalOf(t, p535)
	manifest := localmesh.Manifest{Device: device.UUID(), Expires: now.Add(time.Minute).UnixMilli()}
	route := babel.Route{Prefix: netip.PrefixFrom(device.MeshAddress(), 128), Link: 2}
	view := localmesh.NodeSnapshot{Devices: []localmesh.Manifest{manifest}, Routes: []babel.Route{route}}
	if !eligibleSnapshot(view, device, now) {
		t.Fatal("live multi-hop route rejected")
	}
	checks := []struct {
		name   string
		change func(*localmesh.NodeSnapshot)
	}{
		{"missing manifest", func(v *localmesh.NodeSnapshot) { v.Devices = nil }},
		{"other device", func(v *localmesh.NodeSnapshot) { v.Devices[0].Device = deviceForIndex(999).UUID() }},
		{"expired", func(v *localmesh.NodeSnapshot) { v.Devices[0].Expires = now.Add(-time.Second).UnixMilli() }},
		{"withdrawn", func(v *localmesh.NodeSnapshot) { v.Devices[0].Withdraw = true }},
		{"no route", func(v *localmesh.NodeSnapshot) { v.Routes = nil }},
		{"covering route", func(v *localmesh.NodeSnapshot) {
			v.Routes[0].Prefix = netip.PrefixFrom(device.MeshAddress().WithZone(""), 64)
		}},
		{"other device route", func(v *localmesh.NodeSnapshot) {
			v.Routes[0].Prefix = netip.PrefixFrom(deviceForIndex(999).MeshAddress(), 128)
		}},
		{"unreachable", func(v *localmesh.NodeSnapshot) { v.Routes[0].Unreachable = true }},
		{"local route", func(v *localmesh.NodeSnapshot) { v.Routes[0].Local = true }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			copy := localmesh.NodeSnapshot{Devices: append([]localmesh.Manifest(nil), view.Devices...), Routes: append([]babel.Route(nil), view.Routes...)}
			check.change(&copy)
			if eligibleSnapshot(copy, device, now) {
				t.Fatal("untrusted or unreachable peer admitted")
			}
		})
	}
}
func deviceForIndex(i int) meshid.Device {
	principal := fmt.Sprintf("spiffe://wendy.sh/tenant/%s/device/%08x-0000-4000-8000-00000000%04x", testTenantID, i, i)
	device, _ := meshid.DeviceFromPrincipal(principal)
	return device
}
