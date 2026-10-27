package meshcatalog

import (
	"bytes"
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

// A busy shared link can make steady progress while a catalogue message waits
// behind routing and application traffic. This must not remove the route or
// abort a partly delivered logical message merely because of its queue age.
func TestCatalogueSenderPreservesLinkUnderBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		c, _ := f.newCatalog(t, p533, "default", nil, nil)
		r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
		hub, remote := localmesh.NewControlHub(), localmesh.NewControlHub()
		peer := localmesh.ControlPeer{ID: 1, Device: principalOf(t, p534)}
		remote.Add(peer, nil, func() {})
		<-remote.Events() // Peer-added notification precedes message delivery.
		blocked, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
		first := true
		hub.Add(peer, func(ctx context.Context, frame []byte) error {
			if err := remote.Deliver(peer.ID, frame); err != nil {
				return err
			}
			if first {
				first = false
				close(blocked)
				select {
				case <-release:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}, func() { closed <- struct{}{} })
		r.hub = hub
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.syncPeers(ctx)
		p := r.peers[peer.ID]
		body, err := inventoryWire(inventoryMessage{Message: Message{Kind: "bundle", Bundle: [][]byte{bytes.Repeat([]byte("a"), 20<<10)}}})
		if err != nil {
			t.Fatal(err)
		}
		p.queued.Add(int64(len(body)))
		p.out <- body
		<-blocked
		time.Sleep(46 * time.Second)
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("catalogue queue delay closed a healthy neighbour link")
		default:
		}
		close(release)
		synctest.Wait()
		select {
		case event := <-remote.Events():
			if !bytes.Equal(event.Data, body) {
				t.Fatal("fragmented catalogue message changed")
			}
		default:
			t.Fatal("catalogue sender did not resume after backpressure cleared")
		}
		if p.queued.Load() != 0 {
			t.Fatal("completed message remains in bounded byte accounting")
		}
		next, err := inventoryWire(inventoryMessage{Message: Message{Kind: "snapshot-done"}})
		if err != nil {
			t.Fatal(err)
		}
		p.queued.Add(int64(len(next)))
		p.out <- next
		synctest.Wait()
		select {
		case event := <-remote.Events():
			if !bytes.Equal(event.Data, next) {
				t.Fatal("next catalogue message lost framing")
			}
		default:
			t.Fatal("next catalogue message was not delivered")
		}
		cancel()
		synctest.Wait()
	})
}

// Cancellation must still stop a backpressured sender and close its partial
// logical stream, without leaving an unbounded worker behind.
func TestCatalogueSenderCancelsPartialMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		c, _ := f.newCatalog(t, p533, "default", nil, nil)
		r, _ := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
		r.hub = localmesh.NewControlHub()
		peer := localmesh.ControlPeer{ID: 1, Device: principalOf(t, p534)}
		started, closed := make(chan struct{}), make(chan struct{})
		r.hub.Add(peer, func(ctx context.Context, frame []byte) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}, func() { close(closed) })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r.syncPeers(ctx)
		p := r.peers[peer.ID]
		data, err := inventoryWire(inventoryMessage{Message: Message{Kind: "bundle", Bundle: [][]byte{bytes.Repeat([]byte("a"), 20<<10)}}})
		if err != nil {
			t.Fatal(err)
		}
		p.queued.Add(int64(len(data)))
		p.out <- data
		<-started
		cancel()
		synctest.Wait()
		select {
		case <-closed:
		default:
			t.Fatal("canceled catalogue sender left partial stream open")
		}
		if p.queued.Load() != 0 {
			t.Fatal("canceled in-flight message remains accounted")
		}
	})
}
