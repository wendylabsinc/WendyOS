package localmesh

import (
	"fmt"
	"sort"
	"sync"
	"time"
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
	asset  int32
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

func (p *PeerSelection) Seen(asset int32, medium RadioMedium) {
	if p == nil || asset <= 0 || (medium != RadioNAN && medium != RadioBLE) {
		return
	}
	p.mu.Lock()
	key := radioCandidateKey{asset, medium}
	hint := p.seen[key]
	hint.seen = p.now()
	p.seen[key] = hint
	p.mu.Unlock()
}

// Failed marks a discovery hint temporarily unavailable after a failed radio
// setup. Repeated advertisements do not reset this cooldown. The carrier may
// still retry it; the cooldown only stops a dead candidate from suppressing a
// useful second radio path to an already authenticated peer.
func (p *PeerSelection) Failed(asset int32, medium RadioMedium) {
	if p == nil || asset <= 0 || (medium != RadioNAN && medium != RadioBLE) {
		return
	}
	p.mu.Lock()
	key := radioCandidateKey{asset, medium}
	hint := p.seen[key]
	hint.failedUntil = p.now().Add(30 * time.Second)
	p.seen[key] = hint
	p.mu.Unlock()
}

func (p *PeerSelection) Connected(asset int32, medium RadioMedium) {
	if p == nil || asset <= 0 || (medium != RadioNAN && medium != RadioBLE) {
		return
	}
	p.mu.Lock()
	key := radioCandidateKey{asset, medium}
	hint := p.seen[key]
	hint.seen = p.now()
	hint.failedUntil = time.Time{}
	p.seen[key] = hint
	p.mu.Unlock()
}

func (p *PeerSelection) HasLAN(asset int32) bool {
	if p == nil || p.snapshot == nil {
		return false
	}
	for _, link := range p.snapshot().Links {
		if link.Asset == asset && link.Cost <= MaxLANCost {
			return true
		}
	}
	return false
}

// AllowLAN ranks direct LAN neighbors by their authenticated link cost, then
// asset ID. A newly discovered wired peer may replace a Wi-Fi peer even when
// all three LAN slots are occupied. Radio links do not consume these slots:
// once the LAN link authenticates, AllowRadio sheds the excess radio link.
func (p *PeerSelection) AllowLAN(asset int32, cost uint16) bool {
	if p == nil || p.snapshot == nil {
		return true
	}
	if asset <= 0 || cost == 0 || cost > MaxLANCost {
		return false
	}
	best := map[int32]uint16{asset: cost}
	for _, link := range p.snapshot().Links {
		if link.Cost > MaxLANCost {
			continue
		}
		if link.Asset == asset && link.Cost < cost {
			return false
		}
		if previous, ok := best[link.Asset]; !ok || link.Cost < previous {
			best[link.Asset] = link.Cost
		}
	}
	type ranked struct {
		asset int32
		cost  uint16
	}
	neighbors := make([]ranked, 0, len(best))
	for peer, linkCost := range best {
		neighbors = append(neighbors, ranked{peer, linkCost})
	}
	sort.Slice(neighbors, func(i, j int) bool {
		if neighbors[i].cost != neighbors[j].cost {
			return neighbors[i].cost < neighbors[j].cost
		}
		return neighbors[i].asset < neighbors[j].asset
	})
	for _, neighbor := range neighbors[:min(len(neighbors), 3)] {
		if neighbor.asset == asset {
			return true
		}
	}
	return false
}

// AllowRadio limits the mesh to three distinct peers where possible. When a
// peer is already connected through the other radio, that second link is kept
// only while there is no fresh, different candidate to fill an open slot.
// This permits a two-device NAN+BLE pair to fail over automatically.
func (p *PeerSelection) AllowRadio(asset int32, medium RadioMedium) bool {
	allow, _ := p.AllowRadioReason(asset, medium)
	return allow
}

// AllowRadioReason reports the same decision as AllowRadio plus a stable
// machine-readable reason naming the branch that allowed or vetoed the link.
// Veto reasons exist so a torn-down established link can be attributed to the
// exact clause that fired instead of hypothesizing after the fact.
func (p *PeerSelection) AllowRadioReason(asset int32, medium RadioMedium) (bool, string) {
	if p == nil || p.snapshot == nil {
		return true, "no-policy"
	}
	if asset <= 0 || (medium != RadioNAN && medium != RadioBLE) {
		return false, "invalid-asset-or-medium"
	}
	s := p.snapshot()
	connected := make(map[int32]bool, len(s.Links))
	otherRadio := false
	for _, link := range s.Links {
		connected[link.Asset] = true
		if link.Asset == asset && link.Cost <= MaxLANCost {
			return false, fmt.Sprintf("lan-supersedes(cost=%d)", link.Cost)
		}
		if link.Asset == asset && ((medium == RadioNAN && link.Cost > 512) || (medium == RadioBLE && link.Cost == 512)) {
			otherRadio = true
		}
	}
	if !connected[asset] && len(connected) >= 3 {
		return false, fmt.Sprintf("peer-cap-no-slot(connected=%d)", len(connected))
	}
	if len(connected) > 3 {
		// Concurrent radio handshakes can briefly exceed the target. Keep
		// verified LAN neighbors first, then the lowest-numbered radio peers
		// until the active set converges to three distinct assets.
		lan, radio := make([]int32, 0, len(connected)), make([]int32, 0, len(connected))
		for candidate := range connected {
			isLAN := false
			for _, link := range s.Links {
				if link.Asset == candidate && link.Cost <= MaxLANCost {
					isLAN = true
					break
				}
			}
			if isLAN {
				lan = append(lan, candidate)
			} else {
				radio = append(radio, candidate)
			}
		}
		sort.Slice(lan, func(i, j int) bool { return lan[i] < lan[j] })
		sort.Slice(radio, func(i, j int) bool { return radio[i] < radio[j] })
		keep := append(lan, radio...)
		selected := false
		for _, candidate := range keep[:3] {
			selected = selected || candidate == asset
		}
		if !selected {
			return false, fmt.Sprintf("convergence-not-selected(connected=%d)", len(connected))
		}
	}
	if !otherRadio {
		return true, "first-radio"
	}
	if len(connected) >= 3 {
		return false, fmt.Sprintf("second-radio-slots-full(connected=%d)", len(connected))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for key, hint := range p.seen {
		if now.Sub(hint.seen) > 60*time.Second {
			delete(p.seen, key)
			continue
		}
		if key.asset != asset && !connected[key.asset] && !now.Before(hint.failedUntil) {
			return false, fmt.Sprintf("second-radio-fresh-candidate(hint=%d)", key.asset)
		}
	}
	return true, "second-radio-failover-kept"
}
