package meshsharing

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type activeLeasePolicy struct {
	*fakePolicy
	active func(meshid.Device) bool
}

func (p *activeLeasePolicy) ActiveGatewayLease(peer meshid.Device) bool { return p.active(peer) }

// A live authenticated control lease is stronger reachability evidence than an
// ICMP subprocess timing out on a busy BLE hop. Failed probes remain recorded,
// so loss of that lease takes effect on the next step without extra debounce.
func TestControllerKeepsLiveGatewayLeaseDespiteFailedProbes(t *testing.T) {
	var events []string
	now, donor := time.Now(), policyDonorDevice(t)
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	probe := &fakeProbe{gateway: true}
	live := false
	policy := &activeLeasePolicy{fakePolicy: &fakePolicy{events: &events}, active: func(peer meshid.Device) bool { return live && peer == donor }}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true}
	step := func(want string) {
		t.Helper()
		d, err := c.Step(context.Background(), cfg)
		if err != nil || d.Mode != want {
			t.Fatalf("decision=%+v err=%v want=%s", d, err, want)
		}
	}
	step("offline")
	step("roaming")
	live = true
	probe.gateway = false
	for range 5 {
		step("roaming")
	}
	if c.gatewayHealth[donor].Healthy || c.gatewayHealth[donor].failures != 3 {
		t.Fatalf("active lease reset probe failures: %+v", c.gatewayHealth[donor])
	}
	live = false
	step("offline")
	// A stale promise for the previous donor cannot admit a new donor with no
	// successful probes, and cannot immediately re-admit the failed old donor.
	live = true
	step("offline")
	other := mustPolicyDevice(t, policyTenant, policyDonorLate)
	node.view = gatewayView(other, now)
	node.offers = []meshcatalog.Record{gatewayOffer(other, now)}
	step("offline")
}

func TestActiveGatewayLeaseCannotOverrideSignedEligibility(t *testing.T) {
	for _, name := range []string{"offer withdrawn", "offer expired", "manifest withdrawn", "manifest expired", "route absent", "route unreachable", "participation disabled", "roaming disabled"} {
		t.Run(name, func(t *testing.T) {
			var events []string
			now, donor := time.Now(), policyDonorDevice(t)
			node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
			probe := &fakeProbe{gateway: true}
			p := &activeLeasePolicy{fakePolicy: &fakePolicy{events: &events}, active: func(peer meshid.Device) bool { return peer == donor }}
			c, err := NewWithDeps(policySelf(t), node, p, probe)
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{Participate: true, Roam: true}
			for range 2 {
				if _, err := c.Step(context.Background(), cfg); err != nil {
					t.Fatal(err)
				}
			}
			probe.gateway = false
			switch name {
			case "offer withdrawn":
				node.offers = nil
			case "offer expired":
				node.offers[0].Expires = now.Add(-time.Second).UnixMilli()
			case "manifest withdrawn":
				node.view.Devices[0].Withdraw = true
			case "manifest expired":
				node.view.Devices[0].Expires = now.Add(-time.Second).UnixMilli()
			case "route absent":
				node.view.Routes = nil
			case "route unreachable":
				node.view.Routes[0].Unreachable = true
			case "participation disabled":
				cfg = Config{}
			case "roaming disabled":
				cfg.Roam = false
			}
			d, err := c.Step(context.Background(), cfg)
			if err != nil || d.Mode == "roaming" || d.GatewayDevice.IsValid() {
				t.Fatalf("live lease overrode %s: %+v %v", name, d, err)
			}
		})
	}
}

func TestActiveGatewayLeaseDoesNotExtendSignedExpiryDuringObservation(t *testing.T) {
	var events []string
	now, donor := time.Now(), policyDonorDevice(t)
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	p := &activeLeasePolicy{fakePolicy: &fakePolicy{events: &events}, active: func(meshid.Device) bool {
		time.Sleep(150 * time.Millisecond)
		return true
	}}
	probe := &fakeProbe{gateway: true}
	c, err := NewWithDeps(policySelf(t), node, p, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true}
	for range 2 {
		if _, err := c.Step(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	probe.gateway = false
	for range 2 {
		if _, err := c.Step(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	node.offers[0].Expires = time.Now().Add(75 * time.Millisecond).UnixMilli()
	d, err := c.Step(context.Background(), cfg)
	if err != nil || d.Mode != "offline" {
		t.Fatalf("signed expiry extended by control observation: %+v %v", d, err)
	}
}
