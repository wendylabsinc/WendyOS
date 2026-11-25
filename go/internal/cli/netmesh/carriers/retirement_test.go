package carriers

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The shared Node owns exact-link retirement. Run must release its existing
// slot on completion, preserving the configured cap and explicit seed intent.
func TestRunFullSlotRecoversAfterOwnedCarrierRetires(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "strict-pinned-same-seed", true: "automatic-fallback"}[automatic], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			retired := make(chan struct{})
			entered := make(chan Candidate, 8)
			peer := testDevice(2)
			calls := 0 // Connect is serialized by the single configured slot.
			cfg := testConfig(func(ctx context.Context, c Candidate) error {
				calls++
				c.Authenticated(peer)
				entered <- c
				if calls == 1 {
					select {
					case <-retired:
						return errors.New("Node retired the expired Babel link")
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				<-ctx.Done()
				return ctx.Err()
			})
			seed := Candidate{Transport: TCP, Address: netAddress(2), Device: peer, Cost: 128}
			cfg.Seeds = []Candidate{seed}
			cfg.Sources = []Source{} // Strict intent never starts discovery.
			cfg.Limits.Connections, cfg.Limits.Attempts, cfg.Limits.BLEConnections = 1, 1, 1
			updates := make(chan []Candidate, 1)
			if automatic {
				cfg.Sources = []Source{sourceChannel(updates)}
			}
			done := make(chan error, 1)
			go func() { done <- Run(ctx, cfg) }()
			if c := wait(t, entered); c.key() != seed.key() {
				t.Fatalf("first attachment changed pinned seed: %+v", c)
			}
			if automatic {
				updates <- []Candidate{{Transport: BLE, Address: "radio", Device: peer, PSM: 129, Cost: 4096}}
			}
			select {
			case <-entered:
				t.Fatal("allocated beyond the one-carrier limit before retirement")
			case <-time.After(30 * time.Millisecond):
			}
			close(retired)
			next := wait(t, entered)
			if automatic {
				if next.Transport != BLE {
					t.Fatalf("retired full slot did not admit the healthy alternate: %+v", next)
				}
			} else if next.key() != seed.key() {
				t.Fatalf("strict pinned recovery switched endpoint: %+v", next)
			}
			select {
			case <-entered:
				t.Fatal("replacement exceeded capacity or churned while retained")
			case <-time.After(30 * time.Millisecond):
			}
			cancel()
			wait(t, done)
		})
	}
}
