package meshsession

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

type generationObservedCarrier struct {
	localmesh.DirectRecordCarrier
	reading chan struct{}
	once    sync.Once
}

func (c *generationObservedCarrier) ReceiveDirectRecord(ctx context.Context) ([]byte, error) {
	c.once.Do(func() { close(c.reading) })
	return c.DirectRecordCarrier.ReceiveDirectRecord(ctx)
}

func TestCarrierGenerationJoinsParkedLaneOnRuntimeStop(t *testing.T) {
	m, a := directManagerFixture(t)
	upperClosed, err := m.acquireUpper(context.Background(), a.Peer)
	if err != nil {
		t.Fatal(err)
	}
	defer upperClosed()
	a.Deferred = true
	c := &generationObservedCarrier{DirectRecordCarrier: a.Records, reading: make(chan struct{})}
	a.Records = c
	handler, release, err := m.CarrierGeneration()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() { done <- handler(context.Background(), a) }()
	select {
	case <-c.reading:
	case <-time.After(time.Second):
		t.Fatal("parked carrier did not start")
	}
	released := make(chan struct{})
	go func() { release(); close(released) }()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("runtime stop did not join parked carrier")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("carrier handler survived runtime stop")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.peers) != 0 || m.carrierBound {
		t.Fatal("runtime stop retained application epochs")
	}
}
