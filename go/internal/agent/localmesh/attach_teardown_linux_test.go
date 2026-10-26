//go:build linux

package localmesh

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type teardownTestLink struct {
	done    chan struct{}
	reading chan struct{}
	once    sync.Once
}

func (l *teardownTestLink) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}
func (*teardownTestLink) SendDatagram([]byte) error         { return nil }
func (*teardownTestLink) WriteControl(ControlMessage) error { return nil }
func (l *teardownTestLink) ReadControl() (ControlMessage, error) {
	close(l.reading)
	<-l.done
	return ControlMessage{}, io.EOF
}
func (l *teardownTestLink) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, io.EOF
	}
}

// Hold the event loop after registration, then cancel while its next real FIB
// operation is still pending. The carrier is controlled; the TUN, rules and
// route replacement use the production Kernel in a disposable namespace.
func TestAttachCancellationKeepsKernelOwnedTUN(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires disposable privileged container network namespace")
	}
	for _, registrationReplied := range []bool{true, false} {
		name := "active-workers"
		if !registrationReplied {
			name = "registration-response-pending"
		}
		t.Run(name, func(t *testing.T) {
			self, peer := testCredentials(t)
			node, err := NewNode(context.Background(), t.TempDir(), self, "teardown", 50052)
			if err != nil {
				t.Fatal(err)
			}
			link := &teardownTestLink{done: make(chan struct{}), reading: make(chan struct{})}
			t.Cleanup(func() {
				node.cancel()
				_ = link.Close()
				if err := node.kernel.Close(); err != nil {
					t.Error(err)
				}
			})
			attached := make(chan error, 1)
			go func() { attached <- node.attachLink(node.ctx, peer.Device, 256, link) }()
			var event nodeEvent
			select {
			case event = <-node.events:
			case <-time.After(5 * time.Second):
				t.Fatal("carrier did not request registration")
			}
			if err := node.handle(event); err != nil {
				t.Fatal(err)
			}
			if registrationReplied {
				event.result <- nil
				select {
				case <-link.reading:
				case <-time.After(5 * time.Second):
					t.Fatal("registered carrier did not start its workers")
				}
			}
			// No removal event is handled before the held kernel operation.
			node.cancel()
			var attachErr error
			returned := false
			select {
			case attachErr = <-attached:
				returned = true
			case <-time.After(200 * time.Millisecond):
			}
			_, nextHop, err := meshid.LinkLocalAddresses(self.Device, peer.Device)
			if err != nil {
				t.Fatal(err)
			}
			route := babel.Route{Prefix: netip.PrefixFrom(peer.Device.MeshAddress(), 128), Link: event.add.id, NextHop: nextHop, Metric: 256}
			if err := node.kernel.Apply([]babel.Route{route}); err != nil {
				t.Fatalf("carrier destroyed the kernel-owned TUN before pending FIB replacement: %v", err)
			}
			if _, err := event.add.tun.Stat(); err != nil {
				t.Fatalf("kernel still owns a closed TUN: %v", err)
			}
			if err := node.kernel.Apply(nil); err != nil {
				t.Fatal(err)
			}
			linkName := node.kernel.peers[event.add.id].link.Name
			if err := node.kernel.RemovePeer(event.add.id); err != nil {
				t.Fatal(err)
			}
			if _, err := netlink.LinkByName(linkName); err == nil {
				t.Fatal("kernel removal leaked the TUN")
			}
			if _, err := event.add.tun.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("kernel removal left the descriptor open: %v", err)
			}
			if !returned {
				select {
				case attachErr = <-attached:
				case <-time.After(5 * time.Second):
					t.Fatal("carrier workers did not finish after kernel removal")
				}
			}
			if !errors.Is(attachErr, context.Canceled) {
				t.Fatalf("canceled attachment returned %v", attachErr)
			}
		})
	}
}
