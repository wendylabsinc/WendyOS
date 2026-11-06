package localmesh

import (
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Only automatically discovered Ethernet and infrastructure Wi-Fi links
// suppress radio discovery. Configured TCP peers have cost 256, and may need
// NAN or BLE as a second path when that TCP link fails.
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

// PeerSelection shares one diversity decision across the NAN and BLE
// providers. Discovery is only a hint; the snapshot contains only peers that
// passed device mTLS. Candidate hints expire quickly so stale advertisements
// cannot suppress a useful fallback indefinitely.
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

// Failed marks a discovery hint temporarily unavailable after a failed radio
// setup. Repeated advertisements do not reset this cooldown. The carrier may
// still retry it; the cooldown only stops a dead candidate from suppressing a
// useful second radio path to an already authenticated peer.
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
		if link.Device == asset && link.Cost <= MaxLANCost {
			return true
		}
	}
	return false
}

// AllowLAN admits eligible LAN candidates, preferring a cheaper carrier
// only for the same principal. Ranking unrelated peers at both ends can form
// a closed clique that permanently excludes another reachable device.
// The LAN carrier separately bounds concurrent connections and handshakes.
func (p *PeerSelection) AllowLAN(asset meshid.Device, cost uint16) bool {
	if p == nil || p.snapshot == nil {
		return true
	}
	if asset == (meshid.Device{}) || cost == 0 || cost > MaxLANCost {
		return false
	}
	for _, link := range p.snapshot().Links {
		if link.Device == asset && link.Cost < cost {
			return false
		}
	}
	return true
}

// AllowRadio preserves preferences between carriers reaching the same peer.
// Each provider independently limits outgoing attempts, outgoing connections
// and total connections. Other carriers do not consume that provider's budget.
func (p *PeerSelection) AllowRadio(asset meshid.Device, medium RadioMedium) bool {
	allow, _ := p.AllowRadioReason(asset, medium)
	return allow
}

// AllowRadioReason reports the same decision as AllowRadio plus a stable
// machine-readable reason naming the branch that allowed or vetoed the link.
// Veto reasons exist so a torn-down established link can be attributed to the
// exact clause that fired instead of hypothesizing after the fact.
func (p *PeerSelection) AllowRadioReason(asset meshid.Device, medium RadioMedium) (bool, string) {
	if p == nil || p.snapshot == nil {
		return true, "no-policy"
	}
	if asset == (meshid.Device{}) || (medium != RadioNAN && medium != RadioBLE) {
		return false, "invalid-asset-or-medium"
	}
	s := p.snapshot()
	connected := make(map[meshid.Device]bool, len(s.Links))
	otherRadio := false
	for _, link := range s.Links {
		connected[link.Device] = true
		if link.Device == asset && link.Cost <= MaxLANCost {
			return false, fmt.Sprintf("lan-supersedes(cost=%d)", link.Cost)
		}
		if link.Device == asset && ((medium == RadioNAN && link.Cost > 512) || (medium == RadioBLE && link.Cost == 512)) {
			otherRadio = true
		}
	}
	if !otherRadio {
		return true, "first-radio"
	}
	// Prefer NAN over BLE for the same principal. Both providers recheck this
	// policy periodically: vetoing both duplicates would tear down every path
	// to a retained neighbor. BLE is shed only after NAN is authenticated.
	if medium == RadioNAN {
		return true, "preferred-radio"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for key, hint := range p.seen {
		if now.Sub(hint.seen) > 60*time.Second && !now.Before(hint.failedUntil) {
			delete(p.seen, key)
			continue
		}
		// Only a candidate for this transport can use the slot we free.
		// In particular, unverified NAN presence must not displace a BLE
		// fallback: the transports have independent connection budgets.
		if key.medium == medium && key.asset != asset && !connected[key.asset] && !now.Before(hint.failedUntil) {
			return false, fmt.Sprintf("second-radio-fresh-candidate(principal=%s)", key.asset.Principal())
		}
	}
	return true, "second-radio-failover-kept"
}
