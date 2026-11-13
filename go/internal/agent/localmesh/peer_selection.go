package localmesh

import (
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Automatically discovered Ethernet and infrastructure Wi-Fi use this cost
// range. Configured TCP peers have cost 256. Different media may coexist as
// warm standbys within their independent provider budgets.
const MaxLANCost uint16 = 128

type RadioMedium uint8

const (
	RadioNAN RadioMedium = iota + 1
	RadioBLE
)

type radioCandidate struct {
	seen        time.Time
	failedUntil time.Time
}

type radioCandidateKey struct {
	asset  meshid.Device
	medium RadioMedium
}

// PeerSelection shares authenticated presence and carrier eligibility across
// providers. Their owned connection policies independently enforce capacity and
// reclaim safe redundancy. Discovery remains a bounded scheduling hint.
type PeerSelection struct {
	snapshot func() NodeSnapshot
	mu       sync.Mutex
	seen     map[radioCandidateKey]radioCandidate
	now      func() time.Time
}

func NewPeerSelection(snapshot func() NodeSnapshot) *PeerSelection {
	return &PeerSelection{snapshot: snapshot, seen: make(map[radioCandidateKey]radioCandidate), now: time.Now}
}

// Discovery can contain arbitrarily many unauthenticated NAN principals. This
// cache is only a scheduling hint; it must stay bounded even if radio selection
// never reaches the branch that considers redundant links.
const maxRadioCandidates = 256

func (p *PeerSelection) rememberCandidate(asset meshid.Device, medium RadioMedium, update func(*radioCandidate, time.Time)) {
	if p == nil || asset == (meshid.Device{}) || (medium != RadioNAN && medium != RadioBLE) {
		return
	}
	// Fetch the authenticated snapshot outside the hint lock. Cache eviction
	// never changes that connection state or discards a live peer's record.
	connected := make(map[meshid.Device]bool)
	if p.snapshot != nil {
		for _, link := range p.snapshot().Links {
			connected[link.Device] = true
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for key, hint := range p.seen {
		if !connected[key.asset] && now.Sub(hint.seen) > 60*time.Second && !now.Before(hint.failedUntil) {
			delete(p.seen, key)
		}
	}
	key := radioCandidateKey{asset, medium}
	if _, exists := p.seen[key]; !exists && len(p.seen) >= maxRadioCandidates {
		var oldest radioCandidateKey
		var oldestTime time.Time
		found := false
		for candidate, hint := range p.seen {
			if connected[candidate.asset] || now.Before(hint.failedUntil) {
				continue
			}
			before := candidate.asset.Principal() < oldest.asset.Principal() || (candidate.asset == oldest.asset && candidate.medium < oldest.medium)
			if !found || hint.seen.Before(oldestTime) || (hint.seen.Equal(oldestTime) && before) {
				oldest, oldestTime, found = candidate, hint.seen, true
			}
		}
		if !found {
			return // retain live peers and outstanding retry cooldowns
		}
		delete(p.seen, oldest)
	}
	hint := p.seen[key]
	update(&hint, now)
	p.seen[key] = hint
}

func (p *PeerSelection) Seen(asset meshid.Device, medium RadioMedium) {
	p.rememberCandidate(asset, medium, func(hint *radioCandidate, now time.Time) { hint.seen = now })
}

// Failed retains bounded retry metadata for a discovery hint. Repeated
// advertisements do not reset its cooldown. Providers independently apply their
// existing queue and retry bounds before requesting owned capacity reclamation.
func (p *PeerSelection) Failed(asset meshid.Device, medium RadioMedium) {
	p.rememberCandidate(asset, medium, func(hint *radioCandidate, now time.Time) { hint.failedUntil = now.Add(30 * time.Second) })
}

func (p *PeerSelection) Connected(asset meshid.Device, medium RadioMedium) {
	p.rememberCandidate(asset, medium, func(hint *radioCandidate, now time.Time) {
		hint.seen = now
		hint.failedUntil = time.Time{}
	})
}

func (p *PeerSelection) HasLAN(asset meshid.Device) bool {
	if p == nil || p.snapshot == nil {
		return false
	}
	for _, link := range p.snapshot().Links {
		if link.Device == asset && link.Cost <= MaxLANCost && link.ForwardingReady {
			return true
		}
	}
	return false
}

// AllowLAN admits eligible discovered LAN candidates. The provider queue ranks
// new direct peers before upgrades and redundancy, and its owned policy bounds
// concurrent connections and handshakes.
func (p *PeerSelection) AllowLAN(asset meshid.Device, cost uint16) bool {
	if p == nil || p.snapshot == nil {
		return true
	}
	if asset == (meshid.Device{}) || cost == 0 || cost > MaxLANCost {
		return false
	}
	return true
}

// AllowRadio admits valid media without eagerly pruning a useful standby.
// Each provider independently limits outgoing attempts, outgoing connections
// and total connections, reclaiming only its own safe redundant incarnations.
func (p *PeerSelection) AllowRadio(asset meshid.Device, medium RadioMedium) bool {
	allow, _ := p.AllowRadioReason(asset, medium)
	return allow
}

// AllowRadioReason reports the same decision as AllowRadio plus a stable
// machine-readable reason naming the branch that allowed or vetoed the link.
// Capacity reclamation belongs to the exact owned lease, not to this eligibility
// predicate or to unauthenticated discovery state.
func (p *PeerSelection) AllowRadioReason(asset meshid.Device, medium RadioMedium) (bool, string) {
	if p == nil || p.snapshot == nil {
		return true, "no-policy"
	}
	if asset == (meshid.Device{}) || (medium != RadioNAN && medium != RadioBLE) {
		return false, "invalid-asset-or-medium"
	}
	// Providers preserve useful cross-medium standbys while they have spare
	// capacity. Their owned connection policy reclaims only a slower redundant
	// incarnation when a runnable new direct peer needs that same budget.
	return true, "standby-capacity-managed"
}

// Snapshot returns the immutable authenticated state used by provider-local
// schedulers. Discovery hints remain separate from this routing evidence.
func (p *PeerSelection) Snapshot() NodeSnapshot {
	if p == nil || p.snapshot == nil {
		return NodeSnapshot{}
	}
	return p.snapshot()
}
