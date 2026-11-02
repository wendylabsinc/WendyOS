package localmesh

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type lifecyclePacketNetwork struct {
	reconcileError error
	reconciled     atomic.Int32
	closed         atomic.Int32
	stopped        atomic.Int32
}

func (n *lifecyclePacketNetwork) ReconcileBase() error { n.reconciled.Add(1); return n.reconcileError }
func (*lifecyclePacketNetwork) AddPeer(babel.LinkID, meshid.Device) (io.ReadWriteCloser, error) {
	return nil, errors.New("no physical carrier in lifecycle fixture")
}
func (*lifecyclePacketNetwork) RemovePeer(babel.LinkID) error { return nil }
func (*lifecyclePacketNetwork) Apply([]babel.Route) error     { return nil }
func (n *lifecyclePacketNetwork) Stop() error                 { n.stopped.Add(1); return nil }
func (n *lifecyclePacketNetwork) Close() error                { n.closed.Add(1); return nil }

// The real Node originates and signs its route using the injected packet
// backend on every platform, without opening TUN/netlink or changing the host.
func TestNodeWithPacketNetworkPublishesAndCloses(t *testing.T) {
	credentials, _ := testCredentials(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := &lifecyclePacketNetwork{}
	node, err := NewNodeWithNetwork(ctx, t.TempDir(), credentials, "portable", 50052, backend)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	finished := false
	go func() { done <- node.Run() }()
	t.Cleanup(func() {
		cancel()
		if finished {
			return
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("packet-backed Node did not stop")
		}
	})
	deadline := time.After(5 * time.Second)
	for {
		snapshot := node.Snapshot()
		if len(snapshot.Devices) == 1 {
			manifest := snapshot.Devices[0]
			if manifest.Device != credentials.Device.UUID() || manifest.Route == nil || !snapshot.RouteAuthorizations[*manifest.Route] {
				t.Fatal("packet-backed Node did not publish its signed origin")
			}
			break
		}
		select {
		case err := <-done:
			finished = true
			t.Fatalf("Node ended before publication: %v", err)
		case <-deadline:
			t.Fatal("packet-backed Node did not publish")
		case <-time.After(time.Millisecond):
		}
	}
	if backend.reconciled.Load() == 0 {
		t.Fatal("Node skipped backend reconciliation")
	}
	cancel()
	select {
	case err := <-done:
		finished = true
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Node did not stop")
	}
	if backend.closed.Load() != 1 || backend.stopped.Load() != 1 {
		t.Fatal("Node did not stop and close the supplied network exactly once")
	}
}
func TestNodeWithPacketNetworkClosesAfterSetupFailure(t *testing.T) {
	credentials, _ := testCredentials(t)
	unavailable := errors.New("packet backend unavailable")
	backend := &lifecyclePacketNetwork{reconcileError: unavailable}
	node, err := NewNodeWithNetwork(context.Background(), t.TempDir(), credentials, "portable", 50052, backend)
	if err != nil {
		t.Fatal(err)
	}
	if err = node.Run(); !errors.Is(err, unavailable) {
		t.Fatal("lost backend setup failure", err)
	}
	if backend.closed.Load() != 1 {
		t.Fatal("failed Node did not close the supplied packet network")
	}
}
