package meshsharing

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
)

type fakeNode struct {
	view     localmesh.NodeSnapshot
	offers   []meshcatalog.Record
	events   *[]string
	failRoam bool
}

func (n *fakeNode) Snapshot() localmesh.NodeSnapshot    { return n.view }
func (n *fakeNode) GatewayOffers() []meshcatalog.Record { return n.offers }
func (n *fakeNode) SetUplink(_ context.Context, iface string) error {
	*n.events = append(*n.events, "offer:"+iface)
	return nil
}
func (n *fakeNode) SetRoaming(_ context.Context, enabled bool) error {
	if n.failRoam {
		return errors.New("simulated route failure")
	}
	if enabled {
		*n.events = append(*n.events, "roam:on")
	} else {
		*n.events = append(*n.events, "roam:off")
	}
	return nil
}

func TestDisableWithdrawsOfferEvenIfRouteCleanupFails(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"wlan0": "1.1.1.1"}, uplink: true}
	c, err := NewWithDeps(64, 445, node, &fakePolicy{events: &events}, probe)
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
	node.failRoam = true
	if _, err := c.Step(ctx, Config{}); err == nil || len(events) != 1 || events[0] != "offer:" {
		t.Fatalf("signed offer not withdrawn before failed cleanup: %+v %v", events, err)
	}
	node.failRoam = false
	events = nil
	if _, err := c.Step(ctx, Config{}); err != nil || !slices.Contains(events, "nat:|") {
		t.Fatalf("disable could not retry policy cleanup: %+v %v", events, err)
	}
}

type fakePolicy struct {
	events       *[]string
	failShare    bool
	failWithdraw int
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
func (p *fakePolicy) SetDNS(_ context.Context, iface, address string) error {
	*p.events = append(*p.events, "dns:"+iface+"|"+address)
	return nil
}
func (p *fakePolicy) Close() error { *p.events = append(*p.events, "policy:close"); return nil }

type fakeProbe struct {
	candidates      map[string]string
	uplink, gateway bool
}

func (p *fakeProbe) Candidates(context.Context, Config) (map[string]string, error) {
	return p.candidates, nil
}
func (p *fakeProbe) Uplink(context.Context, string, string) bool          { return p.uplink }
func (p *fakeProbe) Gateway(context.Context, netip.Addr, netip.Addr) bool { return p.gateway }

func TestControllerWithdrawsSignedOfferBeforeNATAndOnUplinkLoss(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"wlan0": "1.1.1.1"}, uplink: true}
	controller, err := NewWithDeps(64, 445, node, policy, probe)
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
	if got := strings.Join(events, ","); got != "offer:,roam:off,dns:|,nat:wlan0|1.1.1.1,offer:wlan0" {
		t.Fatalf("offer preceded owned NAT/DNS setup: %s", got)
	}
	probe.candidates = nil // physical default disappeared; bypass debounce
	events = nil
	decision, err = controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "offline" {
		t.Fatalf("uplink loss not observed: %+v %v", decision, err)
	}
	if got := strings.Join(events, ","); got != "offer:,roam:off,dns:|,nat:|" {
		t.Fatalf("NAT removed before signed offer withdrawal: %s", got)
	}
	events = nil
	if err := controller.Close(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "roam:off,offer:,policy:close" {
		t.Fatalf("unsafe close order: %s", got)
	}
}

func TestControllerRetriesFailedSharingWithdrawalOnNextStep(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"eth0": "1.1.1.1"}, uplink: true}
	c, err := NewWithDeps(64, 445, node, policy, probe)
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
	if got := strings.Join(events, ","); got != "offer:,roam:off,dns:|,nat:|,nat:|" {
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
	node := &fakeNode{view: gatewayView(now), offers: []meshcatalog.Record{gatewayOffer(460, now)}, events: &events}
	policy := &fakePolicy{events: &events}
	probe := &fakeProbe{candidates: map[string]string{"wlmp8": "1.1.1.1"}, uplink: true, gateway: true}
	controller, err := NewWithDeps(64, 445, node, policy, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true, ShareUplink: true}
	if _, err = controller.Step(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	events = nil
	decision, err := controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "roaming" || decision.GatewayAsset != 460 || decision.DNSLink != 8 {
		t.Fatalf("signed donor not selected: %+v %v", decision, err)
	}
	if slices.Contains(events, "offer:wlmp8") || !slices.Contains(events, "roam:on") || !slices.Contains(events, "dns:wlmp8|10.88.1.204") {
		t.Fatalf("borrowed path was re-shared or DNS not projected: %+v", events)
	}
	// Even a healthy ping cannot keep borrowing after the signed capability
	// disappears. Route and DNS must withdraw on this step.
	node.view.Devices[0].Internet = false
	events = nil
	decision, err = controller.Step(ctx, cfg)
	if err != nil || decision.Mode != "offline" || !slices.Contains(events, "roam:off") || !slices.Contains(events, "dns:|") {
		t.Fatalf("signed donor loss retained roaming: %+v %+v %v", decision, events, err)
	}
}

func TestControllerNeverOffersBeforePolicyAndWithdrawsFailedProxy(t *testing.T) {
	ctx := context.Background()
	var events []string
	node := &fakeNode{events: &events}
	policy := &fakePolicy{events: &events, failShare: true}
	probe := &fakeProbe{candidates: map[string]string{"eth0": "10.201.0.1"}, uplink: true}
	c, err := NewWithDeps(64, 445, node, policy, probe)
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
	if got := strings.Join(events, ","); got != "roam:off,dns:|,nat:eth0|10.201.0.1,offer:,nat:|" {
		t.Fatalf("failed policy did not withdraw signed offer: %s", got)
	}
}

func TestControllerDonorSwitchRepointsDNSWithoutAdvertising(t *testing.T) {
	ctx := context.Background()
	var events []string
	now := time.Now()
	node := &fakeNode{view: gatewayView(now), offers: []meshcatalog.Record{gatewayOffer(460, now)}, events: &events}
	probe := &fakeProbe{gateway: true}
	c, err := NewWithDeps(64, 445, node, &fakePolicy{events: &events}, probe)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Participate: true, Roam: true, ShareUplink: true}
	for range 2 {
		if _, err := c.Step(ctx, cfg); err != nil {
			t.Fatal(err)
		}
	}
	view := gatewayView(time.Now())
	id, _ := localmesh.RouterID(64, 461)
	ip, _, _ := localmesh.Addresses(64, 461)
	view.Devices[0].Asset = 461
	view.Routes[0].RouterID = id
	view.Routes[0].Link = 9
	view.Routes[1].RouterID = id
	view.Routes[1].Prefix = netip.PrefixFrom(ip, 32)
	view.Routes[1].Link = 9
	node.view = view
	node.offers = []meshcatalog.Record{gatewayOffer(461, time.Now())}
	events = nil
	if d, err := c.Step(ctx, cfg); err != nil || d.Mode != "offline" {
		t.Fatalf("new donor skipped health confirmation: %+v %v", d, err)
	}
	if !slices.Contains(events, "roam:off") || !slices.Contains(events, "dns:|") {
		t.Fatalf("old donor route/DNS not removed: %v", events)
	}
	events = nil
	if d, err := c.Step(ctx, cfg); err != nil || d.Mode != "roaming" || d.GatewayAsset != 461 {
		t.Fatalf("new donor not selected: %+v %v", d, err)
	}
	if !slices.Contains(events, "dns:wlmp9|10.88.1.205") || slices.Contains(events, "offer:eth0") {
		t.Fatalf("donor switch DNS/offer wrong: %v", events)
	}
}
