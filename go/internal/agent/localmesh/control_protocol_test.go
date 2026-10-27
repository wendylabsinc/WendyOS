package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func TestCatalogueControlFragmentsAndLinkIncarnations(t *testing.T) {
	a, b := NewControlHub(), NewControlHub()
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	b.Add(ControlPeer{ID: 7, Device: device}, func(context.Context, []byte) error { return nil }, func() {})
	a.Add(ControlPeer{ID: 3, Device: device}, func(_ context.Context, frame []byte) error {
		if len(frame) > MaxCatalogFrame {
			t.Fatal("unbounded control frame")
		}
		return b.Deliver(7, frame)
	}, func() {})
	<-a.Events()
	<-b.Events()
	data := bytes.Repeat([]byte("x"), MaxCatalogMessage)
	if err := a.Send(context.Background(), 3, data); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-b.Events():
		if got.Peer.ID != 7 || !bytes.Equal(got.Data, data) {
			t.Fatal("fragmented body changed")
		}
	default:
		t.Fatal("body not delivered")
	}
	a.Remove(3)
	if a.Send(context.Background(), 3, []byte("old")) == nil {
		t.Fatal("stale link accepted send")
	}
	b.Remove(7)
	if b.Deliver(7, []byte("stale")) == nil {
		t.Fatal("stale receive accepted")
	}
}
func TestCatalogueControlMalformedFragmentsAndCancellation(t *testing.T) {
	hub := NewControlHub()
	device := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	hub.Add(ControlPeer{ID: 1, Device: device}, func(ctx context.Context, _ []byte) error { <-ctx.Done(); return ctx.Err() }, func() {})
	frame := make([]byte, 9)
	binary.BigEndian.PutUint32(frame, uint32(MaxCatalogMessage+1))
	if hub.Deliver(1, frame) == nil {
		t.Fatal("oversized allocation accepted")
	}
	binary.BigEndian.PutUint32(frame, 10)
	binary.BigEndian.PutUint32(frame[4:], 1)
	if hub.Deliver(1, frame) == nil {
		t.Fatal("out-of-order fragment accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if err := hub.Send(ctx, 1, []byte("a")); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestClosedCatalogueHubAllowsReentrantPeerCleanup(t *testing.T) {
	hub := NewControlHub()
	hub.Close()
	done := make(chan struct{})
	go func() {
		hub.Add(ControlPeer{ID: 1}, func(context.Context, []byte) error { return nil }, func() {
			// Transport cleanup may remove its endpoint from the same hub.
			hub.Remove(1)
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("late peer registration deadlocked transport cleanup")
	}
}
