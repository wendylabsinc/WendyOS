package ipcam

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

// A camera cabled straight into the device gets no address, because that segment
// has no DHCP server on it. This file decides whether the agent is allowed to be
// that server on a given link, and it is deliberately conservative: handing out
// leases on a network that already has a server would be disruptive, so the
// default is to do nothing.
//
// A link becomes eligible only when all of these hold:
//
//  1. It has carrier and is administratively up.
//  2. It holds no IPv4 address of its own, which is what excludes the uplink.
//  3. A DHCP DISCOVER was seen on it that nothing answered within a timeout.
//
// And a link is permanently disqualified the moment an OFFER from any other
// server is seen on it.

// LinkState is the phase of a link's decision.
type LinkState int

const (
	// LinkObserving means we are watching and have served nothing.
	LinkObserving LinkState = iota
	// LinkServing means we claimed the link and hand out leases on it.
	LinkServing
	// LinkDisqualified means another DHCP server answered here, so we never will.
	LinkDisqualified
)

func (s LinkState) String() string {
	switch s {
	case LinkObserving:
		return "observing"
	case LinkServing:
		return "serving"
	case LinkDisqualified:
		return "disqualified"
	default:
		return "unknown"
	}
}

// unansweredWindow is how long a DISCOVER must go unanswered before we conclude
// the link has no DHCP server. A real server answers in milliseconds; ten seconds
// spans several client retries without being an annoying wait.
const unansweredWindow = 10 * time.Second

// LeaseWindow is how long leases we hand out are valid.
const LeaseWindow = 12 * time.Hour

// Interface is the subset of a network interface the decision needs.
type Interface struct {
	Name      string
	MAC       net.HardwareAddr
	Carrier   bool
	Up        bool
	HasIPv4   bool
	IPv4s     []net.IP
	Loopback  bool
	PointToPo bool // point-to-point, e.g. a tunnel; never a camera link
}

// HasAddress reports whether ip is currently configured on the link.
//
// HasIPv4 is not enough to answer this: a claimed link that has had our segment
// address stripped by whatever else manages the interface can still hold some
// other IPv4 address, and treating that as "still configured" is exactly how the
// camera segment goes silently dead.
func (i Interface) HasAddress(ip net.IP) bool {
	for _, have := range i.IPv4s {
		if have.Equal(ip) {
			return true
		}
	}
	return false
}

// HasOtherIPv4 reports whether another network manager has configured this
// link. A camera segment must yield when an uplink address appears after the
// camera DHCP server started; carrier can stay up through that transition.
func (i Interface) HasOtherIPv4(own net.IP) bool {
	for _, have := range i.IPv4s {
		if !have.Equal(own) {
			return true
		}
	}
	return false
}

// Eligible reports whether a link could host a camera segment. This is the check
// that keeps the agent off the uplink and off anything that already works.
func (i Interface) Eligible() bool {
	if i.Loopback || i.PointToPo {
		return false
	}
	if !i.Carrier || !i.Up {
		return false
	}
	// An interface with an address is either the uplink or already configured by
	// something that knows better than us.
	if i.HasIPv4 {
		return false
	}
	// Wireless interfaces are never a directly-cabled camera, and claiming one
	// would put the agent on someone's home network.
	return !isWireless(i.Name)
}

// isWireless reports whether a name looks like a wireless interface. Linux
// predictable names use a "wl" prefix; the legacy names are wlanN and wifiN.
func isWireless(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"wl", "wlan", "wifi", "ath", "ra"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// linkDecision is the per-link guard state.
type linkDecision struct {
	state LinkState

	// pendingSince is when the first currently-unanswered DISCOVER was seen.
	// Zero means nothing is pending.
	pendingSince time.Time
	pendingXID   uint32
}

// LinkGuard decides, per link, whether the agent may serve DHCP.
//
// It is pure state: callers feed it observed packets and the clock, and it
// answers. That keeps the whole policy testable without a socket.
type LinkGuard struct {
	mu sync.Mutex
	by map[string]*linkDecision

	// serverIDs are addresses this agent serves from. An OFFER carrying one of
	// them is our own and must not disqualify the link.
	serverIDs map[string]bool
}

// NewLinkGuard returns an empty guard.
func NewLinkGuard() *LinkGuard {
	return &LinkGuard{
		by:        make(map[string]*linkDecision),
		serverIDs: make(map[string]bool),
	}
}

// RegisterServerID records an address the agent serves from, so its own offers are
// not mistaken for a competing server.
func (g *LinkGuard) RegisterServerID(ip net.IP) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.serverIDs[ip.String()] = true
}

// State returns the current decision for a link.
func (g *LinkGuard) State(link string) LinkState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.decision(link).state
}

// decision returns the per-link state, creating it on first use. Callers hold mu.
func (g *LinkGuard) decision(link string) *linkDecision {
	d, ok := g.by[link]
	if !ok {
		d = &linkDecision{}
		g.by[link] = d
	}
	return d
}

// Observe feeds a DHCP packet seen on a link into the decision.
//
// A DISCOVER starts the clock. An OFFER or ACK from another server stops the
// agent from ever serving here. Our own offers are ignored.
func (g *LinkGuard) Observe(link string, p *Packet, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.decision(link)
	if d.state == LinkDisqualified {
		return
	}

	switch p.Type {
	case Discover, Request:
		// A REQUEST counts as well as a DISCOVER. A camera we leased before an
		// agent restart renews with a REQUEST addressed to a server that is no
		// longer listening; without this the link sits idle until the camera
		// eventually gives up and falls back to DISCOVER, which can take hours.
		if d.pendingSince.IsZero() {
			d.pendingSince = now
			d.pendingXID = p.XID
		}
	case Offer, Ack, Nak:
		// A reply we did not send means another DHCP server owns this link.
		if p.ServerID != nil && g.serverIDs[p.ServerID.String()] {
			return
		}
		d.state = LinkDisqualified
		d.pendingSince = time.Time{}
	}
}

// ShouldClaim reports whether the agent may now start serving on a link, and
// records that it has. It returns false unless a DISCOVER has gone unanswered for
// unansweredWindow.
func (g *LinkGuard) ShouldClaim(link string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.decision(link)
	if d.state != LinkObserving {
		return false
	}
	if d.pendingSince.IsZero() {
		return false
	}
	if now.Sub(d.pendingSince) < unansweredWindow {
		return false
	}
	d.state = LinkServing
	return true
}

// RestoreClaim moves a link straight to serving, for a segment this agent is
// known to have served before an restart. It records the claim the same way
// ShouldClaim does and refuses for the same reasons.
//
// The unanswered-DISCOVER window is not required here because the evidence it
// gathers has already been gathered: the link was claimed in an earlier process
// and a camera holds a lease on it. Waiting for the camera to speak again would
// mean waiting up to LeaseWindow, during which the segment has no address and
// the camera is unreachable. Callers still listen for a competing server first;
// a disqualifying reply seen in that window leaves the state non-Observing and
// this returns false.
func (g *LinkGuard) RestoreClaim(link string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.decision(link)
	if d.state != LinkObserving {
		return false
	}
	d.state = LinkServing
	return true
}

// Disqualify marks a link as never servable, for reasons outside DHCP traffic
// such as failing to configure an address on it.
func (g *LinkGuard) Disqualify(link string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.decision(link).state = LinkDisqualified
}

// Reset forgets a link, which is what happens when its carrier drops: the next
// cable might be something entirely different.
func (g *LinkGuard) Reset(link string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.by, link)
}

// ResetServing forgets a failed serving decision only if no competing DHCP
// server has disqualified the link in the meantime. It returns whether the
// serving state was reset.
func (g *LinkGuard) ResetServing(link string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.decision(link)
	if d.state != LinkServing {
		return false
	}
	delete(g.by, link)
	return true
}

// CameraSegment is the addressing for a claimed camera link.
//
// 10.98.x is chosen over 192.168.x because the latter very often collides with
// whatever network is on the other side of the uplink, and a collision here would
// send camera traffic out of the wrong interface.
type CameraSegment struct {
	ServerIP  net.IP
	Mask      net.IPMask
	Broadcast net.IP
	PoolBase  net.IP
	PoolSize  int
}

// SegmentFor returns the addressing for the nth claimed camera link.
func SegmentFor(index int) CameraSegment {
	third := byte(index)
	return CameraSegment{
		ServerIP:  net.IPv4(10, 98, third, 1),
		Mask:      net.CIDRMask(24, 32),
		Broadcast: net.IPv4(10, 98, third, 255),
		PoolBase:  net.IPv4(10, 98, third, 50),
		PoolSize:  100,
	}
}

// SegmentIndexFor returns the camera segment an address belongs to, and whether
// it belongs to one at all. It is how a lease recorded in the registry names the
// segment that must be restored after a restart, rather than whichever segment
// happens to be free.
func SegmentIndexFor(ip net.IP) (int, bool) {
	v4 := ip.To4()
	if v4 == nil || v4[0] != 10 || v4[1] != 98 {
		return 0, false
	}
	return int(v4[2]), true
}

// CIDR renders the server address in the form `ip addr add` expects.
func (s CameraSegment) CIDR() string {
	ones, _ := s.Mask.Size()
	return fmt.Sprintf("%s/%d", s.ServerIP.String(), ones)
}

// InterfacesFrom converts Go's interface list into the decision input. sysfs
// supplies carrier, which net.Interface does not expose.
func InterfacesFrom(ifaces []net.Interface, carrier func(name string) bool) []Interface {
	out := make([]Interface, 0, len(ifaces))
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		var ipv4s []net.IP
		if err == nil {
			for _, addr := range addrs {
				if ipnet, ok := addr.(*net.IPNet); ok {
					if v4 := ipnet.IP.To4(); v4 != nil {
						ipv4s = append(ipv4s, v4)
					}
				}
			}
		}
		out = append(out, Interface{
			Name:      iface.Name,
			MAC:       iface.HardwareAddr,
			Carrier:   carrier(iface.Name),
			Up:        iface.Flags&net.FlagUp != 0,
			HasIPv4:   len(ipv4s) > 0,
			IPv4s:     ipv4s,
			Loopback:  iface.Flags&net.FlagLoopback != 0,
			PointToPo: iface.Flags&net.FlagPointToPoint != 0,
		})
	}
	return out
}
