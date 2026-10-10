// Package babel is a deterministic, sans-I/O Babel routing engine for explicit
// point-to-point links. It neither forwards IP packets nor performs network I/O.
// See docs/profile.md for the supported profile and integration obligations.
package babel

import (
	"errors"
	"net/netip"
	"time"
)

const Infinity uint16 = 65535

type RouterID uint64

// LinkID identifies an incarnation, not a reusable interface number. IDs must
// increase for each newly added link; this rejects stale packets after reconnect.
type LinkID uint64

type Config struct {
	RouterID RouterID
	// InitialSeqno must follow the embedding application's restart policy.
	InitialSeqno                                 uint16
	Seed                                         uint64
	HelloInterval, UpdateInterval, SourceGC      time.Duration
	MaxLinks, MaxRoutes, MaxSources, MaxRequests int
	// IPv4ViaIPv6 asserts the forwarding plane can handle RFC 9229 routes and ICMPv4.
	IPv4ViaIPv6 bool
	// AcceptRoute is an optional deterministic embedding-policy filter for
	// remote origin/prefix pairs. Nil accepts all routable prefixes. It is
	// consulted on admission and metric evaluation, so policy changes take
	// effect at the next Step. It must not call back into the engine. This is
	// authorization policy, not cryptographic authentication of Babel updates.
	AcceptRoute func(RouterID, netip.Prefix) bool `json:"-"`
}

type Link struct {
	ID          LinkID
	Local, Peer netip.Addr // unzoned IPv6 link-local; scope is ID
	Cost        uint16     // strictly positive receive cost
	MaxPacket   int        // Babel payload budget, >=512
	// IPv4 is optional, for ordinary IPv4 next-hop advertisements.
	IPv4 netip.Addr
}

// Event is a closed set. Time is supplied separately as elapsed monotonic time.
type Event interface{ babelEvent() }
type Tick struct{}
type AddLink struct{ Link Link }
type RemoveLink struct{ ID LinkID }
type SetCost struct {
	ID   LinkID
	Cost uint16
}
type Receive struct {
	ID     LinkID
	Packet []byte
}

// Originate means the embedding forwarding plane already owns this prefix.
type Originate struct {
	Prefix netip.Prefix
	Metric uint16
}
type Withdraw struct{ Prefix netip.Prefix }

func (Tick) babelEvent()       {}
func (AddLink) babelEvent()    {}
func (RemoveLink) babelEvent() {}
func (SetCost) babelEvent()    {}
func (Receive) babelEvent()    {}
func (Originate) babelEvent()  {}
func (Withdraw) babelEvent()   {}

type Route struct {
	Prefix        netip.Prefix
	RouterID      RouterID
	Seqno, Metric uint16
	Link          LinkID
	NextHop       netip.Addr
	Local         bool
	// Unreachable prohibits fallback to a covering route, including defaults.
	Unreachable bool
}
type Datagram struct {
	Link    LinkID
	Payload []byte
}
type Effects struct {
	// Nonzero Revision requires atomically applying the full Routes snapshot,
	// including drops, then calling Commit. Datagrams are withheld until success.
	Revision  uint64
	Routes    []Route
	Datagrams []Datagram
}
type Neighbour struct {
	Link      LinkID
	Cost      uint16
	Reachable bool
}
type Snapshot struct {
	Now                                  time.Duration
	Seqno                                uint16
	Routes                               []Route
	Neighbours                           []Neighbour
	Sources, Candidates, PendingRequests int
	Counters                             Counters
}
type Counters struct{ Received, Malformed, Stale, Rejected, Sent uint64 }

var (
	ErrConfig   = errors.New("invalid Babel configuration or event")
	ErrTime     = errors.New("monotonic time moved backwards")
	ErrPending  = errors.New("forwarding revision must be committed first")
	ErrRevision = errors.New("incorrect forwarding revision")
	ErrStopped  = errors.New("forwarding failed; engine stopped, discard all forwarding state")
	ErrLimit    = errors.New("Babel resource limit reached")
)

// New constructs an engine without consulting clocks, OS interfaces or entropy.
func New(c Config) (*Engine, error) {
	if c.RouterID == 0 || c.RouterID == RouterID(^uint64(0)) {
		return nil, ErrConfig
	}
	if c.HelloInterval == 0 {
		c.HelloInterval = 4 * time.Second
	}
	if c.UpdateInterval == 0 {
		c.UpdateInterval = 16 * time.Second
	}
	if c.SourceGC == 0 {
		c.SourceGC = 3 * time.Minute
	}
	for _, d := range []time.Duration{c.HelloInterval, c.UpdateInterval} {
		if d < 100*time.Millisecond || d > 65534*10*time.Millisecond || d%(10*time.Millisecond) != 0 {
			return nil, ErrConfig
		}
	}
	if c.SourceGC < 3*time.Minute || c.SourceGC < hold(c.UpdateInterval) || c.SourceGC > 24*time.Hour {
		return nil, ErrConfig
	}
	if c.HelloInterval > 32767*10*time.Millisecond {
		return nil, ErrConfig
	} // IHU interval is twice Hello
	if c.MaxLinks == 0 {
		c.MaxLinks = 64
	}
	if c.MaxRoutes == 0 {
		c.MaxRoutes = 8192
	}
	if c.MaxSources == 0 {
		c.MaxSources = 16384
	}
	if c.MaxRequests == 0 {
		c.MaxRequests = 1024
	}
	if c.MaxLinks < 1 || c.MaxRoutes < 1 || c.MaxSources < 1 || c.MaxRequests < 1 {
		return nil, ErrConfig
	}
	if c.Seed == 0 {
		c.Seed = uint64(c.RouterID)
	}
	return &Engine{cfg: c, seq: c.InitialSeqno, rng: c.Seed, links: map[LinkID]*linkState{}, routes: map[routeKey]*candidate{}, sources: map[sourceKey]*source{}, local: map[netip.Prefix]uint16{}, selected: map[netip.Prefix]Route{}, drops: map[netip.Prefix]time.Duration{}, requests: map[sourceKey]*request{}, out: map[LinkID][]wireMessage{}}, nil
}

func newer(a, b uint16) bool   { d := a - b; return d > 0 && d < 32768 }
func atLeast(a, b uint16) bool { return a == b || newer(a, b) }
func sum(a, b uint16) uint16 {
	n := uint32(a) + uint32(b)
	if n >= uint32(Infinity) {
		return Infinity
	}
	return uint16(n)
}
func hold(d time.Duration) time.Duration { return d * 7 / 2 }
func validPrefix(p netip.Prefix) bool {
	return p.IsValid() && p == p.Masked() && !p.Addr().Is4In6() && p.Addr().Zone() == ""
}

// Appendix C: scoped, multicast and loopback destinations are not mesh routes.
// Defaults remain valid even though their first address is unspecified.
func routablePrefix(p netip.Prefix) bool {
	a := p.Addr()
	return validPrefix(p) && !a.IsLinkLocalUnicast() && !a.IsMulticast() && !a.IsLoopback() && !(a.IsUnspecified() && p.Bits() == a.BitLen())
}
