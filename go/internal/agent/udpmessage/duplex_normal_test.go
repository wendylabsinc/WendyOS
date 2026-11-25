package udpmessage

import (
	"sync"
	"testing"
	"time"
)

// A slow ordered carrier may fill its TCP write window while waiting for the
// application to drain replies. Reply delivery must remain able to make progress.
func TestNormalDuplexDeliveryWhileTransportBackpressured(t *testing.T) {
	manager := NewManager()
	defer manager.Close()
	peer := testPeer(t, 1)
	endpoint, err := manager.Open(peer, func(action func() error) error { return action() })
	if err != nil {
		t.Fatal(err)
	}
	defer endpoint.Close()
	started, drained := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(drained) }) }
	defer release()
	if err = endpoint.SetTransport(MaxChunkSize, true, func([]byte) error { close(started); <-drained; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = endpoint.Send([]byte("ordinary full-duplex request")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("write did not start")
	}
	delivery := make(chan error, 1)
	go func() { delivery <- endpoint.Deliver(func() error { release(); return nil }) }()
	select {
	case err = <-delivery:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(250 * time.Millisecond):
		release()
		<-delivery
		t.Fatal("reply delivery waited for the backpressured write")
	}
}
