package ipcam

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

// managerHarness wires a LinkManager whose every side effect is captured, so the
// scan logic is exercised with no sockets and no real interfaces.
type managerHarness struct {
	mgr *LinkManager
	reg *Registry

	mu        sync.Mutex
	ifaces    []Interface
	added     []string // "link cidr" for each address configured
	removed   []string // "link cidr" for each address removed
	watched   []string
	served    []string
	addErr    error
	onPacket  map[string]func(*Packet)
	onLease   map[string]func(net.HardwareAddr, net.IP, string)
	nowValue  time.Time
	segments  map[string]CameraSegment
	poolsSeen map[string]*LeasePool
}

func newManagerHarness(t *testing.T, ifaces []Interface) *managerHarness {
	t.Helper()
	reg := NewRegistry(filepath.Join(t.TempDir(), "cameras.json"))
	if err := reg.Load(); err != nil {
		t.Fatalf("registry load: %v", err)
	}
	h := &managerHarness{
		reg:       reg,
		ifaces:    ifaces,
		onPacket:  make(map[string]func(*Packet)),
		onLease:   make(map[string]func(net.HardwareAddr, net.IP, string)),
		nowValue:  time.Now(),
		segments:  make(map[string]CameraSegment),
		poolsSeen: make(map[string]*LeasePool),
	}
	m := NewLinkManager(reg, zap.NewNop())
	m.listInterfaces = func() ([]Interface, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]Interface(nil), h.ifaces...), nil
	}
	m.now = func() time.Time {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.nowValue
	}
	// Configuring an address mutates the fake interface list, so a claimed link
	// reports the address it was given. Without that the manager cannot tell an
	// address it configured from one that has been stripped out from under it.
	m.addAddress = func(link, cidr string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.addErr != nil {
			return h.addErr
		}
		h.added = append(h.added, link+" "+cidr)
		h.setAddressLocked(link, cidr, true)
		return nil
	}
	m.delAddress = func(link, cidr string) error {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.removed = append(h.removed, link+" "+cidr)
		h.setAddressLocked(link, cidr, false)
		return nil
	}
	m.watch = func(ctx context.Context, link string, onPacket func(*Packet)) error {
		h.mu.Lock()
		h.watched = append(h.watched, link)
		h.onPacket[link] = onPacket
		h.mu.Unlock()
		<-ctx.Done()
		return nil
	}
	m.serve = func(ctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error {
		h.mu.Lock()
		h.served = append(h.served, link)
		h.segments[link] = seg
		h.poolsSeen[link] = pool
		h.onLease[link] = onLease
		h.mu.Unlock()
		<-ctx.Done()
		return nil
	}
	h.mgr = m
	for _, iface := range ifaces {
		m.allowedDHCP[iface.Name] = true
	}
	return h
}

func TestManagerRequiresExplicitDHCPOptIn(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	allowed := false
	h.mgr.readAllowlist = func(string) (map[string]bool, error) {
		if allowed {
			return map[string]bool{"eth0": true}, nil
		}
		return map[string]bool{}, nil
	}
	if err := h.mgr.LoadDHCPAllowlist("camera-dhcp-interfaces"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be passively watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	if len(h.servedLinks()) != 0 || len(h.addedAddresses()) != 0 {
		t.Fatal("addressless uplink DHCP traffic started an unconfigured camera server")
	}

	allowed = true
	h.mgr.scanOnce(ctx)
	waitFor(t, "opted-in camera link to be served", func() bool { return len(h.servedLinks()) == 1 })
	if !h.mgr.isClaimed("eth0") {
		t.Fatal("opted-in link was not claimed")
	}

	allowed = false
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth0") || len(h.removedAddresses()) != 1 {
		t.Fatal("removing the opt-in did not withdraw the camera server and address")
	}
	allowed = true
	h.feed("eth0", &Packet{Type: Discover, XID: 2})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "re-enabled camera link to be served", func() bool { return len(h.servedLinks()) == 2 })
}

func TestManagerDoesNotRestoreCameraClaimWithoutOptIn(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	if _, err := h.reg.Upsert(Camera{MAC: "02:00:00:00:00:01", Link: "eth0", Address: "10.98.0.50"}); err != nil {
		t.Fatal(err)
	}
	h.mgr.allowedDHCP = map[string]bool{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be passively watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	if h.mgr.isClaimed("eth0") || len(h.addedAddresses()) != 0 {
		t.Fatal("restored a camera DHCP claim without an explicit opt-in")
	}
}

// setAddressLocked adds or removes an address on the fake interface, mirroring
// what `ip addr add/del` would do. Callers hold h.mu.
func (h *managerHarness) setAddressLocked(link, cidr string, present bool) {
	ip, _, err := net.ParseCIDR(cidr)
	if err != nil {
		return
	}
	for i := range h.ifaces {
		if h.ifaces[i].Name != link {
			continue
		}
		var kept []net.IP
		for _, have := range h.ifaces[i].IPv4s {
			if !have.Equal(ip) {
				kept = append(kept, have)
			}
		}
		if present {
			kept = append(kept, ip.To4())
		}
		h.ifaces[i].IPv4s = kept
		h.ifaces[i].HasIPv4 = len(kept) > 0
	}
}

// stripAddress removes an address behind the manager's back, which is what a
// host network manager reconciling the interface does.
func (h *managerHarness) stripAddress(link, cidr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setAddressLocked(link, cidr, false)
}

func (h *managerHarness) addForeignAddress(link, cidr string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setAddressLocked(link, cidr, true)
}

// waitFor polls until cond holds, so tests do not depend on goroutine timing.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *managerHarness) watchedLinks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.watched...)
}

func (h *managerHarness) servedLinks() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.served...)
}

func (h *managerHarness) addedAddresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.added...)
}

func (h *managerHarness) removedAddresses() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.removed...)
}

// claimEth0 drives a harness through the full claim path and returns once the
// link is being served.
func claimEth0(t *testing.T, h *managerHarness, ctx context.Context) {
	t.Helper()
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) >= 1 })
}

// carrierDown drops carrier on a link while leaving the address we configured in
// place, which is the real post-blip state.
func (h *managerHarness) carrierDown(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.ifaces {
		if h.ifaces[i].Name == name {
			h.ifaces[i].Carrier = false
		}
	}
}

func (h *managerHarness) feed(link string, p *Packet) {
	h.mu.Lock()
	fn := h.onPacket[link]
	h.mu.Unlock()
	if fn != nil {
		fn(p)
	}
}

func (h *managerHarness) advance(d time.Duration) {
	h.mu.Lock()
	h.nowValue = h.nowValue.Add(d)
	h.mu.Unlock()
}

func cabledEth(name string) Interface {
	return Interface{Name: name, Carrier: true, Up: true,
		MAC: net.HardwareAddr{0xac, 0x3a, 0xe2, 0x12, 0x3c, 0x23}}
}

// The uplink has an address, so it must never be watched, let alone served.
func TestManagerIgnoresIneligibleLinks(t *testing.T) {
	h := newManagerHarness(t, []Interface{
		{Name: "wlan0", Carrier: true, Up: true, HasIPv4: true},
		{Name: "lo", Carrier: true, Up: true, Loopback: true},
		{Name: "eth1", Carrier: false, Up: true},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	if got := h.watchedLinks(); len(got) != 0 {
		t.Fatalf("watched %v, want nothing", got)
	}
	if got := h.servedLinks(); len(got) != 0 {
		t.Fatalf("served %v, want nothing", got)
	}
}

// A cabled, addressless link is watched, but nothing is served until a DISCOVER
// goes unanswered.
func TestManagerWatchesButDoesNotServeWithoutDiscover(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })

	h.advance(time.Hour)
	h.mgr.scanOnce(ctx)
	if got := h.servedLinks(); len(got) != 0 {
		t.Fatalf("served %v with no DHCP traffic at all", got)
	}
	if got := h.addedAddresses(); len(got) != 0 {
		t.Fatalf("configured %v with no DHCP traffic at all", got)
	}
}

// The plug-and-play path: camera asks, nobody answers, the agent takes the link.
func TestManagerClaimsAfterUnansweredDiscover(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })

	h.feed("eth0", &Packet{Type: Discover, XID: 1,
		CHAddr: net.HardwareAddr{0xec, 0x71, 0xdb, 0x2a, 0xae, 0x7e}, Hostname: "RLC-520A"})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })
	if got := h.addedAddresses(); len(got) != 1 || got[0] != "eth0 10.98.0.1/24" {
		t.Fatalf("configured %v, want eth0 10.98.0.1/24", got)
	}
	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want serving", got)
	}

	// The DISCOVER alone should have registered the camera, so it is listable as
	// known-but-offline before it has an address.
	cams := h.reg.List()
	if len(cams) != 1 {
		t.Fatalf("registry has %d cameras, want 1", len(cams))
	}
	if cams[0].MAC != "ec:71:db:2a:ae:7e" {
		t.Fatalf("MAC = %q", cams[0].MAC)
	}
	if cams[0].Model != "RLC-520A" {
		t.Fatalf("model = %q, want the DHCP hostname", cams[0].Model)
	}
	if cams[0].Link != "eth0" {
		t.Fatalf("link = %q", cams[0].Link)
	}
}

// The guard that matters most: another DHCP server on the link means we never
// serve there, however long we wait.
func TestManagerNeverServesWhereAnotherServerAnswers(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })

	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.feed("eth0", &Packet{Type: Offer, XID: 1, ServerID: net.IPv4(192, 168, 0, 1)})
	h.advance(time.Hour)
	h.mgr.scanOnce(ctx)

	if got := h.servedLinks(); len(got) != 0 {
		t.Fatalf("served %v on a link with an upstream DHCP server", got)
	}
	if got := h.addedAddresses(); len(got) != 0 {
		t.Fatalf("configured %v on a link with an upstream DHCP server", got)
	}
	if got := h.mgr.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
}

// A competing server can appear after our unanswered window has elapsed (for
// example, a slow upstream DHCP server). The manager must stop answering and
// remove its address, not merely change the diagnostic guard state.
func TestManagerWithdrawsWhenAnotherServerAnswersAfterClaim(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stopped := make(chan struct{}, 1)
	h.mgr.serve = func(sctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error {
		h.mu.Lock()
		h.served = append(h.served, link)
		h.segments[link] = seg
		h.poolsSeen[link] = pool
		h.onLease[link] = onLease
		h.mu.Unlock()
		<-sctx.Done()
		stopped <- struct{}{}
		return nil
	}

	claimEth0(t, h, ctx)
	h.feed("eth0", &Packet{Type: Offer, XID: 1, ServerID: net.IPv4(192, 168, 0, 1)})

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the DHCP server kept running after a foreign offer")
	}
	if got := h.mgr.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
	if h.mgr.isClaimed("eth0") {
		t.Fatal("link remained claimed after a foreign offer")
	}
	if got := h.removedAddresses(); len(got) != 1 || got[0] != "eth0 10.98.0.1/24" {
		t.Fatalf("removed %v, want the claimed address", got)
	}

	// Later camera traffic must not resurrect the disqualified link while the
	// same cable remains connected.
	h.feed("eth0", &Packet{Type: Discover, XID: 2})
	h.advance(time.Hour)
	h.mgr.scanOnce(ctx)
	if got := h.servedLinks(); len(got) != 1 {
		t.Fatalf("served %v, want no server after disqualification", got)
	}
}

// A leased camera reaches the registry with its address, which is what makes it
// streamable.
func TestManagerRecordsLease(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })

	h.mu.Lock()
	onLease := h.onLease["eth0"]
	h.mu.Unlock()
	mac := net.HardwareAddr{0xec, 0x71, 0xdb, 0x2a, 0xae, 0x7e}
	onLease(mac, net.IPv4(10, 98, 0, 50), "RLC-520A")

	cams := h.reg.List()
	if len(cams) != 1 {
		t.Fatalf("registry has %d cameras, want 1", len(cams))
	}
	if cams[0].Address != "10.98.0.50" {
		t.Fatalf("address = %q", cams[0].Address)
	}
	if cams[0].ID != IDBandStart {
		t.Fatalf("id = %d, want %d", cams[0].ID, IDBandStart)
	}
}

// If the address cannot be configured we cannot answer, so the link is
// disqualified rather than left half-claimed and silently broken.
func TestManagerDisqualifiesWhenAddressFails(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	h.mu.Lock()
	h.addErr = errors.New("permission denied")
	h.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	if got := h.servedLinks(); len(got) != 0 {
		t.Fatalf("served %v despite failing to configure the address", got)
	}
	if got := h.mgr.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
}

// Two camera links must get different subnets, or their traffic collides.
func TestManagerSecondLinkGetsItsOwnSegment(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "both links to be watched", func() bool { return len(h.watchedLinks()) == 2 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.feed("eth1", &Packet{Type: Discover, XID: 2})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	waitFor(t, "both links to be served", func() bool { return len(h.servedLinks()) == 2 })
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.segments["eth0"].ServerIP.Equal(h.segments["eth1"].ServerIP) {
		t.Fatalf("both links got %v", h.segments["eth0"].ServerIP)
	}
}

// Releasing a lower-numbered link leaves a hole. Allocation must find that
// unused subnet rather than using len(claimed), which would duplicate the still
// active higher-numbered segment.
func TestManagerReusesReleasedSegmentWithoutCollision(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0"), cabledEth("eth1")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "both initial links to be watched", func() bool { return len(h.watchedLinks()) == 2 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.feed("eth1", &Packet{Type: Discover, XID: 2})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "both initial links to be served", func() bool { return len(h.servedLinks()) == 2 })

	h.mgr.release("eth0")
	eth1 := cabledEth("eth1")
	eth1.HasIPv4 = true
	h.mu.Lock()
	h.ifaces = []Interface{eth1, cabledEth("eth2")}
	h.mu.Unlock()
	h.mgr.allowedDHCP["eth2"] = true
	h.mgr.scanOnce(ctx)
	waitFor(t, "replacement link to be watched", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.onPacket["eth2"] != nil
	})
	h.feed("eth2", &Packet{Type: Discover, XID: 3})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "replacement link to be served", func() bool { return len(h.servedLinks()) == 3 })

	h.mu.Lock()
	eth1Segment := h.segments["eth1"]
	eth2Segment := h.segments["eth2"]
	h.mu.Unlock()
	if !eth1Segment.ServerIP.Equal(net.IPv4(10, 98, 1, 1)) {
		t.Fatalf("eth1 server = %v, want 10.98.1.1", eth1Segment.ServerIP)
	}
	if !eth2Segment.ServerIP.Equal(net.IPv4(10, 98, 0, 1)) {
		t.Fatalf("eth2 server = %v, want released 10.98.0.1", eth2Segment.ServerIP)
	}
	if eth1Segment.ServerIP.Equal(eth2Segment.ServerIP) {
		t.Fatalf("active links share server address %v", eth1Segment.ServerIP)
	}
}

// Claiming a link puts our address on it, which makes the link fail the very
// eligibility test that got it claimed. It must stay claimed regardless, or the
// next scan releases it and the next DISCOVER re-claims it, leaking a second
// server every cycle.
func TestManagerKeepsClaimedLinkAfterItGetsAnAddress(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })

	// Claiming configured the address, so the link now reports one and therefore
	// fails the eligibility test that got it claimed. It must stay claimed anyway.
	h.mu.Lock()
	reportsAddress := h.ifaces[0].HasIPv4 && h.ifaces[0].HasAddress(SegmentFor(0).ServerIP)
	h.mu.Unlock()
	if !reportsAddress {
		t.Fatal("claimed link does not report the address that was configured on it")
	}

	h.advance(scanInterval)
	h.mgr.scanOnce(ctx)
	h.advance(scanInterval)
	h.mgr.scanOnce(ctx)

	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want still serving after the address was configured", got)
	}
	if got := h.servedLinks(); len(got) != 1 {
		t.Fatalf("served %v, want exactly one server for the link", got)
	}
	if got := h.addedAddresses(); len(got) != 1 {
		t.Fatalf("configured %v, want the address added once", got)
	}
}

// A link can acquire an uplink address after the camera server has claimed it.
// Keep the carrier connected, as on an Ethernet switch, and ensure the camera
// DHCP server yields to the network manager without waiting for a foreign OFFER.
func TestManagerStopsCameraDHCPWhenUplinkAddressAppears(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)

	h.addForeignAddress("eth0", "192.168.2.3/24")
	h.mgr.scanOnce(ctx)
	if got := h.mgr.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
	if h.mgr.isClaimed("eth0") {
		t.Fatal("camera DHCP server remains claimed on an uplink")
	}
	if got := h.removedAddresses(); len(got) != 1 || got[0] != "eth0 10.98.0.1/24" {
		t.Fatalf("removed addresses = %v, want only the camera address", got)
	}
	h.mu.Lock()
	foreignStillPresent := h.ifaces[0].HasAddress(net.ParseIP("192.168.2.3"))
	h.mu.Unlock()
	if !foreignStillPresent {
		t.Fatal("withdrawing the camera segment removed the uplink address")
	}
	h.mgr.scanOnce(ctx)
	if got := h.servedLinks(); len(got) != 1 {
		t.Fatalf("camera DHCP restarted on the uplink: %v", got)
	}
}

// A link that loses carrier is forgotten, so the next cable is judged afresh
// rather than inheriting a stale decision.
func TestManagerReleasesLinkThatLosesCarrier(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })

	// Carrier loss is debounced, so the release needs the full run of scans.
	h.carrierDown("eth0")
	for range carrierDownScans {
		h.mgr.scanOnce(ctx)
	}

	if got := h.mgr.State("eth0"); got != LinkObserving {
		t.Fatalf("state = %v, want observing after the cable was pulled", got)
	}
}

func TestManagerLeasesDiagnostic(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Discover, XID: 1})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })

	h.mu.Lock()
	pool := h.poolsSeen["eth0"]
	h.mu.Unlock()
	if _, err := pool.Lease(net.HardwareAddr{1, 2, 3, 4, 5, 6}, nil); err != nil {
		t.Fatalf("lease: %v", err)
	}
	got := h.mgr.Leases()
	if len(got["eth0"]) != 1 {
		t.Fatalf("Leases() = %v", got)
	}
}

// A camera rebooting drops carrier for a few seconds. Tearing the segment down for
// that would churn the address and the lease, so a single missed scan is ignored.
func TestManagerToleratesBriefCarrierLoss(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)

	h.carrierDown("eth0")
	h.mgr.scanOnce(ctx) // one scan without carrier

	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want still serving after a single blip", got)
	}
	if got := h.removedAddresses(); len(got) != 0 {
		t.Fatalf("removed %v after a single blip", got)
	}

	// Carrier returns before the second scan: the counter must reset, so a later
	// single blip is tolerated again.
	h.mu.Lock()
	withAddress := cabledEth("eth0")
	withAddress.HasIPv4 = true
	h.ifaces = []Interface{withAddress}
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	h.carrierDown("eth0")
	h.mgr.scanOnce(ctx)
	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want serving; the carrier counter did not reset", got)
	}
}

// A cable really pulled out must release the link AND remove the address. Leaving
// the address behind would keep the link ineligible forever, so the segment could
// never be re-established and the camera could never renew.
func TestManagerReleaseRemovesTheAddress(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)

	h.carrierDown("eth0")
	for i := 0; i < carrierDownScans; i++ {
		h.mgr.scanOnce(ctx)
	}

	if got := h.mgr.State("eth0"); got != LinkObserving {
		t.Fatalf("state = %v, want observing after a sustained carrier loss", got)
	}
	if got := h.removedAddresses(); len(got) != 1 || got[0] != "eth0 10.98.0.1/24" {
		t.Fatalf("removed %v, want the configured address to be removed", got)
	}
}

// Releasing must stop the link's DHCP server, or every claim leaves another one
// running and two servers answer the same camera.
func TestManagerReleaseStopsTheServer(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan struct{}, 4)
	stopped := make(chan struct{}, 4)
	h.mgr.serve = func(sctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error {
		h.mu.Lock()
		h.served = append(h.served, link)
		h.mu.Unlock()
		served <- struct{}{}
		<-sctx.Done()
		stopped <- struct{}{}
		return nil
	}

	claimEth0(t, h, ctx)
	<-served

	h.carrierDown("eth0")
	for i := 0; i < carrierDownScans; i++ {
		h.mgr.scanOnce(ctx)
	}

	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("the link server kept running after the link was released")
	}
}

// A server startup/runtime failure must not leave the address and claim wedged
// forever. Cleanup makes the next client request eligible for a fresh attempt.
func TestManagerRetriesAfterDHCPServerFailure(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h.mgr.serve = func(sctx context.Context, link string, seg CameraSegment, pool *LeasePool, onLease func(net.HardwareAddr, net.IP, string)) error {
		h.mu.Lock()
		h.served = append(h.served, link)
		h.segments[link] = seg
		h.poolsSeen[link] = pool
		h.onLease[link] = onLease
		attempt := len(h.served)
		h.mu.Unlock()
		if attempt == 1 {
			return errors.New("socket failed")
		}
		<-sctx.Done()
		return nil
	}

	claimEth0(t, h, ctx)
	waitFor(t, "failed server claim to be cleaned up", func() bool {
		return !h.mgr.isClaimed("eth0") && len(h.removedAddresses()) == 1
	})
	if got := h.mgr.State("eth0"); got != LinkObserving {
		t.Fatalf("state = %v, want observing after server failure", got)
	}

	// Time alone is insufficient: a fresh request is required before retrying.
	h.advance(time.Hour)
	h.mgr.scanOnce(ctx)
	if got := h.servedLinks(); len(got) != 1 {
		t.Fatalf("served %v without a fresh request", got)
	}

	h.feed("eth0", &Packet{Type: Discover, XID: 2})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)
	waitFor(t, "DHCP server to be retried", func() bool { return len(h.servedLinks()) == 2 })
	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want serving after retry", got)
	}
	if got := h.addedAddresses(); len(got) != 2 {
		t.Fatalf("configured %v, want one address per attempt", got)
	}
}

// After a release the link is unconfigured again, so a fresh DISCOVER re-claims it
// and the camera recovers on its own.
func TestManagerReclaimsAfterRelease(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)

	h.carrierDown("eth0")
	for i := 0; i < carrierDownScans; i++ {
		h.mgr.scanOnce(ctx)
	}

	// The cable is back and, because release removed our address, the link is
	// eligible again. The original watcher is still running and still feeding the
	// guard, so no second watcher is needed.
	h.mu.Lock()
	h.ifaces = []Interface{cabledEth("eth0")}
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	h.feed("eth0", &Packet{Type: Discover, XID: 2})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	waitFor(t, "eth0 to be served again", func() bool { return len(h.servedLinks()) == 2 })
	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want serving after re-claim", got)
	}
}

// The bug this guards: whatever else manages the interface can strip the address
// we added, and a claimed link is otherwise judged on carrier alone. DHCP keeps
// working on an addressless link — the watcher is a packet socket and the server
// binds INADDR_ANY — so leases go on being handed out while every route to the
// camera is gone, and nothing ever notices.
func TestManagerRestoresStrippedAddressOnClaimedLink(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)

	segment := SegmentFor(0).CIDR()
	if got := h.addedAddresses(); len(got) != 1 {
		t.Fatalf("configured %v, want one address for the claim", got)
	}

	// Scanning a healthy claimed link must not touch the address.
	h.mgr.scanOnce(ctx)
	if got := h.addedAddresses(); len(got) != 1 {
		t.Fatalf("configured %v, want no re-add while the address is present", got)
	}

	h.stripAddress("eth0", segment)
	h.mgr.scanOnce(ctx)

	if got := h.addedAddresses(); len(got) != 2 || got[1] != "eth0 "+segment {
		t.Fatalf("configured %v, want the stripped address restored", got)
	}
	if got := h.removedAddresses(); len(got) != 0 {
		t.Fatalf("removed %v, want the claim left intact", got)
	}
	if !h.mgr.isClaimed("eth0") {
		t.Fatal("eth0 was released, want the claim kept across an address loss")
	}
	// Restored means restored: the next scan sees it present and leaves it alone.
	h.mgr.scanOnce(ctx)
	if got := h.addedAddresses(); len(got) != 2 {
		t.Fatalf("configured %v, want no further re-add", got)
	}
}

// A failed restore is retried rather than disqualifying the link, because the
// usual cause is transient and disqualifying would strand the segment until
// carrier is lost.
func TestManagerRetriesFailedAddressRestore(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimEth0(t, h, ctx)

	segment := SegmentFor(0).CIDR()
	h.stripAddress("eth0", segment)
	h.mu.Lock()
	h.addErr = errors.New("device busy")
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)

	if got := h.mgr.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want serving after a failed restore", got)
	}

	h.mu.Lock()
	h.addErr = nil
	h.mu.Unlock()
	h.mgr.scanOnce(ctx)
	if got := h.addedAddresses(); len(got) != 2 {
		t.Fatalf("configured %v, want the restore retried on the next scan", got)
	}
}

// LinkManager state is in-process, so a restart forgets every claim while the
// registry remembers the camera. A camera holding a lease says nothing for up to
// LeaseWindow, so waiting for DHCP traffic leaves it unreachable for twelve
// hours. The claim is restored from the registry instead.
func TestManagerRestoresClaimFromRegistryAfterRestart(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := h.reg.Upsert(Camera{
		MAC:     "ec:71:db:2a:ae:7e",
		Address: "10.98.0.50",
		Link:    "eth0",
		Model:   "RLC-520A",
	}); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}

	// First scan only starts listening: restoring immediately would skip the
	// window in which a competing DHCP server gets to disqualify the link.
	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	if got := h.servedLinks(); len(got) != 0 {
		t.Fatalf("served %v before listening for a competing server", got)
	}

	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })
	if got := h.addedAddresses(); len(got) != 1 || got[0] != "eth0 "+SegmentFor(0).CIDR() {
		t.Fatalf("configured %v, want the segment the camera already holds a lease on", got)
	}
}

// A restored claim must keep the camera's existing subnet, not whichever segment
// happens to be free, or the lease the camera holds would be off-subnet.
func TestManagerRestoredClaimKeepsCameraSegment(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := h.reg.Upsert(Camera{
		MAC:     "ec:71:db:2a:ae:7e",
		Address: "10.98.3.50",
		Link:    "eth0",
	}); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	waitFor(t, "eth0 to be served", func() bool { return len(h.servedLinks()) == 1 })
	if got := h.addedAddresses(); len(got) != 1 || got[0] != "eth0 "+SegmentFor(3).CIDR() {
		t.Fatalf("configured %v, want 10.98.3.1/24 to match the camera's lease", got)
	}
}

// The competing-server guard outranks a restore: a foreign OFFER seen while we
// are listening means this link is not ours, whatever the registry remembers.
func TestManagerDoesNotRestoreClaimOnDisqualifiedLink(t *testing.T) {
	h := newManagerHarness(t, []Interface{cabledEth("eth0")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := h.reg.Upsert(Camera{
		MAC:     "ec:71:db:2a:ae:7e",
		Address: "10.98.0.50",
		Link:    "eth0",
	}); err != nil {
		t.Fatalf("seeding registry: %v", err)
	}

	h.mgr.scanOnce(ctx)
	waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
	h.feed("eth0", &Packet{Type: Offer, XID: 1, ServerID: net.IPv4(192, 168, 1, 1)})
	h.advance(unansweredWindow)
	h.mgr.scanOnce(ctx)

	if got := h.servedLinks(); len(got) != 0 {
		t.Fatalf("served %v on a link another DHCP server owns", got)
	}
	if got := h.addedAddresses(); len(got) != 0 {
		t.Fatalf("configured %v on a disqualified link", got)
	}
}

// A registry entry for a different link, or an address outside the camera
// subnets, is not evidence that this link was ever ours.
func TestManagerDoesNotRestoreClaimWithoutMatchingCamera(t *testing.T) {
	for _, tc := range []struct {
		name string
		cam  Camera
	}{
		{"another link", Camera{MAC: "ec:71:db:2a:ae:01", Address: "10.98.0.50", Link: "eth1"}},
		{"foreign subnet", Camera{MAC: "ec:71:db:2a:ae:02", Address: "192.168.2.69", Link: "eth0"}},
		{"no address", Camera{MAC: "ec:71:db:2a:ae:03", Link: "eth0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newManagerHarness(t, []Interface{cabledEth("eth0")})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if _, err := h.reg.Upsert(tc.cam); err != nil {
				t.Fatalf("seeding registry: %v", err)
			}
			h.mgr.scanOnce(ctx)
			waitFor(t, "eth0 to be watched", func() bool { return len(h.watchedLinks()) == 1 })
			h.advance(unansweredWindow)
			h.mgr.scanOnce(ctx)

			if got := h.servedLinks(); len(got) != 0 {
				t.Fatalf("served %v without evidence the link was ours", got)
			}
		})
	}
}
