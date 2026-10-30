package vpn

import (
	"fmt"
	"slices"
	"testing"
)

func TestInternetFailoverRequiresConsecutiveEvidence(t *testing.T) {
	var state ReachabilityState
	for i, step := range []struct{ probe, online bool }{
		{false, true}, {true, true}, {false, true}, {false, false},
		{true, false}, {false, false}, {true, false}, {true, true},
	} {
		if got := state.Observe(step.probe); got != step.online {
			t.Fatalf("step %d: online=%v, want %v", i, got, step.online)
		}
	}
}

func TestRouteHashCollisionsIncludeEveryDevice(t *testing.T) {
	input := make([]Route, 1024)
	for i := range input {
		input[i] = Route{Selector: fmt.Sprintf("mesh:device-%d", i)}
	}
	forward, err := BuildRoutes(input)
	if err != nil {
		t.Fatal(err)
	}
	bySelector := map[string]string{}
	addresses := map[string]bool{}
	for _, r := range forward {
		if addresses[r.Address] {
			t.Fatal("duplicate address")
		}
		addresses[r.Address] = true
		bySelector[r.Selector] = r.Address
	}
	slices.Reverse(input)
	backward, err := BuildRoutes(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range backward {
		if bySelector[r.Selector] != r.Address {
			t.Fatal("discovery order changed route address")
		}
	}
}
