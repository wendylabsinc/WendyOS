package localmesh

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

type capacityCauseLink struct {
	done, started chan struct{}
	once          sync.Once
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
