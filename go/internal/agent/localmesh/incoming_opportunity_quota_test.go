package localmesh

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func opportunityWorkingBLE(f *incomingHoldFixture) {
	for i := range f.view.Links {
		if f.view.Links[i].Device == f.peer && f.view.Links[i].ForwardingReady {
			f.view.Links[i].Cost = 4096
			f.view.Links[i].Transport = "ble"
		}
	}
}

func TestIncomingOpportunityLastSlotDefersCheaperAndEqualReturns(t *testing.T) {
	for _, cost := range []uint16{128, 4096} {
		t.Run(strconv.Itoa(int(cost)), func(t *testing.T) {
			f := newIncomingHoldFixture(t, 1)
			f.demand(t)
			f.releaseOld()
			opportunityWorkingBLE(f)
			group := strconv.Itoa(int(cost))
			if old := f.p.ReserveGroup("old-outgoing-return", f.peer, true, group); old != nil {
				old.Release()
				t.Fatal("configured cheaper or equal return consumed the last distinct-peer opportunity")
			}
			setup := f.p.ReserveGroup("unknown-incoming-return", meshid.Device{}, false, group)
			if setup == nil {
				t.Fatal("unknown identity could not reach authentication")
			}
			// Capture applies even if this TLS flight crosses the bounded hold.
			f.now = f.now.Add(FailedConnectPenalty + time.Nanosecond)
			closed := 0
			if setup.Ready(f.peer, func() { closed++ }) || closed != 1 || !setup.Retired() {
				t.Fatal("post-auth cheap or equal return bypassed the captured last opportunity")
			}
			setup.Release()
			upgrade := f.p.ReserveGroup("normal-upgrade-after-expiry", f.peer, true, group)
			if upgrade == nil || !upgrade.Ready(f.peer, func() {}) {
				t.Fatal("expired hold changed ordinary upgrade admission")
			}
			upgrade.Release()
		})
	}
}

func TestIncomingOpportunityNewUniqueBindRestoresCheaperReturn(t *testing.T) {
	f := newIncomingHoldFixture(t, 2)
	f.demand(t)
	f.releaseOld()
	opportunityWorkingBLE(f)
	leaf := f.p.ReserveGroup("new-unique", meshid.Device{}, false, "4096")
	if leaf == nil || !leaf.Ready(selectionDevice(40), func() {}) {
		t.Fatal("distinct peer could not authenticate")
	}
	defer leaf.Release()
	if old := f.p.ReserveGroup("cheap-before-bind", f.peer, true, "128"); old != nil {
		old.Release()
		t.Fatal("Ready consumed the opportunity before the distinct peer was admitted")
	}
	BindConnectionLink(WithConnectionLease(context.Background(), leaf), 40, 4096, func() {})
	upgrade := f.p.ReserveGroup("cheap-after-bind", f.peer, true, "128")
	if upgrade == nil || !upgrade.Ready(f.peer, func() {}) {
		t.Fatal("actual distinct-peer Bind did not restore ordinary cheaper upgrade")
	}
	upgrade.Release()
}

func TestIncomingOpportunityUsesOnlyUnreservedNativeSpare(t *testing.T) {
	f := newIncomingHoldFixture(t, 3)
	incumbentPeer := selectionDevice(30)
	f.view.Links = append(f.view.Links, PeerLink{ID: 30, Device: incumbentPeer, Cost: 4096, ForwardingReady: true})
	incumbent := f.p.ReserveGroup("incumbent-incoming", incumbentPeer, false, "4096")
	if incumbent == nil || !incumbent.Ready(incumbentPeer, func() {}) {
		t.Fatal("incumbent incoming fixture missing")
	}
	defer incumbent.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), incumbent), 30, 4096, func() {})
	f.demand(t)
	f.releaseOld()
	opportunityWorkingBLE(f)
	if got := f.p.Counts().Total; got != 1 {
		t.Fatalf("expected two native spare slots, total=%d", got)
	}
	addition := f.p.ReserveGroup("cheap-unreserved-addition", f.peer, true, "128")
	if addition == nil || !addition.Ready(f.peer, func() {}) {
		t.Fatal("hold suppressed an addition that left one native opportunity free")
	}
	defer addition.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), addition), 31, 128, func() {})
	other := selectionDevice(32)
	f.view.Links = append(f.view.Links, PeerLink{ID: 32, Device: other, Cost: 4096, ForwardingReady: true})
	if old := f.p.ReserveGroup("other-connected-addition", other, true, "128"); old != nil {
		old.Release()
		t.Fatal("outgoing budget spare incorrectly overrode the last TOTAL native opportunity")
	}
	leaf := f.p.ReserveGroup("last-slot-new-peer", meshid.Device{}, false, "4096")
	if leaf == nil || !leaf.Ready(selectionDevice(33), func() {}) {
		t.Fatal("distinct peer could not use the protected final native slot")
	}
	defer leaf.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), leaf), 33, 4096, func() {})
	if f.p.Counts().Total != 3 || addition.Retired() || incumbent.Retired() {
		t.Fatal("opportunity allocation exceeded quota or dropped a working owner")
	}
}

func TestIncomingOpportunityProtectsFinalWorkerIndependentlyOfNativeQuota(t *testing.T) {
	for _, tc := range []struct {
		name                                     string
		last, newPeer, lost, retiring, costAware bool
		want                                     bool
	}{
		{"legacy-last-worker", true, false, false, false, false, false},
		{"cost-aware-last-worker", true, false, false, false, true, false},
		{"unreserved-worker-and-native-spare", false, false, false, false, false, true},
		{"distinct-peer-last-worker", true, true, false, false, false, true},
		{"cost-aware-distinct-peer-last-worker", true, true, false, false, true, true},
		{"last-working-path-recovery", true, false, true, false, false, true},
		{"retiring-incarnation-is-not-working", true, false, false, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIncomingHoldFixture(t, 3)
			f.demand(t)
			f.releaseOld()
			opportunityWorkingBLE(f)
			f.view.Links[0].ForwardingReady = !tc.lost
			f.view.Links[0].Retiring = tc.retiring
			peer := f.peer
			if tc.newPeer {
				peer = selectionDevice(50)
			}
			freeWorkers := 2
			if tc.last {
				freeWorkers = 1
			}
			if got := f.p.DeferIncomingCarrier(peer, 128, freeWorkers); got != !tc.want {
				t.Fatalf("pre-TLS worker scheduling defer=%v want=%v", got, !tc.want)
			}
			setup := f.p.ReserveGroup("incoming-unknown", meshid.Device{}, false, "128")
			if setup == nil || f.p.Counts().Total != 1 {
				t.Fatal("worker fixture did not retain two spare native quota slots")
			}
			defer setup.Release()
			ctx := WithConnectionLease(context.Background(), setup)
			if tc.last {
				ctx = WithLastIncomingWorker(ctx)
			}
			closed := 0
			var admitted bool
			if tc.costAware {
				admitted = connectionReadyForCarrier(ctx, peer, 128, func() { closed++ })
			} else {
				admitted = ConnectionReady(ctx, peer, func() { closed++ })
			}
			if admitted != tc.want || setup.Retired() == tc.want || (closed == 1) == tc.want {
				t.Fatalf("worker/native quota distinction lost: admitted=%v retired=%v closed=%d", admitted, setup.Retired(), closed)
			}
		})
	}
}
