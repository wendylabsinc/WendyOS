package timesync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"github.com/wendylabsinc/wendy/go/internal/testfixture/clockproof"
)

func TestRelayedClockRequiresFreshIndependentAuthorityQuorum(t *testing.T) {
	for _, mode := range []string{"valid", "degraded", "single", "wrong-nonce", "wrong-signature", "wrong-authority", "disagree", "oversized", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			authorities := []clockproof.Authority{clockproof.New(t, "one", now), clockproof.New(t, "two", now), clockproof.New(t, "three", now)}
			if mode == "disagree" {
				authorities[1] = clockproof.New(t, "two", now.Add(10*time.Minute))
				authorities[2] = clockproof.New(t, "three", now.Add(20*time.Minute))
			}
			servers := make([]roughtime.Server, len(authorities))
			for i, a := range authorities {
				servers[i] = a.Server
			}
			m := NewManager(nil, "")
			var applied []time.Time
			m.apply = func(at time.Time) { applied = append(applied, at) }
			var mu sync.Mutex
			nonces := map[string]bool{}
			relay := func(ctx context.Context, index uint8, nonce []byte) ([]byte, error) {
				mu.Lock()
				if nonces[string(nonce)] {
					t.Error("receiver reused a nonce")
				}
				nonces[string(nonce)] = true
				mu.Unlock()
				if mode == "single" && index != 0 || mode == "degraded" && index > 1 {
					return nil, errors.New("offline")
				}
				if mode == "oversized" {
					return make([]byte, 4097), nil
				}
				if mode == "wrong-nonce" {
					nonce = make([]byte, 32)
				}
				if mode == "wrong-authority" {
					index = (index + 1) % uint8(len(authorities))
				}
				raw := authorities[index].Reply(nonce)
				if mode == "wrong-signature" {
					raw[len(raw)-1] ^= 1
				}
				return raw, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "canceled" {
				cancel()
			}
			err := m.syncVia(ctx, servers, relay)
			if mode == "valid" || mode == "degraded" {
				if err != nil || len(applied) != 1 || applied[0].Sub(now).Abs() > 2*time.Second {
					t.Fatalf("verified recovery failed: applied=%v err=%v", applied, err)
				}
				c, ok := m.LatestConsensus()
				wantQuorum := 3
				if mode == "degraded" {
					wantQuorum = 2
				}
				if !ok || c.Quorum != wantQuorum {
					t.Fatalf("missing independently verified evidence: %+v", c)
				}
			} else if err == nil || len(applied) != 0 {
				t.Fatalf("%s moved clock: %v %v", mode, applied, err)
			}
		})
	}
}
