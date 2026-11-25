package localmesh

import (
	"context"
	"net/netip"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestKnownAdditionalIncomingPeerCannotEvictOtherWarmBackup(t *testing.T) {
	for _, state := range []string{"working", "unknown", "recovering", "retiring"} {
		t.Run(state, func(t *testing.T) {
			a, b, c, d := selectionDevice(10), selectionDevice(11), selectionDevice(12), selectionDevice(13)
			view := NodeSnapshot{
				Links: []PeerLink{
					{ID: 1, Device: a, Cost: 4096},
					{ID: 2, Device: a, Cost: 128, ForwardingReady: true},
					{ID: 3, Device: b, Cost: 128, ForwardingReady: state != "recovering", Retiring: state == "retiring"},
					{ID: 4, Device: c, Cost: 4096, ForwardingReady: true},
					{ID: 5, Device: d, Cost: 4096, ForwardingReady: true},
				},
				ForwardingRoutes: []babel.Route{
					{Prefix: netip.PrefixFrom(a.MeshAddress(), 128), Link: 2, Metric: 128},
					{Prefix: netip.PrefixFrom(b.MeshAddress(), 128), Link: 3, Metric: 128},
					{Prefix: netip.PrefixFrom(c.MeshAddress(), 128), Link: 4, Metric: 4096},
					{Prefix: netip.PrefixFrom(d.MeshAddress(), 128), Link: 5, Metric: 4096},
				},
			}
			p := NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 3})
			p.SetSnapshot(func() NodeSnapshot { return view })
			p.SetCarrierCost(4096)
			signals := 0
			for _, owner := range []struct {
				key  string
				peer meshid.Device
				link babel.LinkID
			}{{"a", a, 1}, {"c", c, 4}, {"d", d, 5}} {
				lease := p.Reserve(owner.key, owner.peer, false)
				if lease == nil || !lease.Ready(owner.peer, func() { t.Fatal("called physical closer") }) {
					t.Fatal("initial authenticated incoming owner was rejected")
				}
				t.Cleanup(lease.Release)
				BindConnectionLink(WithConnectionLease(context.Background(), lease), owner.link, 4096, func() { signals++ })
			}
			if p.incomingDemandActive() || p.Counts().Total != 3 {
				t.Fatal("fixture did not start full without an incoming hold")
			}
			candidate := b
			if state == "unknown" {
				candidate = meshid.Device{}
			}
			working := state == "working"
			if p.HasWorkingCarrier(candidate) != working {
				t.Fatal("working-carrier scheduling check ignored current route readiness")
			}
			got := p.ReclaimIncomingWorkerFor(candidate, 4096)
			if working {
				if got || signals != 0 || p.incomingDemandActive() {
					t.Fatalf("known already-working peer displaced another warm backup: signals=%d", signals)
				}
			} else if !got || signals != 1 || !p.incomingDemandActive() {
				t.Fatalf("unknown or recovering peer could not signal one safe incoming owner: reclaimed=%v signals=%d", got, signals)
			}
			if p.Counts().Total != 3 {
				t.Fatal("reclamation borrowed capacity before the lifetime owner's Release")
			}
		})
	}
}
