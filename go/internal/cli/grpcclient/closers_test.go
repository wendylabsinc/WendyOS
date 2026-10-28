package grpcclient

import (
	"sync"
	"sync/atomic"
	"testing"
)

type countedOwner struct{ closed *atomic.Int32 }

func (c countedOwner) Close() error { c.closed.Add(1); return nil }

// Parallel service hooks can acquire helpers while a normal run finishes.
// Every helper has exactly one owner even if it arrives during shutdown.
func TestConnectionNormalConcurrentHelperOwnershipAndClose(t *testing.T) {
	var closed atomic.Int32
	connection := &AgentConnection{}
	var workers sync.WaitGroup
	start := make(chan struct{})
	for range 64 {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; connection.AddCloser(countedOwner{&closed}) }()
	}
	close(start)
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if got := closed.Load(); got != 64 {
		t.Fatalf("closed %d of 64 invocation helpers", got)
	}
}
