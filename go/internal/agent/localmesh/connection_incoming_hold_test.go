package localmesh

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type incomingHoldFixture struct {
	p      *ConnectionPolicy
	now    time.Time
	view   NodeSnapshot
	peer   meshid.Device
	old    *ConnectionLease
	closes int
}

func newIncomingHoldFixture(t *testing.T, total int) *incomingHoldFixture {
	t.Helper()
	f := &incomingHoldFixture{now: time.Now(), peer: selectionDevice(10)}
	f.view.Links = []PeerLink{{ID: 1, Device: f.peer, Cost: 4096}, {ID: 2, Device: f.peer, Cost: 128, ForwardingReady: true}}
	f.p = NewConnectionPolicy(selectionDevice(1), true, ConnectionLimits{Outgoing: total, Attempts: total, Total: total})
	f.p.now = func() time.Time { return f.now }
	f.p.SetSnapshot(func() NodeSnapshot { return f.view })
	f.old = f.p.ReserveGroup("old-incoming", f.peer, false, "4096")
	if f.old == nil || !f.old.Ready(f.peer, func() { t.Fatal("reclamation called a physical closer") }) {
		t.Fatal("missing original authenticated incoming carrier")
	}
	BindConnectionLink(WithConnectionLease(context.Background(), f.old), 1, 4096, func() { f.closes++ })
	t.Cleanup(f.old.Release)
	return f
}

func (f *incomingHoldFixture) demand(t *testing.T) {
	t.Helper()
	if !f.p.ReclaimIncomingWorker() || !f.old.Retired() || f.closes != 1 {
		t.Fatal("unknown incoming demand did not signal the exact safe old owner")
	}
}

func (f *incomingHoldFixture) releaseOld() {
	for i, link := range f.view.Links {
		if link.ID == 1 {
			f.view.Links = append(f.view.Links[:i], f.view.Links[i+1:]...)
			break
		}
	}
	f.old.Release()
}

func (f *incomingHoldFixture) oldOutgoing() *ConnectionLease {
	return f.p.ReserveGroup("old-outgoing-retry", f.peer, true, "4096")
}

func TestIncomingDemandHoldStartsAtActualOwnerReleaseAndExpires(t *testing.T) {
	f := newIncomingHoldFixture(t, 1)
	f.demand(t)
	// A closer may remain blocked beyond the entire reuse interval. It must
	// retain its actual slot and cannot spend the waiting peer's opportunity.
	f.now = f.now.Add(2 * FailedConnectPenalty)
	if f.p.Counts().Total != 1 || f.oldOutgoing() != nil {
		t.Fatal("signalling retirement released or borrowed the old owner's slot")
	}
	f.releaseOld()
	f.now = f.now.Add(FailedConnectPenalty - time.Nanosecond)
	if f.oldOutgoing() != nil {
		t.Fatal("opportunity was anchored to retirement rather than actual release")
	}
	f.now = f.now.Add(time.Nanosecond)
	backup := f.oldOutgoing()
	if backup == nil || !backup.Ready(f.peer, func() {}) {
		t.Fatal("leaf disappearance left an indefinite hold instead of restoring standby")
	}
	backup.Release()
}

func TestNormalReleaseDoesNotInventIncomingDemand(t *testing.T) {
	f := newIncomingHoldFixture(t, 1)
	f.releaseOld()
	backup := f.oldOutgoing()
	if backup == nil {
		t.Fatal("ordinary completion invented an incoming-demand delay")
	}
	backup.Release()
}

func TestIncomingSetupCapturesHoldAcrossTLSWithoutRenewingIt(t *testing.T) {
	f := newIncomingHoldFixture(t, 1)
	f.demand(t)
	f.releaseOld()
	f.now = f.now.Add(FailedConnectPenalty - time.Nanosecond)
	setup := f.p.ReserveGroup("unknown-incoming-tls", meshid.Device{}, false, "4096")
	if setup == nil {
		t.Fatal("held capacity rejected an unknown authentication probe")
	}
	f.now = f.now.Add(2 * time.Nanosecond)
	closed := 0
	if setup.Ready(f.peer, func() { closed++ }) || !setup.Retired() || closed != 1 {
		t.Fatal("TLS crossing the deadline let old redundancy consume the held opportunity")
	}
	if f.p.Counts().Total != 1 {
		t.Fatal("post-auth scheduling rejection released capacity before its owner")
	}
	setup.Release()
	backup := f.oldOutgoing()
	if backup == nil {
		t.Fatal("rejecting known redundancy renewed the already expired hold")
	}
	backup.Release()
}

func TestKnownRedundantRetriesAndDuplicateReleaseDoNotRenewHold(t *testing.T) {
	f := newIncomingHoldFixture(t, 1)
	f.demand(t)
	f.releaseOld()
	f.now = f.now.Add(FailedConnectPenalty / 2)
	for i := 0; i < 3; i++ {
		if !f.p.DeferRedundantCarrier(f.peer, 4096) || f.p.ReclaimIncomingWorkerFor(f.peer, 4096) {
			t.Fatal("known old radio refreshed demand or escaped its bounded scheduling gate")
		}
		if f.oldOutgoing() != nil {
			t.Fatal("old outgoing retry consumed the incoming opportunity")
		}
		f.old.Release() // Cleanup may release the same owner more than once.
	}
	setup := f.p.ReserveGroup("known-after-tls", meshid.Device{}, false, "4096")
	if setup == nil || setup.Ready(f.peer, func() {}) {
		t.Fatal("old incoming redundancy was not rejected using its actual identity")
	}
	setup.Release()
	f.now = f.now.Add(FailedConnectPenalty / 2)
	backup := f.oldOutgoing()
	if backup == nil {
		t.Fatal("known retries or duplicate cleanup self-renewed the hold forever")
	}
	backup.Release()
}

func TestIncomingDemandRequiresNewActualBindToClear(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		name := "ready-then-deferral"
		if admitted {
			name = "actual-admission"
		}
		t.Run(name, func(t *testing.T) {
			f := newIncomingHoldFixture(t, 2)
			f.demand(t)
			f.releaseOld()
			leaf := selectionDevice(11)
			setup := f.p.ReserveGroup("new-leaf", meshid.Device{}, false, "4096")
			if setup == nil || !setup.Ready(leaf, func() {}) {
				t.Fatal("new authenticated peer could not use the held opportunity")
			}
			defer setup.Release()
			if f.oldOutgoing() != nil {
				t.Fatal("Ready alone cleared the hold before actual Node admission")
			}
			if admitted {
				BindConnectionLink(WithConnectionLease(context.Background(), setup), 3, 4096, func() {})
				backup := f.oldOutgoing()
				if backup == nil {
					t.Fatal("new actual Bind did not clear demand")
				}
				backup.Release()
			} else {
				// A verified setup can still be refused by global Node capacity.
				// Its normal intentional completion must not consume the hold.
				markConnectionRetry(WithConnectionLease(context.Background(), setup))
				setup.Release()
				if f.oldOutgoing() != nil {
					t.Fatal("deferred verified setup cleared demand without admission")
				}
			}
		})
	}
}

func TestNewBindClearsPendingDemandBeforeOldCloserReleases(t *testing.T) {
	f := newIncomingHoldFixture(t, 2)
	f.demand(t)
	leaf := selectionDevice(11)
	setup := f.p.ReserveGroup("new-leaf", meshid.Device{}, false, "4096")
	if setup == nil || !setup.Ready(leaf, func() {}) {
		t.Fatal("spare slot did not admit a genuinely new peer")
	}
	defer setup.Release()
	BindConnectionLink(WithConnectionLease(context.Background(), setup), 3, 4096, func() {})
	f.now = f.now.Add(2 * FailedConnectPenalty)
	f.releaseOld()
	backup := f.oldOutgoing()
	if backup == nil {
		t.Fatal("late old-owner Release restarted demand already served by a new actual Bind")
	}
	backup.Release()
}

func TestIncomingDemandAllowsFasterCarrierAndLastPathRecovery(t *testing.T) {
	for _, mode := range []string{"faster", "alternate-lost"} {
		t.Run(mode, func(t *testing.T) {
			f := newIncomingHoldFixture(t, 1)
			f.demand(t)
			f.releaseOld()
			group := "64"
			if mode == "alternate-lost" {
				group = "4096"
				f.view.Links[0].ForwardingReady = false
			}
			candidate := f.p.ReserveGroup("usable-path", f.peer, true, group)
			if candidate == nil || !candidate.Ready(f.peer, func() {}) {
				t.Fatal("fairness hold blocked a faster carrier or recovery of the last working path")
			}
			candidate.Release()
		})
	}
}
