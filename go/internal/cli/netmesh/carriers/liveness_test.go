package carriers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestRunStaleAttachedLANCannotVetoOrRetireBLE(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	peer := testDevice(2)
	var mu sync.Mutex
	lanReady := false
	entered, ended := make(chan struct{}, 8), make(chan struct{}, 8)
	updates := make(chan []Candidate, 8)
	cfg := testConfig(func(ctx context.Context, candidate Candidate) error {
		candidate.Authenticated(peer)
		entered <- struct{}{}
		<-ctx.Done()
		ended <- struct{}{}
		return ctx.Err()
	}, sourceChannel(updates))
	cfg.Snapshot = func() localmesh.NodeSnapshot {
		mu.Lock()
		defer mu.Unlock()
		return localmesh.NodeSnapshot{Links: []localmesh.PeerLink{{ID: 1, Device: peer, Cost: 128, ForwardingReady: lanReady}}}
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()
	ble := Candidate{Transport: BLE, Device: peer, Address: "radio", PSM: 129, Cost: 4096}
	updates <- []Candidate{ble}
	wait(t, entered) // Dead but still attached LAN cannot suppress a fresh dial.
	stillAttached := func() {
		t.Helper()
		select {
		case <-ended:
			t.Fatal("stale attached LAN retired authenticated BLE")
		case <-entered:
			t.Fatal("stable eligibility churned BLE")
		case <-time.After(30 * time.Millisecond):
		}
	}
	stillAttached()
	// Repeated discovery hints do not supply local forwarding readiness.
	changed := ble
	changed.Hint[0] = 7
	updates <- []Candidate{changed}
	updates <- []Candidate{ble}
	stillAttached()
	mu.Lock()
	lanReady = true
	mu.Unlock()
	wait(t, ended) // A restored, installed cheaper path still prunes BLE.
	select {
	case <-entered:
		t.Fatal("BLE redialed while cheaper forwarding stayed ready")
	case <-time.After(30 * time.Millisecond):
	}
	mu.Lock()
	lanReady = false
	mu.Unlock()
	wait(t, entered) // No socket-close event or advertisement refresh is needed.
	stillAttached()
	cancel()
	wait(t, done)
}
