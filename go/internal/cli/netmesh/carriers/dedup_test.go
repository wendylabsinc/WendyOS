package carriers

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestRunSameCarrierOwnershipDoesNotWaitForForwarding(t *testing.T) {
	for _, attached := range []bool{false, true} {
		name := "authenticated-before-attachment"
		if attached {
			name = "attached-before-forwarding"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			peer := testDevice(2)
			first := Candidate{Transport: TCP, Address: "192.0.2.2:43024", Device: peer, Cost: 128}
			alias := first
			alias.Address = "192.0.2.3:43024"
			ble := Candidate{Transport: BLE, Address: "radio", PSM: 129, Device: peer, Cost: 4096}
			updates := make(chan []Candidate, 4)
			entered := make(chan Candidate, 8)
			release := make(chan struct{})
			var mu sync.Mutex
			view := localmesh.NodeSnapshot{}
			if attached {
				view.Links = []localmesh.PeerLink{{Device: peer, Transport: TCP, Cost: 128}}
			}
			cfg := testConfig(func(ctx context.Context, c Candidate) error {
				c.Authenticated(peer)
				entered <- c
				if c.Address == first.Address {
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				<-ctx.Done()
				return ctx.Err()
			}, sourceChannel(updates))
			cfg.Snapshot = func() localmesh.NodeSnapshot {
				mu.Lock()
				defer mu.Unlock()
				return localmesh.NodeSnapshot{Links: append([]localmesh.PeerLink(nil), view.Links...)}
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, cfg) }()
			if !attached {
				updates <- []Candidate{first}
				if got := wait(t, entered); got.Address != first.Address {
					t.Fatalf("initial carrier = %+v", got)
				}
			}
			updates <- []Candidate{alias, ble}
			if got := wait(t, entered); got.Transport != BLE {
				t.Fatalf("duplicate TCP alias dialed before forwarding: %+v", got)
			}
			select {
			case got := <-entered:
				t.Fatalf("same carrier duplicated: %+v", got)
			case <-time.After(40 * time.Millisecond):
			}
			// Ownership does not survive the actual connection's lifetime.
			mu.Lock()
			view.Links = nil
			mu.Unlock()
			close(release)
			if got := wait(t, entered); got.Address != alias.Address {
				t.Fatalf("alias did not recover after owner left: %+v", got)
			}
			cancel()
			wait(t, done)
		})
	}
}

func TestRunSameCarrierDedupUsesVerifiedIdentityAndEqualCost(t *testing.T) {
	for _, differentIdentity := range []bool{false, true} {
		name := "lower-cost-upgrade"
		if differentIdentity {
			name = "advertised-peer-is-not-authentication"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			peer := testDevice(2)
			verified := peer
			if differentIdentity {
				verified = testDevice(3)
			}
			first := Candidate{Transport: TCP, Address: "192.0.2.2:43024", Device: peer, Cost: 128}
			second := first
			second.Address = "192.0.2.3:43024"
			if !differentIdentity {
				second.Cost = 64
			}
			updates := make(chan []Candidate, 4)
			entered := make(chan Candidate, 4)
			cfg := testConfig(func(ctx context.Context, c Candidate) error {
				c.Authenticated(verified)
				entered <- c
				<-ctx.Done()
				return ctx.Err()
			}, sourceChannel(updates))
			done := make(chan error, 1)
			go func() { done <- Run(ctx, cfg) }()
			updates <- []Candidate{first}
			wait(t, entered)
			updates <- []Candidate{second}
			if got := wait(t, entered); got.Address != second.Address {
				t.Fatalf("independent candidate suppressed: %+v", got)
			}
			cancel()
			wait(t, done)
		})
	}
}
