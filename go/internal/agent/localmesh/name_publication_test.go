package localmesh

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestNodeFriendlyNameUsesSignedPublicationGate(t *testing.T) {
	n := routePublicationNode(t)
	if err := n.Persist(n.routing.Checkpoint()); err != nil {
		t.Fatal(err)
	}
	first := n.directory.Snapshot(time.Now())[0]
	name := "jetson-a"
	n.SetNameSource(func() string { return name })
	if err := n.refreshName(); err != nil {
		t.Fatal(err)
	}
	if current := n.directory.Snapshot(time.Now())[0]; current.Name != first.Name || current.Revision != first.Revision {
		t.Fatal("unsigned name escaped publication gate")
	}
	n.nextPublication = time.Now().Add(-time.Millisecond)
	if err := n.publish(); err != nil {
		t.Fatal(err)
	}
	current := n.directory.Snapshot(time.Now())[0]
	if current.Name != "jetson-a" || current.Revision != first.Revision+1 || current.Route == nil || !n.Snapshot().RouteAuthorizations[*current.Route] {
		t.Fatalf("signed name = %+v", current)
	}
	name = "jetson--a"
	if err := n.refreshName(); err != nil {
		t.Fatal(err)
	}
	n.nextPublication = time.Now().Add(-time.Millisecond)
	if err := n.publish(); err != nil {
		t.Fatal(err)
	}
	renamed := n.directory.Snapshot(time.Now())[0]
	if renamed.Name != name || renamed.Revision != current.Revision+1 {
		t.Fatalf("renamed announcement = %+v", renamed)
	}
	name = "not a label"
	if err := n.refreshName(); err != nil {
		t.Fatal(err)
	}
	if n.advertisement.Name != renamed.Name {
		t.Fatal("invalid source replaced last valid name")
	}
}

func TestNodeRefreshesFriendlyNameDuringRun(t *testing.T) {
	credentials, _ := testCredentials(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := NewNodeWithNetwork(ctx, t.TempDir(), credentials, "boot-hostname", 50052, &lifecyclePacketNetwork{})
	if err != nil {
		t.Fatal(err)
	}
	var name atomic.Value
	name.Store("jetson-a")
	n.SetNameSource(func() string { return name.Load().(string) })
	done := make(chan error, 1)
	go func() { done <- n.Run() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("node did not stop")
		}
	})
	waitName := func(want string) uint64 {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			for _, m := range n.Snapshot().Devices {
				if m.Device == credentials.Device.UUID() && m.Name == want {
					return m.Revision
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("signed name did not become %q", want)
		return 0
	}
	before := waitName("jetson-a")
	name.Store("jetson--a")
	after := waitName("jetson--a")
	if after <= before {
		t.Fatal("live rename did not advance signed revision")
	}
}

func TestNodeFriendlyNamePublicationFailureRetainsPreviousIntent(t *testing.T) {
	n := routePublicationNode(t)
	if err := n.Persist(n.routing.Checkpoint()); err != nil {
		t.Fatal(err)
	}
	previous := n.advertisement.Name
	n.SetNameSource(func() string { return "jetson" })
	n.nextPublication = time.Now().Add(-time.Millisecond)
	n.store.path = t.TempDir()
	if err := n.refreshName(); err == nil {
		t.Fatal("name publication succeeded without durable state")
	}
	if n.advertisement.Name != previous {
		t.Fatal("failed publication retained rejected name intent")
	}
	if got := n.directory.Snapshot(time.Now())[0].Name; got != previous {
		t.Fatalf("failed name escaped into signed directory: %q", got)
	}
}
