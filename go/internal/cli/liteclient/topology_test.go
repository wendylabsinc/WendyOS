package liteclient

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
)

func TestTopologyPageBudgetAlsoBoundsBackpressuredWrite(t *testing.T) {
	client, _ := connectedOverPipe(t)
	// An established management peer temporarily stops draining. The command
	// write must observe the page budget, before its response timer can start.
	done := make(chan error, 1)
	go func() { _, err := client.NetTopology(context.Background()); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("backpressured topology write = %v", err)
		}
	case <-time.After(litetopology.PageBudget + time.Second):
		t.Fatal("topology write outlived its page budget")
	}
	select {
	case <-client.Done():
	case <-time.After(time.Second):
		t.Fatal("private topology connection was not retired")
	}
}
