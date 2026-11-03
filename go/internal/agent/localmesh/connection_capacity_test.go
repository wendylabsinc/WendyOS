package localmesh

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestConnectionCapacityReclaimsOnlyOwnedSlowerRedundancy(t *testing.T) {
	peer, next := selectionDevice(10), selectionDevice(11)
	view := NodeSnapshot{Links: []PeerLink{{ID: 1, Device: peer, Cost: 4096}, {ID: 2, Device: peer, Cost: 128, ForwardingReady: true}}}
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 2})
	p.SetSnapshot(func() NodeSnapshot { return view })
	old := p.Reserve("old", peer, true)
	if old == nil || !old.Ready(peer, func() { t.Fatal("capacity policy used blocking physical closer") }) {
		t.Fatal("fixture could not own original carrier")
	}
	var signals atomic.Int32
	BindConnectionLink(WithConnectionLease(context.Background(), old), 1, 4096, func() { signals.Add(1) })
	if !p.OutgoingDiscoveryAvailable() {
		t.Fatal("full redundant outgoing budget hid useful discovery capacity")
	}
	if got := p.ReservePreferred("new", next, true, "", true); got != nil {
		t.Fatal("reclamation allocated an extra slot before owner completion")
	}
	if !old.Retired() || signals.Load() != 1 || p.Counts().Outgoing != 1 {
		t.Fatal("exact owned redundancy was not signalled and retained until Release")
	}
	if p.OutgoingDiscoveryAvailable() {
		t.Fatal("closing redundancy was counted as available before normal completion")
	}
	for i := 0; i < 5; i++ {
		if p.ReservePreferred("new", next, true, "", true) != nil {
			t.Fatal("retired lease stopped consuming its actual slot")
		}
	}
	if signals.Load() != 1 {
		t.Fatal("repeated demand signalled more than one close")
	}
	old.Release()
	view.Links = view.Links[1:]
	replacement := p.ReservePreferred("new", next, true, "", true)
	if replacement == nil || p.Counts().Outgoing != 1 || p.Counts().Attempts != 1 {
		t.Fatal("new direct peer could not use the normally released slot")
	}
	replacement.Release()
}

func TestConnectionCapacityDiscoveryDoesNotHideReclaimableIncomingSlot(t *testing.T) {
	peer := selectionDevice(10)
	view := NodeSnapshot{Links: []PeerLink{{ID: 1, Device: peer, Cost: 4096}, {ID: 2, Device: peer, Cost: 128, ForwardingReady: true}}}
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 1})
	p.SetSnapshot(func() NodeSnapshot { return view })
	old := p.Reserve("in", peer, false)
	old.Ready(peer, func() {})
	defer old.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), old), 1, 4096, func() { t.Fatal("advisory capacity changed owned connection state") })
	if !p.IncomingAvailable() {
		t.Fatal("full safe standby suppressed incoming peer discovery")
	}
	view.Links[1].ForwardingReady = false
	if p.IncomingAvailable() {
		t.Fatal("full unique or unready route advertised reclaimable incoming capacity")
	}
}

func TestConnectionCapacityRequiresCurrentAlternateAndRunnableNewPeer(t *testing.T) {
	peer, next := selectionDevice(10), selectionDevice(11)
	for _, mode := range []string{"no-alternate", "uninstalled", "other-peer", "retiring-alternate", "stale-incarnation", "wrong-cost", "connected-upgrade", "failure-backoff", "attempt-full", "duplicate-setup", "installed-transit"} {
		t.Run(mode, func(t *testing.T) {
			view := NodeSnapshot{Links: []PeerLink{{ID: 1, Device: peer, Cost: 4096}, {ID: 2, Device: peer, Cost: 128, ForwardingReady: true}}}
			p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 3})
			p.SetSnapshot(func() NodeSnapshot { return view })
			old := p.Reserve("old", peer, true)
			if old == nil {
				t.Fatal("missing original reservation")
			}
			defer old.Release()
			var signals atomic.Int32
			if mode != "attempt-full" && mode != "duplicate-setup" {
				if !old.Ready(peer, func() {}) {
					t.Fatal("missing verified ownership")
				}
				BindConnectionLink(WithConnectionLease(context.Background(), old), 1, 4096, func() { signals.Add(1) })
			}
			key, candidate, reclaim := "new", next, true
			switch mode {
			case "no-alternate":
				view.Links = view.Links[:1]
			case "uninstalled":
				view.Links[1].ForwardingReady = false
			case "other-peer":
				view.Links[1].Device = next
			case "retiring-alternate":
				view.Links[1].Retiring = true
			case "stale-incarnation":
				view.Links[0].ID = 3
			case "wrong-cost":
				view.Links[0].Cost = 512
			case "installed-transit":
				view.ForwardingRoutes = []babel.Route{{Link: 1, Metric: 512}}
			case "connected-upgrade":
				candidate = peer
			case "failure-backoff":
				reclaim = false
			case "duplicate-setup":
				key, candidate = "old", peer
			}
			if p.ReservePreferred(key, candidate, true, "", reclaim) != nil || old.Retired() || signals.Load() != 0 {
				t.Fatal("non-runnable or unsafe replacement changed owned capacity")
			}
		})
	}
}

func TestConnectionCapacityOutgoingPressureDoesNotRetireIncoming(t *testing.T) {
	peer, other, next := selectionDevice(10), selectionDevice(11), selectionDevice(12)
	view := NodeSnapshot{Links: []PeerLink{{ID: 1, Device: peer, Cost: 4096}, {ID: 2, Device: peer, Cost: 128, ForwardingReady: true}, {ID: 3, Device: other, Cost: 4096}}}
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 3})
	p.SetSnapshot(func() NodeSnapshot { return view })
	incoming := p.Reserve("in", peer, false)
	outgoing := p.Reserve("out", other, true)
	if incoming == nil || outgoing == nil {
		t.Fatal("missing capacity fixtures")
	}
	defer incoming.Release()
	defer outgoing.Release()
	incoming.Ready(peer, func() {})
	outgoing.Ready(other, func() {})
	var signals atomic.Int32
	BindConnectionLink(WithConnectionLease(context.Background(), incoming), 1, 4096, func() { signals.Add(1) })
	BindConnectionLink(WithConnectionLease(context.Background(), outgoing), 3, 4096, func() { signals.Add(1) })
	if p.ReservePreferred("new", next, true, "", true) != nil || signals.Load() != 0 {
		t.Fatal("incoming redundancy was retired even though it cannot free outgoing capacity")
	}
}

func TestIncomingWorkerPressureReclaimsOnlySafeIncomingOwnership(t *testing.T) {
	for _, blocked := range []string{"none", "unready-incoming-alternate", "incoming-transit"} {
		t.Run(blocked, func(t *testing.T) {
			inPeer, outPeer := selectionDevice(10), selectionDevice(11)
			view := NodeSnapshot{Links: []PeerLink{{ID: 1, Device: inPeer, Cost: 512}, {ID: 2, Device: inPeer, Cost: 128, ForwardingReady: true}, {ID: 3, Device: outPeer, Cost: 4096}, {ID: 4, Device: outPeer, Cost: 128, ForwardingReady: true}}}
			p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 3})
			p.SetSnapshot(func() NodeSnapshot { return view })
			in, out := p.Reserve("in", inPeer, false), p.Reserve("out", outPeer, true)
			if in == nil || out == nil || !in.Ready(inPeer, func() {}) || !out.Ready(outPeer, func() {}) {
				t.Fatal("missing owned fixture leases")
			}
			defer in.Release()
			defer out.Release()
			inSignals, outSignals := 0, 0
			BindConnectionLink(WithConnectionLease(context.Background(), in), 1, 512, func() { inSignals++ })
			BindConnectionLink(WithConnectionLease(context.Background(), out), 3, 4096, func() { outSignals++ })
			if blocked == "unready-incoming-alternate" {
				view.Links[1].ForwardingReady = false
			}
			if blocked == "incoming-transit" {
				view.ForwardingRoutes = []babel.Route{{Link: 1, Metric: 900}}
			}
			want := blocked == "none"
			if got := p.ReclaimIncomingWorker(); got != want || in.Retired() != want || out.Retired() || outSignals != 0 || (inSignals == 1) != want {
				t.Fatal("incoming worker pressure reclaimed an unsafe or outgoing incarnation")
			}
			if want {
				if p.ReclaimIncomingWorker() || p.Counts().Total != 2 {
					t.Fatal("incoming reclamation borrowed capacity or repeated its close")
				}
				in.Release()
				if p.ReclaimIncomingWorker() || out.Retired() {
					t.Fatal("outgoing carrier cannot free an incoming worker token")
				}
			}
		})
	}
}

func TestConnectionCapacityConcurrentDemandHasOneRetirement(t *testing.T) {
	peer := selectionDevice(10)
	view := NodeSnapshot{Links: []PeerLink{{ID: 1, Device: peer, Cost: 4096}, {ID: 2, Device: peer, Cost: 128, ForwardingReady: true}}}
	p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 2})
	p.SetSnapshot(func() NodeSnapshot { return view })
	old := p.Reserve("old", peer, true)
	old.Ready(peer, func() {})
	defer old.Release()
	var signals atomic.Int32
	BindConnectionLink(WithConnectionLease(context.Background(), old), 1, 4096, func() { signals.Add(1) })
	var workers sync.WaitGroup
	for i := 20; i < 40; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			if p.ReservePreferred(selectionDevice(i).Principal(), selectionDevice(i), true, "", true) != nil {
				t.Error("capacity oversubscribed")
			}
		}(i)
	}
	workers.Wait()
	if signals.Load() != 1 {
		t.Fatalf("retirement count %d", signals.Load())
	}
}

func TestCandidatePriorityNewPeerThenUpgradeThenStandby(t *testing.T) {
	now := time.Now()
	view := NodeSnapshot{Links: []PeerLink{{Device: selectionDevice(10), Cost: 512, ForwardingReady: true}, {Device: selectionDevice(11), Cost: 128, ForwardingReady: true}, {Device: selectionDevice(12), Cost: 64, Retiring: true}}}
	q := &CandidateQueue{shuffle: func(int, func(int, int)) {}}
	items := map[string]struct {
		peer int
		cost uint16
	}{"standby": {11, 4096}, "upgrade": {10, 128}, "new": {13, 4096}, "retiring": {12, 4096}}
	lookup := func(key string) (meshid.Device, uint16) {
		item := items[key]
		return selectionDevice(item.peer), item.cost
	}
	got := q.OrderPreferred([]string{"standby", "upgrade", "new", "retiring"}, now, view, lookup)
	if !reflect.DeepEqual(got, []string{"new", "retiring", "upgrade", "standby"}) {
		t.Fatalf("priority=%v", got)
	}
	q.Failed("new", now)
	if q.ReclaimEligible("new", now.Add(time.Second)) {
		t.Fatal("failed candidate could evict repeatedly")
	}
	got = q.OrderPreferred([]string{"new", "standby", "upgrade"}, now.Add(time.Second), view, lookup)
	if !reflect.DeepEqual(got, []string{"upgrade", "standby", "new"}) {
		t.Fatalf("penalized priority=%v", got)
	}
	if !q.ReclaimEligible("new", now.Add(FailedConnectPenalty)) {
		t.Fatal("existing penalty never expired")
	}
	// An attached but unusable cheap LAN cannot demote an upgrade from the
	// actually working BLE path to generic redundancy.
	view.Links = []PeerLink{{Device: selectionDevice(10), Cost: 128}, {Device: selectionDevice(10), Cost: 4096, ForwardingReady: true}, {Device: selectionDevice(11), Cost: 128, ForwardingReady: true}}
	items["upgrade"] = struct {
		peer int
		cost uint16
	}{10, 512}
	got = q.OrderPreferred([]string{"standby", "upgrade"}, now, view, lookup)
	if !reflect.DeepEqual(got, []string{"upgrade", "standby"}) {
		t.Fatalf("unusable cheap attachment hid working-path upgrade: %v", got)
	}
}
