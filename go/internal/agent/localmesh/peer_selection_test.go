package localmesh

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestPeerSelectionLANAndRadioDiversity(t *testing.T) {
	now := time.Now()
	snapshot := NodeSnapshot{}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	p.now = func() time.Time { return now }
	p.Seen(selectionDevice(10), RadioNAN)
	if !p.AllowRadio(selectionDevice(10), RadioNAN) || !p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("an available radio peer was rejected")
	}
	snapshot.Links = []PeerLink{{Device: selectionDevice(10), Cost: 512}}
	if !p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("a second radio should provide failover when there is no other candidate")
	}
	p.Seen(selectionDevice(11), RadioBLE)
	if p.AllowRadio(selectionDevice(10), RadioBLE) || !p.AllowRadio(selectionDevice(11), RadioBLE) {
		t.Fatal("a different peer should fill the open slot before duplicating asset 10")
	}
	p.Failed(selectionDevice(11), RadioBLE)
	p.Seen(selectionDevice(11), RadioBLE)
	if !p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("continuing advertisements from a failed candidate suppressed fallback")
	}
	now = now.Add(31 * time.Second)
	if p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("candidate was not reconsidered after failure cooldown")
	}
	p.Connected(selectionDevice(11), RadioBLE)
	if p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("healthy candidate did not reclaim the diverse slot")
	}
	now = now.Add(61 * time.Second)
	if !p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("expired candidate hints should not suppress fallback")
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(10), Cost: 128})
	if p.AllowRadio(selectionDevice(10), RadioNAN) || p.AllowRadio(selectionDevice(10), RadioBLE) || !p.HasLAN(selectionDevice(10)) {
		t.Fatal("authenticated LAN must displace both radios")
	}
}

func TestPeerSelectionConfiguredTCPAndDirectLAN(t *testing.T) {
	snapshot := NodeSnapshot{Links: []PeerLink{{Device: selectionDevice(10), Cost: 256}}}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	if p.HasLAN(selectionDevice(10)) || !p.AllowRadio(selectionDevice(10), RadioNAN) || !p.AllowRadio(selectionDevice(10), RadioBLE) {
		t.Fatal("configured TCP must coexist with NAN and BLE")
	}
	if p.AllowLAN(selectionDevice(10), 256) {
		t.Fatal("configured TCP must not be counted as discovered LAN")
	}
	for _, cost := range []uint16{64, 128} {
		snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(10), Cost: cost})
		if !p.HasLAN(selectionDevice(10)) || p.AllowRadio(selectionDevice(10), RadioNAN) || p.AllowRadio(selectionDevice(10), RadioBLE) {
			t.Fatalf("direct LAN cost %d must suppress radios", cost)
		}
		snapshot.Links = snapshot.Links[:1]
	}
}

func TestPeerSelectionDoesNotShareTransportBudgets(t *testing.T) {
	snapshot := NodeSnapshot{}
	for i := 1; i <= 12; i++ {
		snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(i), Cost: 512})
	}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	if !p.AllowRadio(selectionDevice(50), RadioBLE) || !p.AllowRadio(selectionDevice(50), RadioNAN) {
		t.Fatal("unrelated peers consumed a shared radio budget")
	}
}

func TestPeerSelectionLANReplacesRadioAndSlowerLAN(t *testing.T) {
	snapshot := NodeSnapshot{Links: []PeerLink{
		{Device: selectionDevice(10), Cost: 4096}, {Device: selectionDevice(11), Cost: 512}, {Device: selectionDevice(12), Cost: 4096},
	}}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	if !p.AllowLAN(selectionDevice(13), 64) {
		t.Fatal("three radio peers must not block a direct LAN neighbor")
	}
	snapshot.Links = []PeerLink{
		{Device: selectionDevice(10), Cost: 128}, {Device: selectionDevice(11), Cost: 128}, {Device: selectionDevice(12), Cost: 128},
	}
	if !p.AllowLAN(selectionDevice(13), 64) {
		t.Fatal("existing Wi-Fi neighbors must not block a new wired neighbor")
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(13), Cost: 64})
	if !p.AllowLAN(selectionDevice(12), 128) || !p.AllowLAN(selectionDevice(13), 64) {
		t.Fatal("wired links must not evict a different Wi-Fi-only neighbor")
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(10), Cost: 64})
	if p.AllowLAN(selectionDevice(10), 128) || !p.AllowLAN(selectionDevice(10), 64) {
		t.Fatal("wired links must replace a slower link to the same principal")
	}
	if p.AllowLAN(selectionDevice(10), 256) {
		t.Fatal("higher-cost LAN duplicate should be rejected")
	}
}

// Exercise admission at both endpoints and periodic pruning, including a
// highest-UUID late join. The old local top-three rule leaves that node isolated
// even though every pair has a working Ethernet path.
func TestLANEligibilityDoesNotRankUnrelatedPeersThroughJoinsAndPruning(t *testing.T) {
	for _, size := range []int{5, 8, 16} {
		for seed := int64(0); seed < 4; seed++ {
			t.Run(fmt.Sprintf("nodes=%d/order=%d", size, seed), func(t *testing.T) {
				snapshots := make([]NodeSnapshot, size)
				policies := make([]*PeerSelection, size)
				for i := range policies {
					policies[i] = NewPeerSelection(func() NodeSnapshot { return snapshots[i] })
				}
				edges := make([][2]int, 0)
				for a := 0; a < size; a++ {
					for b := a + 1; b < size; b++ {
						edges = append(edges, [2]int{a, b})
					}
				}
				random := rand.New(rand.NewSource(seed))
				random.Shuffle(len(edges), func(i, j int) { edges[i], edges[j] = edges[j], edges[i] })
				connected := make(map[[2]int]bool)
				refresh := func() {
					for i := range snapshots {
						snapshots[i].Links = nil
					}
					for edge := range connected {
						a, b := edge[0], edge[1]
						snapshots[a].Links = append(snapshots[a].Links, PeerLink{Device: selectionDevice(b + 1), Cost: 64})
						snapshots[b].Links = append(snapshots[b].Links, PeerLink{Device: selectionDevice(a + 1), Cost: 64})
					}
				}
				step := func(active int) {
					for _, edge := range edges {
						a, b := edge[0], edge[1]
						allowed := b < active && policies[a].AllowLAN(selectionDevice(b+1), 64) && policies[b].AllowLAN(selectionDevice(a+1), 64)
						if allowed {
							connected[edge] = true
						} else {
							delete(connected, edge)
						}
						refresh()
					}
				}
				for _, active := range []int{size - 1, size, size - 1, size} {
					for tick := 0; tick < 5; tick++ {
						step(active)
					}
					seen := map[int]bool{0: true}
					queue := []int{0}
					for len(queue) > 0 {
						node := queue[0]
						queue = queue[1:]
						for edge := range connected {
							a, b := edge[0], edge[1]
							if b == node {
								a, b = b, a
							}
							if a == node && !seen[b] {
								seen[b] = true
								queue = append(queue, b)
							}
						}
					}
					if len(seen) != active {
						t.Fatalf("only %d/%d devices connected after admission and pruning", len(seen), active)
					}
				}
			})
		}
	}
}

func TestPopulatedLANRetainsRadioBridge(t *testing.T) {
	snapshot := NodeSnapshot{}
	for i := 1; i <= 8; i++ {
		snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(i), Cost: 64})
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(9), Cost: 256})
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	bridge := selectionDevice(10)
	for _, medium := range []RadioMedium{RadioNAN, RadioBLE} {
		if !p.AllowRadio(bridge, medium) {
			t.Fatal("LAN or configured TCP peers consumed the radio bridge's slot")
		}
	}
	snapshot.Links = append(snapshot.Links, PeerLink{Device: bridge, Cost: 512})
	for tick := 0; tick < 5; tick++ {
		if !p.AllowRadio(bridge, RadioNAN) {
			t.Fatal("periodic pruning removed the only bridge to another LAN")
		}
	}
}

func TestDuplicateRadiosNeverPruneBothPaths(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("full=%v", full), func(t *testing.T) {
			peer := selectionDevice(1)
			snapshot := NodeSnapshot{Links: []PeerLink{{Device: peer, Cost: 4096}}}
			if full {
				snapshot.Links = append(snapshot.Links, PeerLink{Device: selectionDevice(2), Cost: 512}, PeerLink{Device: selectionDevice(3), Cost: 512})
			}
			p := NewPeerSelection(func() NodeSnapshot { return snapshot })
			p.Seen(selectionDevice(4), RadioBLE)
			if !p.AllowRadio(peer, RadioBLE) || !p.AllowRadio(peer, RadioNAN) {
				t.Fatal("retain the sole authenticated BLE path while admitting a preferred NAN replacement")
			}
			snapshot.Links = append(snapshot.Links, PeerLink{Device: peer, Cost: 512})
			if !p.AllowRadio(peer, RadioNAN) || p.AllowRadio(peer, RadioBLE) {
				t.Fatal("both providers must agree to retain NAN when shedding the duplicate BLE link")
			}
		})
	}
}

func TestAllowRadioReasonNamesVetoBranch(t *testing.T) {
	now := time.Now()
	snapshot := NodeSnapshot{}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	p.now = func() time.Time { return now }

	if allow, reason := p.AllowRadioReason(selectionDevice(10), RadioBLE); !allow || reason != "first-radio" {
		t.Fatalf("empty snapshot: allow=%v reason=%q", allow, reason)
	}
	if allow, _ := p.AllowRadioReason(meshid.Device{}, RadioBLE); allow {
		t.Fatal("invalid asset must be vetoed")
	}

	// LAN supersedes: same asset over cheap link.
	snapshot.Links = []PeerLink{{Device: selectionDevice(10), Cost: 64}}
	if allow, reason := p.AllowRadioReason(selectionDevice(10), RadioBLE); allow || reason != "lan-supersedes(cost=64)" {
		t.Fatalf("LAN veto: allow=%v reason=%q", allow, reason)
	}

	snapshot.Links = []PeerLink{{Device: selectionDevice(10), Cost: 512}}
	if allow, reason := p.AllowRadioReason(selectionDevice(10), RadioBLE); !allow || reason != "second-radio-failover-kept" {
		t.Fatalf("NAN failover: %v %s", allow, reason)
	}
	snapshot.Links = []PeerLink{{Device: selectionDevice(1), Cost: 512}, {Device: selectionDevice(2), Cost: 512}, {Device: selectionDevice(3), Cost: 512}}
	if allow, _ := p.AllowRadioReason(selectionDevice(10), RadioBLE); !allow {
		t.Fatal("unrelated radio neighbours vetoed admission")
	}

	// AllowRadio stays consistent with the reasoned decision everywhere above.
	for _, tc := range [][2]int32{{10, 1}, {10, 2}, {1, 2}, {4, 2}} {
		allow, _ := p.AllowRadioReason(selectionDevice(int(tc[0])), RadioMedium(tc[1]))
		if allow != p.AllowRadio(selectionDevice(int(tc[0])), RadioMedium(tc[1])) {
			t.Fatalf("AllowRadio disagrees with AllowRadioReason for asset=%d medium=%d", tc[0], tc[1])
		}
	}
}

func TestRadioDiversityUsesOnlyCandidatesForItsOwnTransport(t *testing.T) {
	now := time.Unix(1000, 0)
	peer, candidate := selectionDevice(1), selectionDevice(2)
	snapshot := NodeSnapshot{Links: []PeerLink{{Device: peer, Cost: 512}, {Device: peer, Cost: 4096}}}
	p := NewPeerSelection(func() NodeSnapshot { return snapshot })
	p.now = func() time.Time { return now }
	p.Seen(candidate, RadioNAN)
	if !p.AllowRadio(peer, RadioBLE) {
		t.Fatal("NAN-only discovery displaced an authenticated BLE fallback")
	}
	p.Seen(candidate, RadioBLE)
	if p.AllowRadio(peer, RadioBLE) || !p.AllowRadio(peer, RadioNAN) {
		t.Fatal("BLE candidate must free only the redundant BLE slot")
	}
	p.Failed(candidate, RadioBLE)
	p.Seen(candidate, RadioBLE)
	if !p.AllowRadio(peer, RadioBLE) {
		t.Fatal("repeated discovery overrode a failed BLE candidate's cooldown")
	}
	now = now.Add(FailedConnectPenalty + time.Second)
	if p.AllowRadio(peer, RadioBLE) {
		t.Fatal("live BLE candidate did not become eligible after cooldown")
	}
	now = now.Add(time.Minute)
	if !p.AllowRadio(peer, RadioBLE) {
		t.Fatal("expired BLE discovery displaced an authenticated fallback")
	}
	p.Seen(candidate, RadioBLE)
	snapshot.Links = snapshot.Links[1:]
	if !p.AllowRadio(peer, RadioBLE) {
		t.Fatal("discovery displaced the peer's sole authenticated path")
	}
	// A candidate already reached on another carrier needs no new radio slot.
	snapshot.Links = append(snapshot.Links, PeerLink{Device: peer, Cost: 512}, PeerLink{Device: candidate, Cost: 64})
	if !p.AllowRadio(peer, RadioBLE) {
		t.Fatal("an already connected candidate displaced a useful fallback")
	}
}

func selectionDevice(number int) meshid.Device {
	tenant, _ := meshid.ParseTenant(testTenantUUID)
	device, _ := meshid.ParseDevice(tenant, fmt.Sprintf("00000000-0000-4000-8000-%012x", number))
	return device
}

func TestPeerSelectionBoundsUntrustedCandidatesAndExpiresOnInsert(t *testing.T) {
	now := time.Now()
	live, failed := selectionDevice(1), selectionDevice(2)
	p := NewPeerSelection(func() NodeSnapshot {
		return NodeSnapshot{Links: []PeerLink{{Device: live, Cost: 512}}}
	})
	p.now = func() time.Time { return now }
	p.Connected(live, RadioNAN)
	p.Seen(failed, RadioNAN)
	p.Failed(failed, RadioNAN)
	for number := 3; number <= 3*maxRadioCandidates; number++ {
		p.Seen(selectionDevice(number), RadioNAN)
		if len(p.seen) > maxRadioCandidates {
			t.Fatalf("untrusted principal flood retained %d candidates", len(p.seen))
		}
	}
	if len(p.seen) != maxRadioCandidates {
		t.Fatalf("cache did not retain its bounded working set: %d", len(p.seen))
	}
	if _, ok := p.seen[radioCandidateKey{selectionDevice(3), RadioNAN}]; ok {
		t.Fatal("deterministic oldest-principal eviction retained the oldest unprotected hint")
	}
	if _, ok := p.seen[radioCandidateKey{live, RadioNAN}]; !ok {
		t.Fatal("candidate flood evicted a live authenticated peer")
	}
	key := radioCandidateKey{failed, RadioNAN}
	until := p.seen[key].failedUntil
	if until != now.Add(30*time.Second) {
		t.Fatal("candidate flood discarded a pending retry cooldown")
	}
	p.Seen(failed, RadioNAN)
	if p.seen[key].failedUntil != until {
		t.Fatal("repeated discovery reset the retry cooldown")
	}
	// No AllowRadio call is needed to reclaim stale discovery entries.
	now = now.Add(61 * time.Second)
	p.Seen(selectionDevice(9999), RadioNAN)
	if len(p.seen) != 2 {
		t.Fatalf("expired candidates survived insertion: %d", len(p.seen))
	}
	if _, ok := p.seen[radioCandidateKey{live, RadioNAN}]; !ok {
		t.Fatal("expiry discarded the live authenticated peer's record")
	}
}

func TestPeerSelectionBoundsEveryCandidateInsertion(t *testing.T) {
	for _, method := range []string{"seen", "failed", "connected"} {
		t.Run(method, func(t *testing.T) {
			p := NewPeerSelection(nil)
			now := time.Now()
			p.now = func() time.Time { return now }
			for number := 1; number <= 2*maxRadioCandidates; number++ {
				device := selectionDevice(number)
				switch method {
				case "seen":
					p.Seen(device, RadioNAN)
				case "failed":
					p.Failed(device, RadioNAN)
				case "connected":
					p.Connected(device, RadioNAN)
				}
				if len(p.seen) > maxRadioCandidates {
					t.Fatalf("%s retained %d entries", method, len(p.seen))
				}
			}
			now = now.Add(61 * time.Second)
			p.Seen(selectionDevice(9999), RadioBLE)
			if len(p.seen) != 1 {
				t.Fatalf("%s entries were not reclaimed on expiry: %d", method, len(p.seen))
			}
		})
	}
}

func TestPeerSelectionConcurrentCandidateFlood(t *testing.T) {
	p := NewPeerSelection(func() NodeSnapshot { return NodeSnapshot{} })
	var workers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for number := 1; number <= maxRadioCandidates; number++ {
				device := selectionDevice(worker*maxRadioCandidates + number)
				p.Seen(device, RadioNAN)
				p.Failed(device, RadioNAN)
				p.Connected(device, RadioNAN)
				p.AllowRadio(device, RadioNAN)
			}
		}()
	}
	workers.Wait()
	if len(p.seen) > maxRadioCandidates {
		t.Fatalf("concurrent advertisements retained %d entries", len(p.seen))
	}
}
