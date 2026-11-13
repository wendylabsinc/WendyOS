package meshsharing

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type fakeNode struct {
	view         localmesh.NodeSnapshot
	offers       []meshcatalog.Record
	events       *[]string
	failOfferFor string
}

func (n *fakeNode) Snapshot() localmesh.NodeSnapshot    { return n.view }
func (n *fakeNode) GatewayOffers() []meshcatalog.Record { return n.offers }
func (n *fakeNode) SetGatewayOffer(_ context.Context, iface string) error {
	*n.events = append(*n.events, "offer:"+iface)
	if iface != "" && iface == n.failOfferFor {
		return errors.New("simulated gateway offer publication failure")
	}
	return nil
}

func TestDisableWithdrawsOfferEvenIfRouteCleanupFails(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"wlan0": "1.1.1.1"}, uplink: true}
	policy := &fakePolicy{events: &events}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, ShareUplink: true}
	for range 2 {
		if _, err := c.Step(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	events = nil
	policy.failGatewayWithdrawal = true
	if _, err := c.Step(ctx, Config{}); err == nil || strings.Join(events, ",") != "offer:,gateway:,dns:|" {
		t.Fatalf("signed offer not withdrawn before failed cleanup: %+v %v", events, err)
	}
	policy.failGatewayWithdrawal = false
	events = nil
	if _, err := c.Step(ctx, Config{}); err != nil || !slices.Contains(events, "nat:|") {
		t.Fatalf("disable could not retry policy cleanup: %+v %v", events, err)
	}
}

type fakePolicy struct {
	events                *[]string
	failShare             bool
	failWithdraw          int
	failGateway           bool
	pendingGateway        int
	failGatewayWithdrawal bool
	failDNS               bool
	failDNSWithdrawal     bool
	cancelOnDNS           context.CancelFunc
	cleanupContexts       []error
	closeErr              error
}

func (p *fakePolicy) SetGateway(ctx context.Context, peer meshid.Device) error {
	if peer == (meshid.Device{}) {
		*p.events = append(*p.events, "gateway:")
		p.cleanupContexts = append(p.cleanupContexts, ctx.Err())
		if p.failGatewayWithdrawal {
			return errors.New("simulated tunnel cleanup failure")
		}
	} else {
		*p.events = append(*p.events, "gateway:"+peer.Principal())
		if p.failGateway {
			return errors.New("simulated tunnel setup failure")
		}
		if p.pendingGateway > 0 {
			p.pendingGateway--
			return errGatewayPending
		}
	}
	return nil
}

func (p *fakePolicy) SetSharing(_ context.Context, iface, dns string) error {
	*p.events = append(*p.events, "nat:"+iface+"|"+dns)
	if p.failShare && iface != "" {
		return errors.New("simulated DNS proxy failure")
	}
	if p.failWithdraw > 0 && iface == "" {
		p.failWithdraw--
		return errors.New("simulated xtables lock")
	}
	return nil
}
func (p *fakePolicy) SetDNS(ctx context.Context, iface, address string) error {
	*p.events = append(*p.events, "dns:"+iface+"|"+address)
	if address == "" {
		p.cleanupContexts = append(p.cleanupContexts, ctx.Err())
	}
	if (address != "" && p.failDNS) || (address == "" && p.failDNSWithdrawal) {
		if p.cancelOnDNS != nil {
			p.cancelOnDNS()
		}
		return errors.New("simulated resolver failure")
	}
	return nil
}
func (p *fakePolicy) Close() error {
	*p.events = append(*p.events, "policy:close")
	return p.closeErr
}

func TestControllerCloseRetainsFailedCleanupForRetry(t *testing.T) {
	var events []string
	want := errors.New("resolver restore failed")
	policy := &fakePolicy{events: &events, closeErr: want}
	c, err := NewWithDeps(policySelf(t), &fakeNode{events: &events}, policy, &fakeProbe{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := c.Close(); !errors.Is(err, want) {
			t.Fatalf("unfinished cleanup reported success: %v", err)
		}
	}
	if _, err := c.Step(context.Background(), Config{Participate: true}); err == nil {
		t.Fatal("closed controller accepted new sharing work")
	}
	policy.closeErr = nil
	if err := c.Close(); err != nil {
		t.Fatalf("cleanup retry failed: %v", err)
	}
	count := len(events)
	if err := c.Close(); err != nil || len(events) != count {
		t.Fatalf("completed cleanup was not idempotent: %v, %v", err, events)
	}
	// Normal policy mutation methods must not be called again after Close;
	// Linux policy rejects those methods once shutdown has begun.
	if got := strings.Join(events, ","); got != "offer:,dns:|,gateway:,policy:close,policy:close,policy:close" {
		t.Fatalf("unexpected retry sequence: %s", got)
	}
}

type fakeProbe struct {
	candidates      map[string]string
	uplink, gateway bool
	gateways        map[netip.Addr]bool
}

func (p *fakeProbe) Candidates(context.Context, Config) (map[string]string, error) {
	return p.candidates, nil
}
func (p *fakeProbe) Uplink(context.Context, string, string) bool { return p.uplink }
func (p *fakeProbe) Gateway(_ context.Context, _, remote netip.Addr) bool {
	if p.gateways != nil {
		return p.gateways[remote]
	}
	return p.gateway
}

func TestControllerUnreachablePreferredDonorDoesNotStarveAlternate(t *testing.T) {
	var events []string
	now := time.Now()
	preferred, alternate := policyDonorDevice(t), mustPolicyDevice(t, policyTenant, policyDonorLate)
	view, other := gatewayView(preferred, now), gatewayView(alternate, now)
	other.Routes[0].Link = 9
	view.Devices = append(view.Devices, other.Devices...)
	view.Routes = append(view.Routes, other.Routes...)
	node := &fakeNode{view: view, offers: []meshcatalog.Record{gatewayOffer(alternate, now), gatewayOffer(preferred, now)}, events: &events}
	probe := &fakeProbe{gateways: map[netip.Addr]bool{alternate.MeshAddress(): true}}
	c, err := NewWithDeps(policySelf(t), node, &fakePolicy{events: &events}, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true, ShareUplink: true}
	step := func(want meshid.Device) {
		t.Helper()
		events = nil
		d, err := c.Step(context.Background(), cfg)
		if err != nil || d.GatewayDevice != want || (want.IsValid() && d.Mode != "roaming") {
			t.Fatalf("got decision %+v, want donor %v: %v", d, want, err)
		}
		if slices.Contains(events, "offer:eth0") {
			t.Fatal("borrowed route was re-exported")
		}
	}
	step(meshid.Device{}) // Both candidates need two successful samples.
	step(alternate)       // The preferred offer and Babel route still exist.
	if !slices.Contains(events, "dns:wlmp9|"+alternate.MeshAddress().String()) {
		t.Fatalf("alternate's DNS path not selected: %v", events)
	}
	probe.gateways[preferred.MeshAddress()] = true
	step(alternate) // A newly recovered preferred donor does not reset the active tunnel.
	step(alternate) // Its health is now good; the active donor remains sticky.
	probe.gateways[alternate.MeshAddress()] = false
	step(alternate)
	step(alternate)
	step(preferred) // Three failed active-donor samples select the healthy alternate.
	// Withdrawal bypasses debounce even though this donor was most recently healthy.
	node.offers = []meshcatalog.Record{gatewayOffer(alternate, now)}
	step(meshid.Device{})
	if _, ok := c.gatewayHealth[preferred]; ok {
		t.Fatal("withdrawn donor retained health history")
	}
}

func TestControllerOfferRefreshDoesNotResetOtherDonorHealth(t *testing.T) {
	var events []string
	now := time.Now()
	a, b := policyDonorDevice(t), mustPolicyDevice(t, policyTenant, policyDonorLate)
	view, other := gatewayView(a, now), gatewayView(b, now)
	view.Devices = append(view.Devices, other.Devices...)
	view.Routes = append(view.Routes, other.Routes...)
	first, second := gatewayOffer(a, now), gatewayOffer(b, now)
	first.Expires -= 1000
	node := &fakeNode{view: view, offers: []meshcatalog.Record{first, second}, events: &events}
	c, err := NewWithDeps(policySelf(t), node, &fakePolicy{events: &events}, &fakeProbe{gateway: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true}
	if _, err := c.Step(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	node.offers[0].Expires += 1000 // Equal expiry now prefers the lower UUID.
	if d, err := c.Step(context.Background(), cfg); err != nil || d.Mode != "roaming" || d.GatewayDevice != a {
		t.Fatalf("offer ranking change discarded healthy alternate samples: %+v %v", d, err)
	}
}

type countingGatewayProbe struct {
	fakeProbe
	mu           sync.Mutex
	calls        []netip.Addr
	unbounded    bool
	wait         bool
	active, peak int
}

func (p *countingGatewayProbe) Gateway(ctx context.Context, local, remote netip.Addr) bool {
	p.mu.Lock()
	p.calls = append(p.calls, remote)
	p.active++
	p.peak = max(p.peak, p.active)
	deadline, bounded := ctx.Deadline()
	if !bounded || time.Until(deadline) > 3*time.Second {
		p.unbounded = true
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}()
	if p.wait {
		<-ctx.Done()
		return false
	}
	return p.fakeProbe.Gateway(ctx, local, remote)
}

func TestControllerBoundsGatewayWorkWithoutStarvingDistantDonor(t *testing.T) {
	var events []string
	now := time.Now()
	node := &fakeNode{events: &events}
	var last meshid.Device
	for i := range 20 {
		donor := mustPolicyDevice(t, policyTenant, fmt.Sprintf("22222222-3333-4444-8555-%012x", i+1))
		view := gatewayView(donor, now)
		node.view.Devices = append(node.view.Devices, view.Devices...)
		node.view.Routes = append(node.view.Routes, view.Routes...)
		node.offers = append(node.offers, gatewayOffer(donor, now))
		last = donor
	}
	probe := &countingGatewayProbe{fakeProbe: fakeProbe{gateways: map[netip.Addr]bool{last.MeshAddress(): true}}}
	c, err := NewWithDeps(policySelf(t), node, &fakePolicy{events: &events}, probe)
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[netip.Addr]bool)
	selected := false
	for range 10 {
		probe.calls = nil
		d, err := c.Step(context.Background(), Config{Participate: true, Roam: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(probe.calls) > 8 || probe.unbounded {
			t.Fatalf("unbounded gateway observation: calls=%d missing deadline=%v", len(probe.calls), probe.unbounded)
		}
		for _, address := range probe.calls {
			seen[address] = true
		}
		if d.GatewayDevice.IsValid() {
			if d.GatewayDevice != last {
				t.Fatalf("selected unreachable donor: %v", d.GatewayDevice)
			}
			selected = true
		} else if selected {
			t.Fatal("rotation stopped probing the current healthy donor")
		}
	}
	if !selected || len(seen) != 20 {
		t.Fatalf("rotation starved a candidate: selected=%v observed=%d/20", selected, len(seen))
	}
	// A stalled batch inherits an earlier caller deadline and does not launch
	// an unbounded number of ping subprocesses while waiting for it.
	probe.wait, probe.calls = true, nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _ = c.Step(ctx, Config{Participate: true, Roam: true})
	if time.Since(started) > time.Second || probe.peak > 4 || len(probe.calls) > 8 {
		t.Fatalf("observation ignored bound: duration=%s parallel=%d calls=%d", time.Since(started), probe.peak, len(probe.calls))
	}
}

func TestControllerWithdrawsSignedOfferBeforeNATAndOnUplinkLoss(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"wlan0": "1.1.1.1"}, uplink: true}
	controller, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true, ShareUplink: true}
	if _, err = controller.Step(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	events = nil
	decision, err := controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "local" || decision.ShareInterface != "wlan0" {
		t.Fatalf("independent local uplink not offered: %+v %v", decision, err)
	}
	if got := strings.Join(events, ","); got != "offer:,gateway:,dns:|,nat:wlan0|1.1.1.1,offer:wlan0" {
		t.Fatalf("offer preceded owned NAT/DNS setup: %s", got)
	}
	probe.candidates = nil // physical default disappeared; bypass debounce
	events = nil
	decision, err = controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "offline" {
		t.Fatalf("uplink loss not observed: %+v %v", decision, err)
	}
	if got := strings.Join(events, ","); got != "offer:,gateway:,dns:|,nat:|" {
		t.Fatalf("NAT removed before signed offer withdrawal: %s", got)
	}
	events = nil
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "offer:,dns:|,gateway:,policy:close" {
		t.Fatalf("unsafe close order: %s", got)
	}
}

func TestControllerRetriesFailedSharingWithdrawalOnNextStep(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"eth0": "1.1.1.1"}, uplink: true}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, ShareUplink: true}
	for range 2 {
		if _, err := c.Step(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	probe.candidates = nil
	policy.failWithdraw = 2 // initial withdrawal and immediate cleanup both fail
	events = nil
	if _, err := c.Step(ctx, cfg); err == nil || !c.shareCleanupPending {
		t.Fatalf("expected pending cleanup after two deletion failures: %v %v", events, err)
	}
	if got := strings.Join(events, ","); got != "offer:,gateway:,dns:|,nat:|,nat:|" {
		t.Fatalf("offer was not withdrawn before failed NAT cleanup: %s", got)
	}
	events = nil
	if _, err := c.Step(ctx, cfg); err != nil || c.shareCleanupPending {
		t.Fatalf("periodic step did not retry cleanup: %v %v", events, err)
	}
	if !slices.Contains(events, "nat:|") || slices.Contains(events, "offer:eth0") {
		t.Fatalf("retry skipped cleanup or reoffered uplink: %v", events)
	}
}

func TestControllerBorrowedInternetNeverBecomesDonor(t *testing.T) {
	ctx := context.Background()
	var events []string
	now := time.Now()
	donor := policyDonorDevice(t)
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"wlmp8": "1.1.1.1"}, uplink: true, gateway: true}
	controller, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true, ShareUplink: true}
	if _, err = controller.Step(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	events = nil
	decision, err := controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "roaming" || decision.GatewayDevice != donor || decision.DNSLink != 8 {
		t.Fatalf("signed donor not selected: %+v %v", decision, err)
	}
	if slices.Contains(events, "offer:wlmp8") || !slices.Contains(events, "gateway:"+donor.Principal()) ||
		!slices.Contains(events, "dns:wlmp8|"+donor.MeshAddress().String()) {
		t.Fatalf("borrowed path was re-shared or DNS not projected: %+v", events)
	}
	// Even a healthy ping cannot keep borrowing after the signed capability
	// disappears. Route and DNS must withdraw on this step.
	node.offers = nil
	events = nil
	decision, err = controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "offline" || !slices.Contains(events, "gateway:") || !slices.Contains(events, "dns:|") {
		t.Fatalf("signed donor loss retained roaming: %+v %+v %v", decision, events, err)
	}
}

func TestControllerNeverOffersBeforePolicyAndWithdrawsFailedProxy(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events, failShare: true}
	probe := &fakeProbe{candidates: map[string]string{"eth0": "10.201.0.1"}, uplink: true}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, ShareUplink: true}
	for range 2 {
		_, err = c.Step(ctx, cfg)
	}
	if err == nil || slices.Contains(events, "offer:eth0") {
		t.Fatalf("offered unhealthy DNS/NAT policy: %v %v", events, err)
	}
	policy.failShare = false
	events = nil
	if _, err = c.Step(ctx, cfg); err != nil || !slices.Contains(events, "offer:eth0") {
		t.Fatalf("did not recover prepared sharing: %v %v", events, err)
	}
	policy.failShare = true
	events = nil
	if _, err = c.Step(ctx, cfg); err == nil {
		t.Fatal("failed running DNS proxy was not detected")
	}
	if got := strings.Join(events, ","); got != "gateway:,dns:|,nat:eth0|10.201.0.1,offer:,nat:|" {
		t.Fatalf("failed policy did not withdraw signed offer: %s", got)
	}
}

func TestControllerDonorSwitchRepointsDNSWithoutAdvertising(t *testing.T) {
	ctx := context.Background()
	var events []string
	now := time.Now()
	donor, late := policyDonorDevice(t), mustPolicyDevice(t, policyTenant, policyDonorLate)
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	probe := &fakeProbe{gateway: true}
	policy := &fakePolicy{events: &events}
	c, err := NewWithDeps(policySelf(t), node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true, ShareUplink: true}
	for range 2 {
		if _, err := c.Step(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	view := gatewayView(late, time.Now())
	view.Routes[0].Link = 9
	node.view = view
	node.offers = []meshcatalog.Record{gatewayOffer(late, time.Now())}
	events = nil
	if d, err := c.Step(ctx, cfg); err != nil || d.Mode != "offline" {
		t.Fatalf("new donor skipped health confirmation: %+v %v", d, err)
	}
	if !slices.Contains(events, "gateway:") || !slices.Contains(events, "dns:|") {
		t.Fatalf("old donor route/DNS not removed: %v", events)
	}
	events = nil
	if d, err := c.Step(ctx, cfg); err != nil || d.Mode != "roaming" || d.GatewayDevice != late || d.DNSLink != 9 {
		t.Fatalf("new donor not selected: %+v %v", d, err)
	}
	if !slices.Contains(events, "dns:wlmp9|"+late.MeshAddress().String()) || slices.Contains(events, "offer:") {
		t.Fatalf("donor switch DNS/offer wrong: %v", events)
	}
}

// A BLE lease negotiation outlasts a step. The controller must report the
// selected donor without claiming Internet access, and must not withdraw the
// gateway: withdrawal cancels the negotiation, which is how every 15 s step
// used to throw away a slow handshake.
func TestControllerKeepsPendingGatewayLeaseAcrossSteps(t *testing.T) {
	ctx := context.Background()
	var events []string
	donor := policyDonorDevice(t)
	now := time.Now()
	node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
	policy := &fakePolicy{events: &events, pendingGateway: 2}
	c, err := NewWithDeps(policySelf(t), node, policy, &fakeProbe{gateway: true})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true}
	if _, err := c.Step(ctx, cfg); err != nil { // Health needs two samples.
		t.Fatal(err)
	}
	for range 2 {
		events = nil
		d, err := c.Step(ctx, cfg)
		if err != nil || d.Mode != "offline" || d.GatewayDevice != donor || !strings.Contains(d.Reason, "negotiating") {
			t.Fatalf("pending lease reported as %+v: %v", d, err)
		}
		if want := []string{"gateway:" + donor.Principal(), "dns:|"}; !slices.Equal(events, want) {
			t.Fatalf("pending lease was withdrawn: got %v, want %v", events, want)
		}
	}
	events = nil
	if d, err := c.Step(ctx, cfg); err != nil || d.Mode != "roaming" || d.GatewayDevice != donor {
		t.Fatalf("collected lease not used: %+v %v", d, err)
	}
	if want := []string{"gateway:" + donor.Principal(), "dns:wlmp8|" + donor.MeshAddress().String()}; !slices.Equal(events, want) {
		t.Fatalf("collected lease: got %v, want %v", events, want)
	}
}

func TestControllerGatewayFailureWithdrawsTunnelAndResolver(t *testing.T) {
	for _, failAt := range []string{"tunnel", "resolver"} {
		t.Run(failAt, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var events []string
			donor := policyDonorDevice(t)
			now := time.Now()
			node := &fakeNode{view: gatewayView(donor, now), offers: []meshcatalog.Record{gatewayOffer(donor, now)}, events: &events}
			policy := &fakePolicy{events: &events}
			c, err := NewWithDeps(policySelf(t), node, policy, &fakeProbe{gateway: true})
			if err != nil {
				t.Fatal(err)
			}
			cfg := Config{Participate: true, Roam: true}
			if _, err := c.Step(ctx, cfg); err != nil {
				t.Fatal(err)
			}
			events, policy.cleanupContexts = nil, nil
			policy.failGateway = failAt == "tunnel"
			policy.failDNS = failAt == "resolver"
			policy.cancelOnDNS = cancel
			if _, err := c.Step(ctx, cfg); err == nil {
				t.Fatal("partially installed gateway policy reported success")
			}
			want := []string{"gateway:" + donor.Principal()}
			if failAt == "resolver" {
				want = append(want, "dns:wlmp8|"+donor.MeshAddress().String())
			}
			want = append(want, "dns:|", "gateway:")
			if !slices.Equal(events, want) {
				t.Fatalf("failure retained borrowed state or advertised it: got %v, want %v", events, want)
			}
			if len(policy.cleanupContexts) != 2 {
				t.Fatalf("did not attempt both resolver and tunnel cleanup: %v", policy.cleanupContexts)
			}
			for _, err := range policy.cleanupContexts {
				if err != nil {
					t.Fatalf("request cancellation prevented cleanup: %v", err)
				}
			}
		})
	}
}
