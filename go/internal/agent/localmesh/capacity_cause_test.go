package localmesh

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type capacityCauseLink struct {
	done, started chan struct{}
	once          sync.Once
}

type capacityCauseNetwork struct {
	*handoffActorNetwork
	tuns staleClockPacketBackend
}

func (n *capacityCauseNetwork) AddPeer(id babel.LinkID, peer meshid.Device) (io.ReadWriteCloser, error) {
	return n.tuns.AddPeer(id, peer)
}
func (n *capacityCauseNetwork) RemovePeer(id babel.LinkID) error { return n.tuns.RemovePeer(id) }

type blockedCapacityCauseLink struct {
	*capacityCauseLink
	entered, release chan struct{}
	closeOnce        sync.Once
}

func (l *blockedCapacityCauseLink) Close() error {
	l.closeOnce.Do(func() { close(l.entered) })
	<-l.release
	return l.capacityCauseLink.Close()
}

func TestGlobalCapacityClosingLeasePreventsSecondProviderRetirement(t *testing.T) {
	f := newCarrierStandbyFixture(t)
	f.addCarrier(128, "infrastructure-wifi")
	f.pump(0)
	f.advance(6*time.Second, 0)
	n := f.nodes[0]
	backend := &capacityCauseNetwork{handoffActorNetwork: f.networks[0]}
	n.kernel = backend
	defer backend.tuns.Close()
	peer := f.nodes[1].Credentials.Device
	policy := NewConnectionPolicy(n.Credentials.Device, true, ConnectionLimits{Outgoing: 2, Attempts: 2, Total: 2})
	policy.SetSnapshot(n.Snapshot)
	first := &capacityCauseLink{done: make(chan struct{}), started: make(chan struct{})}
	blocked := &blockedCapacityCauseLink{capacityCauseLink: &capacityCauseLink{done: make(chan struct{}), started: make(chan struct{})}, entered: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocked.release) }) }
	defer release()
	attach := func(key string, cost uint16, link nodeLink, started <-chan struct{}) (*ConnectionLease, *nodePeer, context.CancelFunc, <-chan error) {
		t.Helper()
		lease := policy.ReserveGroup(key, peer, true, key)
		if lease == nil {
			t.Fatal("missing owned standby lease")
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		done := make(chan error, 1)
		go func() { done <- n.attachLink(WithConnectionLease(ctx, lease), peer, cost, link) }()
		var add nodeEvent
		select {
		case add = <-n.events:
		case <-time.After(time.Second):
			t.Fatal("missing standby actor admission")
		}
		add.result <- n.handle(add)
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("standby workers did not start")
		}
		return lease, add.add, cancel, done
	}
	firstLease, firstPeer, cancelFirst, firstDone := attach("first", 4096, first, first.started)
	defer firstLease.Release()
	blockedLease, blockedPeer, _, blockedDone := attach("blocked", 8192, blocked, blocked.started)
	defer blockedLease.Release()
	for id := babel.LinkID(5); len(n.peers) < 64; id++ {
		n.peers[id] = &nodePeer{id: id, device: selectionDevice(100 + int(id)), cost: 128, retire: make(chan struct{}), link: &handoffActorLink{}}
		n.nextLink = id
	}
	n.refresh()
	if !n.reclaimGlobalRedundancy(selectionDevice(999)) || n.capacityRetiring != blockedPeer.id {
		t.Fatal("global capacity did not choose exact slower owned standby")
	}
	select {
	case <-blocked.entered:
	case <-time.After(time.Second):
		t.Fatal("global victim Close was not entered")
	}
	if !blockedLease.Retired() || policy.Counts().Total != 2 {
		t.Fatal("global cause was not published before blocking Close")
	}
	assertNoSecond := func() {
		t.Helper()
		if policy.ReservePreferred("new", selectionDevice(999), true, "", true) != nil || firstLease.Retired() || firstPeer.retiring() || policy.Counts().Total != 2 {
			t.Fatal("global close pending provoked a second provider retirement")
		}
	}
	assertNoSecond()
	select {
	case remove := <-n.events:
		if remove.id != blockedPeer.id || !remove.remove {
			t.Fatal("global victim requested wrong removal")
		}
		remove.result <- n.handle(remove)
	case <-time.After(time.Second):
		t.Fatal("global victim did not request ordinary Node removal")
	}
	// The Node slot is now free, but the provider's exact lifetime owner has
	// not joined blocked Close. Its lease still prevents another reclamation.
	assertNoSecond()
	select {
	case <-blockedDone:
		t.Fatal("Attach returned without joining its blocked closer")
	default:
	}
	release()
	select {
	case err := <-blockedDone:
		if !errors.Is(err, ErrCarrierCapacityRetry) {
			t.Fatalf("capacity cause lost: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("global victim did not join Close")
	}
	blockedLease.Release()
	if next := policy.ReservePreferred("new", selectionDevice(999), true, "", true); next == nil {
		t.Fatal("normal owner release did not free provider slot")
	} else {
		next.Release()
	}
	if firstPeer.retiring() || firstLease.Retired() {
		t.Fatal("remaining standby was needlessly retired")
	}
	cancelFirst()
	select {
	case remove := <-n.events:
		remove.result <- n.handle(remove)
	case <-time.After(time.Second):
		t.Fatal("remaining standby did not clean up")
	}
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("remaining standby did not join")
	}
}

func (l *capacityCauseLink) Close() error                    { l.once.Do(func() { close(l.done) }); return nil }
func (*capacityCauseLink) SendDatagram([]byte) error         { return nil }
func (*capacityCauseLink) WriteControl(ControlMessage) error { return nil }
func (l *capacityCauseLink) ReadControl() (ControlMessage, error) {
	close(l.started)
	<-l.done
	return ControlMessage{}, io.EOF
}
func (l *capacityCauseLink) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-l.done:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestOwnedCapacityRetirementReturnsIntentionalCauseThroughAttach(t *testing.T) {
	for _, cause := range []string{"provider", "global-node", "babel-expiry", "babel-expiry-then-capacity"} {
		t.Run(cause, func(t *testing.T) {
			self, credentials := testCredentials(t)
			n, err := NewNodeWithNetwork(context.Background(), t.TempDir(), self, "capacity-cause", 50052, &staleClockPacketBackend{})
			if err != nil {
				t.Fatal(err)
			}
			defer n.cancel()
			defer n.kernel.Close()
			policy := NewConnectionPolicy(self.Device, true, ConnectionLimits{Outgoing: 1, Attempts: 1, Total: 1})
			lease := policy.Reserve("old", credentials.Device, true)
			outer := WithConnectionLease(context.Background(), lease)
			link := &capacityCauseLink{done: make(chan struct{}), started: make(chan struct{})}
			defer link.Close()
			done := make(chan error, 1)
			go func() { done <- n.attachLink(outer, credentials.Device, 512, link) }()
			var add nodeEvent
			select {
			case add = <-n.events:
			case <-time.After(time.Second):
				t.Fatal("missing actor registration")
			}
			add.result <- n.handle(add)
			select {
			case <-link.started:
			case <-time.After(time.Second):
				t.Fatal("attachment workers did not start")
			}
			if cause == "provider" {
				policy.SetSnapshot(func() NodeSnapshot {
					return NodeSnapshot{Links: []PeerLink{{ID: add.add.id, Device: credentials.Device, Cost: 512}, {ID: add.add.id + 1, Device: credentials.Device, Cost: 128, ForwardingReady: true}}}
				})
				if policy.ReservePreferred("next", selectionDevice(999), true, "", true) != nil || !lease.Retired() {
					t.Fatal("provider did not reclaim exact bound owned carrier")
				}
			} else if cause == "global-node" {
				add.add.signalCapacityRetire()
			} else {
				add.add.signalRetire()
				if cause == "babel-expiry-then-capacity" {
					add.add.signalCapacityRetire()
				}
			}
			select {
			case remove := <-n.events:
				if !remove.remove || remove.id != add.add.id {
					t.Fatalf("wrong actor cleanup %+v", remove)
				}
				remove.result <- n.handle(remove)
			case <-time.After(time.Second):
				t.Fatal("owned closer did not request exact normal removal")
			}
			select {
			case err := <-done:
				intentional := cause == "provider" || cause == "global-node"
				if errors.Is(err, ErrCarrierCapacityRetry) != intentional || lease.Retired() != intentional {
					t.Fatalf("capacity cause lost or ordinary liveness misclassified: err=%v retired=%v", err, lease.Retired())
				}
			case <-time.After(time.Second):
				t.Fatal("owned attachment did not join normal cleanup")
			}
			if outer.Err() != nil || policy.Counts().Total != 1 {
				t.Fatal("inner completion changed provider context or released its lease early")
			}
			lease.Release()
			if policy.Counts().Total != 0 {
				t.Fatal("provider owner could not release intentional completion")
			}
		})
	}
}
