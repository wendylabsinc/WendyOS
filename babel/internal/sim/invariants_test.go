package sim

import (
	babel "github.com/wendylabsinc/WendyOS/babel"
	"testing"
)

func TestForwardingInvariantDetectsInjectedLoop(t *testing.T) {
	s, err := New(2)
	must(t, err)
	a, b, err := s.Connect(0, 1, 96)
	must(t, err)
	// Deliberately corrupt the forwarder, proving the checker is not merely
	// comparing advertised reachability or trusting the engine's own verdict.
	s.FIB[0] = []babel.Route{{Prefix: prefix(1), RouterID: 2, Link: a.Link, Metric: 96}}
	s.FIB[1] = []babel.Route{{Prefix: prefix(1), RouterID: 2, Link: b.Link, Metric: 192}}
	if _, err = s.Walk(0, prefix(1).Addr()); err == nil {
		t.Fatal("injected loop not detected")
	}
}
