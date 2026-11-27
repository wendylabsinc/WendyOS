package localmesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

type creditLatencyLink struct {
	writes chan ControlMessage
	closed chan struct{}
	once   sync.Once
}

func (*creditLatencyLink) SendDatagram([]byte) error                       { return nil }
func (*creditLatencyLink) ReceiveDatagram(context.Context) ([]byte, error) { return nil, io.EOF }
func (*creditLatencyLink) ReadControl() (ControlMessage, error)            { return ControlMessage{}, io.EOF }
func (l *creditLatencyLink) WriteControl(m ControlMessage) error {
	select {
	case l.writes <- m:
		return nil
	case <-l.closed:
		return io.ErrClosedPipe
	}
}
func (l *creditLatencyLink) Close() error { l.once.Do(func() { close(l.closed) }); return nil }

// This uses Node.Run's real timers and event loop, and its actual control writer.
// No virtual scheduler or synthetic maintenance event participates in progress.
// The same fixture compiles unchanged against main for the latency comparison.
func TestAnnouncementCreditLatency(t *testing.T) {
	for _, capacity := range []int{1, 128} {
		for _, withReply := range []bool{false, true} {
			name := "announcement"
			if withReply {
				name = "reply-priority"
			}
			t.Run(fmt.Sprintf("capacity-%d/%s", capacity, name), func(t *testing.T) {
				self, remote := testCredentials(t)
				n := newActorTestNode(t, self, &handoffActorNetwork{})
				now := time.Now()
				if _, err := n.cache.Put(remote.Certificate.Certificate, now); err != nil {
					t.Fatal(err)
				}
				w, err := SignManifest(routeManifest(remote.Device, now), remote.Certificate.Certificate, remote.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := n.directory.Accept(w, now); err != nil || !changed {
					t.Fatalf("accept: %t %v", changed, err)
				}
				link := &creditLatencyLink{writes: make(chan ControlMessage, 256), closed: make(chan struct{})}
				p := &nodePeer{id: 1, device: remote.Device, link: link, control: make(chan ControlMessage, capacity), done: make(chan struct{}), sync: NewSynchronizer(n.directory, n.cache, n.controlHub.Announcements())}
				n.peers = map[babel.LinkID]*nodePeer{1: p}
				for range capacity {
					p.control <- ControlMessage{Kind: "identity-request", Principal: self.Device.Principal()}
				}
				p.sync.markAnnouncement(w, now)
				if withReply {
					if !n.queueReplies(p, []ControlMessage{{Kind: "identity-request", Principal: remote.Device.Principal()}}) || len(p.replies) != 1 {
						t.Fatal("failed to retain priority reply")
					}
				}
				n.flushAnnouncements(p, now)
				runDone, writerDone := make(chan error, 1), make(chan error, 1)
				go func() { runDone <- n.Run() }()
				writerStarted := false
				t.Cleanup(func() {
					n.cancel()
					_ = link.Close()
					select {
					case err := <-runDone:
						if !errors.Is(err, context.Canceled) {
							t.Errorf("actor exit: %v", err)
						}
					case <-time.After(3 * time.Second):
						t.Error("actor did not stop")
					}
					if writerStarted {
						select {
						case <-writerDone:
						case <-time.After(3 * time.Second):
							t.Error("writer did not stop")
						}
					}
				})
				ctx, cancel := context.WithTimeout(n.ctx, 3*time.Second)
				defer cancel()
				// The barrier means Run has originated its local proof and begun
				// the maintenance timer before the writer frees the first slot.
				if err := n.request(ctx, nodeEvent{}); err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				writerStarted = true
				go func() { writerDone <- n.writeControls(n.ctx, p) }()
				deadline := time.NewTimer(4 * time.Second)
				defer deadline.Stop()
				sawReply := !withReply
				for {
					select {
					case message := <-link.writes:
						if message.Kind == "identity-request" && message.Principal == remote.Device.Principal() {
							sawReply = true
						}
						if message.Gossip != nil && message.Gossip.Op == "have" && message.Gossip.ID() == manifestGossipID(w) {
							if !sawReply {
								t.Fatal("announcement overtook priority reply")
							}
							elapsed := time.Since(start)
							t.Logf("credit_resume_us=%d", elapsed.Microseconds())
							if os.Getenv("WENDY_EXPECT_FAST_CREDIT") == "1" && elapsed >= 500*time.Millisecond {
								t.Fatalf("waited for periodic maintenance: %v", elapsed)
							}
							return
						}
					case <-deadline.C:
						t.Fatal("pending announcement did not resume")
					}
				}
			})
		}
	}
}
