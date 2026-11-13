package ipcam

import (
	"net"
	"testing"
	"time"
)

func TestInterfaceEligible(t *testing.T) {
	base := Interface{Name: "eth0", Carrier: true, Up: true}
	if !base.Eligible() {
		t.Fatal("a cabled ethernet interface with no address should be eligible")
	}

	cases := map[string]Interface{
		// The uplink always has an address; this is the guard that excludes it.
		"has an address":  {Name: "eth0", Carrier: true, Up: true, HasIPv4: true},
		"no carrier":      {Name: "eth0", Carrier: false, Up: true},
		"down":            {Name: "eth0", Carrier: true, Up: false},
		"loopback":        {Name: "lo", Carrier: true, Up: true, Loopback: true},
		"point to point":  {Name: "tun0", Carrier: true, Up: true, PointToPo: true},
		"wireless wlan0":  {Name: "wlan0", Carrier: true, Up: true},
		"wireless wlp3s0": {Name: "wlp3s0", Carrier: true, Up: true},
	}
	for name, iface := range cases {
		t.Run(name, func(t *testing.T) {
			if iface.Eligible() {
				t.Fatalf("%s must not be eligible", name)
			}
		})
	}
}

// The core guarantee: without an unanswered DISCOVER, the agent serves nothing.
func TestGuardDoesNotClaimWithoutDiscover(t *testing.T) {
	g := NewLinkGuard()
	now := time.Now()
	if g.ShouldClaim("eth0", now.Add(time.Hour)) {
		t.Fatal("claimed a link with no DHCP traffic at all")
	}
	if got := g.State("eth0"); got != LinkObserving {
		t.Fatalf("state = %v, want observing", got)
	}
}

func TestGuardClaimsAfterUnansweredDiscover(t *testing.T) {
	g := NewLinkGuard()
	start := time.Now()
	g.Observe("eth0", &Packet{Type: Discover, XID: 1}, start)

	// Too early: a real server might still answer.
	if g.ShouldClaim("eth0", start.Add(unansweredWindow-time.Second)) {
		t.Fatal("claimed before the window elapsed")
	}
	if !g.ShouldClaim("eth0", start.Add(unansweredWindow)) {
		t.Fatal("did not claim after an unanswered DISCOVER")
	}
	if got := g.State("eth0"); got != LinkServing {
		t.Fatalf("state = %v, want serving", got)
	}
	// Claiming twice would start a second server on the same link.
	if g.ShouldClaim("eth0", start.Add(time.Hour)) {
		t.Fatal("claimed the same link twice")
	}
}

// This is the guard that keeps the agent off a managed network: one OFFER from
// somebody else and it never serves here.
func TestGuardDisqualifiedByForeignOffer(t *testing.T) {
	g := NewLinkGuard()
	start := time.Now()
	g.Observe("eth0", &Packet{Type: Discover, XID: 1}, start)
	g.Observe("eth0", &Packet{
		Type:     Offer,
		XID:      1,
		ServerID: net.IPv4(192, 168, 0, 1),
	}, start.Add(time.Millisecond*20))

	if got := g.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
	if g.ShouldClaim("eth0", start.Add(time.Hour)) {
		t.Fatal("claimed a link where another DHCP server answered")
	}
}

// An ACK or NAK from another server is equally disqualifying: it means a server
// is live even if we missed its OFFER.
func TestGuardDisqualifiedByForeignAckOrNak(t *testing.T) {
	for _, kind := range []MessageType{Ack, Nak} {
		t.Run(kind.String(), func(t *testing.T) {
			g := NewLinkGuard()
			now := time.Now()
			g.Observe("eth0", &Packet{Type: Discover}, now)
			g.Observe("eth0", &Packet{Type: kind, ServerID: net.IPv4(192, 168, 0, 1)}, now)
			if got := g.State("eth0"); got != LinkDisqualified {
				t.Fatalf("state = %v, want disqualified", got)
			}
		})
	}
}

// The server identifier is an address, not proof of which host sent a reply.
// The packet watcher must filter our outgoing packets before feeding the guard.
func TestGuardDisqualifiesCompetingServerWithSameAddress(t *testing.T) {
	g := NewLinkGuard()
	ours := net.IPv4(10, 98, 0, 1)

	start := time.Now()
	g.Observe("eth0", &Packet{Type: Discover}, start)
	if !g.ShouldClaim("eth0", start.Add(unansweredWindow)) {
		t.Fatal("did not claim")
	}
	g.Observe("eth0", &Packet{Type: Offer, ServerID: ours}, start.Add(unansweredWindow+time.Second))

	if got := g.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified by an incoming reply even with our server address", got)
	}
}

// An OFFER with no server identifier cannot be attributed to us, so it counts as
// somebody else's. Failing closed is the right direction for this guard.
func TestGuardDisqualifiedByAnonymousOffer(t *testing.T) {
	g := NewLinkGuard()
	now := time.Now()
	g.Observe("eth0", &Packet{Type: Offer}, now)
	if got := g.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
}

// Disqualification is permanent: later DISCOVERs must not resurrect the link.
func TestGuardDisqualificationIsSticky(t *testing.T) {
	g := NewLinkGuard()
	now := time.Now()
	g.Observe("eth0", &Packet{Type: Offer, ServerID: net.IPv4(192, 168, 0, 1)}, now)
	g.Observe("eth0", &Packet{Type: Discover}, now.Add(time.Minute))
	if g.ShouldClaim("eth0", now.Add(time.Hour)) {
		t.Fatal("a later DISCOVER resurrected a disqualified link")
	}
}

// Links are independent: a managed network on one must not stop a camera segment
// on another.
func TestGuardIsPerLink(t *testing.T) {
	g := NewLinkGuard()
	start := time.Now()
	g.Observe("eth0", &Packet{Type: Offer, ServerID: net.IPv4(192, 168, 0, 1)}, start)
	g.Observe("eth1", &Packet{Type: Discover}, start)

	if got := g.State("eth0"); got != LinkDisqualified {
		t.Fatalf("eth0 state = %v, want disqualified", got)
	}
	if !g.ShouldClaim("eth1", start.Add(unansweredWindow)) {
		t.Fatal("eth1 was not claimable")
	}
}

// A dropped cable means the next one could be anything, so the decision resets.
func TestGuardResetForgetsLink(t *testing.T) {
	g := NewLinkGuard()
	now := time.Now()
	g.Observe("eth0", &Packet{Type: Offer, ServerID: net.IPv4(192, 168, 0, 1)}, now)
	g.Reset("eth0")
	if got := g.State("eth0"); got != LinkObserving {
		t.Fatalf("state = %v, want observing after reset", got)
	}
}

func TestGuardResetServingPreservesConcurrentDisqualification(t *testing.T) {
	g := NewLinkGuard()
	now := time.Now()
	g.Observe("eth0", &Packet{Type: Discover}, now)
	if !g.ShouldClaim("eth0", now.Add(unansweredWindow)) {
		t.Fatal("did not enter serving state")
	}
	g.Observe("eth0", &Packet{Type: Offer, ServerID: net.IPv4(192, 168, 0, 1)}, now)

	if g.ResetServing("eth0") {
		t.Fatal("reset a link that had been disqualified")
	}
	if got := g.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
}

func TestGuardExplicitDisqualify(t *testing.T) {
	g := NewLinkGuard()
	g.Disqualify("eth0")
	if got := g.State("eth0"); got != LinkDisqualified {
		t.Fatalf("state = %v, want disqualified", got)
	}
}

func TestLinkStateString(t *testing.T) {
	for state, want := range map[LinkState]string{
		LinkObserving:    "observing",
		LinkServing:      "serving",
		LinkDisqualified: "disqualified",
		LinkState(99):    "unknown",
	} {
		if got := state.String(); got != want {
			t.Fatalf("%d.String() = %q, want %q", state, got, want)
		}
	}
}

func TestSegmentFor(t *testing.T) {
	first := SegmentFor(0)
	if !first.ServerIP.Equal(net.IPv4(10, 98, 0, 1)) {
		t.Fatalf("server ip = %v", first.ServerIP)
	}
	if first.CIDR() != "10.98.0.1/24" {
		t.Fatalf("cidr = %q", first.CIDR())
	}
	if !first.PoolBase.Equal(net.IPv4(10, 98, 0, 50)) {
		t.Fatalf("pool base = %v", first.PoolBase)
	}

	// A second camera link must not overlap the first.
	second := SegmentFor(1)
	if second.ServerIP.Equal(first.ServerIP) {
		t.Fatal("two segments share a server address")
	}
	if second.CIDR() != "10.98.1.1/24" {
		t.Fatalf("second cidr = %q", second.CIDR())
	}
}

// 192.168.x collides with home and office networks often enough that using it for
// the camera segment would send camera traffic out of the wrong interface.
func TestSegmentAvoids192168(t *testing.T) {
	for i := 0; i < 4; i++ {
		if got := SegmentFor(i).ServerIP.String(); !isTenNet(got) {
			t.Fatalf("segment %d server ip = %s, want a 10.x address", i, got)
		}
	}
}

func isTenNet(addr string) bool {
	ip := net.ParseIP(addr).To4()
	return ip != nil && ip[0] == 10
}

func TestInterfacesFrom(t *testing.T) {
	ifaces := []net.Interface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		{Name: "eth0", Flags: net.FlagUp},
	}
	got := InterfacesFrom(ifaces, func(name string) bool { return name == "eth0" })
	if len(got) != 2 {
		t.Fatalf("got %d interfaces, want 2", len(got))
	}
	if !got[0].Loopback {
		t.Fatal("loopback flag lost")
	}
	if !got[1].Carrier || !got[1].Up {
		t.Fatalf("eth0 = %+v, want carrier and up", got[1])
	}
	if got[0].Carrier {
		t.Fatal("carrier reported for lo")
	}
}

// A camera we leased before an agent restart renews with a REQUEST, not a
// DISCOVER. Ignoring that would leave the link idle for hours until the camera
// gave up and started over.
func TestGuardClaimsAfterUnansweredRequest(t *testing.T) {
	g := NewLinkGuard()
	start := time.Now()
	g.Observe("eth0", &Packet{Type: Request, XID: 7}, start)

	if g.ShouldClaim("eth0", start.Add(unansweredWindow-time.Second)) {
		t.Fatal("claimed before the window elapsed")
	}
	if !g.ShouldClaim("eth0", start.Add(unansweredWindow)) {
		t.Fatal("did not claim after an unanswered REQUEST")
	}
}

// A REQUEST on a link where another server is live must still be ignored.
func TestGuardRequestDoesNotOverrideDisqualification(t *testing.T) {
	g := NewLinkGuard()
	now := time.Now()
	g.Observe("eth0", &Packet{Type: Offer, ServerID: net.IPv4(192, 168, 0, 1)}, now)
	g.Observe("eth0", &Packet{Type: Request, XID: 7}, now.Add(time.Second))
	if g.ShouldClaim("eth0", now.Add(time.Hour)) {
		t.Fatal("a REQUEST resurrected a disqualified link")
	}
}
