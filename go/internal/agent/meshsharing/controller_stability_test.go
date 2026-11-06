package meshsharing

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
)

// Independent origins renew their manifests every 20s and gateway offers on a
// different schedule. Freshness is an admission bound, not a reason to destroy
// an already healthy borrower's tunnel and its established NAT connections.
func TestControllerRetainsHealthyDonorAcrossSignedRefresh(t *testing.T) {
	for _, refresh := range []string{"manifest", "offer"} {
		t.Run(refresh, func(t *testing.T) {
			var events []string
			now := time.Now()
			a, b := policyDonorDevice(t), mustPolicyDevice(t, policyTenant, policyDonorLate)
			view, other := gatewayView(a, now), gatewayView(b, now)
			view.Devices = append(view.Devices, other.Devices...)
			view.Routes = append(view.Routes, other.Routes...)
			offers := []meshcatalog.Record{gatewayOffer(a, now), gatewayOffer(b, now)}
			if refresh == "manifest" {
				for i := range offers {
					offers[i].Expires = now.Add(meshcatalog.GatewayOfferLease).UnixMilli()
				}
				view.Devices[0].Issued = now.Add(-10 * time.Second).UnixMilli()
				view.Devices[0].Expires = now.Add(50 * time.Second).UnixMilli()
				view.Devices[1].Issued = now.Add(-20 * time.Second).UnixMilli()
				view.Devices[1].Expires = now.Add(40 * time.Second).UnixMilli()
			} else {
				offers[0].Expires = now.Add(50 * time.Second).UnixMilli()
				offers[1].Expires = now.Add(40 * time.Second).UnixMilli()
			}
			node := &fakeNode{view: view, offers: offers, events: &events}
			controller, err := NewWithDeps(policySelf(t), node, &fakePolicy{events: &events}, &fakeProbe{gateway: true})
			if err != nil {
				t.Fatal(err)
			}
			config := Config{Participate: true, Roam: true}
			for range 2 {
				if _, err := controller.Step(context.Background(), config); err != nil {
					t.Fatal(err)
				}
			}
			if controller.gatewayDevice != a {
				t.Fatalf("initial donor = %s, want %s", controller.gatewayDevice.Principal(), a.Principal())
			}
			// B renews while A remains authenticated, routed, unexpired and
			// healthy. Both probes continue succeeding throughout.
			if refresh == "manifest" {
				node.view.Devices[1].Revision++
				node.view.Devices[1].Issued = now.UnixMilli()
				node.view.Devices[1].Expires = now.Add(time.Minute).UnixMilli()
			} else {
				node.offers[1].Generation++
				node.offers[1].Issued = now.UnixMilli()
				node.offers[1].Expires = now.Add(time.Minute).UnixMilli()
			}
			events = nil
			decision, err := controller.Step(context.Background(), config)
			if err != nil {
				t.Fatal(err)
			}
			if decision.GatewayDevice != a {
				t.Fatalf("healthy established donor changed on %s refresh: got %s, want %s; policy events=%v", refresh, decision.GatewayDevice.Principal(), a.Principal(), events)
			}
		})
	}
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func TestControllerRefreshesUnchangedGatewayOfferBeforeLeaseExpiry(t *testing.T) {
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	controller, err := NewWithDeps(policySelf(t), node, policy, &fakeProbe{
		candidates: map[string]string{"wlan0": "1.1.1.1"}, uplink: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, ShareUplink: true}
	for range 2 {
		if _, err := controller.Step(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	if got := countString(events, "offer:wlan0"); got != 1 {
		t.Fatalf("initial signed offer publications = %d, want 1: %v", got, events)
	}

	events = nil
	if _, err := controller.Step(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(events, "offer:wlan0") {
		t.Fatalf("offer refreshed before its renewal interval: %v", events)
	}

	controller.gatewayOfferRefreshed = time.Now().Add(-gatewayOfferRefreshInterval)
	events = nil
	if _, err := controller.Step(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if got := countString(events, "offer:wlan0"); got != 1 {
		t.Fatalf("due offer refresh publications = %d, want 1: %v", got, events)
	}
	if slices.Contains(events, "offer:") {
		t.Fatalf("successful offer renewal withdrew the capability: %v", events)
	}

	events = nil
	if _, err := controller.Step(context.Background(), Config{Participate: true}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(events, "offer:") || slices.Contains(events, "offer:wlan0") {
		t.Fatalf("disabling sharing did not withdraw the renewed offer: %v", events)
	}
	events = nil
	if _, err := controller.Step(context.Background(), Config{Participate: true}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(events, "offer:wlan0") {
		t.Fatalf("withdrawn offer was resurrected while sharing remained disabled: %v", events)
	}
}

func TestControllerWithdrawsSharingWhenGatewayOfferRenewalFails(t *testing.T) {
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	controller, err := NewWithDeps(policySelf(t), node, policy, &fakeProbe{
		candidates: map[string]string{"wlan0": "1.1.1.1"}, uplink: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, ShareUplink: true}
	for range 2 {
		if _, err := controller.Step(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	node.failOfferFor = "wlan0"
	controller.gatewayOfferRefreshed = time.Now().Add(-gatewayOfferRefreshInterval)
	events = nil
	if _, err := controller.Step(context.Background(), cfg); err == nil {
		t.Fatal("failed gateway offer renewal was not reported")
	}
	if controller.shareKey != "" {
		t.Fatalf("failed renewal retained sharing state %q", controller.shareKey)
	}
	want := []string{"offer:wlan0", "offer:", "nat:|"}
	if !slices.Equal(events[len(events)-len(want):], want) {
		t.Fatalf("renewal failure did not withdraw offer before sharing: got %v, want suffix %v", events, want)
	}
}
